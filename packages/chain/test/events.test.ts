// events.test.ts — the shared chain-event substrate: one interest set, one
// probe, push/note/report semantics. These cover the failure classes the old
// per-consumer poll loops each carried: overlapping ticks, duplicate delivery,
// never-sealing watches, and pushed hints pretending to be verified events.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ChainEvents } from '../src/events.js'
import { nap } from '@nektar/core'
import type { TxStatusEvent } from '../src/interfaces.js'

const mined = (txid: string, mp = { blockHeight: 7 }): TxStatusEvent => ({ txid, status: 'mined', blockHeight: 7, merklePath: mp as never })

test('events: watch → observe delivers a mined event once; duplicates suppressed', async () => {
  let calls = 0
  const ev = new ChainEvents({ observe: () => (calls++, Promise.resolve(mined('t1'))) })
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
  const ev = new ChainEvents({ observe: () => (calls++, Promise.resolve(undefined)) })
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
  const ev = new ChainEvents({ observe: () => (calls++, Promise.resolve(undefined)), height: () => Promise.resolve(tip) })
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
  const ev = new ChainEvents({ observe: () => Promise.resolve(undefined), ttlMs: 1 })
  ev.watch('t1')
  await new Promise((r) => setTimeout(r, 5))
  await ev.tick()
  assert.equal(ev.watching('t1'), false)
})

test('events: abandonAfterBlocks emits abandoned once and drops the watch', async () => {
  let tip = 100
  const ev = new ChainEvents({ observe: () => Promise.resolve(undefined), height: () => Promise.resolve(tip), abandonAfterBlocks: 10 })
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
  const ev = new ChainEvents({ observe: () => Promise.resolve(proven ? mined('t1') : undefined) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  const p = ev.waitFor('t1', 5000)
  ev.note(mined('t1'))
  const ev_ = await p
  assert.equal(ev_?.ev.status, 'mined')
  assert.equal(ev_?.verified, false) // a hint answers the wait — marked unverified
  assert.equal(got.length, 0) // hints never emit — verified delivery is the caller's path
  assert.equal(ev.statusOf('t1')?.ev.status, 'mined')
  assert.equal(ev.statusOf('t1')?.verified, false) // a pushed status stays a hint
  // a pushed terminal status must NOT end interest: an unverifiable MINED
  // keeps the reconcile floor alive — only a verified observe drops the watch
  assert.equal(ev.watching('t1'), true)
  proven = true
  // let the nudge's in-flight probe (started while proven was false) settle —
  // the tick coalesces onto one fetch, and a stale in-flight read is the
  // dedup contract, not a second ask
  await new Promise((r) => setTimeout(r, 0))
  await ev.tick() // the verified observe delivers (proof attached) and drops
  assert.equal(ev.watching('t1'), false)
  assert.equal(got.length, 1) // 'tx' emits on the verified path — !!prev.mp changed
})

test('events: a non-terminal delivery resolves a matching waiter; others stay parked', async () => {
  let deliverable: TxStatusEvent | undefined
  const ev = new ChainEvents({ observe: () => Promise.resolve(deliverable) })
  const unconf = ev.waitFor('t1', 60_000, (e) => e.status === 'unconfirmed')
  const minedW = ev.waitFor('t1', 60_000, (e) => e.status === 'mined')
  deliverable = { txid: 't1', status: 'unconfirmed' }
  await ev.tick()
  assert.equal((await unconf)?.ev.status, 'unconfirmed') // first-delivered-event waiters resolve
  const r = await Promise.race([minedW.then(() => 'resolved'), new Promise((res) => setTimeout(() => res('parked'), 200))])
  assert.equal(r, 'parked') // non-terminal: non-matching waiters keep waiting
  ev.stop()
})

test('events: a terminal event resolves every waiter, matcher or not', async () => {
  const ev = new ChainEvents({ observe: () => Promise.resolve(undefined) })
  const ds = ev.waitFor('t1', 60_000, (e) => e.status === 'double-spent')
  const mn = ev.waitFor('t1', 60_000, (e) => e.merklePath !== undefined)
  ev.report({ txid: 't1', status: 'mined' }) // terminal 'mined' — ends BOTH waits
  const r1 = await Promise.race([ds, new Promise((r) => setTimeout(() => r('HANG'), 500))])
  const r2 = await mn
  assert.notEqual(r1, 'HANG') // no parked-on-the-clock: the wait was over
  assert.equal((r1 as { ev: TxStatusEvent }).ev.status, 'mined') // learns the actual outcome
  assert.equal(r2?.ev.status, 'mined')
  assert.equal(r2?.verified, true) // report() is first-party truth
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
  const ev = new ChainEvents({ observe: () => Promise.resolve(undefined), ttlMs: 1 })
  ev.report({ txid: 't1', status: 'mined' })
  assert.equal(ev.statusOf('t1')?.ev.status, 'mined')
  assert.equal(ev.statusOf('t1')?.verified, true) // report() is first-party truth
  await new Promise((r) => setTimeout(r, 5))
  await ev.tick()
  assert.equal(ev.statusOf('t1'), undefined) // catch-up cache, not permanent memory
})

test('events: report() is the trusted first-party path — emits tx', () => {
  const ev = new ChainEvents({ observe: () => Promise.resolve(undefined) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.report({ txid: 't1', status: 'rejected' })
  assert.equal(got.length, 1)
  assert.equal(got[0].status, 'rejected')
})

test('events: waitFor resolves from a self-observed probe; times out clean', async () => {
  let prove = false
  const ev = new ChainEvents({ observe: () => Promise.resolve(prove ? mined('t1') : undefined) })
  const slow = ev.waitFor('t1', 200)
  setTimeout(() => {
    prove = true
  }, 50)
  const ev_ = await slow
  assert.equal(ev_?.ev.status, 'mined')
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
    observe: (txid) => {
      seen.add(txid)
      return Promise.resolve(undefined)
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
  const ev = new ChainEvents({ observe: () => Promise.resolve(mined('t1')) })
  const got = new Promise<TxStatusEvent>((r) => ev.on('tx', r))
  ev.watch('t1') // no start(), no tick() — the wake alone must deliver
  const e = await Promise.race([got, new Promise<undefined>((r) => setTimeout(() => r(undefined), 500))])
  assert.equal(e?.status, 'mined')
  ev.stop()
})

test('events: note() verify-on-hint — a pushed mined hint is verified and promoted to tx, not an interval later', async () => {
  let proven = false
  const ev = new ChainEvents({ observe: () => Promise.resolve(proven ? mined('t1') : undefined) })
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

test('events: abandon and ttl flush parked waiters — no sleep-to-own-timeout', async () => {
  let tip = 100
  const ev = new ChainEvents({ observe: () => Promise.resolve(undefined), height: () => Promise.resolve(tip), abandonAfterBlocks: 10, ttlMs: 60_000 })
  const p = ev.waitFor('t1', 60_000)
  await ev.tick() // first-seen at tip=100
  tip = 111
  await ev.tick()
  assert.equal(await p, undefined) // the abandoned watch answered the waiter
  const ev2 = new ChainEvents({ observe: () => Promise.resolve(undefined), ttlMs: 1 })
  const p2 = ev2.waitFor('t1', 60_000)
  await new Promise((r) => setTimeout(r, 5))
  await ev2.tick()
  assert.equal(await p2, undefined) // ttl drop answered the waiter too
  ev.stop()
  ev2.stop()
})

test('events: a throwing observe never advances the abandon clock', async () => {
  let tip = 100
  let fail = true
  const ev = new ChainEvents({
    observe: () => {
      if (fail) return Promise.reject(new Error('host down'))
      return Promise.resolve(undefined)
    },
    height: () => Promise.resolve(tip),
    abandonAfterBlocks: 10
  })
  const abandoned: string[] = []
  ev.on('abandoned', ({ txid }) => abandoned.push(txid))
  ev.watch('t1')
  await ev.tick() // first-seen height recorded at tip=100
  tip = 111
  await ev.tick() // 11 unanswered blocks — silence is not a verdict
  tip = 130
  await ev.tick()
  assert.deepEqual(abandoned, [])
  assert.equal(ev.watching('t1'), true)
  fail = false // the host answers again — the clock can count now
  tip = 131
  await ev.tick()
  assert.deepEqual(abandoned, ['t1'])
})

test('events: note() dirties the set — verify-on-hint runs under a static tip', async () => {
  let proven = false
  const ev = new ChainEvents({ observe: () => Promise.resolve(proven ? mined('t1') : undefined), height: () => Promise.resolve(100) })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.watch('t1')
  await ev.tick() // observe misses: scannedAtTip=100, the proof gate closes
  assert.equal(got.length, 0)
  proven = true
  ev.note(mined('t1')) // the pushed hint must reopen the gate — no new block needed
  await ev.tick()
  assert.equal(got.length, 1)
  assert.equal(got[0].status, 'mined')
  ev.stop()
})

// ── bounded probes (EVT-01) ──────────────────────────────────────────────
// one observe/height read that never settles must not pin the substrate:
// the probe gets a real bound (and a real abort), 'couldn't ask' never
// feeds the abandon clock, and the batch's siblings still deliver.

const never = <T>(): Promise<T> => new Promise<T>(() => {})

test('events: an observe that never settles times out — probing clears and the txid re-probes', { timeout: 10_000 }, async () => {
  const stalls: Array<{ what: string; ms: number; txid?: string }> = []
  let calls = 0
  const ev = new ChainEvents({
    observeTimeoutMs: 40,
    observe: () => {
      calls++
      return never<undefined>()
    },
    onStall: (what, ms, txid) => stalls.push({ what, ms, txid })
  })
  ev.watch('t1')
  await ev.tick() // resolves at the bound, not on the dead read
  assert.equal(calls, 1)
  assert.deepEqual(stalls, [{ what: 'observe', ms: 40, txid: 't1' }])
  await ev.tick() // the dead promise is gone — a fresh probe, not a coalesce
  assert.equal(calls, 2)
  assert.equal(stalls.length, 2)
})

test('events: one hung observe does not starve the batch — a mined sibling still delivers', { timeout: 10_000 }, async () => {
  const ev = new ChainEvents({
    observeTimeoutMs: 40,
    observe: (txid) => (txid === 'hung' ? never<undefined>() : Promise.resolve(mined('healthy')))
  })
  const got: TxStatusEvent[] = []
  ev.on('tx', (e) => got.push(e))
  ev.watch('hung')
  ev.watch('healthy')
  await ev.tick()
  assert.deepEqual(
    got.map((e) => e.txid),
    ['healthy']
  )
  assert.equal(ev.watching('healthy'), false)
  assert.equal(ev.watching('hung'), true)
})

test('events: a timed-out observe never advances the abandon clock', { timeout: 10_000 }, async () => {
  let tip = 100
  const ev = new ChainEvents({
    observeTimeoutMs: 40,
    observe: () => never<undefined>(),
    height: () => Promise.resolve(tip),
    abandonAfterBlocks: 10
  })
  const abandoned: string[] = []
  ev.on('abandoned', ({ txid }) => abandoned.push(txid))
  ev.watch('t1')
  await ev.tick() // first-seen at tip=100; the probe times out
  tip = 111
  await ev.tick() // 11 unanswered blocks — a timeout is "couldn't ask"
  tip = 130
  await ev.tick()
  assert.deepEqual(abandoned, [])
  assert.equal(ev.watching('t1'), true)
})

test('events: a height() that never settles times out — the observe batch still runs', { timeout: 10_000 }, async () => {
  const stalls: string[] = []
  let observed = 0
  const ev = new ChainEvents({
    observeTimeoutMs: 40,
    observe: () => (observed++, Promise.resolve(undefined)),
    height: () => never<number>(),
    onStall: (what) => void stalls.push(what)
  })
  ev.watch('t1')
  await ev.tick()
  assert.deepEqual(stalls, ['height'])
  assert.equal(observed, 1, 'a failed height read is not "tip unchanged" — the batch ran')
})

test('events: the bound cancels the work, not just the wait — a nap() inside observe dies at the bound', { timeout: 10_000 }, async () => {
  let napEndedAt: number | undefined
  const t0 = Date.now()
  const ev = new ChainEvents({
    observeTimeoutMs: 40,
    observe: () =>
      nap(10_000)
        .then(() => undefined)
        .finally(() => {
          napEndedAt = Date.now()
        })
  })
  ev.watch('t1')
  await ev.tick()
  assert.ok(napEndedAt !== undefined, 'the ambient signal reached the nap — it rejected, not ran out')
  assert.ok(napEndedAt - t0 < 500, `the orphaned leg died at the bound, not at 10s (${napEndedAt - t0}ms)`)
})

test('events: waitFor on a hung-observe txid times out on its own clock; a healthy txid still resolves', { timeout: 10_000 }, async () => {
  const ev = new ChainEvents({
    observeTimeoutMs: 40,
    observe: (txid) => (txid === 'hung' ? never<undefined>() : Promise.resolve(mined('ok')))
  })
  const miss = await ev.waitFor('hung', 150)
  assert.equal(miss, undefined)
  const hit = await ev.waitFor('ok', 1_000)
  assert.equal(hit?.ev.status, 'mined')
})

test('events: note() with unverified terminal status never prematurely terminates non-matching waitFor', { timeout: 10_000 }, async () => {
  let proven = false
  const ev = new ChainEvents({ observe: () => Promise.resolve(proven ? mined('t1') : undefined) })
  const waiting = ev.waitFor('t1', 5_000, (e) => e.merklePath !== undefined)
  ev.note({ txid: 't1', status: 'mined' }) // hint without merklePath
  const early = await Promise.race([waiting.then(() => 'resolved'), nap(80).then(() => 'parked')])
  assert.equal(early, 'parked', 'unverified hint must not terminate a non-matching waiter')
  proven = true
  await ev.tick()
  const got = await waiting
  assert.equal(got?.verified, true)
  assert.ok(got?.ev.merklePath !== undefined)
})
