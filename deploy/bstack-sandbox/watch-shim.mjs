// watch-shim.mjs — long-poll wake layer in front of ANY stock overlay host.
//
// Stock lightweb hosts speak only BRC-22 /submit + BRC-24 /lookup — pull
// only. Agents that poll them pay one poll-interval per protocol hop; a deal
// is ~8 hops, so wake latency dominates the whole exchange. This shim adds
// push semantics WITHOUT changing the host: it fronts the host's /lookup,
// holds client connections, polls the backend on a tight loop, and releases
// waiters the moment the result set changes.
//
//   GET  /watch?service=<name>&query=<urlencoded-json>&cursor=<hex>&waitMs=<ms>
//        → { changed: true,  cursor }  as soon as the result fingerprint moves
//          { changed: false, cursor }  after waitMs with no change
//        First call without a cursor returns the current fingerprint
//        immediately — clients then long-poll on it.
//   POST /notify {service, query?}   → re-polls the service's lanes NOW and
//        wakes any waiters whose fingerprint moved. A sender's direct-push
//        hint; always safe — waiters re-verify via their own reads, so a
//        spurious wake costs one poll.
//   GET  /healthz → 200 once listening.
//
// One backend poller per (service, query) key, shared by every held
// connection on that key — N watchers on one lane cost one /lookup per
// POLL_MS, not N. A poller lives only while the key has waiters.
//
// Everything degrades to ordinary polling: a client that gets no shim, a
// shim that loses its backend, a peer with no watch URL — all still work.
//
// Env: BACKEND (required) the stock host base URL, PORT (8090), POLL_MS (150),
//      MAX_WAIT_MS (25000), MAX_WAITERS (2000).
import http from 'node:http'
import { createHash } from 'node:crypto'

const BACKEND = (process.env.BACKEND ?? '').replace(/\/+$/, '')
const PORT = Number(process.env.PORT ?? 8090)
const POLL_MS = Number(process.env.POLL_MS ?? 150)
const MAX_WAIT_MS = Number(process.env.MAX_WAIT_MS ?? 25000)
const MAX_WAITERS = Number(process.env.MAX_WAITERS ?? 2000)
const MAX_NOTIFY_BODY = 64 * 1024
const MAX_QUERY_LEN = 4096

if (BACKEND === '') {
  console.error('watch-shim: BACKEND env required (stock host base URL)')
  process.exit(1)
}

// fingerprint = sha256 of the canonically-sorted outputs — change on any
// add/remove/order-shift. We never parse BEEF; the client re-reads anyway.
const fingerprint = (outputs) => {
  const canon = outputs.map((o) => JSON.stringify(o)).sort()
  return createHash('sha256').update(canon.join('')).digest('hex')
}

const lookup = async (service, query) => {
  const r = await fetch(`${BACKEND}/lookup`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ service, query }),
    signal: AbortSignal.timeout(5000)
  })
  if (!r.ok) throw new Error(`backend /lookup ${r.status}`)
  const j = await r.json()
  return j.outputs ?? []
}

// canonical JSON with recursively sorted keys — {wants:{net,domain}} for two
// different domains must be two keys, not one
const canon = (v) => {
  if (Array.isArray(v)) return `[${v.map(canon).join(',')}]`
  if (v !== null && typeof v === 'object')
    return `{${Object.keys(v)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${canon(v[k])}`)
      .join(',')}}`
  return JSON.stringify(v)
}
const keyOf = (service, query) => `${service}|${canon(query)}`

// key -> one shared backend poller + the waiters it releases
const watchers = new Map()

const poll = (key) => {
  const w = watchers.get(key)
  if (w === undefined) return Promise.resolve()
  // concurrent callers share the in-flight poll rather than double-fetching
  // or skipping it — a /watch that needs fresh state awaits the same promise
  if (w.inflight !== undefined) return w.inflight
  w.inflight = (async () => {
    const prev = w.cursor
    try {
      const outputs = await lookup(w.service, w.query)
      w.cursor = fingerprint(outputs)
      w.n = outputs.length
      w.at = Date.now()
      w.lastErr = undefined
      // a waiter resolves when the served fingerprint is not the one it
      // holds — covers a move off the previous cursor AND the first landing
      // after a backend outage (prev === undefined)
      if (w.cursor !== prev || prev === undefined) {
        for (const waiter of w.waiters) {
          if (waiter.want !== w.cursor) {
            w.waiters.delete(waiter)
            waiter.resolve(w.cursor, w.n)
          }
        }
      }
    } catch (e) {
      // backend blip — keep holding; the client's own poll path covers
      w.lastErr = e
    }
    w.inflight = undefined
    schedule(key)
  })()
  return w.inflight
}

const schedule = (key) => {
  const w = watchers.get(key)
  if (w === undefined) return
  clearTimeout(w.pollTimer)
  w.pollTimer = undefined
  if (w.waiters.size === 0) {
    // nobody is watching this key — the poller's job is done
    watchers.delete(key)
    return
  }
  w.pollTimer = setTimeout(() => void poll(key), POLL_MS)
}

const heldWaiters = () => {
  let n = 0
  for (const w of watchers.values()) n += w.waiters.size
  return n
}

// per-remote-address token bucket for /notify: burst 20, refill 20/s
const buckets = new Map()
const notifyAllowed = (ip) => {
  const now = Date.now()
  const b = buckets.get(ip) ?? { tokens: 20, at: now }
  b.tokens = Math.min(20, b.tokens + ((now - b.at) / 1000) * 20)
  b.at = now
  buckets.set(ip, b)
  if (b.tokens < 1) return false
  b.tokens -= 1
  return true
}

const handle = async (req, res) => {
  const url = new URL(req.url ?? '/', 'http://x')
  const json = (code, body) => {
    res.writeHead(code, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body))
  }
  try {
    if (url.pathname === '/healthz') return json(200, { ok: true, backend: BACKEND })

    if (url.pathname === '/notify' && req.method === 'POST') {
      if (!notifyAllowed(req.socket.remoteAddress ?? 'unknown')) return json(429, { error: 'rate limited' })
      let body = ''
      let size = 0
      for await (const c of req) {
        size += c.length
        if (size > MAX_NOTIFY_BODY) return json(413, { error: 'body too large' })
        body += c
      }
      let p
      try {
        p = JSON.parse(body || '{}')
      } catch {
        return json(400, { error: 'malformed json' })
      }
      if (typeof p !== 'object' || p === null || typeof p.service !== 'string' || p.service === '') {
        return json(400, { error: 'service required' })
      }
      // a notify is a hint, not state: re-poll the service's lanes now and
      // let the fingerprint decide who actually wakes
      for (const k of watchers.keys()) if (k.startsWith(`${p.service}|`)) void poll(k)
      return json(200, { ok: true })
    }

    if (url.pathname === '/watch' && req.method === 'GET') {
      const service = url.searchParams.get('service')
      if (!service) return json(400, { error: 'service required' })
      const rawQuery = url.searchParams.get('query') ?? '{}'
      if (rawQuery.length > MAX_QUERY_LEN) return json(400, { error: 'query too large' })
      let query
      try {
        query = JSON.parse(rawQuery)
      } catch {
        return json(400, { error: 'malformed query' })
      }
      const want = url.searchParams.get('cursor')
      const waitMs = Math.min(Number(url.searchParams.get('waitMs') ?? MAX_WAIT_MS), MAX_WAIT_MS)
      const key = keyOf(service, query)
      let w = watchers.get(key)
      if (w === undefined) {
        w = { service, query, cursor: undefined, n: 0, at: 0, lastErr: undefined, waiters: new Set(), pollTimer: undefined, inflight: undefined }
        watchers.set(key, w)
      }
      // one lookup now unless this key's state is already fresh
      if (w.at === 0 || Date.now() - w.at > POLL_MS) await poll(key)
      if (w.cursor !== undefined && (want === null || want !== w.cursor)) {
        return json(200, { changed: want !== null && want !== w.cursor, cursor: w.cursor, n: w.n })
      }
      if (heldWaiters() >= MAX_WAITERS) return json(503, { error: 'too many waiters' })
      // matching cursor — or no state yet because the backend is down: hold
      // until a poll lands a different fingerprint or this request's own
      // waitMs runs out, then degrade (the caller's own poll path covers)
      return await new Promise((resolve) => {
        const waiter = {
          want,
          resolve: (cursor, n) => finish({ changed: want !== null && want !== cursor, cursor, n })
        }
        const finish = (body) => {
          w.waiters.delete(waiter)
          clearTimeout(timer)
          schedule(key)
          resolve(json(200, body))
        }
        const timer = setTimeout(
          () =>
            finish({
              changed: false,
              cursor: want,
              ...(w.lastErr !== undefined ? { backend: (w.lastErr instanceof Error ? w.lastErr.message : JSON.stringify(w.lastErr)).slice(0, 120) } : {})
            }),
          waitMs
        )
        // this request's own poll() may have dropped the entry while it ran
        // (last-waiter-leaves cleanup) — re-attach before joining
        watchers.set(key, w)
        w.waiters.add(waiter)
        schedule(key)
        req.on('close', () => {
          if (req.destroyed && w.waiters.delete(waiter)) {
            clearTimeout(timer)
            schedule(key)
          }
        })
      })
    }

    return json(404, { error: 'not found' })
  } catch (e) {
    return json(500, { error: String(e).slice(0, 200) })
  }
}
const server = http.createServer((req, res) => void handle(req, res))

server.listen(PORT, () => console.log(`watch-shim: :${PORT} → ${BACKEND} (poll ${POLL_MS}ms, maxWait ${MAX_WAIT_MS}ms, maxWaiters ${MAX_WAITERS})`))
