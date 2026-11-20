import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { inRtScope, LocalExecutor, setExecutor, EXEC_SVC } from '../src/rt/exec.js'
import { registerTxHandlers, registerSweepHandlers, type SweepDeps } from '../src/rt/services.js'
import { txEmit } from '../src/rt/emit.js'

// rt-sweep — the Phase 2 legs through the LocalExecutor: sweep.tick fans
// row observations out to per-key tx invocations, sweep.boot re-airs
// prepared rows through tx.resubmit, and the rt scope keeps a handler's
// same-key emit direct (the self-deadlock guard).

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

const scanOf = (obs: Array<{ actionId: string; event: string; guard?: string }>): SweepDeps['scan'] =>
  async function* () {
    for (const o of obs) yield { actionId: o.actionId, observe: { event: o.event, guard: o.guard, observation: { at: 1 } } }
  }

test('sweep.tick: observations fan out to per-key tx invocations and commit', async () => {
  const { kx, store } = await open()
  const ex = new LocalExecutor()
  try {
    await prepared(store, 's:1')
    registerTxHandlers(ex, { store, fx: {}, log: () => {} })
    const stages: Array<Array<{ actionId: string; event: string; guard?: string }>> = [
      [{ actionId: 's:1', event: 'submit.unknown' }],
      [{ actionId: 's:1', event: 'obs.proven', guard: 'proof' }]
    ]
    registerSweepHandlers(ex, {
      scan: () => scanOf(stages.shift() ?? [])(),
      resubmitRows: async () => [],
      log: () => {}
    })
    const r = await ex.call<{ sent: number }>({ service: EXEC_SVC.sweep, handler: 'tick' }, {})
    assert.equal(r.sent, 1)
    await ex.drain()
    assert.equal((await store.walletTx({ actionId: 's:1' }))!.state, 'submission_unknown')

    // a second sweep delivering a verified proof revives through the same object
    const r2 = await ex.call<{ sent: number }>({ service: EXEC_SVC.sweep, handler: 'tick' }, {})
    assert.equal(r2.sent, 1)
    await ex.drain()
    assert.equal((await store.walletTx({ actionId: 's:1' }))!.state, 'confirmed')
  } finally {
    ex.close()
    await kx.destroy()
  }
})

test('sweep.boot: prepared rows re-air through tx.resubmit — the coordinator leg runs inside the object', async () => {
  const { kx, store } = await open()
  const ex = new LocalExecutor()
  const submits: string[] = []
  try {
    await prepared(store, 's:boot')
    registerTxHandlers(ex, {
      store,
      fx: {},
      log: () => {},
      submit: async (actionId) => {
        submits.push(actionId)
        return { state: 'submitted', txid: 'x' }
      }
    })
    registerSweepHandlers(ex, {
      scan: scanOf([]),
      resubmitRows: async () => [{ actionId: 's:boot' }],
      log: () => {}
    })
    const r = await ex.call<{ sent: number }>({ service: EXEC_SVC.sweep, handler: 'boot' }, {})
    assert.equal(r.sent, 1)
    await ex.drain()
    assert.deepEqual(submits, ['s:boot'])
  } finally {
    ex.close()
    await kx.destroy()
  }
})

test('rt scope: a same-key emit inside a handler goes direct — the self-deadlock guard', async () => {
  const { kx, store } = await open()
  const ex = new LocalExecutor()
  const routed: string[] = []
  try {
    const w = await prepared(store, 'a:same')
    ex.register(EXEC_SVC.tx, 'observe', async (arg, k) => {
      routed.push(String(k))
      return { accepted: true }
    })
    setExecutor(ex)
    // outside the scope — the emit routes through the executor
    await txEmit(ex, store, w, 'submit.success', { observation: { at: 1 }, attempt: true }, {})
    assert.deepEqual(routed, ['a:same'])
    // inside the object's own scope — the emit must NOT route back
    const direct = await inRtScope({ txKey: 'a:same' }, () => txEmit(ex, store, w, 'submit.success', { observation: { at: 2 }, attempt: true }, {}))
    assert.equal(direct.accepted, true)
    assert.deepEqual(routed, ['a:same']) // unchanged — the executor was bypassed
    assert.equal((await store.walletTx({ actionId: 'a:same' }))!.state, 'submitted')
  } finally {
    setExecutor(undefined)
    ex.close()
    await kx.destroy()
  }
})
