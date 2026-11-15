// watch-shim.mjs contract — the wake layer's own guarantees:
//   - /watch with no cursor answers immediately (never hangs a boot)
//   - /watch with a matching cursor holds until fingerprint change or waitMs
//   - /notify re-polls the backend NOW for the matching service only
//   - a dead backend holds to waitMs and degrades — callers never error out
// These are the semantics runtime.ts's watchOne relies on: a wake is a hint,
// ordinary polling stays the correctness floor.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawn, type ChildProcess } from 'node:child_process'
import { createServer, type Server } from 'node:http'
import { fileURLToPath } from 'node:url'

const SHIM = fileURLToPath(new URL('../../../../deploy/bstack-sandbox/watch-shim.mjs', import.meta.url))

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

async function stack(t: test.TestContext): Promise<{
  watch: (qs: string) => Promise<{ changed: boolean; cursor: string; tookMs?: number; backend?: string }>
  postNotify: (body: unknown | string) => Promise<Response>
  setOutputs: (o: unknown) => void
  setOutputsFor: (query: unknown, o: unknown) => void
  lookups: () => number
  killBackend: () => Promise<void>
}> {
  let outputs: unknown = [{ beef: [1], outputIndex: 0 }]
  const perQuery = new Map<string, unknown>()
  let lookups = 0
  let backend: Server | null = createServer((req, res) => {
    if (req.url === '/lookup') {
      let body = ''
      req.on('data', (c) => (body += c))
      req.on('end', () => {
        lookups++
        let o = outputs
        try {
          const q = JSON.parse(body).query
          const k = JSON.stringify(q)
          if (perQuery.has(k)) o = perQuery.get(k)
        } catch {
          /* malformed lookup body — serve the default set */
        }
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end(JSON.stringify({ outputs: o }))
      })
      return
    }
    res.writeHead(404)
    res.end()
  })
  await new Promise<void>((r) => backend!.listen(0, '127.0.0.1', r))
  const bport = (backend.address() as { port: number }).port
  const sport = 19000 + Math.floor(Math.random() * 2000)
  let shimErr = ''
  const shim: ChildProcess = spawn(process.execPath, [SHIM], {
    env: { ...process.env, BACKEND: `http://127.0.0.1:${bport}`, PORT: String(sport), POLL_MS: '50' },
    stdio: ['ignore', 'ignore', 'pipe']
  })
  shim.stderr?.on('data', (d) => (shimErr += String(d)))
  t.after(() => {
    shim.kill()
    backend?.close()
  })
  const url = `http://127.0.0.1:${sport}`
  // a held connection can be reset by transient load — the shim's own
  // contract is retried once before it's called a failure
  const get = async (path: string): Promise<Response> => {
    let last: unknown
    for (let i = 0; i < 3; i++) {
      try {
        return await fetch(`${url}${path}`)
      } catch (e) {
        last = e
        if (shim.exitCode !== null) throw new Error(`shim exited (${shim.exitCode}): ${shimErr}`)
        await sleep(150)
      }
    }
    throw last
  }
  for (let i = 0; ; i++) {
    try {
      if ((await get('/healthz')).ok) break
    } catch {
      if (i > 60) throw new Error(`watch-shim never came up: ${shimErr}`)
      await sleep(100)
    }
  }
  const watch = (qs: string) => get(`/watch?${qs}`).then((r) => r.json() as Promise<{ changed: boolean; cursor: string; tookMs?: number; backend?: string }>)
  return {
    watch,
    postNotify: (body: unknown | string) =>
      fetch(`${url}/notify`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: typeof body === 'string' ? body : JSON.stringify(body) }),
    setOutputs: (o) => {
      outputs = o
    },
    setOutputsFor: (query, o) => {
      perQuery.set(JSON.stringify(query), o)
    },
    lookups: () => lookups,
    killBackend: async () => {
      const b = backend
      backend = null
      await new Promise<void>((r) => b!.close(() => r()))
    }
  }
}

test('watch-shim: no cursor answers immediately, matching cursor holds to waitMs, change releases', async (t) => {
  const { watch, setOutputs } = await stack(t)
  const boot = await watch('service=ls_x&query={}')
  assert.equal(boot.changed, false)
  assert.ok(boot.cursor, 'a fresh watch still returns the current fingerprint')
  const t0 = Date.now()
  const held = await watch(`service=ls_x&query=${encodeURIComponent('{}')}&cursor=${boot.cursor}&waitMs=400`)
  assert.equal(held.changed, false)
  assert.ok(Date.now() - t0 >= 350, 'a matching cursor holds the request open')
  setOutputs([{ beef: [2], outputIndex: 0 }])
  const t1 = Date.now()
  const changed = await watch(`service=ls_x&query=${encodeURIComponent('{}')}&cursor=${boot.cursor}&waitMs=20000`)
  assert.equal(changed.changed, true)
  assert.ok(Date.now() - t1 < 5000, 'a fingerprint change releases the waiter well inside waitMs')
})

test('watch-shim: /notify wakes only the matching service waiters', async (t) => {
  const { watch, postNotify, setOutputs } = await stack(t)
  const q = encodeURIComponent('{}')
  const cur = (await watch(`service=ls_x&query=${q}`)).cursor
  // a notify for a DIFFERENT service leaves the waiter held
  const stray = watch(`service=ls_y&query=${q}&cursor=${cur}&waitMs=900`)
  await postNotify({ service: 'ls_z' })
  const t0 = Date.now()
  const strayRes = await stray
  assert.equal(strayRes.changed, false)
  assert.ok(Date.now() - t0 >= 500, 'a notify for another service does not release the waiter')
  // a matching notify re-polls immediately — the changed backend is seen now,
  // not at the next POLL_MS tick
  setOutputs([{ beef: [9], outputIndex: 0 }])
  const held = watch(`service=ls_x&query=${q}&cursor=${cur}&waitMs=30000`)
  await sleep(150)
  const t1 = Date.now()
  await postNotify({ service: 'ls_x' })
  const res = await held
  assert.equal(res.changed, true)
  assert.ok(Date.now() - t1 < 3000, 'notify released the waiter without waiting out waitMs')
  // a notify with nothing changed keeps holding — wakes are hints, not state
  const cur2 = (await watch(`service=ls_x&query=${q}`)).cursor
  const held2 = watch(`service=ls_x&query=${q}&cursor=${cur2}&waitMs=600`)
  await sleep(150)
  await postNotify({ service: 'ls_x' })
  assert.equal((await held2).changed, false, 'a wake with no change settles back to polling')
})

test('watch-shim: backend outage degrades — held watch returns changed:false, never hangs', async (t) => {
  const { watch, killBackend } = await stack(t)
  const q = encodeURIComponent('{}')
  const cur = (await watch(`service=ls_x&query=${q}`)).cursor
  await killBackend()
  const t0 = Date.now()
  const res = await watch(`service=ls_x&query=${q}&cursor=${cur}&waitMs=800`)
  assert.equal(res.changed, false, 'a dead backend holds to the ceiling and reports no change')
  assert.ok(Date.now() - t0 >= 700)
})

test('watch-shim: concurrent watchers on one key share one backend poller', async (t) => {
  const { watch, lookups } = await stack(t)
  const q = encodeURIComponent('{}')
  const cur = (await watch(`service=ls_x&query=${q}`)).cursor
  const start = lookups()
  // two held watches on the SAME key for ~300ms — POLL_MS is 50, so a shared
  // poller costs ~6 lookups; a poller-per-connection would cost ~12+
  const w1 = watch(`service=ls_x&query=${q}&cursor=${cur}&waitMs=350`)
  const w2 = watch(`service=ls_x&query=${q}&cursor=${cur}&waitMs=350`)
  await sleep(300)
  const n = lookups() - start
  await Promise.all([w1, w2])
  assert.ok(n >= 3, `poller actually ran (${n} lookups)`)
  assert.ok(n <= 10, `two watchers on one key share one poller — ${n} lookups in 300ms, a per-connection poller would double it`)
})

test('watch-shim: nested query members key independent watches', async (t) => {
  const { watch, setOutputsFor } = await stack(t)
  // the old keyOf whitelisted top-level names at every depth, collapsing
  // {wants:{net,domain}} for different domains into ONE key — a domain-b
  // change would have woken (or masked) domain-a waiters
  const qa = encodeURIComponent(JSON.stringify({ wants: { net: 'x', domain: 'a' } }))
  const qb = encodeURIComponent(JSON.stringify({ wants: { net: 'x', domain: 'b' } }))
  setOutputsFor({ wants: { net: 'x', domain: 'a' } }, [{ beef: [1], outputIndex: 0 }])
  setOutputsFor({ wants: { net: 'x', domain: 'b' } }, [{ beef: [1], outputIndex: 0 }])
  const curA = (await watch(`service=ls_x&query=${qa}`)).cursor
  const curB = (await watch(`service=ls_x&query=${qb}`)).cursor
  const heldA = watch(`service=ls_x&query=${qa}&cursor=${curA}&waitMs=5000`)
  const heldB = watch(`service=ls_x&query=${qb}&cursor=${curB}&waitMs=800`)
  await sleep(150)
  // only the a-side fingerprint moves
  setOutputsFor({ wants: { net: 'x', domain: 'a' } }, [{ beef: [7], outputIndex: 0 }])
  const t0 = Date.now()
  const resA = await heldA
  assert.equal(resA.changed, true)
  assert.ok(Date.now() - t0 < 3000, "the a waiter woke on its own lane's change")
  const resB = await heldB
  assert.equal(resB.changed, false, "the b waiter held — a different domain's change is not its change")
})

test('watch-shim: /notify requires service, bounded body, and a token bucket', async (t) => {
  const { postNotify } = await stack(t)
  assert.equal((await postNotify({})).status, 400, 'missing service → 400')
  assert.equal((await postNotify({ service: 42 })).status, 400, 'non-string service → 400')
  assert.equal((await postNotify('{not json')).status, 400, 'malformed json → 400')
  assert.equal((await postNotify('x'.repeat(70 * 1024))).status, 413, 'body over 64KB → 413')
  // burst over the 20-token bucket → some 429s
  let saw429 = false
  for (let i = 0; i < 28; i++) {
    const st = (await postNotify({ service: 'ls_x' })).status
    if (st === 429) saw429 = true
    else assert.equal(st, 200)
  }
  assert.ok(saw429, 'a burst over the bucket is rate-limited')
})
