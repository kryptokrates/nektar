import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { MarketEngine } from '../src/market.js'
import type { MarketDeps } from '../src/market/types.js'
import { EXEC_SVC } from '../src/rt/exec.js'
import { type TransportDeps } from '../src/rt/services.js'
import { Dispatcher, wireTransport, TRANSPORT_DRAIN_KEY } from './rig.js'

// rt-transport — the Phase 5 outbox port: due rows as journaled deliver
// invocations keyed by destination, the drain's self-scheduled ceiling,
// and the shared deliverOutboxItem body (ack / retry / defer / skip) the
// local pump and the object both run.

const key = PrivateKey.fromRandom()

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

function deps(store: AutomationStore, extra: Partial<MarketDeps> = {}): MarketDeps {
  return {
    store,
    call: () => Promise.resolve({}),
    memory: {} as never,
    key,
    network: 'regtest',
    height: () => Promise.resolve(1),
    fetchTx: () => Promise.resolve(undefined),
    transport: { send: async () => {} } as never,
    ai: {} as never,
    ...extra
  }
}

const enq = (store: AutomationStore, actionId: string, destination = 'host:a') =>
  store.enqueue({ actionId, operationId: `op:${actionId}`, kind: 'message', destination, payload: { type: 'decline', wantId: 'w', offerId: 'o' } })

void test('drain: due rows fan out per destination key, idempotent per actionId, ceiling self-scheduled', async () => {
  const ex = new Dispatcher()
  const seen: Array<{ handler: string; key?: string; opts?: { delayMs?: number; idempotencyKey?: string } }> = []
  try {
    // spy the raw send surface — the drain's fan-out IS the assertion
    const orig = ex.send.bind(ex)
    ex.send = async (t, a, o) => {
      seen.push({ handler: t.handler, key: t.key, opts: o })
      return orig(t, a, o)
    }
    let next: number | undefined = Date.now() + 30_000
    const stages: Array<Array<import('../src/store.js').OutboxItem>> = [
      [
        { actionId: 'x1', operationId: 'op:x1', kind: 'message', destination: 'host:a', payload: {}, attempts: 0, nextAt: 100 },
        { actionId: 'x2', operationId: 'op:x2', kind: 'message', destination: 'host:b', payload: {}, attempts: 0, nextAt: 100 },
        { actionId: 'x3', operationId: 'op:x3', kind: 'message', destination: 'host:a', payload: {}, attempts: 0, nextAt: 100 }
      ],
      []
    ]
    wireTransport(ex, {
      dueRows: () => Promise.resolve(stages.shift() ?? []),
      nextAt: () => Promise.resolve(next),
      deliverItem: () => Promise.resolve('acked'),
      now: () => Date.now(),
      log: () => {}
    } satisfies TransportDeps)
    ex.register(EXEC_SVC.transport, 'deliver', () => Promise.resolve('acked'))
    const r = await ex.call<{ sent: number; nextAt?: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r.sent, 3)
    assert.equal(r.nextAt, next)
    await ex.drain()
    const delivers = seen.filter((s) => s.handler === 'deliver')
    // per-destination keys — host:a serializes its two rows in order
    assert.deepEqual(
      delivers.map((d) => d.key),
      ['host:a', 'host:b', 'host:a']
    )
    assert.deepEqual(
      delivers.map((d) => d.opts?.idempotencyKey),
      // the key versions by retry state (attempts + next_at) — a retried or
      // re-deferred row must dispatch again; actionId alone would dedupe the
      // retry inside the retention window
      ['deliver:x1:0:100', 'deliver:x2:0:100', 'deliver:x3:0:100']
    )
    // the ceiling armed itself — a delayed drain send at next_at
    const arm = seen.find((s) => s.handler === 'drain')
    assert.equal(arm?.key, TRANSPORT_DRAIN_KEY)
    assert.ok((arm.opts?.delayMs ?? 0) > 0)

    // empty box → no arm
    seen.length = 0
    next = undefined
    const r2 = await ex.call<{ sent: number; nextAt?: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r2.sent, 0)
    assert.equal(r2.nextAt, undefined)
    assert.equal(seen.filter((s) => s.handler === 'drain').length, 0)
  } finally {
    void ex.close()
  }
})

void test('drain: a retried row re-fans as a NEW invocation — dedup is per-attempt, not per-actionId', async () => {
  const ex = new Dispatcher()
  const delivered: number[] = []
  try {
    // the row re-fanned mid-retry keeps its attempt in the key — a re-drain
    // of the same attempt dedupes; the next attempt is new work
    const item: import('../src/store.js').OutboxItem = {
      actionId: 'y1',
      operationId: 'op:y1',
      kind: 'message',
      destination: 'host:c',
      payload: {},
      attempts: 1
    }
    wireTransport(ex, {
      dueRows: () => Promise.resolve([item]),
      nextAt: () => Promise.resolve(undefined),
      deliverItem: (i) => Promise.resolve((delivered.push(i.attempts), 'acked' as const)),
      now: () => Date.now(),
      log: () => {}
    } satisfies TransportDeps)
    const r = await ex.call<{ sent: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r.sent, 1)
    await ex.drain()
    assert.deepEqual(delivered, [1])
    // a SECOND drain of the same attempt sends but the ingress dedup drops it
    // — the attempt's invocation is the dedup identity
    const r2 = await ex.call<{ sent: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r2.sent, 1)
    await ex.drain()
    assert.deepEqual(delivered, [1])
    // a resource defer does NOT spend an attempt (deferOutbox leaves
    // attempts untouched) — the row's retry identity is its moved next_at.
    // Same actionId + same attempts + new next_at must still dispatch, or a
    // funding-starved row dead-letters inside the idempotency retention.
    item.nextAt = Date.now() + 60_000
    const r3 = await ex.call<{ sent: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r3.sent, 1)
    await ex.drain()
    assert.deepEqual(delivered, [1, 1])
  } finally {
    await ex.close()
  }
})

void test('deliverOutboxItem: ack on success, defer on funding-slot, retry on fault, skip on moved row', async () => {
  const { kx, store } = await open()
  try {
    // ack path
    const sends: string[] = []
    const engine = new MarketEngine(deps(store, { transport: { send: (dest: string) => void sends.push(dest) } as never }))
    await enq(store, 'a:ok')
    assert.equal(await engine.deliverOutboxItem((await store.dueOutbox())[0]), 'acked')
    assert.deepEqual(sends, ['host:a'])
    assert.equal((await store.outboxRow('a:ok'))?.status, 'acked')
    // a second drain's replay hits the guard — no double-send
    assert.equal(
      await engine.deliverOutboxItem({ actionId: 'a:ok', operationId: 'op', kind: 'message', destination: 'host:a', payload: {}, attempts: 0 }),
      'skipped'
    )
    assert.equal(sends.length, 1)

    // funding-slot defer — no attempt spent
    const starved = new MarketEngine(deps(store, { transport: { send: async () => Promise.reject(new Error('funding slot empty')) } as never }))
    await enq(store, 'a:def')
    assert.equal(await starved.deliverOutboxItem((await store.dueOutbox()).find((i) => i.actionId === 'a:def')!), 'deferred')
    const def = await store.waitingOn('op:a:def')
    assert.equal(def[0].deferClass, 'funding-slot')
    assert.equal(def[0].attempts, 0)

    // ordinary fault — retryOutbox owns the attempt + backoff
    const flaky = new MarketEngine(deps(store, { transport: { send: async () => Promise.reject(new Error('connection refused')) } as never }))
    await enq(store, 'a:retry')
    assert.equal(await flaky.deliverOutboxItem((await store.dueOutbox()).find((i) => i.actionId === 'a:retry')!), 'retried')
    const ret = await store.waitingOn('op:a:retry')
    assert.equal(ret[0].attempts, 1)
    assert.ok(ret[0].nextAt > Date.now())
  } finally {
    await kx.destroy()
  }
})

void test('outbox lost-wakeup: a deferred/retried row re-pokes the pump — the drain arm lands on the new wake', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  try {
    const seen: Array<{ handler: string; opts?: { delayMs?: number; idempotencyKey?: string } }> = []
    const origSend = ex.send.bind(ex)
    ex.send = async (t, a, o) => {
      seen.push({ handler: t.handler, opts: o })
      return origSend(t, a, o)
    }
    // the store-driven pump: every wake lands as a drain call — the same
    // shape openOutbox wires (pokes coalesce; here a plain call suffices)
    const pokes: Promise<unknown>[] = []
    const DRAIN = { service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY } as const
    store.outboxReady = () => {
      pokes.push(ex.call(DRAIN, {}).catch(() => undefined))
    }
    const engine = new MarketEngine(deps(store, { transport: { send: async () => Promise.reject(new Error('no spendable funding slot')) } as never }))
    wireTransport(ex, {
      dueRows: () => store.dueOutbox(50),
      nextAt: () => store.nextOutboxAt(),
      deliverItem: (item) => engine.deliverOutboxItem(item),
      now: () => Date.now(),
      log: () => {}
    } satisfies TransportDeps)
    // joined sends fire more pokes; walk to quiescence
    const settle = async (): Promise<void> => {
      for (let i = 0; i < 20; i++) {
        await ex.drain()
        if (pokes.length === 0) return
        await Promise.allSettled(pokes.splice(0))
      }
    }
    await enq(store, 'lost1', 'host:down')
    await settle()
    // the defer moved the wake — pending, not due, no attempt spent
    const row = await store.outboxRow('lost1')
    assert.ok(row !== undefined)
    assert.equal(row.status, 'pending')
    assert.equal((await store.waitingOn('op:lost1'))[0].attempts, 0)
    const wakeAt = row.nextAt
    assert.ok(wakeAt > Date.now())
    // the drain re-armed on the POST-defer instant — the arm key must be
    // drain:${new next_at}, never drain:${stale} which dedups against the
    // invocation that ran the stale pass (the observed dead chain: rows
    // overdue 30+ min with no scheduled successor, mainnet deal 16:12)
    const arms = () => seen.filter((s) => s.handler === 'drain').map((s) => s.opts?.idempotencyKey)
    assert.ok(arms().includes(`drain:${wakeAt}`), `no drain arm at the deferred wake ${wakeAt} — arms: ${JSON.stringify(arms())}`)
    // the retry path keeps the same contract — a rescheduled row pokes too
    const before = seen.length
    await store.retryOutbox('lost1', 'host unreachable')
    const retried = await store.outboxRow('lost1')
    assert.ok(retried !== undefined)
    assert.equal((await store.waitingOn('op:lost1'))[0].attempts, 1) // a real retry spends its attempt
    assert.ok(retried.nextAt !== wakeAt)
    await settle()
    assert.ok(arms().includes(`drain:${retried.nextAt}`), `no drain arm at the retried wake ${retried.nextAt} — arms: ${JSON.stringify(arms())}`)
    assert.ok(seen.length > before)
  } finally {
    await ex.close()
    await kx.destroy()
  }
})

void test('drain: in-flight due row does not mask a later pending row or self-dedupe drain arm (rt/dedup-kills-arm)', async () => {
  let clock = 10_000
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx, () => clock)
  await store.init()
  const ex = new Dispatcher()
  try {
    // R1 is due now (T1=10_000); R2 is due later (T2=30_000)
    await store.enqueue({ actionId: 'r1', operationId: 'op:r1', kind: 'message', destination: 'host:a', payload: {} })
    await store.enqueue({ actionId: 'r2', operationId: 'op:r2', kind: 'message', destination: 'host:b', payload: {} }, { delayMs: 20_000 })
    const arms: Array<{ delayMs?: number; idempotencyKey?: string }> = []
    let armed!: () => void
    const armedP = new Promise<void>((r) => (armed = r))
    const origSend = ex.send.bind(ex)
    ex.send = async (t, a, o) => {
      if (t.handler === 'drain') {
        arms.push({ delayMs: o?.delayMs, idempotencyKey: o?.idempotencyKey })
        armed()
      }
      return origSend(t, a, o)
    }
    // R1's deliver stays pending while drain runs (mirrors real Restate detached deliver)
    let releaseR1!: () => void
    const r1Gate = new Promise<void>((r) => (releaseR1 = r))
    wireTransport(ex, {
      dueRows: () => store.dueOutbox(50),
      nextAt: (excludeIds) => store.nextOutboxAt(excludeIds !== undefined && excludeIds.length > 0 ? { excludeIds } : undefined),
      deliverItem: async (item) => {
        if (item.actionId === 'r1') await r1Gate
        await store.ackOutbox(item.actionId)
        return 'acked'
      },
      now: () => clock,
      log: () => {}
    })
    const drainCall = ex.call({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    await armedP
    // even BEFORE R1 acks, drain must have armed directly on R2's 30_000 wake, not R1's 10_000
    assert.deepEqual(arms, [{ delayMs: 20_000, idempotencyKey: 'drain:30000' }])
    releaseR1()
    await drainCall
  } finally {
    await ex.close()
    await kx.destroy()
  }
})
