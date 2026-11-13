// events.test.ts — the shared chain-event substrate: one interest set, one
// probe, push/note/report semantics. These cover the failure classes the old
// per-consumer poll loops each carried: overlapping ticks, duplicate delivery,
// never-sealing watches, and pushed hints pretending to be verified events.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ChainEvents } from '../src/events.js'
import type { TxStatusEvent } from '../src/interfaces.js'

const mined = (txid: string, mp = { blockHeight: 7 }): TxStatusEvent => ({ txid, status: 'mined', blockHeight: 7, merklePath: mp as never })

test('events: watch → observe delivers a mined event once; duplicates suppressed', async () => {
  let calls = 0
  const ev = new ChainEvents({ observe: async () => (calls++, mined('t1')) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.watch('t1')
  await ev.tick()
  await ev.tick() // unwatch fired on 'mined' — second tick observes nothing
  assert.equal(calls, 1)
  assert.equal(got.length, 1)
  assert.equal(got[0].status, 'mined')
})

test('events: pending observations stay watched; no emit until a status exists', async () => {
  let calls = 0
  const ev = new ChainEvents({ observe: async () => (calls++, undefined) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.watch('t1')
  await ev.tick()
  await ev.tick()
  assert.equal(calls, 2)
  assert.equal(got.length, 0)
  assert.ok(ev.watching('t1'))
})

test('events: height gate — a static tip and no new watches skips the batch', async () => {
  let calls = 0
  let tip = 100
  const ev = new ChainEvents({ observe: async () => (calls++, undefined), height: async () => tip })
  ev.watch('t1')
  await ev.tick() // first tick always scans (restart recovery)
  assert.equal(calls, 1)
  await ev.tick() // tip unchanged, nothing new → gated
  assert.equal(calls, 1)
  ev.watch('t2') // dirty flag forces the next scan — the whole batch re-checks
  await ev.tick()
  assert.equal(calls, 3)
  tip = 101 // height advance re-opens the gate
  await ev.tick()
  assert.equal(calls, 5)
})

test('events: ttl drops a never-sealing watch', async () => {
  const ev = new ChainEvents({ observe: async () => undefined, ttlMs: 1 })
  ev.watch('t1')
  await new Promise((r) => setTimeout(r, 5))
  await ev.tick()
  assert.equal(ev.watching('t1'), false)
})

test('events: abandonAfterBlocks emits abandoned once and drops the watch', async () => {
  let tip = 100
  const ev = new ChainEvents({ observe: async () => undefined, height: async () => tip, abandonAfterBlocks: 10 })
  const abandoned: string[] = []
  ev.on('abandoned', ({ txid }) => abandoned.push(txid))
  ev.watch('t1')
  await ev.tick() // first-seen height recorded at tip=100
  tip = 111
  await ev.tick()
  assert.deepEqual(abandoned, ['t1'])
  assert.equal(ev.watching('t1'), false)
  await ev.tick()
  assert.equal(abandoned.length, 1)
})

test('events: note() records pushed status + resolves waitFor without emitting tx — and never drops the floor', async () => {
  let proven = false
  const ev = new ChainEvents({ observe: async () => (proven ? mined('t1') : undefined) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  const p = ev.waitFor('t1', 5000)
  ev.note(mined('t1'))
  const ev_ = await p
  assert.equal(ev_?.status, 'mined')
  assert.equal(got.length, 0) // hints never emit — verified delivery is the caller's path
  assert.equal(ev.statusOf('t1')?.status, 'mined')
  // a pushed terminal status must NOT end interest: an unverifiable MINED
  // keeps the reconcile floor alive — only a verified observe drops the watch
  assert.equal(ev.watching('t1'), true)
  proven = true
  await ev.tick() // the verified observe delivers (proof attached) and drops
  assert.equal(ev.watching('t1'), false)
  assert.equal(got.length, 1) // 'tx' emits on the verified path — !!prev.mp changed
})

test('events: a non-terminal delivery resolves a matching waiter; others stay parked', async () => {
  let deliverable: TxStatusEvent | undefined
  const ev = new ChainEvents({ observe: async () => deliverable })
  const unconf = ev.waitFor('t1', 60_000, (e) => e.status === 'unconfirmed')
  const minedW = ev.waitFor('t1', 60_000, (e) => e.status === 'mined')
  deliverable = { txid: 't1', status: 'unconfirmed' }
  await ev.tick()
  assert.equal((await unconf)?.status, 'unconfirmed') // first-delivered-event waiters resolve
  const r = await Promise.race([minedW.then(() => 'resolved'), new Promise((res) => setTimeout(() => res('parked'), 200))])
  assert.equal(r, 'parked') // non-terminal: non-matching waiters keep waiting
  ev.stop()
})

test('events: a terminal event resolves every waiter, matcher or not', async () => {
  const ev = new ChainEvents({ observe: async () => undefined })
  const ds = ev.waitFor('t1', 60_000, (e) => e.status === 'double-spent')
  const mn = ev.waitFor('t1', 60_000, (e) => e.merklePath !== undefined)
  ev.report({ txid: 't1', status: 'mined' }) // terminal 'mined' — ends BOTH waits
  const r1 = await Promise.race([ds, new Promise((r) => setTimeout(() => r('HANG'), 500))])
  const r2 = await mn
  assert.notEqual(r1, 'HANG') // no parked-on-the-clock: the wait was over
  assert.equal((r1 as TxStatusEvent).status, 'mined') // learns the actual outcome
  assert.equal(r2?.status, 'mined')
})

test('events: concurrent probes of one txid share a single observe', async () => {
  let calls = 0
  const ev = new ChainEvents({
    observe: async () => {
      calls++
      await new Promise((r) => setTimeout(r, 20))
      return undefined
    }
  })
  ev.watch('t1')
  await Promise.all([ev.probeNow('t1'), ev.probeNow('t1'), ev.probeNow('t1')])
  assert.equal(calls, 1)
})

test('events: retained statuses evict on the same ttl sweep as watches', async () => {
  const ev = new ChainEvents({ observe: async () => undefined, ttlMs: 1 })
  ev.report({ txid: 't1', status: 'mined' })
  assert.equal(ev.statusOf('t1')?.status, 'mined')
  await new Promise((r) => setTimeout(r, 5))
  await ev.tick()
  assert.equal(ev.statusOf('t1'), undefined) // catch-up cache, not permanent memory
})

test('events: report() is the trusted first-party path — emits tx', async () => {
  const ev = new ChainEvents({ observe: async () => undefined })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.report({ txid: 't1', status: 'rejected' })
  assert.equal(got.length, 1)
  assert.equal(got[0].status, 'rejected')
})

test('events: waitFor resolves from a self-observed probe; times out clean', async () => {
  let prove = false
  const ev = new ChainEvents({ observe: async () => (prove ? mined('t1') : undefined) })
  const slow = ev.waitFor('t1', 200)
  setTimeout(() => {
    prove = true
  }, 50)
  const ev_ = await slow
  assert.equal(ev_?.status, 'mined')
  const miss = await ev.waitFor('nope', 100)
  assert.equal(miss, undefined)
})

test('events: overlapping ticks do not stack — busy guard', async () => {
  let inflight = 0
  let maxInflight = 0
  const ev = new ChainEvents({
    observe: async () => {
      inflight++
      maxInflight = Math.max(maxInflight, inflight)
      await new Promise((r) => setTimeout(r, 30))
      inflight--
      return undefined
    }
  })
  ev.watch('t1')
  await Promise.all([ev.tick(), ev.tick(), ev.tick()])
  assert.equal(maxInflight, 1)
})

test('events: maxPerTick rotates through an oversized interest set', async () => {
  const seen = new Set<string>()
  const ev = new ChainEvents({
    maxPerTick: 2,
    observe: async (txid) => {
      seen.add(txid)
      return undefined
    }
  })
  for (const t of ['a', 'b', 'c', 'd', 'e']) ev.watch(t)
  await ev.tick()
  assert.equal(seen.size, 2)
  await ev.tick()
  assert.equal(seen.size, 4)
  await ev.tick()
  assert.equal(seen.size, 5)
})

test('events: watch() wakes the substrate — a mined observe arrives without an explicit tick', async () => {
  const ev = new ChainEvents({ observe: async () => mined('t1') })
  const got = new Promise<TxStatusEvent>((r) => ev.on('tx', r))
  ev.watch('t1') // no start(), no tick() — the wake alone must deliver
  const e = await Promise.race([got, new Promise<undefined>((r) => setTimeout(() => r(undefined), 500))])
  assert.equal(e?.status, 'mined')
  ev.stop()
})

test('events: note() verify-on-hint — a pushed mined hint is verified and promoted to tx, not an interval later', async () => {
  let proven = false
  const ev = new ChainEvents({ observe: async () => (proven ? mined('t1') : undefined) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.watch('t1')
  ev.note(mined('t1')) // hint — no 'tx' emit on its own
  assert.equal(got.length, 0)
  proven = true // the proof is now servable — the wake's observe must deliver it
  const e = await Promise.race([new Promise<TxStatusEvent>((r) => ev.on('tx', r)), new Promise<undefined>((r) => setTimeout(() => r(undefined), 500))])
  assert.equal(e?.status, 'mined')
  assert.equal(got.length, 1)
  assert.equal(ev.watching('t1'), false) // verified observe ended interest
  ev.stop()
})
