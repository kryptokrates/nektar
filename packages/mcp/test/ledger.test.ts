import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction, Script } from '@bsv/sdk'
import { ReserveFunder, testWorld } from '@nektar/chain'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { AutomationStore } from '@nektar/automation'
import { wrapBroadcast, leasePredicate, TransactionWatcher, Dispatcher, wireTx, inertTxEffects } from '@nektar/automation'
import { NektarMemory } from '../src/memory.js'

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

void test('ledger: every owner broadcast is recorded with bytes before the outcome is known', async (t) => {
  const { kx, store } = await open()
  const w = await testWorld(t, 2)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, {})
  const _r = await mem.sendSats({ amount: 500, address: PrivateKey.fromRandom().toPublicKey().toAddress([0x6f]) })
  // mem was constructed without a wrapped chain, so this is the owner-tool
  // control: nothing recorded yet. Now wrap and spend again.
  assert.equal((await store.walletTxs(['submitted'])).length, 0)
  // the emit must land on a wired dispatcher — the ambient executor is the
  // restate ingress, which would route this tx observe off the test's store
  // (and its http socket would hold the process open past teardown)
  const ex = new Dispatcher()
  wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
  chain.broadcast = wrapBroadcast(store, chain.broadcast.bind(chain), { fx: inertTxEffects, exec: ex })
  const r2 = await mem.sendSats({ amount: 500, address: PrivateKey.fromRandom().toPublicKey().toAddress([0x6f]) })
  const rec = await store.walletTx({ txid: r2.txid })
  assert.ok(rec, 'owner spend landed in the ledger')
  assert.equal(rec.state, 'submitted')
  assert.ok(rec.rawHex.length > 0)
  assert.ok(rec.feeSats > 0)
  await kx.destroy()
})

void test('ledger: payInvoice exports a signed tx whose input stays leased — no phantom change, no second spend of the same outpoint', async (t) => {
  const { kx, store } = await open()
  const w = await testWorld(t, 1)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, {})
  mem.ledger = store
  const inv = {
    invoiceId: 'i1',
    amount: 300,
    lockingScriptHex: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])).toHex(),
    opReturn: 'nektarpay:i1'
  }
  const r = await mem.payInvoice(inv)
  const rec = await store.walletTx({ txid: r.txid })
  assert.equal(rec!.state, 'exported', 'exported, not submitted')
  const inputTxid = Transaction.fromHex(rec!.rawHex).inputs[0].sourceTXID!
  assert.equal(await store.leasedOutpoint(inputTxid, 0), true, 'the exported input is leased')
  assert.equal(await store.leasedOutpoint(r.txid, 2), false)
  assert.equal((funder as any).utxos.length, 0, 'no unbroadcast change was added back to the reserve')
  await assert.rejects(mem.payInvoice({ ...inv, invoiceId: 'i2' }), /no utxo|no funding UTXO|no spendable output/)
  await kx.destroy()
})

void test('ledger: reserve predicate skips leased outpoints; addOwned dedups', async (_t) => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-ledger-'))
  const { kx, store } = await open()
  const key = PrivateKey.fromRandom()
  const f = new ReserveFunder(key, join(dir, 'reserve.json'))
  const parent = new Transaction()
  parent.addInput({ sourceTXID: 'a'.repeat(64), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
  parent.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  f.addOwned(parent, 0)
  f.addOwned(parent, 0)
  assert.equal(f.utxos.length, 1, 're-adding the same change output does not duplicate it')
  assert.equal((await f.takeReserve(undefined, 500)) !== null, true)
  await store.leaseOutpoints('a1', [{ txid: parent.id('hex'), vout: 0, satoshis: 1000 }])
  f.leased = leasePredicate(store)
  assert.equal(await f.takeReserve(undefined, 500), null, 'a leased outpoint is not handed out')
  await kx.destroy()
})

void test('ledger: EF relay with sparse parent outputs — bookkeeping must not veto a broadcast it cannot record', async (_t) => {
  const { kx, store } = await open()
  const tx = new Transaction()
  tx.addInput({ sourceTXID: 'c'.repeat(64), sourceOutputIndex: 1, unlockingScript: Script.fromHex('51') })
  tx.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()) })
  // EF wires carry the spent output only — the rest of the parent is null holes
  const stub = new Transaction()
  stub.outputs = [null as any, null as any]
  tx.inputs[0].sourceTransaction = stub as any
  const inner = () => Promise.resolve({ status: 'success' as const, txid: tx.id('hex'), description: 'ok' })
  const ex2 = new Dispatcher()
  wireTx(ex2, { store, fx: inertTxEffects, log: () => {} })
  const wrapped = wrapBroadcast(store, inner, { fx: inertTxEffects, exec: ex2 })
  const r = await wrapped(tx)
  assert.equal(r.status, 'success', 'the broadcast result reaches the caller even though nothing was recordable')
  await kx.destroy()
})

void test('ledger: an exported row the counterparty aired settles; an unaired one expires and frees its input', async (_t) => {
  const { kx, store } = await open()
  const key = PrivateKey.fromRandom()
  let seed = 0
  const mk = async (suffix: string) => {
    const parent = new Transaction()
    parent.addInput({ sourceTXID: (++seed).toString(16).padStart(64, '0'), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
    parent.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
    tx.addOutput({ satoshis: 100, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
    await store.recordBroadcast(
      {
        txid: tx.id('hex'),
        rawHex: tx.toHex(),
        parents: { [parent.id('hex')]: parent.toHex() },
        feeSats: 0,
        outs: [{ txid: parent.id('hex'), vout: 0, satoshis: 1000 }]
      },
      `owner:${suffix}`,
      'owner',
      'exported-payment'
    )
    await store.setTxState(`owner:${suffix}`, 'exported', { invoiceId: suffix })
    return parent.id('hex')
  }
  // an aired export settles like any broadcast — 'confirmed' burns its leases
  const airedParent = await mk('aired')
  const ex = new Dispatcher()
  wireTx(ex, { store, fx: inertTxEffects, log: () => {} })
  const settled = new TransactionWatcher(store, () => Promise.resolve({ status: 'proven', source: 'test' }), { effects: inertTxEffects, exec: ex })
  await settled.tick()
  assert.equal((await store.walletTx({ actionId: 'owner:aired' }))!.state, 'confirmed', 'a landed export confirms')
  assert.equal((await store.leaseState(airedParent, 0))!.state, 'spent')
  // an unaired export past its TTL releases the lease and fires the revive hook once
  const staleParent = await mk('stale')
  let revived = 0
  const ex2 = new Dispatcher()
  wireTx(ex2, {
    store,
    fx: {
      ...inertTxEffects,
      exportExpired: () => {
        revived++
      }
    },
    log: () => {}
  })
  const expiring = new TransactionWatcher(store, () => Promise.resolve(undefined), {
    exportedTtlMs: 0,
    effects: {
      ...inertTxEffects,
      exportExpired: () => {
        revived++
      }
    },
    exec: ex2
  })
  await expiring.tick()
  assert.equal((await store.walletTx({ actionId: 'owner:stale' }))!.state, 'expired', 'unaired export past TTL expires')
  assert.equal(revived, 1, 'the revive hook fired')
  assert.equal((await store.leaseState(staleParent, 0))!.state, 'released', 'the stranded lease released')
  const again = await expiring.tick()
  assert.equal(revived, 1, 'expired rows leave the sweep — the hook is not re-fired')
  assert.equal(again, 0)
  // a fresh export inside its window is untouched
  await mk('fresh')
  const ex3 = new Dispatcher()
  wireTx(ex3, { store, fx: inertTxEffects, log: () => {} })
  const waiting = new TransactionWatcher(store, () => Promise.resolve(undefined), { exportedTtlMs: 60_000, effects: inertTxEffects, exec: ex3 })
  await waiting.tick()
  assert.equal((await store.walletTx({ actionId: 'owner:fresh' }))!.state, 'exported', 'inside the window the row waits')
  await kx.destroy()
})

// (releaseUnsigned was deleted as dead code — the abandon-prepared semantic
// lives on reconcileUncertain's path, covered by state-audit's F3-regression)
