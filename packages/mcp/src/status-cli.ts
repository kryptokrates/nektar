// status-cli.ts — one-command health + discovery status for both source
// checkouts and no-build release artifacts (`nektar status`).
import { existsSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { nektarEnv, writeOut, type EnvSource } from '@nektar/core'
import { discoverDaemon } from './daemon.js'

export interface StatusIo {
  out?: (s: string) => void
  fetchImpl?: typeof fetch
  env?: EnvSource
}

export async function runStatusCli(argv: string[], io: StatusIo = {}): Promise<number> {
  const out = io.out ?? writeOut
  const fetchImpl = io.fetchImpl ?? fetch
  const env = io.env ?? process.env
  const asJson = argv.includes('--json')
  const dataDir = resolve(nektarEnv('NEKTAR_DATA_DIR', env) ?? '.nektar')

  const daemon = await discoverDaemon({ dataDir, env, fetchImpl, timeoutMs: 800 })
  let doctor: Record<string, unknown> | undefined
  if (daemon) {
    const headers: Record<string, string> = { 'content-type': 'application/json' }
    if (daemon.token) headers.authorization = `Bearer ${daemon.token}`
    try {
      const r = await fetchImpl(`${daemon.url}/v1/tools/doctor`, {
        method: 'POST',
        headers,
        body: '{}',
        signal: AbortSignal.timeout(2_000)
      })
      if (r.ok) doctor = (await r.json()) as Record<string, unknown>
    } catch {
      /* partial status still useful */
    }
  }

  let restate: string = nektarEnv('NEKTAR_RESTATE_URL', env) ? `configured (${nektarEnv('NEKTAR_RESTATE_URL', env)})` : 'unavailable'
  if (restate === 'unavailable') {
    for (const u of ['http://127.0.0.1:19070', 'http://127.0.0.1:9070']) {
      try {
        const r = await fetchImpl(`${u}/version`, { signal: AbortSignal.timeout(600) })
        if (r.status < 500 || r.ok) {
          restate = `live (${u})`
          break
        }
      } catch {
        /* try next */
      }
    }
  }
  if (restate === 'unavailable') {
    const bin = join(fileURLToPath(new URL('../../restate', import.meta.url)), 'restate-server')
    if (existsSync(bin)) restate = `bundled (${bin})`
  }

  const report = {
    daemon: daemon ? { alive: true, url: daemon.url, mcpUrl: daemon.mcpUrl, network: daemon.network } : { alive: false },
    restate,
    dataDir,
    ...(doctor ? { doctor } : {})
  }

  if (asJson) {
    out(JSON.stringify(report, null, 2) + '\n')
    return daemon ? 0 : 1
  }

  out(`nektar status\n`)
  out(`  daemon  : ${daemon ? `UP at ${daemon.url} (${daemon.network ?? 'unknown'})` : 'not running'}\n`)
  out(`  restate : ${restate}\n`)
  out(`  dataDir : ${dataDir}${existsSync(dataDir) ? '' : ' (not created yet)'}\n`)
  if (doctor) {
    const shards = Array.isArray(doctor.shardNames) ? (doctor.shardNames as string[]).join(', ') : ''
    const recCount = typeof doctor.records === 'number' ? doctor.records : 0
    const shardCount = typeof doctor.shards === 'number' ? doctor.shards : 0
    out(`  memory  : ${recCount} record(s) across ${shardCount} shard(s)${shards ? ` [${shards}]` : ''}\n`)
    const hints = Array.isArray(doctor.hints) ? (doctor.hints as string[]) : []
    for (const h of hints) out(`  hint    : ${h}\n`)
  } else {
    out(`  next    : start local daemon with \`nektar --port 18420\` (or \`nektar dock\` to wire AI apps)\n`)
  }
  return daemon ? 0 : 1
}
