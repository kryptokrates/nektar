// API-001: minimal versioned HTTP API (node:http, no framework). Same tools
// as MCP, plus public read endpoints. Every route is in OPENAPI (tested).
import { createServer, type IncomingMessage, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import type { MamemMemory } from './memory.js'
import { TOOLS, visibleTools, callTool } from './server.js'

const json = (v: unknown) => JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? x.toString() : x instanceof Uint8Array ? Buffer.from(x).toString('hex') : x))

export const OPENAPI = {
  openapi: '3.1.0',
  info: { title: 'mamem API', version: '1.0.0', description: 'Verifiable encrypted agent memory on BSV. Proofs establish inclusion, integrity and existence-by-time — never truth.' },
  paths: {
    '/mcp': {
      post: { summary: 'MCP Streamable HTTP (JSON-RPC; initialize creates a session, mcp-session-id header thereafter)', responses: { 200: { description: 'JSON or SSE stream' } } },
      get: { summary: 'MCP server-to-client SSE stream for a session', responses: { 200: { description: 'SSE' } } },
      delete: { summary: 'end an MCP session', responses: { 200: { description: 'closed' } } }
    },
    '/v1/health': { get: { summary: 'liveness', responses: { 200: { description: 'ok' } } } },
    '/v1/openapi.json': { get: { summary: 'this document', responses: { 200: { description: 'OpenAPI 3.1' } } } },
    '/v1/metrics': { get: { summary: 'Prometheus metrics (text/plain 0.0.4)', responses: { 200: { description: 'metrics' } } } },
    '/v1/tools': { get: { summary: 'list tools (same as MCP tools/list)', responses: { 200: { description: 'tool schemas' } } } },
    '/v1/tools/{name}': {
      post: {
        summary: 'call a tool (same semantics as MCP tools/call); confidential reads need identity + clearance in the body',
        parameters: [{ name: 'name', in: 'path', required: true, schema: { type: 'string', enum: TOOLS.map(t => t.name) } }],
        requestBody: { content: { 'application/json': { schema: { type: 'object' } } } },
        responses: { 200: { description: 'tool result' }, 400: { description: 'tool error with reason' } }
      }
    },
    '/v1/proof/{shard}/{seq}': {
      get: { summary: 'public proof bundle + verification result for one record', parameters: [{ name: 'shard', in: 'path', required: true, schema: { type: 'string' } }, { name: 'seq', in: 'path', required: true, schema: { type: 'integer' } }], responses: { 200: { description: 'bundle + result' } } }
    },
    '/v1/checkpoint/{shard}': {
      get: { summary: 'latest signed checkpoint of the shard (LOG-007)', parameters: [{ name: 'shard', in: 'path', required: true, schema: { type: 'string' } }], responses: { 200: { description: 'checkpoint + log pubkey' }, 404: { description: 'no anchor yet' } } }
    },
    '/v1/wallet': { get: { summary: 'fee wallet: address, balance, spend caps, runway (main only)', responses: { 200: { description: 'wallet ops' }, 404: { description: 'ops not enabled' } } } },
    '/v1/costs': { get: { summary: 'broadcast cost ledger entries (main only)', responses: { 200: { description: 'cost records' }, 404: { description: 'ops not enabled' } } } }
  }
} as const

async function body (req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const c of req) { size += (c as Buffer).length; if (size > 1_000_000) throw new Error('body too large'); chunks.push(c as Buffer) }
  return chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : {}
}

export function createHttpApi (mem: MamemMemory, ops?: { wallet: () => Promise<unknown>, costs: () => unknown }): Server {
  // MCP Streamable HTTP: one transport + McpServer per session (mcp-session-id)
  const sessions = new Map<string, import('@modelcontextprotocol/sdk/server/streamableHttp.js').StreamableHTTPServerTransport>()
  const mcp = async (req: IncomingMessage, res: import('node:http').ServerResponse) => {
    const { StreamableHTTPServerTransport } = await import('@modelcontextprotocol/sdk/server/streamableHttp.js')
    const { createMcpServer } = await import('./mcp-server.js')
    const sid = req.headers['mcp-session-id']
    const parsed = req.method === 'POST' ? await body(req) : undefined
    let transport = typeof sid === 'string' ? sessions.get(sid) : undefined
    if (!transport) {
      if (req.method !== 'POST' || typeof sid === 'string') { res.writeHead(404, { 'content-type': 'application/json' }); res.end(json({ jsonrpc: '2.0', error: { code: -32001, message: 'unknown MCP session' }, id: null })); return }
      const t: import('@modelcontextprotocol/sdk/server/streamableHttp.js').StreamableHTTPServerTransport = new StreamableHTTPServerTransport({ sessionIdGenerator: () => randomUUID(), onsessioninitialized: id => { sessions.set(id, t) } })
      t.onclose = () => { if (t.sessionId) sessions.delete(t.sessionId) }
      await createMcpServer(mem).connect(t)
      transport = t
    }
    await transport.handleRequest(req, res, parsed)
  }
  const server = createServer(async (req, res) => {
    const send = (code: number, v: unknown, type = 'application/json') => { res.writeHead(code, { 'content-type': type }); res.end(typeof v === 'string' ? v : json(v)) }
    const url = new URL(req.url ?? '/', 'http://x')
    const p = url.pathname.split('/').filter(Boolean)
    // MAMEM_HTTP_TOKEN: when set, everything except /v1/health requires a
    // bearer token — the gate that makes a public bind (0.0.0.0 / a hosted
    // box) safe. Unset = localhost-trusted behaviour (the default bind is
    // 127.0.0.1 anyway).
    const tok = process.env.MAMEM_HTTP_TOKEN
    if (tok && url.pathname !== '/v1/health' && req.headers.authorization !== `Bearer ${tok}`) {
      return send(401, { error: 'unauthorized' })
    }
    try {
      if (url.pathname === '/mcp') return await mcp(req, res)
      if (req.method === 'GET' && url.pathname === '/v1/health') return send(200, { ok: true })
      if (req.method === 'GET' && url.pathname === '/v1/openapi.json') return send(200, OPENAPI)
      if (req.method === 'GET' && url.pathname === '/v1/metrics') return send(200, mem.metrics.render(), 'text/plain; version=0.0.4')
      if (req.method === 'GET' && url.pathname === '/v1/tools') return send(200, { tools: visibleTools() })
      if (req.method === 'POST' && p[0] === 'v1' && p[1] === 'tools' && p.length === 3) {
        if (!visibleTools().some(t => t.name === p[2])) return send(404, { error: `unknown tool ${p[2]}` })
        try { return send(200, await callTool(mem, p[2]!, await body(req))) } catch (e) { return send(400, { error: (e as Error).message }) }
      }
      if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'proof' && p.length === 4) {
        const r = await mem.prove(decodeURIComponent(p[2]!), Number(p[3]))
        return send(200, { result: r.result, bundle: r.bundle ?? null })
      }
      if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'checkpoint' && p.length === 3) {
        const cp = mem.checkpoints(decodeURIComponent(p[2]!)).at(-1)
        return cp ? send(200, { checkpoint: cp, logPublicKey: mem.logPublicKey() }) : send(404, { error: 'no checkpoint' })
      }
      if (req.method === 'GET' && url.pathname === '/v1/wallet') {
        return ops ? send(200, await ops.wallet()) : send(404, { error: 'ops not enabled (main only)' })
      }
      if (req.method === 'GET' && url.pathname === '/v1/costs') {
        return ops ? send(200, { costs: ops.costs() }) : send(404, { error: 'ops not enabled (main only)' })
      }
      return send(404, { error: 'not found' })
    } catch (e) { if (!res.headersSent) send(500, { error: (e as Error).message }) }
  })
  server.on('close', () => { for (const t of sessions.values()) void t.close() })
  return server
}
