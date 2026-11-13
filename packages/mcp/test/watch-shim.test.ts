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

async function stack(t: test.TestContext): Promise<{ url: string; setOutputs: (o: unknown) => void; killBackend: () => Promise<void> }> {
  let outputs: unknown = [{ beef: [1], outputIndex: 0 }]
  let backend: Server | null = createServer((req, res) => {
    if (req.url === '/lookup') {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ outputs }))
      return
    }
    res.writeHead(404)
    res.end()
  })
  await new Promise<void>((r) => backend!.listen(0, '127.0.0.1', r))
  const bport = (backend.address() as { port: number }).port
  const sport = 19000 + Math.floor(Math.random() * 2000)
  const shim: ChildProcess = spawn(process.execPath, [SHIM], {
    env: { ...process.env, BACKEND: `http://127.0.0.1:${bport}`, PORT: String(sport), POLL_MS: '50' },
    stdio: 'ignore',
  })
  t.after(() => {
    shim.kill()
    backend?.close()
  })
  const url = `http://127.0.0.1:${sport}`
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(`${url}/healthz`)).ok) break
    } catch {
      if (i > 60) throw new Error('watch-shim never came up')
      await sleep(100)
    }
  }
  return {
    url,
    setOutputs: (o) => {
      outputs = o
    },
    killBackend: async () => {
      const b = backend
      backend = null
      await new Promise<void>((r) => b!.close(() => r()))
    },
  }
}

const watch = (url: string, qs: string) => fetch(`${url}/watch?${qs}`).then((r) => r.json() as Promise<{ changed: boolean; cursor: string; tookMs?: number; backend?: string }>)

test('watch-shim: no cursor answers immediately, matching cursor holds to waitMs, change releases', async (t) => {
  const { url, setOutputs } = await stack(t)
  const boot = await watch(url, 'service=ls_x&query={}')
  assert.equal(boot.changed, false)
  assert.ok(boot.cursor, 'a fresh watch still returns the current fingerprint')
  const t0 = Date.now()
  const held = await watch(url, `service=ls_x&query=${encodeURIComponent('{}')}&cursor=${boot.cursor}&waitMs=400`)
  assert.equal(held.changed, false)
  assert.ok(Date.now() - t0 >= 350, 'a matching cursor holds the request open')
  setOutputs([{ beef: [2], outputIndex: 0 }])
  const t1 = Date.now()
  const changed = await watch(url, `service=ls_x&query=${encodeURIComponent('{}')}&cursor=${boot.cursor}&waitMs=20000`)
  assert.equal(changed.changed, true)
  assert.ok(Date.now() - t1 < 5000, 'a fingerprint change releases the waiter well inside waitMs')
})

test('watch-shim: /notify wakes only the matching service waiters', async (t) => {
  const { url, setOutputs } = await stack(t)
  const q = encodeURIComponent('{}')
  const cur = (await watch(url, `service=ls_x&query=${q}`)).cursor
  // a notify for a DIFFERENT service leaves the waiter held
  const stray = watch(url, `service=ls_y&query=${q}&cursor=${cur}&waitMs=900`)
  await fetch(`${url}/notify`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ service: 'ls_z' }) })
  const t0 = Date.now()
  const strayRes = await stray
  assert.equal(strayRes.changed, false)
  assert.ok(Date.now() - t0 >= 500, 'a notify for another service does not release the waiter')
  // a matching notify re-polls immediately — the changed backend is seen now,
  // not at the next POLL_MS tick
  setOutputs([{ beef: [9], outputIndex: 0 }])
  const held = watch(url, `service=ls_x&query=${q}&cursor=${cur}&waitMs=30000`)
  await sleep(150)
  const t1 = Date.now()
  await fetch(`${url}/notify`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ service: 'ls_x' }) })
  const res = await held
  assert.equal(res.changed, true)
  assert.ok(Date.now() - t1 < 3000, 'notify released the waiter without waiting out waitMs')
  // a notify with nothing changed keeps holding — wakes are hints, not state
  const cur2 = (await watch(url, `service=ls_x&query=${q}`)).cursor
  const held2 = watch(url, `service=ls_x&query=${q}&cursor=${cur2}&waitMs=600`)
  await sleep(150)
  await fetch(`${url}/notify`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ service: 'ls_x' }) })
  assert.equal((await held2).changed, false, 'a wake with no change settles back to polling')
})

test('watch-shim: backend outage degrades — held watch returns changed:false, never hangs', async (t) => {
  const { url, killBackend } = await stack(t)
  const q = encodeURIComponent('{}')
  const cur = (await watch(url, `service=ls_x&query=${q}`)).cursor
  await killBackend()
  const t0 = Date.now()
  const res = await watch(url, `service=ls_x&query=${q}&cursor=${cur}&waitMs=800`)
  assert.equal(res.changed, false, 'a dead backend holds to the ceiling and reports no change')
  assert.ok(Date.now() - t0 >= 700)
})
