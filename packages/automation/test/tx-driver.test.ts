import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory, { type Knex } from 'knex'
import { currentSignal, deadline, nap } from '@nektar/core'
import { PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator, type WalletDeps } from '../src/wallet.js'
import { TransactionWatcher, custodyTxEffects } from '../src/ledger.js'
import { emitTxEvent, replayTxEffects, inertTxEffects, type TxEffects } from '../src/machines/effects.js'
import { Dispatcher, wireTx } from './rig.js'

// Phase B — tx.machine as DRIVER: the table decides, store.applyTxEvent
// commits state+event+in-tx intents atomically, emitTxEvent runs the
// post-commit custody effects. Fakes sit at the documented dep seams
// (broadcast, txSpent/txDied/recordAired, observeFn) — never re-derive the
// transition table here.

const key = PrivateKey.fromRandom()

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

let txSeq = 0
function tx(): Transaction {
  const t = new Transaction()
  t.addOutput({ satoshis: 1 + txSeq++, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  return t
}

async function prepared(store: AutomationStore, actionId: string, leased = true) {
  const t = tx()
  await store.recordPrepared(
    { actionId, operationId: `op:${actionId}`, kind: 'escrow', txid: t.id('hex'), rawHex: t.toHex(), parents: {}, feeSats: 1 },
    leased ? [{ txid: 'ff'.repeat(32), vout: 0, satoshis: 1000 }] : undefined
  )
  return t.id('hex')
}

// custody hooks take the rebuilt Transaction — the old WalletDeps shape;
// the helper folds them into one total TxEffects shared by the coordinator
// and the observe handler (production: custodyTxEffects fills that role)
function wallet(
  store: AutomationStore,
  over: Partial<WalletDeps> & {
    txSpent?: (tx: Transaction) => void | Promise<void>
    txDied?: (tx: Transaction) => void | Promise<void>
    recordAired?: (tx: Transaction) => void | Promise<void>
  } = {},
  ex?: Dispatcher
) {
  const calls = { broadcast: 0, txSpent: 0, txDied: 0, recordAired: 0 }
  let w!: WalletCoordinator
  const fx: TxEffects = {
    txSpent: async (wt) => {
      calls.txSpent++
      await over.txSpent?.(await w.rebuildTx(wt))
    },
    txDied: async (wt) => {
      calls.txDied++
      await over.txDied?.(await w.rebuildTx(wt))
    },
    recordAired: async (wt) => {
      calls.recordAired++
      await over.recordAired?.(await w.rebuildTx(wt))
    },
    exportExpired: () => {}
  }
  const deps: WalletDeps = {
    broadcast:
      over.broadcast ??
      (() => {
        calls.broadcast++
        return Promise.resolve({ status: 'success' })
      }),
    fx,
    ...(ex !== undefined ? { exec: ex } : {})
  }
  w = new WalletCoordinator(store, deps)
  if (ex !== undefined)
    wireTx(ex, {
      store,
      // the observe handler runs the coordinator's OWN effect set — the
      // one-total-set rule means the object and the caller never disagree
      fx,
      log: () => {},
      submit: (id) => w.submit(id)
    })
  return { w, calls }
}

const leaseState = async (kx: Knex, actionId: string) => (await kx('wallet_reservations').where({ action_id: actionId }).first())?.state as string | undefined

const SEEN = { status: 'seen', source: 'chain' } as const
const PROVEN = { status: 'proven', source: 'chain', blockHeight: 1 } as const

void test('driver: submit success → submitted, event row, lease released, tx-spent fired', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a1'
  await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, {}, ex)
  const r = await w.submit(id)
  assert.equal(r.state, 'submitted')
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'submitted')
  const evs = await store.walletTxEvents(id)
  assert.equal(evs.length, 1)
  assert.deepEqual([evs[0].from, evs[0].event, evs[0].to], ['prepared', 'submit.success', 'submitted'])
  assert.equal(await leaseState(kx, id), 'released')
  assert.equal(calls.txSpent, 1)
  assert.equal(calls.txDied, 0)
})

void test('driver: definitive refusal → rejected, tx-died fired, lease released', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a2'
  await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, { broadcast: () => Promise.resolve({ status: 'error', description: 'missing inputs' }) }, ex)
  const r = await w.submit(id)
  assert.equal(r.state, 'rejected')
  assert.equal(calls.txDied, 1)
  assert.equal(await leaseState(kx, id), 'released')
  const evs = await store.walletTxEvents(id)
  assert.equal(evs[evs.length - 1].event, 'submit.refused')
})

// the 3bb8271b incident: we refused the bytes, the counterparty aired them.
// refused → late proof must revive: custody re-marks, the day ledger counts
// the spend, the lease ends 'spent' — and the aired spend is counted ONCE.
void test('driver: refused-then-aired revive (3bb8271b fixture)', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a3'
  const txid = await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, { broadcast: () => Promise.resolve({ status: 'error', description: 'missing inputs' }) }, ex)
  await w.submit(id)
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'rejected')
  assert.equal(calls.txDied, 1)
  assert.equal(await leaseState(kx, id), 'released')

  const st = await w.observe(txid, PROVEN)
  assert.equal(st, 'confirmed')
  assert.equal(calls.txSpent, 1)
  assert.equal(calls.recordAired, 1)
  assert.equal(await leaseState(kx, id), 'spent')
  const evs = await store.walletTxEvents(id)
  assert.deepEqual(
    evs.map((e) => `${e.from}-${e.event}->${e.to}`),
    ['prepared-submit.refused->rejected', 'rejected-obs.proven->confirmed']
  )
})

void test('driver: revive via seen-then-proven still counts the aired spend once', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a4'
  const txid = await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, { broadcast: () => Promise.resolve({ status: 'error', description: 'missing inputs' }) }, ex)
  await w.submit(id)
  await w.observe(txid, SEEN)
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'observed')
  assert.equal(calls.txSpent, 1)
  assert.equal(calls.recordAired, 1)
  await w.observe(txid, PROVEN)
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'confirmed')
  assert.equal(calls.recordAired, 1)
  assert.equal(await leaseState(kx, id), 'spent')
})

void test('driver: transport failure → submission_unknown, never a verdict; replay converges after restart', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a5'
  await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const first = wallet(
    store,
    {
      broadcast: () => {
        throw new Error('connection refused')
      }
    },
    ex
  )
  const r = await first.w.submit(id)
  assert.equal(r.state, 'submission_unknown')
  assert.equal((await store.walletTx({ actionId: id }))!.attempts, 1)

  // "restart": a fresh coordinator over the same store — no in-memory state
  const second = wallet(store, {}, ex)
  const rs = await second.w.resubmitPending()
  assert.equal(rs.find((x) => x.actionId === id)!.state, 'submitted')
  assert.equal(second.calls.txSpent, 1)
  const evs = await store.walletTxEvents(id)
  assert.deepEqual(
    evs.map((e) => `${e.from}-${e.event}->${e.to}`),
    ['prepared-submit.unknown->submission_unknown', 'submission_unknown-submit.success->submitted']
  )
})

// the leg-context contract on the money path: an ambient abort that fires
// after broadcast may have sent bytes is a TRANSPORT-class outcome —
// submission_unknown, never rejected, and custody stays claimed (the bytes
// may have aired; resubmitPending is the convergence path)
void test('driver: leg timeout mid-broadcast → submission_unknown, custody untouched', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a5b'
  await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(
    store,
    {
      broadcast: async () => {
        // the wire call a signal-aware broadcaster makes — ioSignal()/ambient
        // inside the submit fetch in production; the leg's bound must reach it
        await nap(60_000, { signal: currentSignal() })
        return { status: 'success' }
      }
    },
    ex
  )
  const leg = deadline(() => w.submit(id), 30)
  assert.deepEqual(await leg, { outcome: 'timeout' }, 'the leg bound abandons the waiter — the orphan keeps running')
  await nap(40) // the orphaned submit lands its classification
  const wtx = (await store.walletTx({ actionId: id }))!
  assert.equal(wtx.state, 'submission_unknown', 'an abort after possible send is never a verdict')
  assert.equal(calls.txDied, 0, 'custody must not roll back — the bytes may have left')
  assert.equal(await leaseState(kx, id), 'leased', 'the claim stays: the tx may be on the wire')
})

void test('driver: impossible transitions write nothing — no state, no event row', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a6'
  const txid = await prepared(store, id)
  // prepared --obs.proven--> confirmed must NOT exist: proof before air
  const bad = await emitTxEvent(store, (await store.walletTx({ actionId: id }))!, 'obs.proven', { guard: 'proof' }, inertTxEffects)
  assert.equal(bad.accepted, false)
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'prepared')
  assert.equal((await store.walletTxEvents(id)).length, 0)
  // confirmed --obs.seen--> anything must not exist either
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w } = wallet(store, {}, ex)
  await w.submit(id)
  await w.observe(txid, PROVEN)
  const before = (await store.walletTxEvents(id)).length
  const late = await w.observe(txid, SEEN)
  assert.equal(late, 'confirmed')
  assert.equal((await store.walletTxEvents(id)).length, before)
})

void test('driver: duplicate seen is harmless; proven-before-seen confirms', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a7'
  const txid = await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w } = wallet(store, {}, ex)
  await w.submit(id)
  assert.equal(await w.observe(txid, SEEN), 'observed')
  assert.equal(await w.observe(txid, SEEN), 'observed') // dup: self-edge, still observed
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'observed')

  const id2 = 'a8'
  const txid2 = await prepared(store, id2)
  const { w: w2 } = wallet(store, {}, ex)
  await w2.submit(id2)
  assert.equal(await w2.observe(txid2, PROVEN), 'confirmed')
})

void test('driver: post-commit effect failure leaves the committed transition intact and replays', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a9'
  await prepared(store, id)
  await store.setTxState(id, 'rejected', { reason: 'refused' }, false)
  // rejected --obs.proven--> confirmed carries tx-spent — make it throw:
  // the transition must still commit, the event stays uneffected
  await emitTxEvent(
    store,
    (await store.walletTx({ actionId: id }))!,
    'obs.proven',
    { guard: 'proof', observation: PROVEN },
    {
      ...inertTxEffects,
      txSpent: () => {
        throw new Error('custody store down')
      }
    }
  )
  const row = await store.walletTx({ actionId: id })
  assert.equal(row!.state, 'confirmed')
  assert.equal(await leaseState(kx, id), 'spent') // in-tx intent still ran
  const pending = await store.pendingTxEffects()
  assert.equal(pending.length, 1)
  assert.equal(pending[0].actionId, id)
  // the crash-repair seam: replay with healthy effects re-runs the intents
  let spent = 0
  let aired = 0
  const done = await replayTxEffects(store, {
    ...inertTxEffects,
    txSpent: () => {
      spent++
    },
    recordAired: () => {
      aired++
    }
  })
  assert.equal(done, 1)
  assert.equal(spent, 1)
  assert.equal(aired, 1)
  assert.equal((await store.pendingTxEffects()).length, 0)
})

void test('driver: crash between act and record replays on resubmitPending', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a10'
  await prepared(store, id)
  // act succeeded, "crash" before the coordinator's own custody act —
  // txSpent throws, leaving the committed submit.success event uneffected
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w } = wallet(
    store,
    {
      txSpent: () => {
        throw new Error('crashed post-commit')
      }
    },
    ex
  )
  const r = await w.submit(id)
  assert.equal(r.state, 'submitted')
  assert.equal((await store.pendingTxEffects()).length, 1)
  // "restart": fresh coordinator, broadcast now answers already-known —
  // resubmitPending replays the pending custody intent
  let replayed = 0
  const { w: w2 } = wallet(
    store,
    {
      broadcast: () => Promise.resolve({ status: 'error', description: 'txn-already-in-mempool' }),
      txSpent: () => {
        replayed++
      }
    },
    ex
  )
  await w2.resubmitPending()
  assert.equal(replayed, 1)
  assert.equal((await store.pendingTxEffects()).length, 0)
})

void test('driver: applyTxEvent on a missing action rolls back — nothing written', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  await assert.rejects(store.applyTxEvent('ghost', 'obs.seen', { guard: 'verified' }))
  assert.equal((await store.walletTxEvents('ghost')).length, 0)
})

void test('watcher driver: proven row confirms + spends leases through declared intents', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'w1'
  await prepared(store, id)
  await store.setTxState(id, 'submitted', {}, false)
  const calls = { spent: 0, aired: 0 }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const effects: TxEffects = {
    ...inertTxEffects,
    txSpent: () => {
      calls.spent++
    },
    recordAired: () => {
      calls.aired++
    }
  }
  wireTx(ex, { store, fx: effects, log: () => {} })
  const wt = new TransactionWatcher(store, () => Promise.resolve({ status: 'proven', source: 'chain' }), { effects, exec: ex })
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'confirmed')
  assert.equal(await leaseState(kx, id), 'spent')
  assert.equal(calls.spent, 0) // live→confirmed edge carries leases->spent, not tx-spent
})

void test('watcher driver: dead row revive fires tx-spent + record-aired', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'w2'
  await prepared(store, id)
  await store.setTxState(id, 'rejected', { reason: 'refused' }, false)
  const calls = { spent: 0, aired: 0 }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const effects: TxEffects = {
    ...inertTxEffects,
    txSpent: () => {
      calls.spent++
    },
    recordAired: () => {
      calls.aired++
    }
  }
  wireTx(ex, { store, fx: effects, log: () => {} })
  const wt = new TransactionWatcher(store, () => Promise.resolve({ status: 'proven', source: 'chain' }), { effects, exec: ex })
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'confirmed')
  assert.equal(calls.spent, 1)
  assert.equal(calls.aired, 1)
  const evs = await store.walletTxEvents(id)
  assert.equal(evs[evs.length - 1].event, 'obs.proven')
})

void test('watcher driver: exported row past TTL expires and fires export-expired', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'w3'
  await prepared(store, id)
  await store.setTxState(id, 'exported', {}, false)
  await kx('wallet_transactions')
    .where({ action_id: id })
    .update({ at: Date.now() - 1000 })
  const calls = { expired: 0 }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const effects: TxEffects = {
    ...inertTxEffects,
    exportExpired: () => {
      calls.expired++
    }
  }
  wireTx(ex, { store, fx: effects, log: () => {} })
  const wt = new TransactionWatcher(store, () => Promise.resolve(undefined), { exportedTtlMs: 500, effects, exec: ex })
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'expired')
  assert.equal(calls.expired, 1)
  assert.equal(await leaseState(kx, id), 'released')
})

void test('effects: an intent with no effector is not done — the event stays pending for replay', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'e1'
  await prepared(store, id)
  await store.setTxState(id, 'rejected', { reason: 'refused' }, false)
  // a partial set is an untyped caller bug the dispatcher must surface:
  // obs.proven@proof carries tx-spent + record-aired — {} runs none of it
  const r = await emitTxEvent(store, (await store.walletTx({ actionId: id }))!, 'obs.proven', { guard: 'proof' }, {} as TxEffects)
  assert.equal(r.accepted, true)
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'confirmed')
  const pending = await store.pendingTxEffects()
  assert.equal(pending.length, 1, 'a missing effector is a failed effect — the event must stay pending, not mark done')
  let spent = 0
  let aired = 0
  const done = await replayTxEffects(store, {
    ...inertTxEffects,
    txSpent: () => {
      spent++
    },
    recordAired: () => {
      aired++
    }
  })
  assert.equal(done, 1)
  assert.equal(spent, 1)
  assert.equal(aired, 1)
  assert.equal((await store.pendingTxEffects()).length, 0)
})

void test('driver: abandon commits release-leases + tx-died — a dead verdict frees the claim', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'e2'
  await prepared(store, id)
  await store.setTxState(id, 'submitted', {}, false)
  let died = 0
  await emitTxEvent(
    store,
    (await store.walletTx({ actionId: id }))!,
    'abandon',
    { observation: { reason: 'not mined' } },
    {
      ...inertTxEffects,
      txDied: () => {
        died++
      }
    }
  )
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'abandoned')
  assert.equal(await leaseState(kx, id), 'released', 'abandon frees the claim in-tx — no hand-rolled release')
  assert.equal(died, 1, 'every non-confirmed terminal verdict runs txDied')
})

void test('watcher driver: export.ttl returns inputs to custody through the tx object', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  // an exported row whose input's parent bytes sit in the parents map —
  // revive needs the parent, not a chain lookup (no spendStatus/txLookup here)
  const parent = new Transaction()
  parent.addInput({ sourceTXID: 'aa'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromHex('51') })
  parent.addOutput({ satoshis: 5000, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  const x = new Transaction()
  x.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51') })
  x.addOutput({ satoshis: 4900, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  const id = 'w4'
  await store.recordPrepared(
    {
      actionId: id,
      operationId: 'op:w4',
      kind: 'payment',
      txid: x.id('hex'),
      rawHex: x.toHex(),
      parents: { [parent.id('hex')]: parent.toHex() },
      feeSats: 100
    },
    [{ txid: parent.id('hex'), vout: 0, satoshis: 5000 }]
  )
  await store.setTxState(id, 'exported', {}, false)
  // the ONE production effect set — no spendStatus: 'unknown' still frees
  const revived: Array<{ txid: string; vout: number }> = []
  const fx = custodyTxEffects({
    funds: {
      spent: () => {},
      died: () => {},
      revive: (p: Transaction, vout: number) => {
        revived.push({ txid: p.id('hex'), vout })
      }
    },
    log: () => {}
  })
  const ex = new Dispatcher()
  t.after(() => ex.close())
  wireTx(ex, { store, fx, log: () => {} })
  const wt = new TransactionWatcher(store, () => Promise.resolve(undefined), { exportedTtlMs: 0, effects: fx, exec: ex })
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'expired')
  assert.deepEqual(revived, [{ txid: parent.id('hex'), vout: 0 }], 'the unaired export hands its input back through the tx object')
  assert.equal(await leaseState(kx, id), 'released')
  assert.equal((await store.pendingTxEffects()).length, 0)
})

void test('custodyTxEffects: txSpent reconciles the reserve only on a dead→live revive', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  let afterChange = 0
  const fx = custodyTxEffects({
    funds: { spent: () => {}, died: () => {}, revive: () => {} },
    afterChange: () => {
      afterChange++
      return Promise.resolve()
    },
    log: () => {}
  })
  // submit.success on a live row — custody marks, but nothing was dead: no reconcile
  const id = 'c1'
  await prepared(store, id)
  await emitTxEvent(store, (await store.walletTx({ actionId: id }))!, 'submit.success', { attempt: true }, fx)
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'submitted')
  assert.equal(afterChange, 0, 'a live→submitted mark never reconciles')
  // dead→live revive — the refused tx aired: custody re-marks AND reconciles
  const id2 = 'c2'
  await prepared(store, id2)
  await store.setTxState(id2, 'rejected', { reason: 'refused' }, false)
  await emitTxEvent(store, (await store.walletTx({ actionId: id2 }))!, 'obs.seen', { guard: 'verified' }, fx)
  assert.equal((await store.walletTx({ actionId: id2 }))!.state, 'observed')
  assert.equal(afterChange, 1, 'the revive re-mark reconciles the reserve once')
})

void test('both ledgers: the same driver semantics on a second store instance', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const { kx: kx2, store: store2 } = await open()
  t.after(() => kx2.destroy())
  for (const s of [store, store2]) {
    const id = `both:${s === store ? 'records' : 'automation'}`
    const txid = await prepared(s, id)
    const ex = new Dispatcher()
    t.after(() => ex.close())
    const { w, calls } = wallet(s, { broadcast: () => Promise.resolve({ status: 'error', description: 'missing inputs' }) }, ex)
    await w.submit(id)
    await w.observe(txid, PROVEN)
    assert.equal((await s.walletTx({ actionId: id }))!.state, 'confirmed')
    assert.equal(calls.recordAired, 1)
    assert.equal((await s.walletTxEvents(id)).length, 2)
  }
})
