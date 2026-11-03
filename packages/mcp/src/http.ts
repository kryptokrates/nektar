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
    '/v1/verify': {
      get: {
        summary: 'scan a receipt token — traffic-light checks that the answer receipt exists, re-hashes, is provenance-signed + anchored, and its cited fragments are anchored. JSON by default; Accept: text/html (or ?format=html) returns a check page with a QR.',
        parameters: [{ name: 'r', in: 'query', required: true, schema: { type: 'string', description: 'receipt token mamemr1.<base64url hash>' } }],
        responses: { 200: { description: 'check list + ok flag' }, 400: { description: 'missing/malformed token' } }
      }
    },
    '/v1/pay/quote': {
      get: {
        summary: 'payment invoice for a paid tool (MAMEM_PAYMENTS=1) — 402 + PAYMENT-REQUIRED header carrying an x402 v2 PaymentRequired; pay the invoice with a signed tx then retry the tool with payment:{invoiceId,transaction}',
        parameters: [{ name: 'tool', in: 'query', required: true, schema: { type: 'string', enum: ['anchor', 'groundedAnswer', 'commitReceipt'] } }],
        responses: { 402: { description: 'PaymentRequired invoice' }, 404: { description: 'payments disabled' } }
      }
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
    // MAMEM_HTTP_TOKEN: when set, everything except the public read-only
    // endpoints (/v1/health, /v1/verify — a QR scan can't send headers, and
    // verification output is read-only anyway) requires a bearer token —
    // the gate that makes a public bind safe. Unset = localhost-trusted
    // behaviour (the default bind is 127.0.0.1 anyway).
    const tok = process.env.MAMEM_HTTP_TOKEN
    const publicPath = url.pathname === '/v1/health' || url.pathname === '/v1/verify'
    if (tok && !publicPath && req.headers.authorization !== `Bearer ${tok}`) {
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
      if (req.method === 'GET' && url.pathname === '/v1/verify') {
        const tok = url.searchParams.get('r')
        const seq = url.searchParams.get('s')
        if (!tok && seq === null) return send(400, { error: 'missing ?r=<receipt token> or ?s=<receipt seq>' })
        try {
          const { verifyReceipt } = await import('./receipt.js')
          let arg: { token?: string, receiptHash?: string } = { token: tok! }
          if (!tok) {
            // ?s=N — human-readable form: the Nth record on __receipts__
            const rec = (await mem.recall('__receipts__', { seq: Number(seq) })).items[0]
            const h = rec?.text?.split(':')[1]
            if (!h) return send(404, { error: `no receipt at __receipts__ seq ${seq}` })
            arg = { receiptHash: h }
          }
          const result = await verifyReceipt(mem, arg)
          if (url.searchParams.get('format') === 'html' || req.headers.accept?.includes('text/html')) {
            const base = process.env.MAMEM_VERIFY_BASE ?? `http://${req.headers.host ?? 'localhost'}`
            const q = tok ? `r=${tok}` : `s=${seq}`
            return send(200, await verifyPage(result, `${base}/v1/verify?${q}&format=html`), 'text/html; charset=utf-8')
          }
          return send(200, result)
        } catch (e) { return send(400, { error: (e as Error).message }) }
      }
      if (req.method === 'GET' && url.pathname === '/v1/pay/quote') {
        const { paymentsEnabled, PAID_TOOLS, paymentRequired } = await import('./pay.js')
        if (!paymentsEnabled()) return send(404, { error: 'payments disabled (MAMEM_PAYMENTS=1)' })
        const tool = url.searchParams.get('tool') ?? 'groundedAnswer'
        if (!PAID_TOOLS.has(tool)) return send(400, { error: `not a paid tool: ${tool}` })
        const pr = paymentRequired(tool)
        res.writeHead(402, { 'content-type': 'application/json', 'PAYMENT-REQUIRED': Buffer.from(JSON.stringify(pr.required)).toString('base64') })
        return res.end(json(pr))
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

/** Minimal self-contained check page (no external resources — offline-safe).
 *  The QR encodes this same page URL so a phone scan re-opens the result. */
async function verifyPage (r: { ok: boolean, receiptHash: string, checks: Array<{ name: string, ok: boolean | 'info' | 'skip', detail: string }>, receipt?: unknown }, pageUrl: string): Promise<string> {
  const rec = r.receipt as { answer?: string, fragmentRefs?: Array<{ shard: string, seq: number }>, checkResult?: string, modelId?: { provider?: string, release?: string }, payment?: { txid: string, amountSats: number, payTo: string } } | undefined
  const QR = await import('qrcode')
  const qr = await QR.toDataURL(pageUrl, { margin: 1, width: 220 })
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  const icon = (o: boolean | 'info' | 'skip') => o === true ? '✓' : o === false ? '✗' : '·'
  const cls = (o: boolean | 'info' | 'skip') => o === true ? 'ok' : o === false ? 'bad' : 'info'
  return `<!doctype html><html><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>${r.ok ? '✓ verified answer' : '✗ receipt failed'}${rec?.answer ? ` — ${esc(rec.answer.slice(0, 60))}` : ''}</title>
<style>body{font:15px/1.5 ui-monospace,monospace;background:#0d1117;color:#c9d1d9;max-width:560px;margin:24px auto;padding:0 16px}
h1{font-size:18px}.badge{display:inline-block;padding:2px 10px;border-radius:4px;font-weight:700}
.ok{background:#12331c;color:#4ade80}.bad{background:#3d1215;color:#f87171}.info{color:#8b949e}
ul{list-style:none;padding:0}li{margin:6px 0}.d{color:#8b949e;font-size:13px;margin-left:22px}
img{display:block;margin:20px auto;border:8px solid #fff;border-radius:6px}
.ans{background:#161b22;border:1px solid #30363d;border-radius:6px;padding:10px 12px;margin:14px 0}
.ans .h{color:#8b949e;font-size:11px;text-transform:uppercase;letter-spacing:.5px;margin-bottom:4px}
.note{color:#8b949e;font-size:12px;margin-top:18px;border-top:1px solid #30363d;padding-top:12px}
code{word-break:break-all;color:#79c0ff}</style></head><body>
<h1>mamem answer receipt <span class="badge ${cls(r.ok)}">${r.ok ? 'VERIFIED' : 'FAILED'}</span></h1>
<div class=d style="margin-left:0;margin-bottom:8px">${r.ok
  ? 'This exact answer was committed to an anchored, signed log. The checks below prove it was recorded unaltered and that every cited record exists on-chain. It does <b>not</b> prove the answer is true.'
  : 'Something in this receipt did not check out — see the ✗ lines below.'}</div>
<code>${esc(r.receiptHash)}</code>
${rec?.answer !== undefined ? `<div class=ans><div class=h>committed answer — compare with what you were shown</div>${esc(rec.answer)}</div>` : ''}
${rec ? `<div class="d" style="margin-left:0">cites ${(rec.fragmentRefs ?? []).map(f => `${esc(f.shard)}:${f.seq}`).join(', ') || '—'} · model ${esc(`${rec.modelId?.provider ?? 'undeclared'}/${rec.modelId?.release ?? 'undeclared'}`)} (declared) · checkResult ${esc(rec.checkResult ?? '?')}${rec.payment ? ` · paid ${rec.payment.amountSats} sats (tx ${esc(rec.payment.txid.slice(0, 16))}…)` : ''}</div>` : ''}
<ul>${r.checks.map(c => `<li><span class="${cls(c.ok)}">${icon(c.ok)}</span> ${esc(c.name)}<div class=d>${esc(c.detail)}</div></li>`).join('')}</ul>
<img src="${qr}" alt="scan to re-verify">
<div class=note>Proves the answer was committed and is derivable from sealed, anchored records — not that it is true. Re-scan the QR to re-check.</div>
</body></html>`
}
