// API-001: minimal versioned HTTP API (node:http, no framework). Same tools
// as MCP, plus public read endpoints. Every route is in OPENAPI (tested).
import { createServer, type IncomingMessage, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import type { NektarMemory } from './memory.js'
import { TOOLS, visibleTools, callTool } from './server.js'
import { receiptToken } from './receipt.js'

const json = (v: unknown) => JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? x.toString() : x instanceof Uint8Array ? Buffer.from(x).toString('hex') : x))

export const OPENAPI = {
  openapi: '3.1.0',
  info: { title: 'nektar API', version: '1.0.0', description: 'Verifiable encrypted agent memory on BSV. Proofs establish inclusion, integrity and existence-by-time — never truth.' },
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
    '/v1/zkproof/{commit}': {
      get: { summary: 'a committed ZK proof artifact by sha256 — private-cite proofs are public (they reveal nothing) so verifiers re-run them', parameters: [{ name: 'commit', in: 'path', required: true, schema: { type: 'string' } }], responses: { 200: { description: 'proof bytes' }, 404: { description: 'unknown/mismatched commit' } } }
    },
    '/v1/checkpoint/{shard}': {
      get: { summary: 'latest signed checkpoint of the shard (LOG-007)', parameters: [{ name: 'shard', in: 'path', required: true, schema: { type: 'string' } }], responses: { 200: { description: 'checkpoint + log pubkey' }, 404: { description: 'no anchor yet' } } }
    },
    '/.well-known/nektar': { get: { summary: 'capability/pack discovery doc — verify base + offerable public shards (NEKTAR_PACK_SHARE=0 disables)', responses: { 200: { description: 'discovery doc' }, 404: { description: 'pack sharing disabled' } } } },
    '/v1/pack/{shard}': { get: { summary: 'serve a public shard as a portable pack (records + proof bundles) — importShard re-verifies every bundle; private/gated shards refuse', parameters: [{ name: 'shard', in: 'path', required: true, schema: { type: 'string' } }], responses: { 200: { description: 'ShardPack JSON' }, 403: { description: 'private or level-gated' }, 404: { description: 'unknown/disabled' } } } },
    '/v1/verify': {
      get: {
        summary: 'scan a receipt token — traffic-light checks that the answer receipt exists, re-hashes, is provenance-signed + anchored, and its cited fragments are anchored. JSON by default; Accept: text/html (or ?format=html) returns a check page with a QR.',
        parameters: [{ name: 'r', in: 'query', required: true, schema: { type: 'string', description: 'receipt token nektarr1.<base64url hash>' } }],
        responses: { 200: { description: 'check list + ok flag' }, 400: { description: 'missing/malformed token' } }
      }
    },
    '/v1/badge': {
      get: {
        summary: 'verification label as an SVG badge — clients embed it so the verdict is always visible (green verified / red failed / grey unknown)',
        parameters: [{ name: 'r', in: 'query', required: true, schema: { type: 'string', description: 'receipt token nektarr1.<base64url hash>' } }],
        responses: { 200: { description: 'image/svg+xml badge' }, 400: { description: 'missing/malformed token' } }
      }
    },
    '/.well-known/x402': { get: { summary: 'payment catalog — priced tools, scheme and network (x402 discovery doc)', responses: { 200: { description: 'catalog' }, 404: { description: 'payments disabled' } } } },
    '/dashboard': { get: { summary: 'operator audit surface — one HTML page: identity, money, receipts, market. Token-gated (non-public route).', responses: { 200: { description: 'text/html dashboard' } } } },
    '/v1/pay/quote': {
      get: {
        summary: 'payment invoice for a paid tool (NEKTAR_PAYMENTS=1) — 402 + PAYMENT-REQUIRED header carrying an x402 v2 PaymentRequired; pay the invoice with a signed tx then retry the tool with payment:{invoiceId,transaction}',
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

export function createHttpApi (mem: NektarMemory, ops?: { wallet: () => Promise<unknown>, costs: () => unknown, importFunding?: (txid: string) => Promise<unknown> }): Server {
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
      await createMcpServer(mem, ops).connect(t)
      transport = t
    }
    await transport.handleRequest(req, res, parsed)
  }
  const server = createServer(async (req, res) => {
    const send = (code: number, v: unknown, type = 'application/json') => { res.writeHead(code, { 'content-type': type }); res.end(typeof v === 'string' ? v : json(v)) }
    const url = new URL(req.url ?? '/', 'http://x')
    const p = url.pathname.split('/').filter(Boolean)
    // NEKTAR_HTTP_TOKEN: when set, everything except the public read-only
    // endpoints (/v1/health, /v1/verify — a QR scan can't send headers, and
    // verification output is read-only anyway — plus /.well-known/x402,
    // which is a discovery doc agents must find unauthenticated) requires a
    // bearer token — the gate that makes a public bind safe. Unset =
    // localhost-trusted behaviour (the default bind is 127.0.0.1 anyway).
    const tok = process.env.NEKTAR_HTTP_TOKEN
    const publicPath = url.pathname === '/v1/health' || url.pathname === '/v1/verify' || url.pathname === '/v1/badge' || url.pathname === '/.well-known/x402' || url.pathname === '/.well-known/nektar' || url.pathname.startsWith('/v1/pack/') || url.pathname.startsWith('/v1/zkproof/') || url.pathname === '/v1/arc-callback'
    if (tok && !publicPath && req.headers.authorization !== `Bearer ${tok}`) {
      return send(401, { error: 'unauthorized' })
    }
    try {
      if (url.pathname === '/mcp') return await mcp(req, res)
      if (req.method === 'GET' && url.pathname === '/v1/health') return send(200, { ok: true })
      if (req.method === 'GET' && url.pathname === '/v1/openapi.json') return send(200, OPENAPI)
      if (req.method === 'GET' && url.pathname === '/v1/metrics') return send(200, mem.metrics.render(), 'text/plain; version=0.0.4')
      if (req.method === 'GET' && url.pathname === '/dashboard/events') {
        // SSE — one line per state change; the page swaps its blocks on each
        res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive' })
        res.write(': connected\n\n')
        const push = (name: string) => { try { res.write(`data: ${name}\n\n`) } catch {} }
        mem.events.on('change', push)
        const beat = setInterval(() => { try { res.write(': beat\n\n') } catch {} }, 15_000)
        req.on('close', () => { clearInterval(beat); mem.events.off('change', push) })
        return
      }
      if (req.method === 'GET' && url.pathname === '/dashboard') {
        // the operator audit surface — one page: identity, money, receipts,
        // market. Every block degrades to an honest empty state.
        const esc = (s: unknown) => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]!)
        const d = await mem.doctor()
        if (ops?.wallet) { const w = await ops.wallet() as Record<string, unknown>; d.funding = { ...w, funded: Number(w.balanceSats ?? 0) > 0 } }
        const host = req.headers.host ?? 'localhost'
        const base = process.env.NEKTAR_VERIFY_BASE ?? `http://${host}`
        // receipts — newest last N
        const rrecs = await mem.records('__receipts__').catch(() => [] as Array<{ seq: number, data?: string }>) as Array<{ seq: number, data?: string }>
        const receiptRows = rrecs.slice(-10).reverse().map(r => {
          let amt = '', tool = ''
          const m = r.data?.match(/^receipt:[0-9a-f]+:([A-Za-z0-9_-]+)$/)
          if (m) { try { const j = JSON.parse(Buffer.from(m[1]!, 'base64url').toString()); amt = String(j.payment?.amountSats ?? j.payment?.amount ?? ''); tool = String(j.tool ?? j.toolName ?? '') } catch {} }
          return `<div class="row"><span class="dim">#${r.seq}</span><span>${esc(tool || 'answer')}</span><span>${amt ? esc(amt) + ' sats' : ''}</span><a class="pill" href="${base}/v1/verify?s=${r.seq}&format=html">verify ↗</a></div>`
        }).join('')
        // market — offerable shards + autorespond + fraud
        const cat = await mem.shardCatalog().catch(() => [] as Array<{ shard: string, domains: string[], offers?: Array<{ kind: string, priceSats: number }> }>)
        const marketRows = cat.map(m => `<div class="row"><span><b>${esc(m.shard)}</b></span><span class="dim">${esc(m.domains.join(', '))}</span><span>${esc(m.offers?.map(o => `${o.kind} ${o.priceSats}ⓢ`).join(' · ') ?? '')}</span></div>`).join('')
        const dataDir = (mem as unknown as { opts?: { dataDir?: string } }).opts?.dataDir ?? '.nektar'
        let autoLine = ''
        try {
          const { readFileSync, existsSync } = await import('node:fs')
          const { join } = await import('node:path')
          const st = join(dataDir, 'autorespond.state.json')
          if (existsSync(st)) { const s = JSON.parse(readFileSync(st, 'utf8')); autoLine = `<div class="row dim">autorespond — ${(s.responded ?? []).length} wants answered</div>` }
        } catch {}
        let fraudRows = ''
        const rv = process.env.NEKTAR_OVERLAY_URL
        if (rv) {
          try {
            const fr = await fetch(`${rv.replace(/\/+$/, '')}/events?kind=fraud&since=-1`, { signal: AbortSignal.timeout(5000) })
            const fj = await fr.json() as { events?: Array<{ seq: number, pub: string }> }
            if (fj.events?.length) fraudRows = fj.events.slice(-5).map(e => `<div class="row"><span class="warn">fraud</span> <span class="dim">#${e.seq} · ${esc(String(e.pub).slice(0, 16))}…</span></div>`).join('')
          } catch {}
        }
        // agents panel — this agent's declared identity + discovered peers
        let agentsRows = `<div class="row"><span><b>${esc(process.env.NEKTAR_MODEL_ID ?? 'this agent')}</b></span><span class="dim">${esc(mem.operatorKey().toPublicKey().toString().slice(0, 16))}…</span></div>`
        if (rv) {
          try {
            const dr = await fetch(`${rv.replace(/\/+$/, '')}/directory`, { signal: AbortSignal.timeout(5000) })
            const dj = await dr.json() as { ads?: Array<{ ad?: { endpoint?: string } }> }
            agentsRows += `<div class="row"><span class="dim">peers on rendezvous</span><span>${dj.ads?.length ?? 0} discovered</span></div>`
          } catch {}
        }
        const f = d.funding as Record<string, unknown>
        const bal = Number(f.balanceSats ?? f.satoshis ?? 0)
        const up = process.uptime()
        const upStr = up > 86400 ? `${Math.floor(up / 86400)}d` : up > 3600 ? `${Math.floor(up / 3600)}h` : `${Math.floor(up / 60)}m`
        const badge = (on: boolean, yes: string, no: string) => `<span class="badge ${on ? 'on' : 'off'}">${on ? yes : no}</span>`
        const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="30"><title>nektar — operator</title>
<style>body{transition:none}#live{display:inline-block;width:7px;height:7px;border-radius:50%;background:#4cc26b;margin-left:6px;vertical-align:middle}
body{background:#0d0d0d;color:#eaeaea;font:14px/1.6 ui-monospace,monospace;max-width:880px;margin:0 auto;padding:48px 20px}
header{margin-bottom:40px}h1{font-size:22px;margin:0 0 6px;font-weight:600}
.sub{color:#888;font-size:13px}.sub code{background:#1a1a1a;padding:1px 6px;border-radius:3px;color:#bbb}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:16px}
@media (max-width:640px){.grid{grid-template-columns:1fr}}
.card{background:#161616;border:1px solid #262626;border-radius:10px;padding:20px 22px}
.stream{margin-top:12px;padding-top:8px;border-top:1px solid #1f1f1f;font-size:11px;color:#555;min-height:1em;max-height:4.2em;overflow:hidden}
.stream div{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.stream .t{color:#f5b301}
.top{display:flex;justify-content:space-between;align-items:baseline;margin-bottom:12px}
.label{font-size:11px;letter-spacing:.14em;color:#888;text-transform:uppercase}
.tags{font-size:11px;color:#666;text-align:right}
.tags .badge{margin-left:6px}
.big{font-size:30px;color:#f5b301;font-weight:600}.big small{font-size:14px;color:#888;font-weight:400}
.meta{color:#888;font-size:13px;margin-top:8px}
.badge{display:inline-block;font-size:11px;padding:2px 10px;border-radius:20px;margin-left:8px}
.badge.on{background:#1c2b1e;color:#4cc26b}.badge.off{background:#222;color:#777}
.row{display:flex;align-items:center;gap:14px;padding:9px 0;border-bottom:1px solid #1f1f1f;font-size:13px}
.row:last-child{border-bottom:none}.row .dim{color:#777}.row b{color:#eaeaea;font-weight:600}
.pill{margin-left:auto;background:#1a1a1a;border:1px solid #2a2a2a;border-radius:20px;padding:2px 12px;color:#f5b301;text-decoration:none;font-size:12px}
a{color:#f5b301;text-decoration:none}.warn{color:#e5534b}
.empty{color:#666;font-size:13px;padding:6px 0}
footer{margin-top:28px;color:#555;font-size:12px}
</style></head><body>
<header>
<h1>nektar<span id="live" title="live — refreshes silently every 5s"></span></h1>
<div class="sub">${esc(d.network)} · pub <code>${esc(mem.operatorKey().toPublicKey().toString().slice(0, 24))}…</code> · up ${upStr} · live</div>
</header>
<div id="app" class="grid">
<div class="card">
<div class="top"><span class="label">Money</span><span class="tags">${badge(!!f.funded, 'funded', 'unfunded')}${badge(d.payments === 'on', 'pay on', 'pay off')} · ${esc(f.utxoCount ?? f.utxos ?? '—')} utxos</span></div>
<div class="big">${bal.toLocaleString()} <small>sats</small></div>
<div class="stream" data-ev="money"></div>
</div>
<div class="card">
<div class="top"><span class="label">Receipts</span><span class="tags">${rrecs.length} total</span></div>
${receiptRows || '<div class="empty">no receipts yet — your agent hasn’t answered anything</div>'}
<div class="stream" data-ev="receipts"></div>
</div>
<div class="card">
<div class="top"><span class="label">Market</span><span class="tags">${cat.length} for sale</span></div>
${marketRows || '<div class="empty">nothing for sale yet — describeShard a public shard to list it</div>'}
${autoLine}${fraudRows}
<div class="stream" data-ev="market"></div>
</div>
<div class="card">
<div class="top"><span class="label">Agents</span><span class="tags">${esc(process.env.NEKTAR_MODEL_ID ?? 'unattested')}</span></div>
${agentsRows}
<div class="stream" data-ev="agents"></div>
</div>
</div>
<footer>${d.shards} shards · ${d.records} records · ${d.anchoredRecords} anchored · ${d.unprovenAnchors} unproven · prover: ${esc(d.prover)}</footer>
<script>
const reload = async () => {
  try {
    const t = await (await fetch(location.pathname, { cache: 'no-store' })).text()
    const m = t.match(/<div id="app">([\\s\\S]*)<\\/div>\\s*<script>/)
    if (m) document.getElementById('app').innerHTML = m[1]
  } catch {}
}
const PANEL = { remember: 'market', describeShard: 'market', publishOffer: 'market', publishWant: 'market', respond: 'market', groundedAnswer: 'receipts', commitReceipt: 'receipts', attestInference: 'receipts', recall: 'receipts', peerCall: 'money', payInvoice: 'money', payQuote: 'money', sale: 'money', claim: 'money', fundingImport: 'money', delegate: 'agents', revokeGrant: 'agents', attest: 'agents', importRecord: 'agents', importShard: 'agents' }
const stamp = e => '<div><span class="t">' + new Date().toLocaleTimeString() + '</span> ' + e + '</div>'
const es = new EventSource('/dashboard/events')
es.onmessage = m => {
  const name = m.data || 'change', panel = PANEL[name.split('#')[0]] ?? 'market'
  const s = document.querySelector('.stream[data-ev="' + panel + '"]')
  if (s) { s.insertAdjacentHTML('afterbegin', stamp(name)); while (s.children.length > 4) s.lastChild.remove() }
  reload()
}
es.onerror = () => { document.getElementById('live').style.background = '#e5534b' }
es.onopen = () => { document.getElementById('live').style.background = '#4cc26b' }
</script>
</body></html>`
        return send(200, html, 'text/html; charset=utf-8')
      }
      if (req.method === 'GET' && url.pathname === '/v1/tools') return send(200, { tools: visibleTools() })
      if (req.method === 'POST' && p[0] === 'v1' && p[1] === 'tools' && p.length === 3) {
        if (!visibleTools().some(t => t.name === p[2])) return send(404, { error: `unknown tool ${p[2]}` })
        try { return send(200, await callTool(mem, p[2]!, await body(req), { ops, external: !tok })) } catch (e) { return send(400, { error: (e as Error).message }) }
      }
      if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'zkproof' && p.length === 3) {
        const bytes = mem.getProof(decodeURIComponent(p[2]!))
        if (!bytes) return send(404, { error: 'no proof artifact under that commitment' })
        res.writeHead(200, { 'content-type': 'application/octet-stream' })
        return res.end(Buffer.from(bytes))
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
            const base = process.env.NEKTAR_VERIFY_BASE ?? `http://${req.headers.host ?? 'localhost'}`
            const q = tok ? `r=${tok}` : `s=${seq}`
            mem.metrics.inc('nektar_verify_views_total', {}, 1, 'verify page views')
            // viral edge: cited shards this operator offers publicly become
            // a one-line import CTA — "take the memory behind this answer"
            const offerable = new Set(await mem.publicShards().catch(() => [] as string[]))
            const cited = [...new Set(((result.receipt as { fragmentRefs?: Array<{ shard: string }> } | undefined)?.fragmentRefs ?? []).map(f => f.shard).filter(s => offerable.has(s)))]
            return send(200, await verifyPage(result, `${base}/v1/verify?${q}&format=html`,
              { base, importShards: cited, totals: { views: mem.metrics.value('nektar_verify_views_total'), anchors: mem.metrics.value('nektar_anchors_total') },
                operator: mem.fundingStatus().address }), 'text/html; charset=utf-8')
          }
          return send(200, result)
        } catch (e) { return send(400, { error: (e as Error).message }) }
      }
      if (req.method === 'GET' && url.pathname === '/v1/badge') {
        const tok = url.searchParams.get('r')
        if (!tok) return send(400, { error: 'missing ?r=<receipt token>' })
        try {
          const { verifyReceipt } = await import('./receipt.js')
          const result = await verifyReceipt(mem, { token: tok })
          const pass = (result.checks ?? []).filter(c => c.ok === true).length
          const total = (result.checks ?? []).filter(c => c.ok !== 'info' && c.ok !== 'skip').length
          const att = (result.receipt as { modelAttestation?: { verifierTxid?: string } } | undefined)?.modelAttestation
          const value = result.ok ? `verified ${pass}/${total}${att?.verifierTxid ? ' · model on-chain' : att ? ' · model attested' : ''}` : 'failed'
          return send(200, badgeSvg(value, result.ok ? '#2ea043' : '#e5534b'), 'image/svg+xml; charset=utf-8')
        } catch {
          return send(200, badgeSvg('unverifiable', '#6e7681'), 'image/svg+xml; charset=utf-8')
        }
      }
      // ARC tx-status callback — the broadcaster pushes SEEN_ON_NETWORK →
      // MINED etc. for every tx we submitted with X-CallbackUrl, so anchor
      // confirmations arrive as events, not polls
      if (req.method === 'POST' && url.pathname === '/v1/arc-callback') {
        try {
          const j = await body(req)
          mem.metrics.inc('nektar_arc_callbacks_total', { status: String(j.txStatus ?? '?').toLowerCase() }, 1, 'ARC status callbacks received')
          mem.events.emit('arcStatus', { txid: j.txid, status: j.txStatus, detail: j.extraInfo })
          return send(200, { ok: true })
        } catch { return send(400, { error: 'bad callback' }) }
      }
      if (req.method === 'GET' && url.pathname === '/.well-known/x402') {
        const { paymentsEnabled, PAID_TOOLS, priceFor } = await import('./pay.js')
        if (!paymentsEnabled()) return send(404, { error: 'payments disabled (NEKTAR_PAYMENTS=1)' })
        const n = process.env.NEKTAR_NETWORK ?? 'mock'
        return send(200, {
          x402Version: 2,
          scheme: 'exact',
          network: n === 'main' ? 'bsv:mainnet' : n === 'ttn' ? 'bsv:teratestnet' : n === 'regtest' ? 'bsv:regtest' : 'bsv:mock',
          resources: [...PAID_TOOLS].map(t => ({ url: `tool://${t}`, description: `nektar ${t}`, priceSats: priceFor(t), payVia: `call ${t} without payment → invoice in the result, or GET /v1/pay/quote?tool=${t}` }))
        })
      }
      // pack sharing: any visitor may fetch a public shard's pack + discover
      // what's offered — the viral cold-start edge (importShard re-verifies
      // every bundle, so serving adds zero trust). NEKTAR_PACK_SHARE=0 opts out.
      if (req.method === 'GET' && url.pathname === '/.well-known/nektar') {
        if (/^(0|false|no)$/i.test(process.env.NEKTAR_PACK_SHARE ?? '1')) return send(404, { error: 'pack sharing disabled (NEKTAR_PACK_SHARE=0)' })
        const base = process.env.NEKTAR_VERIFY_BASE ?? `http://${req.headers.host ?? 'localhost'}`
        const pub = await mem.publicShards()
        const catalog = await mem.shardCatalog().catch(() => [] as import('./memory.js').ShardManifest[])
        return send(200, {
          v: 1, kind: 'nektar-discovery', verifyBase: `${base}/v1/verify`, logPublicKey: mem.logPublicKey(),
          publicShards: await Promise.all(pub.map(async s => {
            const manifest = catalog.find(m => m.shard === s)
            const head = mem.tailLink(s)
            return {
              shard: s,
              shardId: manifest?.shardId,
              records: (await mem.records(s)).length,
              domains: manifest?.domains,
              desc: manifest?.desc,
              offers: manifest?.offers,
              headTxid: head?.txid,
              packUrl: `${base}/v1/pack/${encodeURIComponent(s)}`,
              import: { tool: 'importShard', args: { from: base, shard: s } }
            }
          }))
        })
      }
      if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'pack' && p.length === 3) {
        if (/^(0|false|no)$/i.test(process.env.NEKTAR_PACK_SHARE ?? '1')) return send(404, { error: 'pack sharing disabled (NEKTAR_PACK_SHARE=0)' })
        try {
          const pack = await mem.exportShardPack(decodeURIComponent(p[2]!))
          mem.metrics.inc('nektar_packs_served_total', {}, 1, 'public shard packs served')
          // bstore mirror — durable bounty-paid retention when a gateway is set
          if (process.env.NEKTAR_BSTORE_URL) {
            fetch(`${process.env.NEKTAR_BSTORE_URL.replace(/\/$/, '')}/objects`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(pack) }).catch(() => {})
          }
          return send(200, pack)
        } catch (e) {
          const m = (e as Error).message
          return send(/private|level>0/.test(m) ? 403 : 404, { error: m })
        }
      }
      if (req.method === 'GET' && url.pathname === '/v1/pay/quote') {
        const { paymentsEnabled, PAID_TOOLS, paymentRequired } = await import('./pay.js')
        if (!paymentsEnabled()) return send(404, { error: 'payments disabled (NEKTAR_PAYMENTS=1)' })
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
 *  The QR encodes this same page URL so a phone scan re-opens the result.
 *  Written for humans: the answer leads, each check is a plain-English
 *  claim, and the raw internals stay visible as fine print for auditors. */
/** shields-style badge — the always-visible verification label */
function badgeSvg (value: string, color: string): string {
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  const lw = 52, vw = Math.max(60, value.length * 6.5 + 14)
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${lw + vw}" height="20" role="img" aria-label="nektar: ${esc(value)}"><title>nektar: ${esc(value)}</title><rect width="${lw}" height="20" fill="#555" rx="3"/><rect x="${lw}" width="${vw}" height="20" fill="${color}" rx="3"/><rect x="${lw}" width="4" height="20" fill="${color}"/><g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11"><text x="${lw / 2}" y="14">nektar</text><text x="${lw + vw / 2}" y="14">${esc(value)}</text></g></svg>`
}

async function verifyPage (r: { ok: boolean, receiptHash: string, checks: Array<{ name: string, ok: boolean | 'info' | 'skip', detail: string }>, receipt?: unknown, anchorTxid?: string }, pageUrl: string, ctx?: { base: string, importShards: string[], totals: { views: number, anchors: number }, operator?: string }): Promise<string> {
  const rec = r.receipt as { answer?: string, fragmentRefs?: Array<{ shard: string, seq: number }>, checkResult?: string, modelId?: { provider?: string, release?: string }, modelAttestation?: { verifierTxid?: string, publicStatement?: string }, payment?: { txid: string, amountSats: number, payTo: string } } | undefined
  const QR = await import('qrcode')
  const qr = await QR.toDataURL(pageUrl, { margin: 1, width: 180 })
  const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  const net = process.env.NEKTAR_NETWORK ?? 'mock'
  const netLabel = net === 'main' ? 'BSV mainnet — real coins' : net === 'ttn' ? 'BSV public testnet — real transactions, test coins' : net === 'regtest' ? 'local Teranode regtest' : 'simulated chain — demo only'
  const icon = (o: boolean | 'info' | 'skip') => o === true ? '✓' : o === false ? '✗' : '·'
  const cls = (o: boolean | 'info' | 'skip') => o === true ? 'ok' : o === false ? 'bad' : 'info'
  // plain-English title per check — the raw detail stays as the sub-line
  const title = (name: string): string =>
    name === 'found' ? 'This receipt exists in the log'
      : name === 'hash' ? 'The record is unaltered'
      : name === 'provenance' ? 'Signed by the service operator'
      : name === 'anchored' ? 'Anchored on the blockchain'
      : name === 'fragment (private)' ? 'Cited record — private shard (ZK-verified)'
      : name.startsWith('fragment ') ? `Cited record ${name.slice(9)} exists on-chain`
      : name === 'grounding' ? 'The answer traces to cited records'
      : name === 'checkResult' ? 'Grounding verdict'
      : name === 'paid' ? 'Paid for on-chain'
      : name === 'model' ? 'Model execution'
      : name
  const cites = (rec?.fragmentRefs ?? []).map(f => `${f.shard}:${f.seq}`)
  const meta = rec ? [
    cites.length ? `built from ${cites.length} cited record${cites.length === 1 ? '' : 's'} (${esc(cites.join(', '))})` : 'no cited records',
    rec.modelAttestation?.verifierTxid
      ? `model execution: consensus-verified on-chain (Groth16, ${esc(rec.modelAttestation.verifierTxid.slice(0, 12))}…)`
      : rec.modelAttestation
        ? `model execution: statement-attested (${esc((rec.modelAttestation.publicStatement ?? '').slice(0, 14))}… — off-chain)`
        : `declared model: ${esc(rec.modelId?.provider ?? 'undeclared')}/${esc(rec.modelId?.release ?? 'undeclared')} (declared, not proven)`,
    rec.payment ? `paid ${rec.payment.amountSats} sats` : null
  ].filter(Boolean).join(' · ') : ''
  const tok = r.receiptHash ? receiptToken(r.receiptHash) : null
  const badgeUrl = tok ? `${new URL(pageUrl).origin}/v1/badge?r=${tok}` : null
  const badgeMd = badgeUrl ? `[![verified answer](${badgeUrl})](${pageUrl.replace('&format=html', '')})` : null
  const ogDesc = r.ok
    ? `${r.checks.filter(c => c.ok === true).length}/${r.checks.length} checks passed — committed, unaltered, anchored, derivable. Never truth.`
    : `receipt failed verification — ${r.checks.filter(c => c.ok === false).length} check(s) broken`
  const explorer = net === 'main' ? 'https://whatsonchain.com' : net === 'stn' ? 'https://stn.whatsonchain.com' : net === 'ttn' || net === 'test' ? 'https://test.whatsonchain.com' : null
  const anchorUrl = explorer && r.anchorTxid ? `${explorer}/tx/${r.anchorTxid}` : null
  return `<!doctype html><html><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>${r.ok ? '✓ verified answer' : '✗ receipt failed'}${rec?.answer ? ` — ${esc(rec.answer.slice(0, 60))}` : ''}</title>
<meta property="og:title" content="${r.ok ? '✓ verified answer receipt' : '✗ receipt failed verification'} — nektar">
<meta property="og:description" content="${esc(ogDesc)}">
<meta property="og:type" content="website"><meta property="og:url" content="${esc(pageUrl)}">
<meta name="twitter:card" content="summary">
<style>body{font:15px/1.55 system-ui,sans-serif;background:#0d1117;color:#c9d1d9;max-width:600px;margin:24px auto;padding:0 16px}
h1{font-size:19px;margin-bottom:2px}.net{color:#8b949e;font-size:12px;margin-bottom:14px}.mono{font-family:ui-monospace,monospace;font-size:11px;word-break:break-all}
.badge{display:inline-block;padding:3px 12px;border-radius:5px;font-weight:700}
.ok{background:#12331c;color:#4ade80}.bad{background:#3d1215;color:#f87171}.info{color:#8b949e}
.lede{color:#8b949e;font-size:14px;margin:0 0 16px}
.ans{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px 18px;margin:0 0 6px;font-size:17px;line-height:1.5}
.meta{color:#8b949e;font-size:12.5px;margin:10px 0 0}
ul{list-style:none;padding:0}li{margin:10px 0;font-weight:600}
.d{color:#8b949e;font-size:12.5px;font-weight:400;margin-left:22px}
img{display:block;margin:22px auto 6px;border:6px solid #fff;border-radius:6px}
.qrcap{text-align:center;color:#6e7681;font-size:11.5px}.qrcap a{color:#58a6ff;text-decoration:none}
.sharebtn{display:block;width:100%;margin:18px 0 4px;padding:15px;background:#1f6feb;color:#fff;border:0;border-radius:8px;font-size:16px;font-weight:700;cursor:pointer}
.sharebtn:hover{background:#388bfd}.sharebtn.done{background:#12331c;color:#4ade80}
.md{display:block;background:#161b22;border:1px solid #30363d;border-radius:6px;padding:8px 10px;margin-top:8px;font:11.5px ui-monospace,monospace;color:#8b949e;word-break:break-all}
details{margin:16px 0;color:#8b949e}summary{cursor:pointer;font-size:13px}details>div{margin-top:10px}</style></head><body>
<h1>Answer receipt <span class="badge ${cls(r.ok)}">${r.ok ? 'VERIFIED' : 'FAILED'}</span></h1>
<div class=net>${esc(netLabel)}${ctx?.operator ? ` · operator <span class=mono>${esc(ctx.operator)}</span>` : ''}</div>
<div class=lede>${r.ok
  ? 'This answer was recorded in a signed, append-only log on the blockchain — unchanged and anchored. <b>Recorded ≠ true.</b>'
  : 'Something in this receipt did not check out — see the ✗ lines below.'}</div>
${rec?.answer !== undefined ? `<div class=ans>${esc(rec.answer)}</div>` : ''}
<button class=sharebtn id=sharebtn>Share this verification</button>
<script>document.getElementById('sharebtn').onclick=async function(){var u=${JSON.stringify(pageUrl.replace('&format=html',''))};this.className='sharebtn';if(navigator.share){try{await navigator.share({title:'verified answer — nektar',url:u});this.textContent='\\u2713 shared';this.className='sharebtn done';return}catch(e){if(e&&e.name==='AbortError')return}}try{await navigator.clipboard.writeText(u);this.textContent='\\u2713 link copied';this.className='sharebtn done'}catch(e){this.textContent=u}}</script>
<img src="${qr}" alt="scan to re-verify"><div class=qrcap>scan to re-check</div>
${anchorUrl ? `<div class=qrcap><a href="${anchorUrl}" target=_blank rel=noopener>view the anchor transaction ↗</a></div>` : ''}
<details${r.ok ? '' : ' open'}><summary>details · ${r.checks.filter(c => c.ok === true).length}/${r.checks.filter(c => c.ok !== 'info' && c.ok !== 'skip').length} checks</summary><div>
<ul>${r.checks.map(c => `<li><span class="${cls(c.ok)}">${icon(c.ok)}</span> ${esc(title(c.name))}<div class=d>${esc(c.detail)}</div></li>`).join('')}</ul>
${meta ? `<div class=meta>${meta}</div>` : ''}
<code class=md>${esc('git clone https://github.com/kryptokrates/nektar && pnpm mcp:install')}</code>
${badgeMd ? `<code class=md>${esc(badgeMd)}</code>` : ''}
</div></details>
</body></html>`
}
