#!/usr/bin/env node
// mamem MCP server.
//   stdio (default):      node dist/src/bin.js
//   Streamable HTTP+REST:  MAMEM_HTTP_PORT=8080 node dist/src/bin.js
// Backend via MAMEM_NETWORK=mock (default, offline, zero setup) | regtest
// (persistent, see runtime.ts). Diagnostics go to stderr — stdout is the MCP channel.
import { openRuntime, configFromEnv } from './runtime.js'
import { serveStdio } from './server.js'
import { createHttpApi } from './http.js'

const rt = await openRuntime(configFromEnv())
process.stderr.write(`mamem: network=${rt.network}\n`)
const port = process.env.MAMEM_HTTP_PORT
if (port) {
  createHttpApi(rt.mem).listen(Number(port), process.env.MAMEM_HTTP_HOST ?? '127.0.0.1', () => process.stderr.write(`mamem: HTTP + MCP on :${port} (/v1, /mcp)\n`))
}
let stopping = false
const stop = async () => { if (stopping) return; stopping = true; await rt.close().catch(() => {}); process.exit(0) }
process.on('SIGINT', () => { void stop() }); process.on('SIGTERM', () => { void stop() })
if (!port) {
  await serveStdio(rt.mem)
  // the MCP client went away → exit (otherwise `docker run -i` containers leak
  // and several instances end up sharing one data volume)
  process.stdin.on('end', () => { void stop() }); process.stdin.on('close', () => { void stop() })
}
