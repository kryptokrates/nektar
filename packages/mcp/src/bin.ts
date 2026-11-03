#!/usr/bin/env node
// nektar MCP server.
//   stdio (default):      node dist/src/bin.js
//   Streamable HTTP+REST:  NEKTAR_HTTP_PORT=8080 node dist/src/bin.js
// Backend via NEKTAR_NETWORK=mock (default, offline, zero setup) | regtest
// (persistent, see runtime.ts). Diagnostics go to stderr — stdout is the MCP channel.
import { openRuntime, configFromEnv } from './runtime.js'
import { serveStdio } from './mcp-server.js'
import { createHttpApi } from './http.js'

const rt = await openRuntime(configFromEnv())
process.stderr.write(`nektar: network=${rt.network}\n`)
const { paymentsEnabled, requireOperatorWallet } = await import('./pay.js')
if (paymentsEnabled()) process.stderr.write(`nektar: payments on — settling to ${requireOperatorWallet()}\n`)
const port = process.env.NEKTAR_HTTP_PORT
if (port) {
  createHttpApi(rt.mem, rt.ops).listen(Number(port), process.env.NEKTAR_HTTP_HOST ?? '127.0.0.1', () => process.stderr.write(`nektar: HTTP + MCP on :${port} (/v1, /mcp)\n`))
}
let stopping = false
const stop = async () => { if (stopping) return; stopping = true; await rt.close().catch(() => {}); process.exit(0) }
process.on('SIGINT', () => { void stop() }); process.on('SIGTERM', () => { void stop() })
if (!port) {
  await serveStdio(rt.mem, rt.ops)
  // the MCP client went away → exit (otherwise `docker run -i` containers leak
  // and several instances end up sharing one data volume)
  process.stdin.on('end', () => { void stop() }); process.stdin.on('close', () => { void stop() })
}
