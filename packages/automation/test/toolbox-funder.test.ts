import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Beef, MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { LiveWalletServices, WalletFunds } from '../src/toolbox.js'

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
const PKH = '00'.repeat(20)
const mkFunds = (wallet: unknown, services?: unknown, log?: (s: string) => void) => new WalletFunds(wallet as never, services as never, { pkhHex: PKH, log })

test('WalletFunds: refresh maps spendable basket outputs to txs', async () => {
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
  const f = mkFunds(w.wallet)
  await f.refresh()
  assert.equal(f.utxos.length, 1) // non-spendable filtered out
  assert.equal(f.utxos[0].tx.id('hex'), a.id('hex'))
  assert.equal(await f.has(a.id('hex'), 0), true)
})

// spv/proofless-funding: a BEEF whose entry carries the tx BUMP hands the
// utxo out WITH its merkle path — the bare-hex rebuild used to strip it, and
// every tree minted on that utxo lost its zero-conf anchor
test('WalletFunds: refresh keeps the merkle path a proven BEEF carries', async () => {
  const a = fundingTx(1000)
  a.merklePath = new MerklePath(7, [
    [
      { offset: 0, hash: '00'.repeat(32) },
      { offset: 1, hash: a.id('hex'), txid: true }
    ]
  ])
  const w = mkWallet([{ outpoint: `${a.id('hex')}.0`, satoshis: 1000, spendable: true }], a.toBEEF())
  const f = mkFunds(w.wallet)
  await f.refresh()
  assert.equal(f.utxos.length, 1)
  assert.ok(f.utxos[0].tx.merklePath !== undefined, 'the BEEF-carried proof must survive refresh')
  assert.equal(f.utxos[0].tx.merklePath?.blockHeight, 7)
})

test('WalletFunds: refresh leaves an unproven tx pathless', async () => {
  const a = fundingTx(1000)
  const w = mkWallet([{ outpoint: `${a.id('hex')}.0`, satoshis: 1000, spendable: true }], a.toBEEF())
  const f = mkFunds(w.wallet)
  await f.refresh()
  assert.equal(f.utxos.length, 1)
  assert.equal(f.utxos[0].tx.merklePath, undefined)
})

test('WalletFunds: take honors lease, deadParent and the sats bound', async () => {
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
  // c is below the sats bound; a is leased; b wins as smallest eligible
  const f = mkFunds(w.wallet)
  f.leased = async (txid: string) => txid === a.id('hex')
  const pick = await f.take('deal', 500)
  assert.equal(pick.tx.id('hex'), b.id('hex'))
  // a dead-parent hint skips the candidate but never relinquishes it —
  // permanent drops are the reconcile path's job
  const g = mkFunds(w.wallet)
  g.hintSkip = async (tx: Transaction) => tx.id('hex') === a.id('hex')
  const pick2 = await g.take('deal', 500)
  assert.equal(pick2.tx.id('hex'), b.id('hex'))
  assert.equal(w.relinquished.length, 0)
})

test('WalletFunds: drop relinquishes; adopt dedups; revive lifts the mark', async () => {
  const a = fundingTx(800)
  const w = mkWallet([{ outpoint: `${a.id('hex')}.0`, satoshis: 800, spendable: true }], a.toBEEF())
  const f = mkFunds(w.wallet)
  await f.refresh()
  await f.burn(a.id('hex'), 0)
  assert.equal(f.utxos.length, 0)
  await new Promise((r) => setTimeout(r, 10))
  assert.deepEqual(w.relinquished, [`${a.id('hex')}.0`])
  // adopt respects the tombstone — an index re-announce can't resurrect it
  await f.adopt(a, 0)
  assert.equal(f.utxos.length, 0)
  // revive is the deliberate re-own — the caller verified it live
  await f.revive(a, 0)
  assert.equal(f.utxos.length, 1)
  // duplicates are no-ops
  await f.adopt(a, 0)
  await f.adopt(a, 0)
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

test('WalletFunds: a mined funding tx internalizes on its own proof — no parent fetches', async () => {
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
  const f = mkFunds(w.wallet, services, (s: string) => logs.push(s))
  await f.adopt(tx, 0)
  assert.deepEqual(logs, [])
  assert.equal(rawCalls, 0, 'a proven subject needs no ancestry')
  assert.equal(f.utxos.length, 1)
  assert.equal(w.internalized.length, 1)
})

// take() reserves the picked outpoint — a second take can't receive it
test('WalletFunds: take reserves the pick until spent or released', async () => {
  const a = fundingTx(1000)
  const b = fundingTx(2000)
  const { wallet } = mkWallet(
    [
      { outpoint: `${a.id('hex')}.0`, satoshis: 1000, spendable: true },
      { outpoint: `${b.id('hex')}.0`, satoshis: 2000, spendable: true }
    ],
    mergedBeef(a, b)
  )
  const f = mkFunds(wallet)
  const u1 = await f.take('deal', 500)
  assert.equal(u1.tx.id('hex'), a.id('hex'))
  const u2 = await f.take('deal', 500)
  assert.equal(u2.tx.id('hex'), b.id('hex')) // a is reserved
  await assert.rejects(() => f.take('deal', 500), /no spendable output/)
  // release frees the reservation; spent() frees by consuming the input
  f.release(a.id('hex'), 0)
  const u3 = await f.take('deal', 500)
  assert.equal(u3.tx.id('hex'), a.id('hex'))
})
