#!/usr/bin/env node
// nektar MCP server.
//   stdio (default):      node dist/src/bin.js
//   Streamable HTTP+REST:  NEKTAR_HTTP_PORT=8080 node dist/src/bin.js
// Backend via NEKTAR_NETWORK=regtest (default, local Teranode stack) | ttn | main
// (persistent, see runtime.ts). Diagnostics go to stderr — stdout is the MCP channel.
import { openRuntime, configFromEnv } from './runtime.js'
import { serveStdio } from './mcp-server.js'
import { createHttpApi } from './http.js'
import { existsSync, readFileSync } from 'node:fs'

// --env <regtest|ttn|main> + optional .env beside cwd → minimal-typing boot.
// .env lines are applied only where the var isn't already set (shell wins).
const argv = process.argv.slice(2)
const envFlag = argv.includes('--env') ? argv[argv.indexOf('--env') + 1] : undefined
if (existsSync('.env')) {
  for (const line of readFileSync('.env', 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m && process.env[m[1]] === undefined) process.env[m[1]] = m[2].trim()
  }
}
if (envFlag) {
  if (!/^(regtest|ttn|main)$/.test(envFlag)) {
    process.stderr.write(`nektar: --env must be regtest|ttn|main, got "${envFlag}"\n`)
    process.exit(2)
  }
  process.env.NEKTAR_NETWORK = envFlag
}
const portFlag = argv.includes('--port') ? argv[argv.indexOf('--port') + 1] : undefined
if (portFlag) process.env.NEKTAR_HTTP_PORT = portFlag
const hostFlag = argv.includes('--host') ? argv[argv.indexOf('--host') + 1] : undefined
if (hostFlag) process.env.NEKTAR_HTTP_HOST = hostFlag

const rt = await openRuntime(configFromEnv())
process.stderr.write(`nektar: network=${rt.network}\n`)
const { paymentsEnabled, requireOperatorWallet } = await import('./pay.js')
if (paymentsEnabled()) process.stderr.write(`nektar: payments on — settling to ${requireOperatorWallet()}\n`)
if (/^(1|true|yes)$/i.test(process.env.NEKTAR_AUTOMATION ?? '')) {
  const { openAutomation } = await import('./automation/runtime.js')
  const { callTool } = await import('./server.js')
  const mh = (process.env.NEKTAR_MARKET_HOSTS ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  // unattended mode is honored ONLY behind a valid conformance token —
  // it implies the strict gate so a red preflight refuses to start
  const unattended = /^(1|true|yes)$/i.test(process.env.NEKTAR_UNATTENDED ?? '')
  const strict = unattended || /^(1|true|yes)$/i.test(process.env.NEKTAR_AUTOMATION_STRICT ?? '')
  // the transport knows the broadcast endpoint — an HTTP answer proves the
  // socket where a txState 'unknown' cannot
  const broadcasterProbe = async (): Promise<boolean> => {
    try {
      if (rt.network === 'regtest') {
        const url = process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx'
        await fetch(url, { method: 'POST', body: '', signal: AbortSignal.timeout(3_000) })
        return true // any HTTP answer = endpoint up
      }
      const base = process.env.NEKTAR_BROADCASTER_URL ?? 'https://arcade.gorillapool.io'
      await fetch(`${base}/tx/${'00'.repeat(32)}`, { signal: AbortSignal.timeout(5_000) })
      return true
    } catch {
      return false
    }
  }
  rt.mem.automation = await openAutomation(rt.mem, {
    network: rt.network,
    dataDir: process.env.NEKTAR_DATA_DIR ?? '.nektar',
    marketHosts: mh,
    localHosts: process.env.NEKTAR_MARKET_LOCAL_HOSTS === '1',
    boxOffice: process.env.NEKTAR_BBOX_OFFICE ?? undefined,
    logStream: process.env.NEKTAR_LOG_STREAM ?? undefined,
    strict,
    unattended,
    broadcasterProbe,
    publishFinger: /^(1|true|yes)$/i.test(process.env.NEKTAR_BFINGER_PUBLISH ?? ''),
    watchdogMs: process.env.NEKTAR_WATCHDOG_MS ? Number(process.env.NEKTAR_WATCHDOG_MS) : undefined,
    intervalMs: Number(process.env.NEKTAR_AUTOMATION_INTERVAL_MS ?? 1000),
    // chains without a status push (polled broadcasters) mark
    // funding trees proven only here — let ops tune the cadence
    upkeepMs: process.env.NEKTAR_AUTOMATION_UPKEEP_MS ? Number(process.env.NEKTAR_AUTOMATION_UPKEEP_MS) : undefined,
    call: (tool, args) => callTool(rt.mem, tool, args, { ops: rt.ops }),
    opts: {
      ...(rt.network === 'main' ? { saleMaxFee: 500 } : {}),
      ...(process.env.NEKTAR_OFFER_TTL_BLOCKS ? { offerTtlBlocks: Number(process.env.NEKTAR_OFFER_TTL_BLOCKS) } : {})
    },
    log: (line) =>
      process.stderr.write(`nektar: ${line}
`)
  })
  process.stderr.write(`nektar: automation on — ${mh.length} market host(s); policies and approvals via 'pnpm automation'
`)
  if (mh.length === 0)
    process.stderr.write(`nektar: automation: NEKTAR_MARKET_HOSTS is empty — wants and offers cannot be published
`)
}
const port = process.env.NEKTAR_HTTP_PORT
if (port) {
  createHttpApi(rt.mem, rt.ops).listen(Number(port), process.env.NEKTAR_HTTP_HOST ?? '127.0.0.1', () =>
    process.stderr.write(`nektar: HTTP + MCP on :${port} (/v1, /mcp)\n`)
  )
}
let stopping = false
const stop = async () => {
  if (stopping) return
  stopping = true
  await rt.mem.automation?.stop().catch(() => {})
  await rt.close().catch(() => {})
  process.exit(0)
}
process.on('SIGINT', () => {
  void stop()
})
process.on('SIGTERM', () => {
  void stop()
})
if (!port) {
  await serveStdio(rt.mem, rt.ops)
  // the MCP client went away → exit (otherwise `docker run -i` containers leak
  // and several instances end up sharing one data volume)
  process.stdin.on('end', () => {
    void stop()
  })
  process.stdin.on('close', () => {
    void stop()
  })
}
