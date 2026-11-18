import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { AutomationStore } from '@nektar/automation'
import { MarketEngine, trackFanout, queueRepairs, ScriptedExecutor } from '@nektar/automation'
import type { MarketMessage, MarketOptions } from '@nektar/automation'

const open = async (now: () => number) => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx, now)
  await store.init()
  return { kx, store }
}

const engine = (
  store: AutomationStore,
  transport: { send?: (to: string, m: MarketMessage) => Promise<void>; repair?: (h: string, k: string) => Promise<void> },
  opts?: Partial<MarketOptions>
) =>
  new MarketEngine({
    store,
    memory: { shardManifest: async () => undefined, records: async () => [] },
    key: PrivateKey.fromRandom(),
    network: 'testnet',
    call: async () => ({}),
    height: async () => 100,
    fetchTx: async () => undefined,
    transport: {
      publishWant: async () => {},
      publishOffer: async () => {},
      send: transport.send ?? (async () => {}),
      ...(transport.repair !== undefined ? { repair: transport.repair } : {})
    },
    ai: new ScriptedExecutor(),
    ...(opts !== undefined ? { opts } : {})
  })

test('fanout repair: a repair-host row calls transport.repair(host, key); a failing one retries transport-classed alone', async () => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  try {
    const repairs: Array<[string, string]> = []
    const eng = engine(store, {
      repair: async (host, key) => {
        repairs.push([host, key])
        throw new Error('fetch failed')
      }
    })
    await store.enqueue({ actionId: 'repair:h:h1', operationId: 'lanes', kind: 'repair-host', destination: 'h1', payload: { key: 'h' } })
    await store.enqueue({
      actionId: 'msg:1',
      operationId: 'lanes',
      kind: 'message',
      destination: 'pub',
      payload: { type: 'accept' } as unknown as MarketMessage
    })
    await eng.pumpOutbox()
    assert.deepEqual(repairs, [['h1', 'h']], 'the repair leg ran with the row destination + journal key')
    const rows = await kx('market_outbox').select('action_id', 'status', 'defer_class')
    const repair = rows.find((r) => r.action_id === 'repair:h:h1')!
    const msg = rows.find((r) => r.action_id === 'msg:1')!
    assert.equal(repair.status, 'pending')
    assert.equal(repair.defer_class, 'transport', 'the failure class rides the row')
    assert.equal(msg.status, 'acked', 'the unrelated row delivered and is untouched')
    // ten failures dead-letter ONLY the repair row
    for (let i = 0; i < 10; i++) {
      now += 400_000
      await eng.pumpOutbox()
    }
    const after = await kx('market_outbox').select('action_id', 'status', 'attempts')
    assert.equal(after.find((r) => r.action_id === 'repair:h:h1')!.status, 'dead')
    assert.equal(after.find((r) => r.action_id === 'msg:1')!.status, 'acked')
  } finally {
    await kx.destroy()
  }
})

test('outbox pump: a wedged deliver cannot starve the rows behind it', async () => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  try {
    const sent: string[] = []
    // a deliver that never resolves — the live-R1 shape (a pass parked on a
    // dead host's leg while the fresh accept queued unattempted behind it)
    const never = new Promise<void>(() => {})
    const eng = engine(
      store,
      {
        send: async (to) => {
          sent.push(to)
        },
        repair: async () => {
          await never
        }
      },
      { deliverBoundMs: 50 }
    )
    await store.enqueue({ actionId: 'repair:h:dead', operationId: 'lanes', kind: 'repair-host', destination: 'dead', payload: { key: 'h' } })
    await store.enqueue({
      actionId: 'op:accept',
      operationId: 'op',
      kind: 'message',
      destination: 'seller',
      payload: { type: 'accept' } as unknown as MarketMessage
    })
    // serial drain would hang forever on the repair; pooled dispatch must
    // resolve once the accept lands and the wedged leg hits its bound
    await eng.pumpOutbox()
    assert.deepEqual(sent, ['seller'], 'the fresh send delivered despite the wedged repair ahead of it')
    const rows = await kx('market_outbox').select('action_id', 'status', 'last_error')
    assert.equal(rows.find((r) => r.action_id === 'op:accept')!.status, 'acked')
    const rep = rows.find((r) => r.action_id === 'repair:h:dead')!
    assert.equal(rep.status, 'pending', 'the wedged leg retries, it is not dead-lettered')
    assert.match(rep.last_error ?? '', /deliver bound/)
  } finally {
    await kx.destroy()
  }
})

test('trackFanout: missed → immediate row, pending → delayed row, late success acks both', async () => {
  const enqueued: Array<{ o: any; opts?: { delayMs?: number } }> = []
  const acked: string[] = []
  const settled = Promise.resolve({ took: ['h1', 'h2'], missed: [] })
  await trackFanout(
    { hash: 'h', missed: [{ host: 'h1', error: 'http 503' }], pending: ['h2'], settled },
    {
      enqueue: async (o, opts) => {
        enqueued.push({ o, opts })
        return true
      },
      ackOutbox: async (id) => {
        acked.push(id)
      }
    }
  )
  assert.equal(enqueued.length, 2)
  const imm = enqueued.find((e) => e.o.destination === 'h1')!
  const delayed = enqueued.find((e) => e.o.destination === 'h2')!
  assert.equal(imm.opts, undefined, 'a missed leg repairs immediately')
  assert.equal(delayed.opts?.delayMs, 20_000, 'a pending leg waits out the straggler window first')
  for (const e of enqueued) {
    assert.equal(e.o.operationId, 'lanes')
    assert.equal(e.o.kind, 'repair-host')
    assert.deepEqual(e.o.payload, { key: 'h' })
    assert.equal(e.o.actionId, `repair:h:${e.o.destination}`)
  }
  await new Promise((r) => setImmediate(r))
  assert.deepEqual(acked.sort(), ['repair:h:h1', 'repair:h:h2'], 'a leg that landed after quorum cancels its own repair row')
})

test('republish repair: a missed host gets a :proved repair row that survives a terminal submit-fanout row', async () => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  try {
    // the submit fan-out already wrote repair:h:host1 — if that row is
    // terminal, a plain repair: enqueue is a silent no-op, so the republish
    // path uses a distinct :proved actionId
    await store.enqueue({ actionId: 'repair:h:host1', operationId: 'lanes', kind: 'repair-host', destination: 'host1', payload: { key: 'h' } })
    await store.ackOutbox('repair:h:host1')
    const io = { enqueue: (o: Parameters<typeof store.enqueue>[0], opts?: { delayMs?: number }) => store.enqueue(o, opts) }
    await queueRepairs(io, 'h', ['host1'], { idSuffix: ':proved' })
    const due = await store.dueOutbox(10)
    const row = due.find((r) => r.actionId === 'repair:h:host1:proved')
    assert.ok(row !== undefined, 'a republish missed host gets its own repair row despite the terminal :submit one')
    assert.equal(row.kind, 'repair-host')
    assert.equal(row.destination, 'host1')
    assert.deepEqual(row.payload, { key: 'h' })
    // and the plain id confirms the dedup it is avoiding
    assert.equal(
      await io.enqueue({ actionId: 'repair:h:host1', operationId: 'lanes', kind: 'repair-host', destination: 'host1', payload: { key: 'h' } }),
      false
    )
  } finally {
    await kx.destroy()
  }
})
