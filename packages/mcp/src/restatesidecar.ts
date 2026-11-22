// packages/mcp/src/restatesidecar.ts — the bundled executor plane for host
// builds (the release artifact). Resolution order, cheapest first:
//   1. NEKTAR_RESTATE_URL set            → external, nothing to do
//   2. a live admin API answers          → adopt it (user-run restate, or a
//      second nektar instance already spawned one — ports are fixed so two
//      instances share, never double-spawn)
//   3. bundled restate-server exists      → spawn it as a sidecar: native on
//      linux/darwin, through wsl.exe on win32 (restatedev ships no windows
//      binary; WSL2 localhostForwarding mirrors the ports onto Windows).
//   4. anything fails                    → degrade: the memory path works
//      without restate; the market plane logs what it needs.
// The bundled ports are off nektar's own :8080 HTTP default — 18080/19070.
import { spawn, spawnSync, type ChildProcess, type SpawnOptions } from 'node:child_process'
import { closeSync, existsSync, mkdirSync, openSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { nektarEnv, setNektarEnv } from '@nektar/core'

const INGRESS_PORT = 18080
const ADMIN_PORT = 19070
// dist/src/bin.js → ../../ = artifact root in the deploy tree; in the repo
// checkout it lands on packages/mcp/restate which never exists → degrade path
const RESTATE_DIR = join(dirname(fileURLToPath(import.meta.url)), '../../restate')
export type SidecarState = 'configured' | 'adopted' | 'spawned' | 'degraded'

type SpawnImpl = (cmd: string, args: string[], opts: SpawnOptions) => ChildProcess

async function adminAlive(admin: string, fetchImpl: typeof fetch, timeoutMs = 1500): Promise<boolean> {
  try {
    const r = await fetchImpl(`${admin}/version`, { signal: AbortSignal.timeout(timeoutMs) })
    // /version is the admin liveness probe — any HTTP answer is a live restate
    // (a 4xx still means a restate-shaped thing answered, not dead air)
    return r.status < 500 || r.ok
  } catch {
    return false
  }
}

/** wsl readiness: 'none' (no wsl.exe / feature off), 'nodistro' (WSL present
 *  but no installed distribution — `wsl --status` lies about usability, seen
 *  on the windows-latest CI runner), 'ok' (`wsl -e true` actually ran). */
function wslStatus(spawnSyncImpl: typeof spawnSync): 'none' | 'nodistro' | 'ok' {
  try {
    if (spawnSyncImpl('wsl.exe', ['--status'], { stdio: 'pipe', timeout: 5000 }).status !== 0) return 'none'
    if (spawnSyncImpl('wsl.exe', ['-e', 'true'], { stdio: 'pipe', timeout: 15_000 }).status !== 0) return 'nodistro'
    return 'ok'
  } catch {
    return 'none'
  }
}

/** windows path → /mnt/<drive>/... — needs the real wsl.exe, not a stub */
function wslpath(winPath: string, spawnSyncImpl: typeof spawnSync): string {
  const r = spawnSyncImpl('wsl.exe', ['-e', 'wslpath', '-a', winPath], { encoding: 'utf8', timeout: 5000 })
  if (r.status !== 0 || !r.stdout?.trim()) throw new Error(`wslpath failed for ${winPath}`)
  return r.stdout.trim()
}

/** the windows host IP as WSL sees it — default route's gateway. The
 *  restate inside WSL calls our endpoint on this, not on its own localhost. */
function wslHostGateway(spawnSyncImpl: typeof spawnSync): string | undefined {
  const r = spawnSyncImpl('wsl.exe', ['-e', 'sh', '-c', 'ip route show default'], { encoding: 'utf8', timeout: 5000 })
  const m = r.status === 0 ? r.stdout.match(/default via (\S+)/) : null
  return m?.[1]
}

export async function ensureRestate(
  opts: {
    log?: (msg: string) => void
    env?: NodeJS.ProcessEnv
    fetchImpl?: typeof fetch
    spawnImpl?: SpawnImpl
    spawnSyncImpl?: typeof spawnSync
    platform?: NodeJS.Platform
    restateDir?: string
    // registers a teardown — killing the spawned process differs per platform:
    // native is child.kill(), the WSL bridge must pkill the linux binary inside
    // the VM (wsl.exe's death does NOT propagate — the process is orphaned onto
    // the WSL init, seen live)
    onCleanup?: (fn: () => void) => void
  } = {}
): Promise<SidecarState> {
  const env = opts.env ?? process.env
  const log = opts.log ?? (() => {})
  const fetchImpl = opts.fetchImpl ?? fetch
  const spawnImpl = opts.spawnImpl ?? spawn
  const spawnSyncImpl = opts.spawnSyncImpl ?? spawnSync
  const platform = opts.platform ?? process.platform
  const restateDir = opts.restateDir ?? RESTATE_DIR

  if (nektarEnv('NEKTAR_RESTATE_URL', env)) return 'configured'

  const ingressUrl = `http://127.0.0.1:${INGRESS_PORT}`
  const adminUrl = `http://127.0.0.1:${ADMIN_PORT}`
  for (const [admin, ingress] of [
    [adminUrl, ingressUrl],
    ['http://127.0.0.1:9070', 'http://127.0.0.1:8080']
  ] as const) {
    if (await adminAlive(admin, fetchImpl)) {
      setNektarEnv('NEKTAR_RESTATE_URL', ingress, env)
      setNektarEnv('NEKTAR_RESTATE_ADMIN', admin, env)
      // no win32 restate build exists — an adopted server here always lives
      // inside WSL/docker, so the endpoint must advertise the host's gateway
      // IP for it to call back (loopback is the VM, not us). An explicit
      // NEKTAR_RESTATE_ENDPOINT_URI still wins.
      if (platform === 'win32' && nektarEnv('NEKTAR_RESTATE_ENDPOINT_URI', env) === undefined) {
        const gw = wslHostGateway(spawnSyncImpl)
        if (gw) setNektarEnv('NEKTAR_RESTATE_ENDPOINT_URI', `http://${gw}:${nektarEnv('NEKTAR_RESTATE_ENDPOINT_PORT', env) ?? '9080'}`, env)
      }
      log(`restate: adopting live server at ${admin}`)
      return 'adopted'
    }
  }

  const bin = join(restateDir, 'restate-server')
  if (!existsSync(bin)) {
    log('restate: no bundled server and none answering — market plane disabled (set NEKTAR_RESTATE_URL to enable deals)')
    return 'degraded'
  }

  const dataDir = nektarEnv('NEKTAR_DATA_DIR', env) ?? '.nektar'
  const work = join(dataDir, 'restate')

  let child: ChildProcess
  let wslBin: string | undefined
  let logFd: number | undefined
  try {
    mkdirSync(join(work, 'restate-data'), { recursive: true })
    const toml = join(work, 'nektar-restate.toml')
    writeFileSync(
      toml,
      `# nektar sidecar — generated at boot; ports sit off nektar's own :8080
disable-telemetry = true

[ingress]
bind-address = "127.0.0.1:${INGRESS_PORT}"

[admin]
bind-address = "127.0.0.1:${ADMIN_PORT}"
`
    )
    logFd = openSync(join(work, 'restate.log'), 'a')
    if (platform === 'win32') {
      const ws = wslStatus(spawnSyncImpl)
      if (ws !== 'ok') {
        log(
          ws === 'nodistro'
            ? 'restate: bundled server is a linux binary and WSL has no installed distro — market plane disabled (run once: wsl --install -d Ubuntu; or set NEKTAR_RESTATE_URL)'
            : 'restate: bundled server is a linux binary and WSL is not installed — market plane disabled (install WSL once: wsl --install; or set NEKTAR_RESTATE_URL)'
        )
        return 'degraded'
      }
      wslBin = wslpath(bin, spawnSyncImpl)
      child = spawnImpl(
        'wsl.exe',
        [
          '-e',
          wslBin,
          '--base-dir',
          wslpath(join(work, 'restate-data'), spawnSyncImpl),
          '--listen-mode',
          'tcp',
          '--config-file',
          wslpath(toml, spawnSyncImpl),
          '--no-logo'
        ],
        { stdio: ['ignore', logFd, logFd] }
      )
      const binPath = wslBin
      opts.onCleanup?.(() => {
        try {
          child.kill()
        } catch {
          /* already gone */
        }
        try {
          spawnSyncImpl('wsl.exe', ['-e', 'pkill', '-f', binPath], { timeout: 5000 })
        } catch {
          /* wsl gone or pkill failed — the orphan still answers adoption */
        }
      })
    } else {
      child = spawnImpl(bin, ['--base-dir', join(work, 'restate-data'), '--listen-mode', 'tcp', '--config-file', toml, '--no-logo'], {
        stdio: ['ignore', logFd, logFd]
      })
      opts.onCleanup?.(() => {
        try {
          child.kill()
        } catch {
          /* already gone */
        }
      })
    }
  } catch (e) {
    if (logFd !== undefined) {
      try {
        closeSync(logFd)
      } catch {
        /* best effort */
      }
    }
    log(`restate: spawn failed — ${(e as Error).message}; market plane disabled`)
    return 'degraded'
  }

  // restate takes a few seconds to provision a fresh cluster
  const deadline = Date.now() + 30_000
  while (Date.now() < deadline) {
    if (await adminAlive(adminUrl, fetchImpl, 2000)) {
      setNektarEnv('NEKTAR_RESTATE_URL', ingressUrl, env)
      setNektarEnv('NEKTAR_RESTATE_ADMIN', adminUrl, env)
      if (wslBin !== undefined && nektarEnv('NEKTAR_RESTATE_ENDPOINT_URI', env) === undefined) {
        // the endpoint must advertise the windows host as WSL sees it —
        // 127.0.0.1 inside WSL is the VM, not us
        const gw = wslHostGateway(spawnSyncImpl)
        if (gw) setNektarEnv('NEKTAR_RESTATE_ENDPOINT_URI', `http://${gw}:${nektarEnv('NEKTAR_RESTATE_ENDPOINT_PORT', env) ?? '9080'}`, env)
      }
      log(`restate: sidecar up (ingress :${INGRESS_PORT}, admin :${ADMIN_PORT}${wslBin !== undefined ? ', via WSL' : ''})`)
      return 'spawned'
    }
    await new Promise((r) => setTimeout(r, 500))
  }
  try {
    child.kill()
  } catch {
    /* already gone */
  }
  if (wslBin !== undefined) {
    try {
      spawnSyncImpl('wsl.exe', ['-e', 'pkill', '-f', wslBin], { timeout: 5000 })
    } catch {
      /* best effort */
    }
  }
  log(`restate: sidecar did not come up within 30s — market plane disabled (see ${join(work, 'restate.log')})`)
  return 'degraded'
}
