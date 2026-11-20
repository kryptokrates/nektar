import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { P2PKH, PrivateKey, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator } from '../src/wallet.js'
import { LocalExecutor, setExecutor, EXEC_SVC, CUSTODY_KEY, inRtScope } from '../src/rt/exec.js'
import { custodyMarkRejected, custodyResubmitPending, custodySubmit } from '../src/rt/emit.js'
import { registerCustodyHandlers, registerTxHandlers } from '../src/rt/services.js'

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

const coordinator = (store: AutomationStore, extra: { txDied?: (tx: Transaction) => void } = {}) =>
  new WalletCoordinator(store, { broadcast: async (tx) => ({ status: 'success', txid: tx.id('hex') }), ...extra })

test('custody legs route to the fixed key — and a held tx key forwards as txScope', async () => {
  const ex = new LocalExecutor()
  const seen: Array<{ handler: string; key?: string; arg: Record<string, unknown> }> = []
  try {
    for (const h of ['submit', 'markRejected', 'resubmitPending'])
      ex.register(EXEC_SVC.custody, h, async (arg, k) => {
        seen.push({ handler: h, key: k, arg: (arg ?? {}) as Record<string, unknown> })
        return { state: 'submitted', txid: 'x' }
      })
    setExecutor(ex)
    const wallet = {} as WalletCoordinator
    await custodySubmit(ex, wallet, 'a:1')
    await custodyMarkRejected(ex, wallet, 'a:1', { cause: 'x' })
    await custodyResubmitPending(ex, wallet)
    // inside a tx object's scope the held key crosses the custody hop
    await inRtScope({ txKey: 'a:held' }, () => custodySubmit(ex, wallet, 'a:held'))
    assert.deepEqual(
      seen.map((s) => ({ handler: s.handler, key: s.key, txScope: s.arg.txScope })),
      [
        { handler: 'submit', key: CUSTODY_KEY, txScope: undefined },
        { handler: 'markRejected', key: CUSTODY_KEY, txScope: undefined },
        { handler: 'resubmitPending', key: CUSTODY_KEY, txScope: undefined },
        { handler: 'submit', key: CUSTODY_KEY, txScope: 'a:held' }
      ]
    )
  } finally {
    setExecutor(undefined)
    ex.close()
  }
})

test('custody.submit: the real coordinator leg — broadcast + emit commits through the tx object', async () => {
  const { kx, store } = await open()
  const ex = new LocalExecutor()
  const spent: string[] = []
  try {
    await prepared(store, 'c:sub')
    registerTxHandlers(ex, { store, fx: { txSpent: async (w) => void spent.push(w.txid) }, log: () => {} })
    const wallet = coordinator(store)
    registerCustodyHandlers(ex, { wallet, log: () => {} })
    setExecutor(ex)
    const r = await custodySubmit(ex, wallet, 'c:sub')
    assert.equal(r.state, 'submitted')
    assert.equal((await store.walletTx({ actionId: 'c:sub' }))!.state, 'submitted')
    assert.equal(spent.length, 1, 'the aired verdict ran its custody intent')
  } finally {
    setExecutor(undefined)
    ex.close()
    await kx.destroy()
  }
})

test('custody.markRejected + resubmitPending: dead verdicts and the boot scan run through the object', async () => {
  const { kx, store } = await open()
  const ex = new LocalExecutor()
  const died: string[] = []
  try {
    await prepared(store, 'c:rej')
    await prepared(store, 'c:reair')
    registerTxHandlers(ex, { store, fx: { txDied: async (w) => void died.push(w.txid) }, log: () => {} })
    const wallet = coordinator(store)
    registerCustodyHandlers(ex, { wallet, log: () => {} })
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
    ex.close()
    await kx.destroy()
  }
})
