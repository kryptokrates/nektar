import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Beef, PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { ToolboxReserveFunder } from '../src/toolbox.js'

// Mock BRC-100 wallet: listOutputs serves a canned spendable set + the BEEF
// the class rebuilds Transaction objects from. internalize/relinquish are
// recorded so custody transitions are assertable.
interface Out { outpoint: string; satoshis: number; spendable: boolean }
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
