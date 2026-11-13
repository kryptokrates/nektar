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
//   POST /notify {service?, query?}   → wakes matching waiters early. A
//        sender's direct-push hint; always safe — waiters re-verify via their
//        own reads, so a spurious wake costs one poll.
//   GET  /healthz → 200 once listening.
//
// Everything degrades to ordinary polling: a client that gets no shim, a
// shim that loses its backend, a peer with no watch URL — all still work.
//
// Env: BACKEND (required) the stock host base URL, PORT (8090), POLL_MS (150),
//      MAX_WAIT_MS (25000).
import http from 'node:http'
import { createHash } from 'node:crypto'

const BACKEND = (process.env.BACKEND ?? '').replace(/\/+$/, '')
const PORT = Number(process.env.PORT ?? 8090)
const POLL_MS = Number(process.env.POLL_MS ?? 150)
const MAX_WAIT_MS = Number(process.env.MAX_WAIT_MS ?? 25000)

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

// waiters keyed by service|canon(query) so /notify can target a lane
const keyOf = (service, query) => `${service}|${JSON.stringify(query, Object.keys(query ?? {}).sort())}`
const waiters = new Map() // key -> Set<resolve>
const wake = (key) => {
  const set = waiters.get(key)
  if (set === undefined) return
  waiters.delete(key)
  for (const r of set) r()
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
      let body = ''
      for await (const c of req) body += c
      const p = JSON.parse(body || '{}')
      if (p.service !== undefined) {
        wake(keyOf(p.service, p.query))
        // senders can't reproduce a foreign watcher's exact query shape, so
        // the hint is service-scoped: every held poll for the service
        // re-checks. Always honest — a spurious wake costs one lookup
        for (const k of [...waiters.keys()]) if (k.startsWith(`${p.service}|`)) wake(k)
      } else {
        for (const k of [...waiters.keys()]) wake(k)
      }
      return json(200, { ok: true })
    }

    if (url.pathname === '/watch' && req.method === 'GET') {
      const service = url.searchParams.get('service')
      if (!service) return json(400, { error: 'service required' })
      const query = JSON.parse(url.searchParams.get('query') ?? '{}')
      const want = url.searchParams.get('cursor')
      const waitMs = Math.min(Number(url.searchParams.get('waitMs') ?? MAX_WAIT_MS), MAX_WAIT_MS)
      const key = keyOf(service, query)

      const deadline = Date.now() + waitMs
      let lastErr
      for (;;) {
        let outputs
        try {
          outputs = await lookup(service, query)
          lastErr = undefined
        } catch (e) {
          lastErr = e
          // backend blip — keep holding; the client's own poll path covers
          outputs = undefined
        }
        if (outputs !== undefined) {
          const cursor = fingerprint(outputs)
          // no cursor → report state immediately; changed → wake now
          if (want === null || want !== cursor) {
            return json(200, { changed: want !== null && want !== cursor, cursor, n: outputs.length })
          }
        }
        if (Date.now() >= deadline) {
          return json(200, {
            changed: false,
            cursor: want,
            ...(lastErr !== undefined ? { backend: (lastErr instanceof Error ? lastErr.message : JSON.stringify(lastErr)).slice(0, 120) } : {})
          })
        }
        // wait for either the notify edge or the next backend poll — remove
        // the waiter either way so a timed-out leg can't accumulate
        let set = waiters.get(key)
        if (set === undefined) waiters.set(key, (set = new Set()))
        let done
        await Promise.race([
          new Promise((r) => setTimeout(r, POLL_MS)),
          new Promise((r) => {
            done = () => r()
            set.add(done)
          })
        ])
        set.delete(done)
        if (req.destroyed) return // client went away
      }
    }

    return json(404, { error: 'not found' })
  } catch (e) {
    return json(500, { error: String(e).slice(0, 200) })
  }
}
const server = http.createServer((req, res) => void handle(req, res))

server.listen(PORT, () => console.log(`watch-shim: :${PORT} → ${BACKEND} (poll ${POLL_MS}ms, maxWait ${MAX_WAIT_MS}ms)`))
