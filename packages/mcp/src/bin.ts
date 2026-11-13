#!/usr/bin/env node
// nektar MCP server.
//   stdio (default):      node dist/src/bin.js
//   Streamable HTTP+REST:  NEKTAR_HTTP_PORT=8080 node dist/src/bin.js
// Backend via NEKTAR_NETWORK=regtest (default, local Teranode stack) | ttn | main
// (persistent, see runtime.ts). Diagnostics go to stderr — stdout is the MCP channel.
import { nektarEnv, setNektarEnv } from '@nektar/core'
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
    if (m && nektarEnv(m[1]) === undefined) setNektarEnv(m[1], m[2].trim())
  }
}
if (envFlag) {
  if (!/^(regtest|ttn|main)$/.test(envFlag)) {
    process.stderr.write(`nektar: --env must be regtest|ttn|main, got "${envFlag}"\n`)
    process.exit(2)
  }
  setNektarEnv('NEKTAR_NETWORK', envFlag)
}
const portFlag = argv.includes('--port') ? argv[argv.indexOf('--port') + 1] : undefined
if (portFlag) setNektarEnv('NEKTAR_HTTP_PORT', portFlag)
const hostFlag = argv.includes('--host') ? argv[argv.indexOf('--host') + 1] : undefined
if (hostFlag) setNektarEnv('NEKTAR_HTTP_HOST', hostFlag)

const rt = await openRuntime(configFromEnv())
process.stderr.write(`nektar: network=${rt.network}\n`)
const { paymentsEnabled, requireOperatorWallet } = await import('./pay.js')
if (paymentsEnabled()) {
  requireOperatorWallet()
  process.stderr.write('nektar: payments on\n')
}
if (/^(1|true|yes)$/i.test(nektarEnv('NEKTAR_AUTOMATION') ?? '')) {
  const { openAutomation } = await import('@nektar/automation')
  const { callTool } = await import('./server.js')
  const mh = (nektarEnv('NEKTAR_MARKET_HOSTS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  // unattended mode is honored ONLY behind a valid conformance token —
  // it implies the strict gate so a red preflight refuses to start
  const unattended = /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_UNATTENDED') ?? '')
  const strict = unattended || /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_AUTOMATION_STRICT') ?? '')
  // the transport knows the broadcast endpoint — an HTTP answer proves the
  // socket where a txState 'unknown' cannot
  const broadcasterProbe = async (): Promise<boolean> => {
    try {
      if (rt.network === 'regtest') {
        const url = nektarEnv('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx'
        await fetch(url, { method: 'POST', body: '', signal: AbortSignal.timeout(3_000) })
        return true // any HTTP answer = endpoint up
      }
      const base = nektarEnv('NEKTAR_BROADCASTER_URL') ?? 'https://arcade.gorillapool.io'
      await fetch(`${base}/tx/${'00'.repeat(32)}`, { signal: AbortSignal.timeout(5_000) })
      return true
    } catch {
      return false
    }
  }
  rt.mem.automation = await openAutomation(rt.mem, {
    network: rt.network,
    dataDir: nektarEnv('NEKTAR_DATA_DIR') ?? '.nektar',
    marketHosts: mh,
    localHosts: nektarEnv('NEKTAR_MARKET_LOCAL_HOSTS') === '1',
    boxOffice: nektarEnv('NEKTAR_BBOX_OFFICE') ?? undefined,
    logStream: nektarEnv('NEKTAR_LOG_STREAM') ?? undefined,
    strict,
    unattended,
    broadcasterProbe,
    publishFinger: /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_BFINGER_PUBLISH') ?? ''),
    watchdogMs: nektarEnv('NEKTAR_WATCHDOG_MS') ? Number(nektarEnv('NEKTAR_WATCHDOG_MS')) : undefined,
    intervalMs: Number(nektarEnv('NEKTAR_AUTOMATION_INTERVAL_MS') ?? 1000),
    watchHosts: (nektarEnv('NEKTAR_WATCH_URLS') ?? '')
      .split(',')
      .map((u) => u.trim())
      .filter((u) => u !== ''),
    // our own shim's public URL — goes into the bfinger record so peers can
    // /notify our long-polls on outbound sends (needs NEKTAR_BFINGER_PUBLISH)
    watchPublic: nektarEnv('NEKTAR_WATCH_PUBLIC') !== undefined && nektarEnv('NEKTAR_WATCH_PUBLIC') !== '' ? nektarEnv('NEKTAR_WATCH_PUBLIC') : undefined,
    // per-read failure ceiling on host lookups — a host slower than this is
    // unreachable for the pass, never a wait on the hot path
    laneReadTimeoutMs: nektarEnv('NEKTAR_LANE_READ_TIMEOUT_MS') ? Number(nektarEnv('NEKTAR_LANE_READ_TIMEOUT_MS')) : undefined,
    // hot-path cadence while any deal is live — wakes and inbound items hold
    // it; silence falls back to intervalMs
    hotMs: nektarEnv('NEKTAR_HOT_INTERVAL_MS') ? Number(nektarEnv('NEKTAR_HOT_INTERVAL_MS')) : undefined,
    // the smallest collecting window before an acceptable offer may
    // early-select — a cheaper offer inside the window still wins; collectMs
    // remains the outer ceiling
    minCollectMs: nektarEnv('NEKTAR_MIN_COLLECT_MS') ? Number(nektarEnv('NEKTAR_MIN_COLLECT_MS')) : undefined,
    // chains without a status push (polled broadcasters) mark
    // funding trees proven only here — let ops tune the cadence
    upkeepMs: nektarEnv('NEKTAR_AUTOMATION_UPKEEP_MS') ? Number(nektarEnv('NEKTAR_AUTOMATION_UPKEEP_MS')) : undefined,
    call: (tool, args) => callTool(rt.mem, tool, args, { ops: rt.ops }),
    opts: {
      ...(rt.network === 'main' ? { saleMaxFee: 500 } : {}),
      ...(nektarEnv('NEKTAR_OFFER_TTL_BLOCKS') ? { offerTtlBlocks: Number(nektarEnv('NEKTAR_OFFER_TTL_BLOCKS')) } : {}),
      ...(nektarEnv('NEKTAR_REFUND_BLOCKS') ? { refundBlocks: Number(nektarEnv('NEKTAR_REFUND_BLOCKS')) } : {}),
      // the direct rail — NEKTAR_DIRECT_MAX_SATS caps buyer-side outright
      // payments (payment-final, no refund); NEKTAR_DIRECT_ACCEPT=1 advertises
      // it on our quotes. Both off by default: escrow stays the default rail.
      ...(nektarEnv('NEKTAR_DIRECT_MAX_SATS') ? { directMaxSats: Number(nektarEnv('NEKTAR_DIRECT_MAX_SATS')) } : {}),
      ...(nektarEnv('NEKTAR_DIRECT_ACCEPT') === '1' ? { directAccept: true } : {}),
      // NEKTAR_AUTO_USEFUL=1 — a verified disclosure closes the deal as
      // `useful` without waiting on an operator markUsefulness call
      ...(nektarEnv('NEKTAR_AUTO_USEFUL') === '1' ? { autoUseful: true } : {})
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
const port = nektarEnv('NEKTAR_HTTP_PORT')
if (port) {
  createHttpApi(rt.mem, rt.ops).listen(Number(port), nektarEnv('NEKTAR_HTTP_HOST') ?? '127.0.0.1', () =>
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
