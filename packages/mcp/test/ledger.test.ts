import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction, Script } from '@bsv/sdk'
import { ReserveFunder, stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { AutomationStore } from '../src/automation/store.js'
import { wrapBroadcast, leasePredicate } from '../src/automation/ledger.js'
import { NektarMemory } from '../src/memory.js'

async function open() {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  return { kx, store }
}

test('ledger: every owner broadcast is recorded with bytes before the outcome is known', async (t) => {
  const { kx, store } = await open()
  const w = await stackWorld(t, 2)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, {})
  const r = await mem.sendSats({ amount: 500, address: PrivateKey.fromRandom().toPublicKey().toAddress([0x6f]) })
  // mem was constructed without a wrapped chain, so this is the owner-tool
  // control: nothing recorded yet. Now wrap and spend again.
  assert.equal((await store.walletTxs(['submitted'])).length, 0)
  chain.broadcast = wrapBroadcast(store, chain.broadcast.bind(chain))
  const r2 = await mem.sendSats({ amount: 500, address: PrivateKey.fromRandom().toPublicKey().toAddress([0x6f]) })
  const rec = await store.walletTx({ txid: r2.txid })
  assert.ok(rec, 'owner spend landed in the ledger')
  assert.equal(rec!.state, 'submitted')
  assert.ok(rec!.rawHex.length > 0)
  assert.ok(rec!.feeSats > 0)
  await kx.destroy()
})

test('ledger: payInvoice exports a signed tx whose input stays leased — no phantom change, no second spend of the same outpoint', async () => {
  const { kx, store } = await open()
  const w = await stackWorld(t, 1)
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
  const inputTxid = Transaction.fromHex(rec!.rawHex).inputs[0]!.sourceTXID!
  assert.equal(await store.leasedOutpoint(inputTxid, 0), true, 'the exported input is leased')
  assert.equal((await store.leasedOutpoint(r.txid, 2)) ?? false, false)
  assert.equal((funder as any).utxos.length, 0, 'no unbroadcast change was added back to the reserve')
  await assert.rejects(mem.payInvoice({ ...inv, invoiceId: 'i2' }), /no utxo|no funding UTXO/)
  await kx.destroy()
})

test('ledger: reserve predicate skips leased outpoints; addOwned dedups', async (t) => {
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

test('ledger: releaseUnsigned frees only a never-submitted preparation', async (t) => {
  const { kx, store } = await open()
  const tx = new Transaction()
  tx.addInput({ sourceTXID: 'b'.repeat(64), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
  tx.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()) })
  await store.recordBroadcast(
    { txid: tx.id('hex'), rawHex: tx.toHex(), parents: {}, feeSats: 0, outs: [{ txid: 'b'.repeat(64), vout: 0, satoshis: 100 }] },
    'owner:x',
    'owner',
    'owner'
  )
  assert.equal(await store.releaseUnsigned('owner:x'), true)
  assert.equal(await store.leasedOutpoint('b'.repeat(64), 0), false)
  assert.equal(await store.releaseUnsigned('owner:x'), false)
  await kx.destroy()
})
