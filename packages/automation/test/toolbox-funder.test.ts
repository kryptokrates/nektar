import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Beef, MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { LiveWalletServices, ToolboxReserveFunder } from '../src/toolbox.js'

// Mock BRC-100 wallet: listOutputs serves a canned spendable set + the BEEF
// the class rebuilds Transaction objects from. internalize/relinquish are
// recorded so custody transitions are assertable.
interface Out {
  outpoint: string
  satoshis: number
  spendable: boolean
}
const fundingTx = (sats: number): Transaction => {
  const tx = new Transaction()
  tx.addOutput({ lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()), satoshis: sats })
  return tx
}
const mergedBeef = (...txs: Transaction[]): number[] => {
  const beef = new Beef()
  for (const tx of txs) beef.mergeTransaction(tx)
  return beef.toBinary()
}
const mkWallet = (outs: Out[], beef: number[]) => {
  const relinquished: string[] = []
  const internalized: number[] = []
  return {
    relinquished,
    internalized,
    wallet: {
      listOutputs: async () => ({ outputs: outs, BEEF: beef }),
      relinquishOutput: async (a: { output: string }) => {
        relinquished.push(a.output)
        return { relinquished: true }
      },
      internalizeAction: async (a: { tx: number[] }) => {
        internalized.push(a.tx.length)
        return { accepted: true }
      }
    }
  }
}

test('ToolboxReserveFunder: refresh maps spendable basket outputs to txs', async () => {
  const a = fundingTx(1000)
  const b = fundingTx(500)
  const beef = a.toBEEF()
  const w = mkWallet(
    [
      { outpoint: `${a.id('hex')}.0`, satoshis: 1000, spendable: true },
      { outpoint: `${b.id('hex')}.0`, satoshis: 500, spendable: false }
    ],
    beef
  )
  const f = new ToolboxReserveFunder(w.wallet as never)
  await f.refresh()
  assert.equal(f.utxos.length, 1) // non-spendable filtered out
  assert.equal(f.utxos[0].tx.id('hex'), a.id('hex'))
  assert.equal(f.isSpent(a.id('hex'), 0), false)
  assert.equal(f.isSpent(b.id('hex'), 0), true) // absent from spendable set
  assert.equal(await f.has(a.id('hex'), 0), true)
})

// spv/proofless-funding: a BEEF whose entry carries the tx BUMP hands the
// utxo out WITH its merkle path — the bare-hex rebuild used to strip it, and
// every tree minted on that utxo lost its zero-conf anchor
test('ToolboxReserveFunder: refresh keeps the merkle path a proven BEEF carries', async () => {
  const a = fundingTx(1000)
  a.merklePath = new MerklePath(7, [
    [
      { offset: 0, hash: '00'.repeat(32) },
      { offset: 1, hash: a.id('hex'), txid: true }
    ]
  ])
  const w = mkWallet([{ outpoint: `${a.id('hex')}.0`, satoshis: 1000, spendable: true }], a.toBEEF())
  const f = new ToolboxReserveFunder(w.wallet as never)
  await f.refresh()
  assert.equal(f.utxos.length, 1)
  assert.ok(f.utxos[0].tx.merklePath !== undefined, 'the BEEF-carried proof must survive refresh')
  assert.equal(f.utxos[0].tx.merklePath?.blockHeight, 7)
})

test('ToolboxReserveFunder: refresh leaves an unproven tx pathless', async () => {
  const a = fundingTx(1000)
  const w = mkWallet([{ outpoint: `${a.id('hex')}.0`, satoshis: 1000, spendable: true }], a.toBEEF())
  const f = new ToolboxReserveFunder(w.wallet as never)
  await f.refresh()
  assert.equal(f.utxos.length, 1)
  assert.equal(f.utxos[0].tx.merklePath, undefined)
})

test('ToolboxReserveFunder: takeReserve honors lease, spendCheck and minSats', async () => {
  const a = fundingTx(700)
  const b = fundingTx(900)
  const c = fundingTx(300)
  const w = mkWallet(
    [
      { outpoint: `${a.id('hex')}.0`, satoshis: 700, spendable: true },
      { outpoint: `${b.id('hex')}.0`, satoshis: 900, spendable: true },
      { outpoint: `${c.id('hex')}.0`, satoshis: 300, spendable: true }
    ],
    mergedBeef(a, b, c)
  )
  const f = new ToolboxReserveFunder(w.wallet as never)
  await f.refresh()
  // c is below the 546 floor; a is leased; b wins as smallest eligible
  f.leased = async (txid) => txid === a.id('hex')
  const pick = await f.takeReserve()
  assert.equal(pick?.tx.id('hex'), b.id('hex'))
  // spendCheck-marked outputs are dropped and relinquished, never returned
  const g = new ToolboxReserveFunder(w.wallet as never)
  await g.refresh()
  const pick2 = await g.takeReserve(async (txid) => txid === a.id('hex'))
  assert.equal(pick2?.tx.id('hex'), b.id('hex')) // a spend-checked out, c below floor
  assert.ok(w.relinquished.includes(`${a.id('hex')}.0`))
})

test('ToolboxReserveFunder: markSpent relinquishes + addOwned dedups', async () => {
  const a = fundingTx(800)
  const w = mkWallet([{ outpoint: `${a.id('hex')}.0`, satoshis: 800, spendable: true }], a.toBEEF())
  const f = new ToolboxReserveFunder(w.wallet as never)
  await f.refresh()
  f.markSpent(a.id('hex'), 0)
  assert.equal(f.utxos.length, 0)
  assert.equal(f.isSpent(a.id('hex'), 0), true)
  await new Promise((r) => setTimeout(r, 10))
  assert.deepEqual(w.relinquished, [`${a.id('hex')}.0`])
  // addOwned pushes into the spendable cache once; duplicates are no-ops
  await f.addOwned(a, 0)
  await f.addOwned(a, 0)
  assert.equal(f.utxos.length, 1)
  assert.equal(w.internalized.length, 1)
})

test('LiveWalletServices.postBeef: the relayed tx keeps its input sources — the guard can price it', async () => {
  // the wallet re-relays its own unconfirmed txs; rebuilt from bare hex the
  // inputs carried no source outputs and SpendGuard refused every one
  const key = PrivateKey.fromRandom()
  const parent = fundingTx(1000)
  parent.outputs[0].lockingScript = new P2PKH().lock(key.toPublicKey().toHash())
  const child = new Transaction()
  child.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  child.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toHash()), satoshis: 900 })
  await child.sign()
  const beef = new Beef()
  beef.mergeTransaction(child)
  const seen: Transaction[] = []
  const svc = new LiveWalletServices({
    chain: 'test',
    mc: {},
    tracker: { currentHeight: async () => 1, isValidRootForHeight: async () => true },
    broadcast: async (tx) => {
      seen.push(tx)
      return { status: 'success', txid: tx.id('hex') }
    }
  })
  const r = await svc.postBeef(beef as never, [child.id('hex')])
  assert.equal(r[0].status, 'success')
  assert.equal(seen.length, 1)
  assert.equal(seen[0].id('hex'), child.id('hex'))
  assert.equal(seen[0].inputs[0].sourceTransaction?.outputs[0].satoshis, 1000)
})

test('ToolboxReserveFunder: a mined funding tx internalizes on its own proof — no parent fetches', async () => {
  // a many-input sweep: fetching every parent is pure indexer load, and one
  // rate-limited parent used to fail the whole import
  const tx = fundingTx(5000)
  for (let i = 1; i <= 12; i++)
    tx.addInput({ sourceTXID: i.toString(16).padStart(2, '0').repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  const txid = tx.id('hex')
  const mp = new MerklePath(100, [
    [
      { offset: 0, hash: txid, txid: true },
      { offset: 1, hash: '00'.repeat(32) }
    ]
  ])
  let rawCalls = 0
  const services = {
    getRawTx: async () => {
      rawCalls++
      return { txid: '', rawTx: undefined }
    },
    getMerklePath: async (id: string) => (id === txid ? { merklePath: mp } : {})
  }
  const w = mkWallet([], [])
  const logs: string[] = []
  const f = new ToolboxReserveFunder(w.wallet as never, services as never, (s) => logs.push(s))
  await f.addOwned(tx, 0)
  assert.deepEqual(logs, [])
  assert.equal(rawCalls, 0, 'a proven subject needs no ancestry')
  assert.equal(f.utxos.length, 1)
  assert.equal(w.internalized.length, 1)
})
