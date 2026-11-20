import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory, { type Knex } from 'knex'
import { currentSignal, deadline, nap, setNektarEnv } from '@nektar/core'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator, type WalletDeps } from '../src/wallet.js'
import { TransactionWatcher } from '../src/ledger.js'
import { emitTxEvent, replayTxEffects, type TxEffects } from '../src/machines/effects.js'
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

function wallet(store: AutomationStore, over: Partial<WalletDeps> = {}, ex?: Dispatcher) {
  const calls = { broadcast: 0, txSpent: 0, txDied: 0, recordAired: 0 }
  const deps: WalletDeps = {
    broadcast:
      over.broadcast ??
      (async () => {
        calls.broadcast++
        return { status: 'success' }
      }),
    txSpent: () => {
      calls.txSpent++
    },
    txDied: () => {
      calls.txDied++
    },
    recordAired: () => {
      calls.recordAired++
    },
    ...over,
    ...(ex !== undefined ? { exec: ex } : {})
  }
  const w = new WalletCoordinator(store, deps)
  if (ex !== undefined)
    wireTx(ex, {
      store,
      // the observe handler's intents run this coordinator's own deps —
      // wallet.fx()'s rebuild-then-call mapping, verbatim test-side
      fx: {
        txSpent: async (wt) => await deps.txSpent?.(await w.rebuildTx(wt)),
        txDied: async (wt) => await deps.txDied?.(await w.rebuildTx(wt)),
        recordAired: async (wt) => await deps.recordAired?.(await w.rebuildTx(wt))
      },
      log: () => {},
      submit: (id) => w.submit(id)
    })
  return { w, calls }
}

const leaseState = async (kx: Knex, actionId: string) => (await kx('wallet_reservations').where({ action_id: actionId }).first())?.state as string | undefined

const SEEN = { status: 'seen', source: 'chain' } as const
const PROVEN = { status: 'proven', source: 'chain', blockHeight: 1 } as const

test('driver: submit success → submitted, event row, lease released, tx-spent fired', async (t) => {
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

test('driver: definitive refusal → rejected, tx-died fired, lease released', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a2'
  await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, { broadcast: async () => ({ status: 'error', description: 'missing inputs' }) }, ex)
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
test('driver: refused-then-aired revive (3bb8271b fixture)', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a3'
  const txid = await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, { broadcast: async () => ({ status: 'error', description: 'missing inputs' }) }, ex)
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

test('driver: revive via seen-then-proven still counts the aired spend once', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a4'
  const txid = await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const { w, calls } = wallet(store, { broadcast: async () => ({ status: 'error', description: 'missing inputs' }) }, ex)
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

test('driver: transport failure → submission_unknown, never a verdict; replay converges after restart', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a5'
  await prepared(store, id)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const first = wallet(
    store,
    {
      broadcast: async () => {
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
test('driver: leg timeout mid-broadcast → submission_unknown, custody untouched', async (t) => {
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

test('driver: impossible transitions write nothing — no state, no event row', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'a6'
  const txid = await prepared(store, id)
  // prepared --obs.proven--> confirmed must NOT exist: proof before air
  const bad = await emitTxEvent(store, (await store.walletTx({ actionId: id }))!, 'obs.proven', { guard: 'proof' }, {})
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

test('driver: duplicate seen is harmless; proven-before-seen confirms', async (t) => {
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

test('driver: post-commit effect failure leaves the committed transition intact and replays', async (t) => {
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
      txSpent: () => {
        throw new Error('custody store down')
      },
      recordAired: () => {}
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

test('driver: crash between act and record replays on resubmitPending', async (t) => {
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
      broadcast: async () => ({ status: 'error', description: 'txn-already-in-mempool' }),
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

test('driver: applyTxEvent on a missing action rolls back — nothing written', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  await assert.rejects(store.applyTxEvent('ghost', 'obs.seen', { guard: 'verified' }))
  assert.equal((await store.walletTxEvents('ghost')).length, 0)
})

test('watcher driver: proven row confirms + spends leases through declared intents', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'w1'
  await prepared(store, id)
  await store.setTxState(id, 'submitted', {}, false)
  const calls = { spent: 0, aired: 0 }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const effects: TxEffects = {
    txSpent: () => {
      calls.spent++
    },
    recordAired: () => {
      calls.aired++
    }
  }
  wireTx(ex, { store, fx: effects, log: () => {} })
  const wt = new TransactionWatcher(
    store,
    async () => ({ status: 'proven', source: 'chain' }),
    async () => {},
    { effects, exec: ex }
  )
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'confirmed')
  assert.equal(await leaseState(kx, id), 'spent')
  assert.equal(calls.spent, 0) // live→confirmed edge carries leases->spent, not tx-spent
})

test('watcher driver: dead row revive fires tx-spent + record-aired', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const id = 'w2'
  await prepared(store, id)
  await store.setTxState(id, 'rejected', { reason: 'refused' }, false)
  const calls = { spent: 0, aired: 0 }
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const effects: TxEffects = {
    txSpent: () => {
      calls.spent++
    },
    recordAired: () => {
      calls.aired++
    }
  }
  wireTx(ex, { store, fx: effects, log: () => {} })
  const wt = new TransactionWatcher(
    store,
    async () => ({ status: 'proven', source: 'chain' }),
    async () => {},
    { effects, exec: ex }
  )
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'confirmed')
  assert.equal(calls.spent, 1)
  assert.equal(calls.aired, 1)
  const evs = await store.walletTxEvents(id)
  assert.equal(evs[evs.length - 1].event, 'obs.proven')
})

test('watcher driver: exported row past TTL expires and fires export-expired', async (t) => {
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
    exportExpired: () => {
      calls.expired++
    }
  }
  wireTx(ex, { store, fx: effects, log: () => {} })
  const wt = new TransactionWatcher(
    store,
    async () => undefined,
    async () => {},
    { exportedTtlMs: 500, effects, exec: ex }
  )
  await wt.tick()
  assert.equal((await store.walletTx({ actionId: id }))!.state, 'expired')
  assert.equal(calls.expired, 1)
  assert.equal(await leaseState(kx, id), 'released')
})

test('both ledgers: the same driver semantics on a second store instance', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const { kx: kx2, store: store2 } = await open()
  t.after(() => kx2.destroy())
  for (const s of [store, store2]) {
    const id = `both:${s === store ? 'records' : 'automation'}`
    const txid = await prepared(s, id)
    const ex = new Dispatcher()
    t.after(() => ex.close())
    const { w, calls } = wallet(s, { broadcast: async () => ({ status: 'error', description: 'missing inputs' }) }, ex)
    await w.submit(id)
    await w.observe(txid, PROVEN)
    assert.equal((await s.walletTx({ actionId: id }))!.state, 'confirmed')
    assert.equal(calls.recordAired, 1)
    assert.equal((await s.walletTxEvents(id)).length, 2)
  }
})

test('kill switch: NEKTAR_TX_FSM=legacy keeps the hand-rolled path and writes no event rows', async (t) => {
  setNektarEnv('NEKTAR_TX_FSM', 'legacy')
  try {
    const { kx, store } = await open()
    t.after(() => kx.destroy())
    const id = 'l1'
    await prepared(store, id)
    const { w, calls } = wallet(store)
    const r = await w.submit(id)
    assert.equal(r.state, 'submitted')
    assert.equal(calls.txSpent, 1)
    assert.equal(await leaseState(kx, id), 'released')
    assert.equal((await store.walletTxEvents(id)).length, 0) // legacy: no event log
  } finally {
    setNektarEnv('NEKTAR_TX_FSM', undefined)
  }
})
