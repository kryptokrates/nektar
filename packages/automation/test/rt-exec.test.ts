import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { executor, RestateExecutor, setExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { emitTxEvent, inertTxEffects } from '../src/machines/effects.js'
import { txEmit } from '../src/rt/emit.js'
import { Dispatcher, wireTx } from './rig.js'

// rt-exec — the executor seam itself: Dispatcher fan-out, the
// RestateExecutor wire format, txEmit routing (same-key scope =
// emitTxEvent verbatim; otherwise the call goes through the object),
// and the ambient executor() singleton. Fakes sit at the seams — fetch
// for the wire test, a registered handler for the dispatch test.

const key = PrivateKey.fromRandom()
let txSeq = 0

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

async function prepared(store: AutomationStore, actionId: string) {
  const t = new Transaction()
  t.addOutput({ satoshis: 1 + txSeq++, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  await store.recordPrepared({ actionId, operationId: `op:${actionId}`, kind: 'escrow', txid: t.id('hex'), rawHex: t.toHex(), parents: {}, feeSats: 1 })
  return (await store.walletTx({ actionId }))!
}

void test('Dispatcher: call dispatches to the registered handler; send fires it async', async () => {
  const ex = new Dispatcher()
  let sent: unknown
  ex.register('svc', 'call-me', (arg, k) => Promise.resolve({ arg, k }))
  ex.register('svc', 'fire-me', (arg) => {
    sent = arg
    return Promise.resolve()
  })
  const r = await ex.call<{ arg: unknown; k: unknown }>({ service: 'svc', handler: 'call-me', key: 'k1' }, { n: 1 })
  assert.deepEqual(r, { arg: { n: 1 }, k: 'k1' })
  await ex.send({ service: 'svc', handler: 'fire-me' }, { n: 2 })
  await new Promise((res) => setImmediate(res))
  assert.deepEqual(sent, { n: 2 })
  await assert.rejects(() => ex.call({ service: 'svc', handler: 'missing' }, null), /no handler/)
  void ex.close()
})

void test('Dispatcher: delayMs past the int32 timer ceiling clamps, never fires early', async () => {
  const ex = new Dispatcher()
  let fired = false
  ex.register('svc', 'due', () => {
    fired = true
    return Promise.resolve()
  })
  // the refund_pending residence bound (180d) overflows setTimeout's int32
  // delay to 1ms — the arm must clamp to the platform ceiling instead of
  // dispatching immediately (the domain deadline, not the timer, is due)
  await ex.send({ service: 'svc', handler: 'due' }, {}, { delayMs: 180 * 86_400_000 })
  await new Promise((res) => setTimeout(res, 50))
  assert.equal(fired, false)
  void ex.close()
})

void test('RestateExecutor: wire format is /restate/{call,send}/ paths with delay + idempotency-key', async () => {
  const seen: Array<{ url: string; headers: Record<string, string>; body: string }> = []
  const realFetch = globalThis.fetch
  globalThis.fetch = ((url: unknown, init?: { headers?: Record<string, string>; body?: string }) => {
    seen.push({ url: String(url), headers: init?.headers ?? {}, body: init?.body ?? '' })
    return Promise.resolve(new Response('{"accepted":true}', { status: 200, headers: { 'content-type': 'application/json' } }))
  }) as typeof fetch
  try {
    const ex = new RestateExecutor('http://ingress.test:8080')
    await ex.send({ service: EXEC_SVC.tx, handler: 'observe', key: 'a:1' }, { event: 'obs.proven' }, { delayMs: 5_000, idempotencyKey: 'k-1' })
    assert.equal(seen[0].url, `http://ingress.test:8080/restate/send/${EXEC_SVC.tx}/a%3A1/observe?delay=5s`)
    assert.equal(seen[0].headers['idempotency-key'], 'k-1')
    assert.equal(JSON.parse(seen[0].body).event, 'obs.proven')
    const r = await ex.call<{ accepted: boolean }>({ service: EXEC_SVC.tx, handler: 'observe', key: 'a:1' }, { event: 'submit.success' })
    assert.equal(r.accepted, true)
    assert.equal(seen[1].url, `http://ingress.test:8080/restate/call/${EXEC_SVC.tx}/a%3A1/observe`)
  } finally {
    globalThis.fetch = realFetch
  }
})

void test('txEmit: same-key scope is emitTxEvent verbatim; otherwise the call routes through the nektar-tx object', async () => {
  const { kx, store } = await open()
  try {
    // the verbatim commit path — what same-key emits inside a handler run
    const w = await prepared(store, 'a:local')
    const direct = await emitTxEvent(store, w, 'submit.success', { observation: { at: 1 }, attempt: true }, inertTxEffects)
    assert.equal(direct.accepted, true)
    assert.equal(direct.next, 'submitted')

    // a Dispatcher + the SAME handler bodies services.ts serves on the
    // endpoint — the routed emit runs the real leg one dispatch layer deep
    const w2 = await prepared(store, 'a:routed')
    const ex = new Dispatcher()
    wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
    const routed = await txEmit(ex, store, w2, 'submit.success', { observation: { at: 2 }, attempt: true }, inertTxEffects)
    assert.equal(routed.accepted, true)
    assert.equal(routed.next, 'submitted')
    assert.equal((await store.walletTx({ actionId: 'a:routed' }))!.state, 'submitted')
    void ex.close()
  } finally {
    await kx.destroy()
  }
})

void test('executor(): always the shared RestateExecutor; setExecutor repoints the ambient seam', () => {
  const e = executor()
  assert.ok(e instanceof RestateExecutor)
  // the singleton: setExecutor is the test seam, executor() the default
  const dispatcher = new Dispatcher()
  setExecutor(dispatcher)
  try {
    assert.equal(executor(), dispatcher)
  } finally {
    setExecutor(undefined)
  }
})
