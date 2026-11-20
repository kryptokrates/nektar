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
    call: async () => ({}),
    memory: {} as never,
    key,
    network: 'regtest',
    height: async () => 1,
    fetchTx: async () => undefined,
    transport: { send: async () => {} } as never,
    ai: {} as never,
    ...extra
  } as MarketDeps
}

const enq = (store: AutomationStore, actionId: string, destination = 'host:a') =>
  store.enqueue({ actionId, operationId: `op:${actionId}`, kind: 'message', destination, payload: { type: 'decline', wantId: 'w', offerId: 'o' } })

test('drain: due rows fan out per destination key, idempotent per actionId, ceiling self-scheduled', async () => {
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
        { actionId: 'x1', operationId: 'op:x1', kind: 'message', destination: 'host:a', payload: {}, attempts: 0 },
        { actionId: 'x2', operationId: 'op:x2', kind: 'message', destination: 'host:b', payload: {}, attempts: 0 },
        { actionId: 'x3', operationId: 'op:x3', kind: 'message', destination: 'host:a', payload: {}, attempts: 0 }
      ],
      []
    ]
    wireTransport(ex, {
      dueRows: async () => stages.shift() ?? [],
      nextAt: async () => next,
      deliverItem: async () => 'acked',
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
      ['deliver:x1', 'deliver:x2', 'deliver:x3']
    )
    // the ceiling armed itself — a delayed drain send at next_at
    const arm = seen.find((s) => s.handler === 'drain')
    assert.equal(arm?.key, TRANSPORT_DRAIN_KEY)
    assert.ok((arm?.opts?.delayMs ?? 0) > 0)

    // empty box → no arm
    seen.length = 0
    next = undefined
    const r2 = await ex.call<{ sent: number; nextAt?: number }>({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
    assert.equal(r2.sent, 0)
    assert.equal(r2.nextAt, undefined)
    assert.equal(seen.filter((s) => s.handler === 'drain').length, 0)
  } finally {
    ex.close()
  }
})

test('deliverOutboxItem: ack on success, defer on funding-slot, retry on fault, skip on moved row', async () => {
  const { kx, store } = await open()
  try {
    // ack path
    const sends: string[] = []
    const engine = new MarketEngine(deps(store, { transport: { send: async (dest: string) => void sends.push(dest) } as never }))
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
