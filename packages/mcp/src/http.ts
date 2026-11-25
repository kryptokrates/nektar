// API-001: minimal versioned HTTP API (node:http, no framework). Same tools
// as MCP, plus public read endpoints. Every route is in OPENAPI (tested).
import { nektarEnv, deadline, HEX64 } from '@nektar/core'
import { isHeaderUnavailable, KNOWN_STATUSES, MINED_WORDS, REJECTED_WORDS } from '@nektar/chain'

const KNOWN_SET = new Set(KNOWN_STATUSES)
import { marketHosts, addSavedHost, removeSavedHost } from './hosts.js'
import { faucetEnabled, faucetDrip } from './faucet.js'
import { createServer, type IncomingMessage, type Server } from 'node:http'
import { createHash, randomUUID, timingSafeEqual } from 'node:crypto'
import { MerklePath } from '@bsv/sdk'
import type { NektarMemory } from './memory.js'
import { visibleTools, callTool } from './server.js'
import { OPENAPI } from './openapi.js'
import { badgeSvg, cardPage, dashboardState, dashboardFile, verifyPage, safeHost } from './pages.js'
import { createAuthManager, type AuthManager } from './auth.js'
import { buildAgentCard, handleA2aRoute } from './a2a.js'
import { handleProxyRoute } from './proxy.js'
import { handleOnboardRoute } from './onboard.js'
import { handleAdaptersRoute } from './adapters-export.js'

export { OPENAPI }

const json = (v: unknown) => JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? x.toString() : x instanceof Uint8Array ? Buffer.from(x).toString('hex') : x))

/** One broadcaster tx-status payload → memory side effects. Shared by the
 *  inbound webhook route and the outbound-only /events SSE reader (runtime):
 *  REJECTED/DOUBLE_SPEND_ATTEMPTED → hintTxRejected (wake-only — a status
 *  string carries no proof object and never lands a durable verdict);
 *  MINED/IMMUTABLE → the carried/fetched BUMP must verify against our own
 *  tracker before deliverTxProof — a status is a hint, never proof on the
 *  broadcaster's word. */
export async function handleBroadcasterStatus(
  mem: NektarMemory,
  j: { txid?: string; txStatus?: string; merklePath?: string; extraInfo?: string }
): Promise<void> {
  const status = String(j.txStatus ?? '?')
  // the raw status/txid are unbounded unauth input — normalize before they
  // become metric labels / hintedTxids entries (both retain forever)
  const norm = KNOWN_SET.has(status.toUpperCase()) ? status.toUpperCase() : 'OTHER'
  mem.metrics.inc('nektar_broadcaster_callbacks_total', { status: norm.toLowerCase() }, 1, 'broadcaster status callbacks received')
  mem.events.emit('broadcasterStatus', { txid: j.txid, status, detail: j.extraInfo })
  // a pushed rejection is wake-only — it pokes subscribers into re-asking
  // txFacts; the durable verdict belongs to verified chain events, a
  // first-party refusal, or the abandon horizon
  const txidOk = typeof j.txid === 'string' && HEX64.test(j.txid)
  const txid = txidOk ? (j.txid as string) : undefined
  if (txid !== undefined && REJECTED_WORDS.includes(norm)) mem.hintTxRejected(txid)
  // N9 — the status is an unauthenticated input, not proof. A carried BUMP
  // must verify against our own tracker before it counts; without one we
  // fetch evidence; without evidence the tx stays unconfirmed.
  if (MINED_WORDS.includes(norm) && txid !== undefined) {
    try {
      let mp: import('@bsv/sdk').MerklePath | undefined
      if (typeof j.merklePath === 'string' && j.merklePath.length > 0) {
        try {
          const cand = MerklePath.fromHex(j.merklePath)
          if (await cand.verify(txid, mem.sdkTracker())) mp = cand
        } catch (e) {
          if (isHeaderUnavailable(e)) throw e
          /* unusable — fetch evidence instead */
        }
      }
      if (mp === undefined) {
        const cand2 = await mem.lookupProof(txid)
        // the fetched path must verify against our own tracker too —
        // the broadcaster's word alone is a hint, never a proof
        if (cand2 !== undefined) {
          try {
            if (await cand2.verify(txid, mem.sdkTracker())) mp = cand2
          } catch (e) {
            if (isHeaderUnavailable(e)) throw e
            /* unverifiable */
          }
        }
      }
      if (mp) mem.deliverTxProof(txid, mp)
      // no fallback: a status without retrievable evidence is a hint, not a confirmation
    } catch (e) {
      if (isHeaderUnavailable(e)) throw e
      /* evidence fetch failed — leave unconfirmed, reconcile later */
    }
  }
}

const BODY_MAX = 16_000_000
// aggregate bounds — a per-request cap alone lets N concurrent senders buffer
// N×BODY_MAX; the pair below keeps total buffered bytes + concurrent readers
// bounded regardless of connection count
const BODY_INFLIGHT_MAX = 64
const BODY_BYTES_MAX = 256_000_000
let bodyInflight = 0
let bodyBytes = 0

async function body(req: IncomingMessage): Promise<unknown> {
  const declared = Number(req.headers['content-length'] ?? 0)
  if (declared > BODY_MAX) throw new Error('body too large')
  if (bodyInflight >= BODY_INFLIGHT_MAX || bodyBytes + Math.min(declared, BODY_MAX) > BODY_BYTES_MAX) throw new Error('server busy')
  bodyInflight++
  let read = 0
  try {
    const chunks: Buffer[] = []
    for await (const c of req) {
      read += (c as Buffer).length
      bodyBytes += (c as Buffer).length
      if (read > BODY_MAX) throw new Error('body too large')
      // aggregate bound mid-read — an undeclared-length body skipped the
      // entry pre-check and only pays the aggregate here (still bounded by
      // the in-flight cap)
      if (bodyBytes > BODY_BYTES_MAX) throw new Error('server busy')
      chunks.push(c as Buffer)
    }
    return chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : {}
  } finally {
    bodyInflight--
    bodyBytes -= read
  }
}

export function createHttpApi(
  mem: NektarMemory,
  ops?: { wallet?: () => Promise<unknown>; costs?: () => unknown; importFunding?: (txid: string) => Promise<unknown> },
  httpOpts?: { auth?: AuthManager; fetchImpl?: typeof fetch; adaptersRoot?: string }
): Server {
  const auth = httpOpts?.auth ?? createAuthManager()
  // MCP Streamable HTTP: one transport + McpServer per session (mcp-session-id).
  // Idle sessions are reaped — a session survives a dropped socket by design,
  // so nothing else bounds the map.
  const sessions = new Map<string, { t: import('@modelcontextprotocol/sdk/server/streamableHttp.js').StreamableHTTPServerTransport; lastSeen: number }>()
  const SESSION_IDLE_MS = 10 * 60_000
  const SESSION_MAX = 256
  const sessionReaper = setInterval(() => {
    const now = Date.now()
    for (const [_id, s] of sessions) if (now - s.lastSeen > SESSION_IDLE_MS) void s.t.close()
  }, 60_000)
  sessionReaper.unref()
  // /v1/pack memoization — keyed on shard content (head+count); concurrent
  // full exports are capped
  const packCache = new Map<string, { key: string; pack: unknown }>()
  let packInflight = 0
  // /state short TTL cache + in-flight deduplication — dashboard polls and
  // SSE-triggered reloads share one computation; invalidated on mem 'change'
  const STATE_CACHE_MS = 1_500
  let stateCache: { host: string | undefined; at: number; body: Record<string, unknown> } | undefined
  const stateInflight = new Map<string, Promise<Record<string, unknown>>>()
  const clearStateCache = () => {
    stateCache = undefined
  }
  mem.events.on('change', clearStateCache)
  // /dashboard/events subscriber cap
  let sseClients = 0
  const SSE_MAX = 32
  const mcp = async (req: IncomingMessage, res: import('node:http').ServerResponse, authed: boolean) => {
    const { StreamableHTTPServerTransport } = await import('@modelcontextprotocol/sdk/server/streamableHttp.js')
    const { createMcpServer } = await import('./mcp-server.js')
    const sid = req.headers['mcp-session-id']
    const parsed = req.method === 'POST' ? await body(req) : undefined
    let entry = typeof sid === 'string' ? sessions.get(sid) : undefined
    if (!entry) {
      if (req.method !== 'POST' || typeof sid === 'string') {
        res.writeHead(404, { 'content-type': 'application/json' })
        res.end(json({ jsonrpc: '2.0', error: { code: -32001, message: 'unknown MCP session' }, id: null }))
        return
      }
      if (sessions.size >= SESSION_MAX) {
        res.writeHead(503, { 'content-type': 'application/json' })
        res.end(json({ jsonrpc: '2.0', error: { code: -32000, message: 'too many MCP sessions' }, id: null }))
        return
      }
      const t: import('@modelcontextprotocol/sdk/server/streamableHttp.js').StreamableHTTPServerTransport = new StreamableHTTPServerTransport({
        sessionIdGenerator: () => randomUUID(),
        onsessioninitialized: (id) => {
          sessions.set(id, { t, lastSeen: Date.now() })
        }
      })
      t.onclose = () => {
        if (t.sessionId) sessions.delete(t.sessionId)
      }
      // manifest-priced recall/exportShard must invoice unauthenticated MCP
      // callers exactly like REST — the external flag is the caller's actual
      // auth state, not the transport
      await createMcpServer(mem, ops, { external: !authed }).connect(t)
      entry = { t, lastSeen: Date.now() }
    }
    entry.lastSeen = Date.now()
    const transport = entry.t
    await transport.handleRequest(req, res, parsed)
  }
  const server = createServer(
    (req, res) =>
      void (async () => {
        const send = (code: number, v: unknown, type = 'application/json') => {
          res.writeHead(code, { 'content-type': type })
          res.end(typeof v === 'string' ? v : json(v))
        }
        const url = new URL(req.url ?? '/', 'http://x')
        const p = url.pathname.split('/').filter(Boolean)
        const readJsonBody = () => body(req)
        try {
          if (await auth.handleRoute(req, res, url, readJsonBody)) return
          if (handleAdaptersRoute(req, res, url, httpOpts?.adaptersRoot)) return
        } catch (e) {
          if (!res.headersSent) send(400, { error: (e as Error).message })
          return
        }
        const st = auth.authState(req)
        // L0 LLM Memory Proxy manages its own auth so callers can pass
        // X-Nektar-Token or composite `nkt:<nektarToken>:<providerKey>` keys.
        if (url.pathname.startsWith('/v1/proxy/')) {
          try {
            await handleProxyRoute(req, res, url, readJsonBody, { mem, alreadyAuthed: st.authed, fetchImpl: httpOpts?.fetchImpl })
          } catch (e) {
            if (!res.headersSent) send(500, { error: (e as Error).message })
          }
          return
        }
        const publicPath =
          url.pathname === '/' ||
          url.pathname === '/dashboard' ||
          url.pathname.startsWith('/assets/') ||
          url.pathname === '/v1/health' ||
          url.pathname === '/health' ||
          url.pathname === '/healthz' ||
          url.pathname === '/v1/automation/health' ||
          url.pathname === '/v1/verify' ||
          url.pathname === '/v1/badge' ||
          url.pathname === '/card' ||
          url.pathname === '/.well-known/x402' ||
          url.pathname === '/.well-known/nektar' ||
          url.pathname === '/.well-known/agent.json' ||
          url.pathname.startsWith('/v1/pack/') ||
          url.pathname.startsWith('/v1/proof/') ||
          url.pathname.startsWith('/v1/zkproof/') ||
          url.pathname === '/v1/broadcaster-callback' ||
          url.pathname === '/v1/arc-callback' ||
          url.pathname === '/v1/lane' ||
          url.pathname === '/v1/faucet' ||
          url.pathname === '/v1/pay/quote'
        const authed = st.authed
        if (!publicPath && !authed) {
          return send(401, { error: 'unauthorized' })
        }
        try {
          if (url.pathname === '/mcp') return await mcp(req, res, authed)
          if (url.pathname === '/a2a') return await handleA2aRoute(req, res, readJsonBody, mem, ops, authed)
          if (await handleOnboardRoute(req, res, url, readJsonBody, { mem, ops, fetchImpl: httpOpts?.fetchImpl, isOperator: () => st.operator })) return
          if (req.method === 'GET' && (url.pathname === '/v1/health' || url.pathname === '/health' || url.pathname === '/healthz'))
            return send(200, { ok: true })
          if (req.method === 'POST' && url.pathname === '/v1/lane') {
            // direct carrier push — the same bytes a box/lane read would
            // fetch, admitted through the same verify+dedupe before ingest
            const au = mem.automation
            if (au?.pushCarrier === undefined) return send(404, { error: 'no automation host' })
            const laneBody = (await body(req).catch(() => ({}))) as { beef?: string }
            const beef = typeof laneBody.beef === 'string' ? Buffer.from(laneBody.beef, 'hex') : undefined
            if (beef === undefined || beef.length === 0 || beef.length > 128 * 1024 || beef.toString('hex') !== laneBody.beef)
              return send(400, { error: 'a hex carrier beef is required (<= 128 KiB)' })
            const kind = await au.pushCarrier([...beef]).catch((e) => {
              // a header outage reaches the outer 500 — "couldn't ask,
              // retry", never the 422 refusal a submitter would record
              if (isHeaderUnavailable(e)) throw e
              return false
            })
            return kind === false ? send(422, { error: 'carrier failed verification' }) : send(200, { ok: true, kind })
          }
          if (req.method === 'GET' && url.pathname === '/v1/openapi.json') return send(200, OPENAPI)
          if (req.method === 'GET' && url.pathname === '/v1/metrics') return send(200, mem.metrics.render(), 'text/plain; version=0.0.4')
          if (req.method === 'GET' && url.pathname === '/events') {
            // SSE — one line per state change; the page swaps its blocks on each
            if (sseClients >= SSE_MAX) return send(429, { error: 'too many dashboard subscribers' })
            sseClients++
            res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive' })
            res.write(': connected\n\n')
            const push = (name: string) => {
              try {
                res.write(`data: ${name}\n\n`)
              } catch {}
            }
            mem.events.on('change', push)
            const beat = setInterval(() => {
              try {
                res.write(': beat\n\n')
              } catch {}
            }, 15_000)
            req.on('close', () => {
              sseClients--
              clearInterval(beat)
              mem.events.off('change', push)
            })
            return
          }
          if (req.method === 'GET' && url.pathname === '/state') {
            const host = req.headers.host
            if (stateCache !== undefined && stateCache.host === host && Date.now() - stateCache.at < STATE_CACHE_MS) {
              return send(200, stateCache.body)
            }
            const hostKey = host ?? ''
            let inflight = stateInflight.get(hostKey)
            if (inflight === undefined) {
              inflight = dashboardState(mem, ops, host)
                .then((b) => {
                  stateCache = { host, at: Date.now(), body: b }
                  return b
                })
                .finally(() => {
                  stateInflight.delete(hostKey)
                })
              stateInflight.set(hostKey, inflight)
            }
            return send(200, await inflight)
          }
          if (req.method === 'GET' && url.pathname.startsWith('/assets/')) {
            const name = url.pathname.slice('/assets/'.length)
            if (!/^[\w.-]+$/.test(name)) return send(400, { error: 'bad asset name' })
            const a = dashboardFile(`assets/${name}`)
            if (!a) return send(404, { error: 'no such asset' })
            res.writeHead(200, { 'content-type': a.type, 'cache-control': 'public, max-age=31536000, immutable' })
            return res.end(a.body)
          }
          // old tabler-era URL — the app lives at /
          if (req.method === 'GET' && url.pathname === '/dashboard') return res.writeHead(301, { location: '/' }).end()
          if (req.method === 'GET' && url.pathname === '/') {
            const html = dashboardFile('index.html')
            if (html) {
              res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
              return res.end(html.body)
            }
            return send(
              200,
              '<!doctype html><html><body style="background:#221e1d;color:#eae6e3;font:15px system-ui;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0"><div>dashboard bundle missing — run <code>pnpm --filter @nektar/mcp build</code></div></body></html>',
              'text/html; charset=utf-8'
            )
          }
          // public surface: {pass} ONLY — reserves, hosts and policy details
          // never leave the unauthenticated path (preflight's full report is
          // the token-gated route below)
          if (req.method === 'GET' && url.pathname === '/v1/automation/health') {
            const au = mem.automation
            if (!au) return send(404, { error: 'automation not enabled' })
            return send(200, { pass: au.health().pass })
          }
          if (req.method === 'GET' && url.pathname === '/v1/automation/preflight') {
            const au = mem.automation
            if (!au) return send(404, { error: 'automation not enabled' })
            return send(200, await au.preflight())
          }
          if (req.method === 'GET' && url.pathname === '/v1/automation/state') {
            const au = mem.automation
            if (!au) return send(404, { error: 'automation not enabled' })
            return send(200, {
              operations: (await au.store.operations()).map((o) => ({
                operationId: o.operationId,
                role: o.role,
                kind: o.kind,
                state: o.state,
                lastError: o.lastError
              })),
              approvals: await au.store.approvals('pending')
            })
          }
          if (req.method === 'POST' && p[0] === 'v1' && p[1] === 'automation' && p[2] === 'approvals' && p.length === 4) {
            const au = mem.automation
            if (!au) return send(404, { error: 'automation not enabled' })
            const operator = nektarEnv('NEKTAR_OPERATOR_TOKEN')
            if (!operator) return send(403, { error: 'operator approvals disabled — set NEKTAR_OPERATOR_TOKEN' })
            if (!st.operator) return send(403, { error: 'operator token required (the agent bearer cannot approve)' })
            const origin = req.headers.origin
            if (origin && !/^https?:\/\/(localhost|127\.0\.0\.1)(:\d+)?$/i.test(origin)) return send(403, { error: 'bad origin' })
            const decisionBody = (await body(req).catch(() => ({}))) as { approve?: boolean }
            let r
            try {
              r = await au.store.decideApproval(p[3], decisionBody.approve === true, 'dashboard:operator')
            } catch (e) {
              return send(400, { error: (e as Error).message })
            }
            mem.events.emit('change', 'automation')
            return send(200, r)
          }
          // /v1/hosts — the join flow. Paste-anything (bare host, host/path, a
          // well-known URL) → origin; a bounded GET of its /.well-known/nektar
          // proves a nektar answers there and returns its advertised peers,
          // which is how one pasted host bootstraps the whole join. Persists
          // to hosts.json (env ∪ file = marketHosts()) and hot-adds into the
          // running lanes via au.addHost — no restart, no env edit.
          if (req.method === 'POST' && url.pathname === '/v1/hosts') {
            const operator = nektarEnv('NEKTAR_OPERATOR_TOKEN')
            if (!operator) return send(403, { error: 'host join/leave disabled — set NEKTAR_OPERATOR_TOKEN' })
            if (!st.operator) return send(403, { error: 'operator token required' })
            const bu = ((await body(req).catch(() => ({}))) as { url?: unknown }).url
            const u0 = (typeof bu === 'string' ? bu : '').trim()
            if (u0 === '') return send(400, { error: 'url required' })
            let origin: string
            try {
              const u = new URL(/^https?:\/\//i.test(u0) ? u0 : `https://${u0}`)
              const local = /^(localhost|127\.|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|\[?::1\]?)/.test(u.hostname)
              if (u.protocol !== 'https:' && !local) throw new Error('https only')
              origin = u.origin
            } catch {
              return send(400, { error: 'not a usable host URL (paste a nektar URL or origin)' })
            }
            const probe = await deadline(
              (signal) =>
                fetch(`${origin}/.well-known/nektar`, { signal }).then(async (r) => (r.ok ? ((await r.json()) as Record<string, unknown>) : undefined)),
              4_000
            ).catch(() => undefined)
            const doc = probe?.outcome === 'settled' ? probe.value : undefined
            if (doc === undefined) return send(502, { error: `nothing answered at ${origin}/.well-known/nektar` })
            if (doc.kind !== 'nektar-discovery') return send(502, { error: `${origin} answers but is not a nektar` })
            addSavedHost(origin)
            mem.automation?.addHost?.(origin)
            const known = new Set(marketHosts())
            const discovered = ((doc.market as { hosts?: unknown[] }).hosts ?? [])
              .map((h) => String(h).replace(/\/+$/, ''))
              .filter((h) => h !== '' && h !== origin && !known.has(h))
            mem.events.emit('change', 'automation')
            return send(200, { url: origin, pub: doc.operatorPub ?? null, discovered })
          }
          // DELETE /v1/hosts?url= — leave a host: a saved join is dropped; an
          // env or seed host lands in `blocked` so the leave survives restart.
          // au.removeHost splices the live lane array + marks it down now.
          if (req.method === 'DELETE' && url.pathname === '/v1/hosts') {
            const operator = nektarEnv('NEKTAR_OPERATOR_TOKEN')
            if (!operator) return send(403, { error: 'host join/leave disabled — set NEKTAR_OPERATOR_TOKEN' })
            if (!st.operator) return send(403, { error: 'operator token required' })
            const u = (url.searchParams.get('url') ?? '').trim().replace(/\/+$/, '')
            if (u === '') return send(400, { error: 'url required' })
            if (!marketHosts().includes(u)) return send(404, { error: `${u} is not a market host` })
            removeSavedHost(u)
            mem.automation?.removeHost?.(u)
            mem.events.emit('change', 'automation')
            return send(200, { url: u, removed: true })
          }
          // POST /v1/faucet {address} — a self-serve testnet drip so a
          // stranger's funding step stops being a conversation. The spend
          // rides sendSats (SqlFunds + SpendGuard); faucet.ts owns the
          // per-address/daily caps. Gate is evaluated per request.
          if (req.method === 'POST' && url.pathname === '/v1/faucet') {
            if (!faucetEnabled()) return send(404, { error: 'no faucet on this instance' })
            const fa = ((await body(req).catch(() => ({}))) as { address?: unknown }).address
            if (typeof fa !== 'string' || fa.trim() === '') return send(400, { error: 'address required' })
            const r = await faucetDrip({ mem, address: fa, dataDir: nektarEnv('NEKTAR_DATA_DIR') ?? '.nektar' }).catch((e: Error) => ({
              ok: false as const,
              status: 502,
              error: `drip failed: ${e.message.slice(0, 120)}`
            }))
            if (!r.ok) return send(r.status, { error: r.error })
            return send(200, { txid: r.txid, sats: r.sats, network: nektarEnv('NEKTAR_NETWORK') ?? 'regtest' })
          }
          if (req.method === 'GET' && url.pathname === '/v1/tools') return send(200, { tools: await visibleTools(mem) })
          if (req.method === 'POST' && p[0] === 'v1' && p[1] === 'tools' && p.length === 3) {
            if (!(await visibleTools(mem)).some((t) => t.name === p[2])) return send(404, { error: `unknown tool ${p[2]}` })
            try {
              return send(200, await callTool(mem, p[2], await body(req), { ops, external: !authed }))
            } catch (e) {
              return send(400, { error: (e as Error).message })
            }
          }
          if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'zkproof' && p.length === 3) {
            const bytes = mem.getProof(decodeURIComponent(p[2]))
            if (!bytes) return send(404, { error: 'no proof artifact under that commitment' })
            res.writeHead(200, { 'content-type': 'application/octet-stream' })
            return res.end(Buffer.from(bytes))
          }
          if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'proof' && p.length === 4) {
            // parity with the prove tool — a private shard's plain merkle
            // citation is a leak even on this public route
            const shard = decodeURIComponent(p[2])
            if (mem.shardPrivate(shard) || shard.startsWith('__'))
              return send(403, { error: 'private shard — plain citations refused; verify via proveMembership' })
            const r = await mem.prove(shard, Number(p[3]))
            return send(200, { result: r.result, bundle: r.bundle ?? null })
          }
          if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'checkpoint' && p.length === 3) {
            const cp = mem.checkpoints(decodeURIComponent(p[2])).at(-1)
            return cp ? send(200, { checkpoint: cp, logPublicKey: await mem.logPublicKey() }) : send(404, { error: 'no checkpoint' })
          }
          if (req.method === 'GET' && url.pathname === '/v1/verify') {
            const tok = url.searchParams.get('r')
            const seq = url.searchParams.get('s')
            if (!tok && seq === null) return send(400, { error: 'missing ?r=<receipt token> or ?s=<receipt seq>' })
            try {
              const { verifyReceipt } = await import('./receipt.js')
              let arg: { token?: string; receiptHash?: string } = { token: tok! }
              if (!tok) {
                // ?s=N — human-readable form: the Nth record on __receipts__
                const rec = (await mem.recall('__receipts__', { seq: Number(seq) })).items[0]
                const h = rec.text?.split(':')[1]
                if (!h) return send(404, { error: `no receipt at __receipts__ seq ${seq}` })
                arg = { receiptHash: h }
              }
              const result = await verifyReceipt(mem, arg)
              if (url.searchParams.get('format') === 'html' || req.headers.accept?.includes('text/html')) {
                const base = nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://${safeHost(req.headers.host)}`
                const q = tok ? `r=${tok}` : `s=${seq}`
                mem.metrics.inc('nektar_verify_views_total', {}, 1, 'verify page views')
                // viral edge: cited shards this operator offers publicly become
                // a one-line import CTA — "take the memory behind this answer"
                const offerable = new Set(await mem.publicShards().catch(() => [] as string[]))
                const cited = [
                  ...new Set(
                    ((result.receipt as { fragmentRefs?: Array<{ shard: string }> } | undefined)?.fragmentRefs ?? [])
                      .map((f) => f.shard)
                      .filter((s) => offerable.has(s))
                  )
                ]
                return send(
                  200,
                  await verifyPage(result, `${base}/v1/verify?${q}&format=html`, {
                    base,
                    importShards: cited,
                    totals: { views: mem.metrics.value('nektar_verify_views_total'), anchors: mem.metrics.value('nektar_anchors_total') },
                    operator: (await mem.fundingStatus()).address
                  }),
                  'text/html; charset=utf-8'
                )
              }
              return send(200, result)
            } catch (e) {
              return send(400, { error: (e as Error).message })
            }
          }
          if (req.method === 'GET' && url.pathname === '/v1/badge') {
            const tok = url.searchParams.get('r')
            if (!tok) return send(400, { error: 'missing ?r=<receipt token>' })
            try {
              const { verifyReceipt } = await import('./receipt.js')
              const result = await verifyReceipt(mem, { token: tok })
              const pass = result.checks.filter((c) => c.ok === true).length
              const total = result.checks.filter((c) => c.ok !== 'info' && c.ok !== 'skip').length
              const att = (result.receipt as { modelAttestation?: { verifierTxid?: string } } | undefined)?.modelAttestation
              const value = result.ok ? `verified ${pass}/${total}${att?.verifierTxid ? ' · model on-chain' : att ? ' · model attested' : ''}` : 'failed'
              return send(200, badgeSvg(value, result.ok ? '#2ea043' : '#e5534b'), 'image/svg+xml; charset=utf-8')
            } catch {
              return send(200, badgeSvg('unverifiable', '#6e7681'), 'image/svg+xml; charset=utf-8')
            }
          }
          // broadcaster tx-status callback — pushes SEEN_ON_NETWORK → MINED etc.
          // for every tx we submitted with X-CallbackUrl, so anchor confirmations
          // arrive as events, not polls. /v1/arc-callback is kept for transactions
          // submitted before the rename (they carry the old URL). Outbound-only
          // mode instead consumes the same statuses via the /events SSE stream —
          // the handler below is shared by both paths.
          if (req.method === 'POST' && (url.pathname === '/v1/broadcaster-callback' || url.pathname === '/v1/arc-callback')) {
            try {
              // NEKTAR_CALLBACK_TOKEN was sent as X-CallbackToken; BSV Broadcaster
              // delivers it on the webhook as Authorization: Bearer (openapi)
              const cbTok = nektarEnv('NEKTAR_CALLBACK_TOKEN')
              if (cbTok) {
                const expected = Buffer.from(`Bearer ${cbTok}`)
                const given = Buffer.from(String(req.headers.authorization ?? ''))
                if (given.length !== expected.length || !timingSafeEqual(given, expected)) return send(401, { error: 'bad callback token' })
              } else if ((nektarEnv('NEKTAR_NETWORK') ?? 'regtest') !== 'regtest') {
                return send(403, { error: 'callback disabled on live networks without NEKTAR_CALLBACK_TOKEN' })
              }
              const j = (await body(req)) as { txid?: string; txStatus?: string; merklePath?: string; extraInfo?: string }
              await handleBroadcasterStatus(mem, j)
              return send(200, { ok: true })
            } catch (e) {
              if (isHeaderUnavailable(e)) return send(503, { error: 'headers unavailable — retry' })
              return send(400, { error: 'bad callback' })
            }
          }
          // F3 budget state — the cumulative ledger's live numbers for a grant
          if (req.method === 'GET' && url.pathname === '/v1/budget') {
            const gid = url.searchParams.get('grantId') ?? ''
            const s = gid ? mem.budget?.status(gid) : undefined
            return send(200, { grantId: gid, budget: s ?? null })
          }
          if (req.method === 'GET' && url.pathname === '/.well-known/x402') {
            const { paymentsEnabled, PAID_TOOLS, priceFor } = await import('./pay.js')
            if (!paymentsEnabled()) return send(404, { error: 'payments disabled (NEKTAR_PAYMENTS=1)' })
            const n = nektarEnv('NEKTAR_NETWORK') ?? 'regtest'
            return send(200, {
              x402Version: 2,
              scheme: 'exact',
              network: n === 'main' ? 'bsv:mainnet' : n === 'testnet' ? 'bsv:testnet' : n === 'regtest' ? 'bsv:regtest' : 'bsv:regtest',
              resources: [...PAID_TOOLS].map((t) => ({
                url: `tool://${t}`,
                description: `nektar ${t}`,
                priceSats: priceFor(t),
                payVia: `call ${t} without payment → invoice in the result, or GET /v1/pay/quote?tool=${t}`
              }))
            })
          }
          if (req.method === 'GET' && url.pathname === '/.well-known/agent.json') {
            return send(200, buildAgentCard(mem, req))
          }
          if (req.method === 'GET' && url.pathname === '/.well-known/nektar') {
            if (/^(0|false|no)$/i.test(nektarEnv('NEKTAR_PACK_SHARE') ?? '1')) return send(404, { error: 'pack sharing disabled (NEKTAR_PACK_SHARE=0)' })
            const base = nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://${req.headers.host ?? 'localhost'}`
            const pub = (await mem.publicShards()).filter((s) => !s.startsWith('__'))
            const catalog = await mem.shardCatalog().catch(() => [] as import('./memory.js').ShardManifest[])
            return send(200, {
              v: 1,
              kind: 'nektar-discovery',
              verifyBase: `${base}/v1/verify`,
              card: `${base}/card`,
              logPublicKey: await mem.logPublicKey(),
              operatorPub: await mem.operatorSigner().pubKeyHex(),
              // self-describing market config — is this instance a quorum
              // participant, and through which hosts? Report the live engine
              // state (mem.automation) not just the env flag.
              market: {
                automation: mem.automation != null,
                // the effective set — env or baked-in seeds, same list the
                // runtime reads through
                hosts: marketHosts(),
                push: nektarEnv('NEKTAR_PUSH_PUBLIC') ?? null
              },
              publicShards: await Promise.all(
                pub.map(async (s) => {
                  const manifest = catalog.find((m) => m.shard === s)
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
                    ...(mem.packStoreRef(s) !== undefined ? { store: mem.packStoreRef(s) } : {}),
                    import: { tool: 'importShard', args: { from: base, shard: s } }
                  }
                })
              )
            })
          }
          // /card — the human twin of the discovery doc: the one URL the
          // dashboard's share button produces. Public like the well-known
          // doc it renders; same NEKTAR_PACK_SHARE gate.
          if (req.method === 'GET' && url.pathname === '/card') {
            if (/^(0|false|no)$/i.test(nektarEnv('NEKTAR_PACK_SHARE') ?? '1')) return send(404, { error: 'pack sharing disabled (NEKTAR_PACK_SHARE=0)' })
            return send(200, await cardPage(mem, req.headers.host), 'text/html; charset=utf-8')
          }
          if (req.method === 'GET' && p[0] === 'v1' && p[1] === 'pack' && p.length === 3) {
            if (/^(0|false|no)$/i.test(nektarEnv('NEKTAR_PACK_SHARE') ?? '1')) return send(404, { error: 'pack sharing disabled (NEKTAR_PACK_SHARE=0)' })
            const shard = decodeURIComponent(p[2])
            // system + private shards never travel via packs — the public
            // export surface is for shareable public shards only
            if (shard.startsWith('__') || mem.shardPrivate(shard)) return send(403, { error: 'shard not shareable via pack' })
            try {
              // memoize on the shard's content key — export re-runs a full
              // prove+verify per record, so repeat/concurrent hits must not
              // pay it again. A shifted head/record count busts the entry.
              const head = mem.tailLink(shard)?.txid ?? ''
              const n = await mem.shardCount(shard)
              const ckey = `${shard}|${head}|${n}`
              const etag = `W/"${createHash('sha256').update(ckey).digest('hex').slice(0, 32)}"`
              if (req.headers['if-none-match'] === etag && packCache.get(shard)?.key === ckey) return send(304, '', 'text/plain')
              if (packInflight >= 2) return send(429, { error: 'pack export busy — retry shortly' })
              let cached = packCache.get(shard)
              if (cached?.key !== ckey) {
                packInflight++
                try {
                  cached = { key: ckey, pack: await mem.exportShardPack(shard) }
                  packCache.set(shard, cached)
                } finally {
                  packInflight--
                }
              }
              mem.metrics.inc('nektar_packs_served_total', {}, 1, 'public shard packs served')
              res.writeHead(200, { 'content-type': 'application/json', etag })
              return res.end(json(cached.pack))
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
            const pr = await paymentRequired(mem, tool)
            res.writeHead(402, { 'content-type': 'application/json', 'PAYMENT-REQUIRED': Buffer.from(JSON.stringify(pr.required)).toString('base64') })
            return res.end(json(pr))
          }
          if (req.method === 'GET' && url.pathname === '/v1/wallet') {
            return ops?.wallet ? send(200, await ops.wallet()) : send(404, { error: 'ops not enabled (main only)' })
          }
          if (req.method === 'GET' && url.pathname === '/v1/costs') {
            return ops?.costs ? send(200, { costs: ops.costs() }) : send(404, { error: 'ops not enabled (main only)' })
          }
          return send(404, { error: 'not found' })
        } catch (e) {
          if (!res.headersSent) send(500, { error: (e as Error).message })
        }
      })()
  )
  server.on('close', () => {
    clearInterval(sessionReaper)
    mem.events.off('change', clearStateCache)
    for (const s of sessions.values()) void s.t.close()
  })
  return server
}
