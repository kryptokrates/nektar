// GOAL-v7 V3: the DisclosureSale covenant under real script eval
// (MockChain verifyScripts runs the actual locking script via SDK Spend).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript } from '@bsv/sdk'
import { Hash } from '@bsv/sdk'
import { MockChain } from '@mamem/chain'
import { DisclosureSaleHost, type SaleUtxo } from '../src/sale.js'

const seller = PrivateKey.fromRandom()
const sellerPkh = Buffer.from(seller.toPublicKey().toHash()).toString('hex')
const buyer = PrivateKey.fromRandom()
const buyerPkh = Buffer.from(buyer.toPublicKey().toHash()).toString('hex')
const WRAPPED = Buffer.from('the-wrapped-payload-preimage-of-disclosure').toString('hex')
const H_WRAPPED = Buffer.from(Hash.sha256(Buffer.from(WRAPPED, 'hex'))).toString('hex')
const cfg = { sellerPkh, price: 5_000n, hWrapped: H_WRAPPED, refundAfter: 100n, maxFee: 10n }

function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const sale = new DisclosureSaleHost(cfg)
  const parent = new Transaction()
  parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new LockingScript() })
  parent.addOutput({ satoshis: 10_000, lockingScript: LockingScript.fromHex(sale.lockingScript()) })
  const utxo: SaleUtxo = { txid: parent.id('hex'), vout: 0, satoshis: 10_000, script: sale.lockingScript() }
  return { chain, sale, parent, utxo }
}

test('V3 sale: buy reveals wrapped and pays seller atomically; wrong preimage rejected', async () => {
  const { chain, sale, parent, utxo } = setup()
  chain.registerParent(parent)
  const good = sale.buildBuy(utxo, WRAPPED, buyerPkh, 4_000n)
  const r = await chain.broadcast(good.tx)
  assert.equal(r.status, 'success', `buy accepted: ${(r as any).description ?? ''}`)
  assert.equal(good.tx.outputs[0]!.satoshis, 5_000, 'seller paid exactly price')
  // a different preimage fails sha256(wrapped) === hWrapped
  const bad = sale.buildBuy(utxo, Buffer.from('wrong-payload').toString('hex'), buyerPkh, 4_000n)
  assert.notEqual((await chain.broadcast(bad.tx)).status, 'success', 'wrong wrapped must be rejected')
})

test('V3 sale: refund gated by locktime + seller sig', async () => {
  const { chain, sale, parent, utxo } = setup()
  chain.registerParent(parent)
  const early = sale.buildRefund(utxo, seller, 50, 9_990n)
  assert.notEqual((await chain.broadcast(early.tx)).status, 'success', 'refund before refundAfter must be rejected')
  const wrongKey = sale.buildRefund(utxo, PrivateKey.fromRandom(), 100, 9_990n)
  assert.notEqual((await chain.broadcast(wrongKey.tx)).status, 'success', 'refund with non-seller sig must be rejected')
  const ok = sale.buildRefund(utxo, seller, 100, 9_990n)
  const r = await chain.broadcast(ok.tx)
  assert.equal(r.status, 'success', `refund at height accepted: ${(r as any).description ?? ''}`)
})
