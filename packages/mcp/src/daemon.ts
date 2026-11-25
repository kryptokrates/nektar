// daemon.ts — zero-config network default + local daemon discovery ("one brain,
// many mouths"). Lets stdio MCP clients transparently bridge to an already-live
// loopback nektar instance instead of colliding on instance.lock, and lets
// portable hooks / native plugins discover the running server with zero config.
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { nektarEnv, sparse, type EnvSource } from '@nektar/core'

export const DEFAULT_DAEMON_PORT = 18420
const DAEMON_FILE = 'daemon.json'

export interface DaemonInfo {
  url: string
  pid: number
  network: string
  startedAt: number
  token?: string
}

export interface DiscoveredDaemon {
  url: string
  mcpUrl: string
  token?: string
  network?: string
}

/** Zero-config network resolution: explicit export > --env > profile > .env.
 *  When completely unconfigured, pick `regtest` ONLY if regtest credentials
 *  are present (preserving local Teranode dev flows); otherwise default to
 *  `testnet` so bare `./nektar` or `docker run` boots cleanly out of the box. */
export function resolveDefaultNetwork(opts: {
  envNet?: string
  flagNet?: string
  profileNet?: string
  dotNet?: string
  env?: EnvSource
  dot?: Record<string, string>
}): { network: 'regtest' | 'testnet' | 'main'; defaulted: boolean } {
  const explicit = opts.envNet ?? opts.flagNet ?? opts.profileNet ?? opts.dotNet
  if (explicit === 'regtest' || explicit === 'testnet' || explicit === 'main') {
    return { network: explicit, defaulted: false }
  }
  const env = opts.env ?? process.env
  const dot = opts.dot ?? {}
  const hasRegtestCreds =
    nektarEnv('NEKTAR_RPC_PASS', env) !== undefined ||
    nektarEnv('NEKTAR_RPC_PASS_FILE', env) !== undefined ||
    nektarEnv('NEKTAR_REGTEST_WIF', env) !== undefined ||
    sparse(dot['NEKTAR_RPC_PASS']) !== undefined ||
    sparse(dot['NEKTAR_REGTEST_WIF']) !== undefined
  return { network: hasRegtestCreds ? 'regtest' : 'testnet', defaulted: true }
}

/** Persist the running HTTP surface location (and optional local bearer token,
 *  mode 0600) for hooks, plugins, and 2nd-client bridges. */
export function writeDaemonInfo(dataDir: string, info: DaemonInfo): void {
  try {
    mkdirSync(dataDir, { recursive: true })
    const file = join(dataDir, DAEMON_FILE)
    writeFileSync(file, JSON.stringify(info, null, 2) + '\n', { mode: 0o600 })
    try {
      chmodSync(file, 0o600)
    } catch {
      /* Windows NTFS inherits user ACL */
    }
  } catch {
    /* non-fatal discovery hint */
  }
}

/** Remove daemon.json only if it still belongs to this PID (never clobber a newer daemon). */
export function clearDaemonInfo(dataDir: string, pid: number): void {
  const p = join(dataDir, DAEMON_FILE)
  if (!existsSync(p)) return
  try {
    const cur = JSON.parse(readFileSync(p, 'utf8')) as { pid?: number }
    if (cur.pid === undefined || cur.pid === pid) rmSync(p, { force: true })
  } catch {
    /* best effort */
  }
}

function readDaemonEntry(dir: string): { url: string; token?: string } | undefined {
  const p = join(dir, DAEMON_FILE)
  if (!existsSync(p)) return undefined
  try {
    const j = JSON.parse(readFileSync(p, 'utf8')) as { url?: unknown; token?: unknown }
    if (typeof j.url !== 'string' || !/^https?:\/\//i.test(j.url)) return undefined
    const token = typeof j.token === 'string' && j.token.trim() !== '' ? j.token.trim() : undefined
    return { url: j.url.replace(/\/+$/, ''), ...(token ? { token } : {}) }
  } catch {
    return undefined
  }
}

/** Probe candidate loopback/configured URLs and return the first live nektar
 *  daemon verified via `/.well-known/agent.json` (`name === 'nektar'`). */
export async function discoverDaemon(
  opts: {
    dataDir?: string
    env?: EnvSource
    fetchImpl?: typeof fetch
    timeoutMs?: number
    candidates?: string[]
  } = {}
): Promise<DiscoveredDaemon | undefined> {
  const env = opts.env ?? process.env
  const fetchImpl = opts.fetchImpl ?? fetch
  const timeoutMs = opts.timeoutMs ?? 600
  const envToken = nektarEnv('NEKTAR_HTTP_TOKEN', env)

  const seen = new Set<string>()
  const list: Array<{ url: string; token?: string }> = []
  const add = (u: string | undefined, tok?: string) => {
    if (!u) return
    const norm = u.trim().replace(/\/+$/, '')
    if (!norm || seen.has(norm)) return
    seen.add(norm)
    list.push({ url: norm, ...(tok ? { token: tok } : {}) })
  }

  if (opts.candidates !== undefined) {
    for (const c of opts.candidates) add(c)
  } else {
    add(nektarEnv('NEKTAR_DAEMON_URL', env))
    add(nektarEnv('NEKTAR_URL', env))
    const dataDir = opts.dataDir ?? nektarEnv('NEKTAR_DATA_DIR', env) ?? '.nektar'
    const d1 = readDaemonEntry(dataDir)
    if (d1) add(d1.url, d1.token)
    const d2 = readDaemonEntry(join(homedir(), '.nektar'))
    if (d2) add(d2.url, d2.token)
    const envPort = nektarEnv('NEKTAR_HTTP_PORT', env)
    if (envPort) add(`http://127.0.0.1:${envPort}`)
    add(`http://127.0.0.1:${DEFAULT_DAEMON_PORT}`)
    add('http://127.0.0.1:8080')
  }

  for (const item of list) {
    const base = item.url
    try {
      const r = await fetchImpl(`${base}/.well-known/agent.json`, { signal: AbortSignal.timeout(timeoutMs) })
      if (!r.ok) continue
      const card = (await r.json()) as {
        name?: string
        network?: string
        endpoints?: { mcp?: string }
      }
      if (card.name !== 'nektar') continue
      const token = envToken ?? item.token
      return {
        url: base,
        mcpUrl: `${base}/mcp`,
        ...(token ? { token } : {}),
        ...(typeof card.network === 'string' ? { network: card.network } : {})
      }
    } catch {
      /* unreachable or not nektar — try next candidate */
    }
  }
  return undefined
}
