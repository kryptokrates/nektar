#!/usr/bin/env node
// nektar MCP server.
//   stdio (default):      node dist/src/bin.js
//   Streamable HTTP+REST:  NEKTAR_HTTP_PORT=8080 node dist/src/bin.js
// Backend via NEKTAR_NETWORK=mock (default, offline, zero setup) | regtest
// (persistent, see runtime.ts). Diagnostics go to stderr — stdout is the MCP channel.
import { openRuntime, configFromEnv } from './runtime.js'
import { serveStdio } from './mcp-server.js'
import { createHttpApi } from './http.js'
import { existsSync, readFileSync } from 'node:fs'

// --env <mock|regtest|ttn|main> + optional .env beside cwd → minimal-typing boot.
// .env lines are applied only where the var isn't already set (shell wins).
const argv = process.argv.slice(2)
const envFlag = argv.includes('--env') ? argv[argv.indexOf('--env') + 1] : undefined
if (existsSync('.env')) {
  for (const line of readFileSync('.env', 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m && process.env[m[1]!] === undefined) process.env[m[1]!] = m[2]!.trim()
  }
}
if (envFlag) {
  if (!/^(mock|regtest|ttn|main)$/.test(envFlag)) { process.stderr.write(`nektar: --env must be mock|regtest|ttn|main, got "${envFlag}"\n`); process.exit(2) }
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
  const rvs = (process.env.NEKTAR_OVERLAY_URL ?? '').split(',').map(s => s.trim()).filter(Boolean)
  rt.mem.automation = await openAutomation(rt.mem, {
    network: rt.network, dataDir: rt.network === 'mock' && !process.env.NEKTAR_DATA_DIR ? undefined : (process.env.NEKTAR_DATA_DIR ?? '.nektar'),
    rendezvousUrls: rvs, keeperUrl: process.env.NEKTAR_MAILBOX_URL, overlayToken: process.env.NEKTAR_OVERLAY_TOKEN,
    marketHosts: (process.env.NEKTAR_MARKET_HOSTS ?? '').split(',').map(s => s.trim()).filter(Boolean),
    intervalMs: Number(process.env.NEKTAR_AUTOMATION_INTERVAL_MS ?? 5000),
    call: (tool, args) => callTool(rt.mem, tool, args, { ops: rt.ops }),
    opts: rt.network === 'main' ? { saleMaxFee: 500 } : {},
    log: line => process.stderr.write(`nektar: ${line}
`)
  })
  process.stderr.write(`nektar: automation on — ${rvs.length} rendezvous host(s); policies and approvals via 'pnpm automation'
`)
}
const port = process.env.NEKTAR_HTTP_PORT
if (port) {
  createHttpApi(rt.mem, rt.ops).listen(Number(port), process.env.NEKTAR_HTTP_HOST ?? '127.0.0.1', () => process.stderr.write(`nektar: HTTP + MCP on :${port} (/v1, /mcp)\n`))
}
let stopping = false
const stop = async () => { if (stopping) return; stopping = true; await rt.mem.automation?.stop().catch(() => {}); await rt.close().catch(() => {}); process.exit(0) }
process.on('SIGINT', () => { void stop() }); process.on('SIGTERM', () => { void stop() })
if (!port) {
  await serveStdio(rt.mem, rt.ops)
  // the MCP client went away → exit (otherwise `docker run -i` containers leak
  // and several instances end up sharing one data volume)
  process.stdin.on('end', () => { void stop() }); process.stdin.on('close', () => { void stop() })
}
