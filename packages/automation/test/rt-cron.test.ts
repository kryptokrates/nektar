// rt-cron.test.ts — the cadence objects + notify legs through the test
// Dispatcher: tick runs the journaled pass then self-arms at its
// ceiling; notify's peer/carrier handlers run the fetch legs keyed by
// destination.
import test from 'node:test'
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { EXEC_SVC, PermanentError, RetryAfterError } from '../src/rt/exec.js'
import { CRON_KEY } from '../src/rt/services.js'
import { openLanes } from '../src/lanes.js'
import type { Rt } from '../src/rtctx.js'
import { Dispatcher, wireCron, wireLeg, wireNotify } from './rig.js'

void test('cron.tick: runs the pass then self-arms at ceilingMs — the chain re-fires', async () => {
  const ex = new Dispatcher()
  const sent: Array<{ delayMs?: number; target: string }> = []
  const orig = ex.send.bind(ex)
  ex.send = (t, a, o) => {
    sent.push({ delayMs: o?.delayMs, target: `${t.service}/${t.handler}:${t.key ?? '-'}` })
    return orig(t, a, o)
  }
  let ticks = 0
  wireCron(ex, EXEC_SVC.upkeep, {
    tick: () => {
      ticks++
      return Promise.resolve()
    },
    ceilingMs: () => 5,
    log: () => {}
  })
  await ex.call({ service: EXEC_SVC.upkeep, handler: 'tick', key: CRON_KEY }, {})
  assert.equal(ticks, 1)
  assert.deepEqual(sent, [{ delayMs: 5, target: `${EXEC_SVC.upkeep}/tick:${CRON_KEY}` }])
  // the arm is a real timer locally — wait it out; the chain tick self-arms again
  await new Promise((r) => setTimeout(r, 30))
  assert.ok((ticks as number) >= 2, `self-arm re-fired the tick (ticks=${ticks})`)
  await ex.close()
})

void test('cron.tick: a failing pass propagates — under restate the retry IS the next attempt', async () => {
  const ex = new Dispatcher()
  const sent: number[] = []
  const orig = ex.send.bind(ex)
  ex.send = (t, a, o) => {
    sent.push(o?.delayMs ?? -1)
    return orig(t, a, o)
  }
  let calls = 0
  wireCron(ex, EXEC_SVC.watchdog, {
    tick: () => {
      calls++
      throw new Error('probe down')
    },
    ceilingMs: () => 60_000,
    log: () => {}
  })
  // the thrown leg surfaces to the caller; the self-arm send never ran —
  // under restate the failed INVOCATION is retried (the retry is the
  // cadence; an arm would only double-fire). No arm, no silent stop either:
  // the suspended invocation is the "who notices" answer.
  // NOTE: the call goes keyless here — a keyed invocation's rejection rides
  // the dispatcher's per-key chain, whose cleanup branch rejects a void'd
  // promise the test can't reach (a post-test unhandledRejection). The body
  // under test doesn't read the key, so the leg itself is unchanged.
  await assert.rejects(ex.call({ service: EXEC_SVC.watchdog, handler: 'tick' }, {}), /probe down/)
  assert.equal(calls, 1)
  assert.equal(sent.length, 0)
  await ex.close()
})

void test('notify: peer + carrier legs run keyed by destination', async () => {
  const ex = new Dispatcher()
  const legs: string[] = []
  wireNotify(ex, {
    peer: (watch, service, query) => Promise.resolve(void legs.push(`peer:${watch}:${service}:${JSON.stringify(query)}`)),
    carrier: (push, beefHex) => Promise.resolve(void legs.push(`carrier:${push}:${beefHex.slice(0, 8)}`)),
    log: () => {}
  })
  await ex.call({ service: EXEC_SVC.notify, handler: 'peer', key: 'https://a.example' }, { watch: 'https://a.example', service: 'ls_nektar' })
  await ex.call(
    { service: EXEC_SVC.notify, handler: 'peer', key: 'https://b.example' },
    { watch: 'https://b.example', service: 'ls_bbox', query: { office: 'o1' } }
  )
  await ex.call(
    { service: EXEC_SVC.notify, handler: 'carrier', key: 'https://a.example/v1/lane' },
    { push: 'https://a.example/v1/lane', beefHex: 'deadbeefcafe' }
  )
  assert.deepEqual(legs, [
    'peer:https://a.example:ls_nektar:undefined',
    'peer:https://b.example:ls_bbox:{"office":"o1"}',
    'carrier:https://a.example/v1/lane:deadbeef'
  ])
  await ex.close()
})

void test('legObject: event-sink leg runs journaled on its key, no self-arm', async () => {
  const ex = new Dispatcher()
  let calls = 0
  wireLeg(ex, EXEC_SVC.watcher, () => {
    calls++
    return Promise.resolve()
  })
  await ex.call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
  await ex.call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
  assert.equal(calls, 2)
  await ex.close()
})

void test('cron.tick: early wake before bucket boundary never self-dedupes the chain (rt/dedup-kills-chain)', async () => {
  const ex = new Dispatcher()
  const arms: Array<{ key?: string; after?: number }> = []
  const orig = ex.send.bind(ex)
  ex.send = (t, a, o) => {
    arms.push({ key: o?.idempotencyKey, after: (a as { after?: number } | undefined)?.after })
    return orig(t, a, o)
  }
  let clock = 10_000
  wireCron(ex, EXEC_SVC.upkeep, {
    tick: () => Promise.resolve(),
    ceilingMs: () => 1_000,
    now: () => clock,
    log: () => {}
  })
  // tick 1 at 10_000 arms bucket 11_000
  await ex.call({ service: EXEC_SVC.upkeep, handler: 'tick', key: CRON_KEY }, {})
  assert.deepEqual(arms[0], { key: `${EXEC_SVC.upkeep}:tick:11000`, after: 11_000 })
  // simulate early wake (clock skew: 1ms before 11_000) carrying { after: 11_000 }
  clock = 10_999
  await ex.call({ service: EXEC_SVC.upkeep, handler: 'tick', key: CRON_KEY }, { after: 11_000 })
  assert.deepEqual(arms[1], { key: `${EXEC_SVC.upkeep}:tick:12000`, after: 12_000 }, 'must advance strictly past arg.after instead of self-deduping')
  await ex.close()
})

void test('notify legs: HTTP 4xx throws PermanentError, HTTP 429 Retry-After throws RetryAfterError (rt/poisoned-leg)', async () => {
  const srv = createServer((req, res) => {
    req.resume()
    req.on('end', () => {
      if (req.url === '/reject/notify' || req.url === '/reject') {
        res.statusCode = 422
        res.end('bad carrier')
        return
      }
      if (req.url === '/limit/notify' || req.url === '/limit') {
        res.statusCode = 429
        res.setHeader('retry-after', '7')
        res.end('slow down')
        return
      }
      res.statusCode = 500
      res.end('transient')
    })
  })
  await new Promise<void>((resolve) => srv.listen(0, '127.0.0.1', resolve))
  const port = (srv.address() as { port: number }).port
  const base = `http://127.0.0.1:${port}`
  try {
    const rt = {
      cfg: { network: 'regtest' },
      mem: {},
      log: () => {},
      store: {},
      marketHosts: [],
      localNet: true,
      downNow: new Set(),
      watchBases: []
    } as unknown as Rt
    openLanes(rt)
    assert.ok(rt.notifyLegs !== undefined)
    await assert.rejects(rt.notifyLegs.peer(`${base}/reject`, 'ls_nektar'), (e: unknown) => e instanceof PermanentError && e.code === 422)
    await assert.rejects(rt.notifyLegs.carrier(`${base}/reject`, 'aa'), (e: unknown) => e instanceof PermanentError && e.code === 422)
    await assert.rejects(rt.notifyLegs.peer(`${base}/limit`, 'ls_nektar'), (e: unknown) => e instanceof RetryAfterError && e.retryAfterMs === 7000)
    await assert.rejects(rt.notifyLegs.carrier(`${base}/limit`, 'aa'), (e: unknown) => e instanceof RetryAfterError && e.retryAfterMs === 7000)
    await assert.rejects(
      rt.notifyLegs.carrier(`${base}/err`, 'aa'),
      (e: unknown) => e instanceof Error && !(e instanceof PermanentError) && !(e instanceof RetryAfterError)
    )
  } finally {
    srv.close()
  }
})
