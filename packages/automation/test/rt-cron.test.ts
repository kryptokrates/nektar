// rt-cron.test.ts — the cadence objects + notify legs through the test
// Dispatcher: tick runs the journaled pass then self-arms at its
// ceiling; notify's peer/carrier handlers run the fetch legs keyed by
// destination.
import test from 'node:test'
import assert from 'node:assert/strict'
import { EXEC_SVC } from '../src/rt/exec.js'
import { CRON_KEY } from '../src/rt/services.js'
import { Dispatcher, wireCron, wireLeg, wireNotify } from './rig.js'

test('cron.tick: runs the pass then self-arms at ceilingMs — the chain re-fires', async () => {
  const ex = new Dispatcher()
  const sent: Array<{ delayMs?: number; target: string }> = []
  const orig = ex.send.bind(ex)
  ex.send = (t, a, o) => {
    sent.push({ delayMs: o?.delayMs, target: `${t.service}/${t.handler}:${t.key ?? '-'}` })
    return orig(t, a, o)
  }
  let ticks = 0
  wireCron(ex, EXEC_SVC.upkeep, {
    tick: async () => {
      ticks++
    },
    ceilingMs: () => 5,
    log: () => {}
  })
  await ex.call({ service: EXEC_SVC.upkeep, handler: 'tick', key: CRON_KEY }, {})
  assert.equal(ticks, 1)
  assert.deepEqual(sent, [{ delayMs: 5, target: `${EXEC_SVC.upkeep}/tick:${CRON_KEY}` }])
  // the arm is a real timer locally — wait it out; the chain tick self-arms again
  await new Promise((r) => setTimeout(r, 30))
  assert.ok(ticks >= 2, `self-arm re-fired the tick (ticks=${ticks})`)
  await ex.close()
})

test('cron.tick: a failing pass propagates — under restate the retry IS the next attempt', async () => {
  const ex = new Dispatcher()
  const sent: number[] = []
  const orig = ex.send.bind(ex)
  ex.send = (t, a, o) => {
    sent.push(o?.delayMs ?? -1)
    return orig(t, a, o)
  }
  let calls = 0
  wireCron(ex, EXEC_SVC.watchdog, {
    tick: async () => {
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

test('notify: peer + carrier legs run keyed by destination', async () => {
  const ex = new Dispatcher()
  const legs: string[] = []
  wireNotify(ex, {
    peer: async (watch, service, query) => void legs.push(`peer:${watch}:${service}:${JSON.stringify(query)}`),
    carrier: async (push, beefHex) => void legs.push(`carrier:${push}:${beefHex.slice(0, 8)}`),
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

test('legObject: event-sink leg runs journaled on its key, no self-arm', async () => {
  const ex = new Dispatcher()
  let calls = 0
  wireLeg(ex, EXEC_SVC.watcher, async () => {
    calls++
  })
  await ex.call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
  await ex.call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
  assert.equal(calls, 2)
  await ex.close()
})
