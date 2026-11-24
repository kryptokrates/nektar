import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { P2PKH, PrivateKey, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator } from '../src/wallet.js'
import { setExecutor, EXEC_SVC, CUSTODY_KEY, inRtScope, PermanentError } from '../src/rt/exec.js'
import { custodyMarkRejected, custodyResubmitPending, custodyResubmitPoke, custodySubmit } from '../src/rt/emit.js'
import { inertTxEffects, type TxEffects } from '../src/machines/effects.js'
import { Dispatcher, wireCustody, wireTx } from './rig.js'

// rt-custody — the Phase 4 leg: the coordinator's custody verbs as
// serialized, journaled invocations on the single-key custody object. What
// this proves: submit/markRejected/resubmitPending route to the fixed key,
// the bodies run the real coordinator (emits land on the tx object), and
// txScope forwarding keeps a resubmit from deadlocking its own tx key.

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

const coordinator = (store: AutomationStore) =>
  new WalletCoordinator(store, { broadcast: (tx) => Promise.resolve({ status: 'success', txid: tx.id('hex') }), fx: inertTxEffects })

void test('custody legs key per actionId — the scan stays on the fixed key and never forwards txScope', async () => {
  const ex = new Dispatcher()
  const seen: Array<{ handler: string; key?: string; arg: Record<string, unknown> }> = []
  try {
    for (const h of ['submit', 'markRejected', 'resubmitPending'])
      ex.register(EXEC_SVC.custody, h, (arg, k) => {
        seen.push({ handler: h, key: k, arg: (arg ?? {}) as Record<string, unknown> })
        return Promise.resolve({ state: 'submitted', txid: 'x' })
      })
    setExecutor(ex)
    const wallet = {} as WalletCoordinator
    await custodySubmit(ex, wallet, 'a:1')
    await custodyMarkRejected(ex, wallet, 'a:1', { cause: 'x' })
    await custodyResubmitPending(ex, wallet)
    await inRtScope({ txKey: 'a:held' }, () => custodySubmit(ex, wallet, 'a:held'))
    assert.deepEqual(
      seen.map((s) => ({ handler: s.handler, key: s.key, hasTxScope: 'txScope' in s.arg })),
      [
        { handler: 'submit', key: 'a:1', hasTxScope: false },
        { handler: 'markRejected', key: 'a:1', hasTxScope: false },
        { handler: 'resubmitPending', key: CUSTODY_KEY, hasTxScope: false },
        { handler: 'submit', key: 'a:held', hasTxScope: false }
      ]
    )
  } finally {
    setExecutor(undefined)
    void ex.close()
  }
})

void test('custody.submit: the real coordinator leg — broadcast + emit commits through the tx object', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  const spent: string[] = []
  try {
    await prepared(store, 'c:sub')
    const fx: TxEffects = { ...inertTxEffects, txSpent: (w) => void spent.push(w.txid) }
    wireTx(ex, { store, fx, log: () => {} })
    const wallet = coordinator(store)
    wireCustody(ex, { wallet, log: () => {} })
    setExecutor(ex)
    const r = await custodySubmit(ex, wallet, 'c:sub')
    assert.equal(r.state, 'submitted')
    assert.equal((await store.walletTx({ actionId: 'c:sub' }))!.state, 'submitted')
    assert.equal(spent.length, 1, 'the aired verdict ran its custody intent')
  } finally {
    setExecutor(undefined)
    void ex.close()
    await kx.destroy()
  }
})

void test('custody.markRejected + resubmitPending: dead verdicts and the boot scan run through the object', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  const died: string[] = []
  try {
    await prepared(store, 'c:rej')
    await prepared(store, 'c:reair')
    const fx: TxEffects = { ...inertTxEffects, txDied: (w) => void died.push(w.txid) }
    wireTx(ex, { store, fx, log: () => {} })
    const wallet = coordinator(store)
    wireCustody(ex, { wallet, log: () => {} })
    setExecutor(ex)

    await custodyMarkRejected(ex, wallet, 'c:rej', { cause: 'escrow_rejected' })
    assert.equal((await store.walletTx({ actionId: 'c:rej' }))!.state, 'rejected')
    assert.equal(died.length, 1, 'the dead verdict fired txDied through the machine write')

    // a missing row is a no-op verdict, not a crash
    await custodyMarkRejected(ex, wallet, 'c:gone', {})

    // the boot scan re-airs the remaining prepared row through the object
    const rows = await custodyResubmitPending(ex, wallet)
    assert.deepEqual(rows, [{ actionId: 'c:reair', state: 'submitted' }])
    assert.equal((await store.walletTx({ actionId: 'c:reair' }))!.state, 'submitted')
  } finally {
    setExecutor(undefined)
    void ex.close()
    await kx.destroy()
  }
})

void test('custody.submit: a submission_unknown outcome does not swallow subsequent retries (rt/dedup-kills-retry)', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  let tries = 0
  try {
    await prepared(store, 'c:retry')
    wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
    const wallet = new WalletCoordinator(store, {
      broadcast: (tx) => {
        tries++
        if (tries === 1) return Promise.resolve({ status: 'error', description: 'transport: 503' })
        return Promise.resolve({ status: 'success', txid: tx.id('hex') })
      },
      fx: inertTxEffects,
      exec: ex
    })
    wireCustody(ex, { wallet, log: () => {} })
    setExecutor(ex)
    const r1 = await custodySubmit(ex, wallet, 'c:retry')
    assert.equal(r1.state, 'submission_unknown')
    // second submit (boot sweep / funding resume) must actually re-run, not return the cached unknown
    const r2 = await custodySubmit(ex, wallet, 'c:retry')
    assert.equal(r2.state, 'submitted')
    assert.equal(tries, 2)
  } finally {
    setExecutor(undefined)
    void ex.close()
    await kx.destroy()
  }
})

void test('custody.resubmitPending: routes per-row submits through the actionId key (no global-vs-actionId race)', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  const submitKeys: string[] = []
  try {
    await prepared(store, 'c:fan1')
    const wallet = coordinator(store)
    wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
    wireCustody(ex, { wallet, log: () => {} })
    // wrap submit to observe the key it arrived on
    ex.register(EXEC_SVC.custody, 'submit', async (arg, k) => {
      submitKeys.push(k ?? '')
      return await wallet.submit((arg as { actionId: string }).actionId)
    })
    setExecutor(ex)
    const rows = await custodyResubmitPending(ex, wallet)
    assert.deepEqual(rows, [{ actionId: 'c:fan1', state: 'submitted' }])
    assert.deepEqual(submitKeys, ['c:fan1'], 'resubmitPending must serialize each row on its actionId key')
  } finally {
    setExecutor(undefined)
    void ex.close()
    await kx.destroy()
  }
})

void test('custody.resubmitPending: fans out distinct actionIds concurrently', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  let inflight = 0
  let maxInflight = 0
  try {
    await prepared(store, 'c:par1')
    await prepared(store, 'c:par2')
    const wallet = coordinator(store)
    wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
    wireCustody(ex, { wallet, log: () => {} })
    ex.register(EXEC_SVC.custody, 'submit', async (arg) => {
      inflight++
      maxInflight = Math.max(maxInflight, inflight)
      await new Promise((r) => setTimeout(r, 40))
      inflight--
      return await wallet.submit((arg as { actionId: string }).actionId)
    })
    setExecutor(ex)
    const rows = await custodyResubmitPending(ex, wallet)
    assert.equal(rows.length, 2)
    assert.equal(maxInflight, 2, 'distinct actionId submits must run concurrently, not serialized behind custody/global')
  } finally {
    setExecutor(undefined)
    void ex.close()
    await kx.destroy()
  }
})

void test('custody.submit: missing row throws PermanentError (terminal, never retries to pause)', async () => {
  const { kx, store } = await open()
  try {
    const wallet = coordinator(store)
    await assert.rejects(wallet.submit('missing:action'), (e: unknown) => e instanceof PermanentError && e.code === 404)
  } finally {
    await kx.destroy()
  }
})

void test('custody.resubmitPending + poke: a failing row does not wedge sibling rows (rt/global-lock)', async () => {
  const { kx, store } = await open()
  const ex = new Dispatcher()
  try {
    await prepared(store, 'c:bad')
    await prepared(store, 'c:good')
    const wallet = coordinator(store)
    wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
    wireCustody(ex, { wallet, log: () => {} })
    const submitted: string[] = []
    ex.register(EXEC_SVC.custody, 'submit', async (arg) => {
      const id = (arg as { actionId: string }).actionId
      if (id === 'c:bad') throw new PermanentError('poisoned row')
      submitted.push(id)
      return await wallet.submit(id)
    })
    setExecutor(ex)
    const rows = await custodyResubmitPending(ex, wallet)
    assert.deepEqual(rows, [{ actionId: 'c:good', state: 'submitted' }])
    assert.deepEqual(submitted, ['c:good'])

    await prepared(store, 'c:good2')
    await custodyResubmitPoke(ex, wallet, 42)
    await ex.drain()
    assert.deepEqual(submitted, ['c:good', 'c:good2'])
  } finally {
    setExecutor(undefined)
    void ex.close()
    await kx.destroy()
  }
})
