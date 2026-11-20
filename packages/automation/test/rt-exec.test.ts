import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory, { type Knex } from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { executor, executorFromEnv, LocalExecutor, RestateExecutor, setExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { registerTxHandlers } from '../src/rt/services.js'
import { txEmit } from '../src/rt/emit.js'

// rt-exec — the executor seam itself: LocalExecutor dispatch, the
// RestateExecutor wire format, txEmit routing (undefined executor =
// emitTxEvent verbatim; executor = the call goes through the object),
// and the NEKTAR_EXECUTOR env switch. Fakes sit at the seams — fetch for
// the wire test, a registered handler for the dispatch test.

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

test('LocalExecutor: call dispatches to the registered handler; send fires it async', async () => {
  const ex = new LocalExecutor()
  let sent: unknown
  ex.register('svc', 'call-me', async (arg, k) => ({ arg, k }))
  ex.register('svc', 'fire-me', async (arg) => {
    sent = arg
  })
  const r = await ex.call<{ arg: unknown; k: unknown }>({ service: 'svc', handler: 'call-me', key: 'k1' }, { n: 1 })
  assert.deepEqual(r, { arg: { n: 1 }, k: 'k1' })
  await ex.send({ service: 'svc', handler: 'fire-me' }, { n: 2 })
  await new Promise((res) => setImmediate(res))
  assert.deepEqual(sent, { n: 2 })
  await assert.rejects(() => ex.call({ service: 'svc', handler: 'missing' }, null), /no handler/)
  ex.close()
})

test('RestateExecutor: wire format is /restate/{call,send}/ paths with delay + idempotency-key', async () => {
  const seen: Array<{ url: string; headers: Record<string, string>; body: string }> = []
  const realFetch = globalThis.fetch
  globalThis.fetch = (async (url: unknown, init: { headers?: Record<string, string>; body?: string }) => {
    seen.push({ url: String(url), headers: init?.headers ?? {}, body: init?.body ?? '' })
    return new Response('{"accepted":true}', { status: 200, headers: { 'content-type': 'application/json' } })
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

test('txEmit: no executor is emitTxEvent verbatim; an executor routes through the nektar-tx object', async () => {
  const { kx, store } = await open()
  try {
    // no executor — the direct commit path, unchanged
    const w = await prepared(store, 'a:local')
    const direct = await txEmit(undefined, store, w, 'submit.success', { observation: { at: 1 }, attempt: true }, {})
    assert.equal(direct.accepted, true)
    assert.equal(direct.next, 'submitted')

    // a LocalExecutor + the SAME handler bodies services.ts serves under
    // restate — the routed emit runs the real leg one dispatch layer deep
    const w2 = await prepared(store, 'a:routed')
    const ex = new LocalExecutor()
    registerTxHandlers(ex, { store, fx: {}, log: () => {} })
    const routed = await txEmit(ex, store, w2, 'submit.success', { observation: { at: 2 }, attempt: true }, {})
    assert.equal(routed.accepted, true)
    assert.equal(routed.next, 'submitted')
    assert.equal((await store.walletTx({ actionId: 'a:routed' }))!.state, 'submitted')
    ex.close()
  } finally {
    await kx.destroy()
  }
})

test('NEKTAR_EXECUTOR: restate wires the RestateExecutor; anything else is local', () => {
  assert.equal(executorFromEnv({}), undefined)
  assert.equal(executorFromEnv({ NEKTAR_EXECUTOR: 'local' }), undefined)
  const e = executorFromEnv({ NEKTAR_EXECUTOR: 'restate', NEKTAR_RESTATE_URL: 'http://x:1' })
  assert.ok(e instanceof RestateExecutor)
  // the singleton: setExecutor is the test seam, executor() the env default
  const local = new LocalExecutor()
  setExecutor(local)
  try {
    assert.equal(executor(), local)
  } finally {
    setExecutor(undefined)
  }
})
