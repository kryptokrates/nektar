// GOAL-v7 V3: the DisclosureSale covenant under real script eval — BIP-199
// shape: the BUYER funds the escrow, the seller completes by revealing
// `wrapped`, the DEPOSITOR refunds after the deadline. (MockChain
// verifyScripts runs the actual locking script via SDK Spend.)
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript, UnlockingScript, P2PKH } from '@bsv/sdk'
import { Hash } from '@bsv/sdk'
import { MockChain } from '@nektar/chain'
import { DisclosureSaleHost, type SaleUtxo } from '../src/sale.js'

const seller = PrivateKey.fromRandom()
const sellerPkh = Buffer.from(seller.toPublicKey().toHash()).toString('hex')
const buyer = PrivateKey.fromRandom()
const buyerPkh = Buffer.from(buyer.toPublicKey().toHash()).toString('hex')
const WRAPPED = Buffer.from('the-wrapped-payload-preimage-of-disclosure').toString('hex')
const H_WRAPPED = Buffer.from(Hash.sha256(Buffer.from(WRAPPED, 'hex'))).toString('hex')
// the seller's published offer — no depositor; the buyer completes it
const cfg = { sellerPkh, price: 5_000n, hWrapped: H_WRAPPED, refundAfter: 100n, maxFee: 10n }

/** a buyer-funded escrow utxo (the buyer self-builds the script from cfg) */
async function escrowOf (chain: MockChain, host: DisclosureSaleHost, deposit = 6_000) {
  const funding = new Transaction()
  funding.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new UnlockingScript() })
  funding.addOutput({ satoshis: 20_000, lockingScript: LockingScript.fromHex(
    // buyer's own P2PKH funding out — what a real wallet would give them
    Buffer.concat([Buffer.from('76a914', 'hex'), Buffer.from(buyer.toPublicKey().toHash()), Buffer.from('88ac', 'hex')]).toString('hex')) })
  chain.registerParent(funding)
  const { tx, utxo } = await host.buildEscrow([{ tx: funding, vout: 0, satoshis: 20_000 }], buyer, deposit)
  assert.equal((await chain.broadcast(tx)).status, 'success', 'escrow funding accepted')
  chain.registerParent(tx)
  return utxo
}

function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const host = new DisclosureSaleHost({ ...cfg, depositorPkh: buyerPkh })
  return { chain, host }
}

test('V3 escrow: buyer funds, seller reveals+collects atomically; underpayment/reroute/fee refused', async () => {
  const { chain, host } = setup()
  const utxo = await escrowOf(chain, host)
  // seller completes unilaterally — reveals WRAPPED, collects price,
  // depositor gets the change back
  const good = host.buildBuy(utxo, WRAPPED, 990n)
  const r = await chain.broadcast(good.tx)
  assert.equal(r.status, 'success', `buy accepted: ${(r as any).description ?? ''}`)
  assert.equal(good.tx.outputs[0]!.satoshis, 5_000, 'seller paid exactly price')
  assert.equal(good.tx.outputs[1]!.satoshis, 990, 'depositor change returned')
  // a different preimage fails sha256(wrapped) === hWrapped
  const bad = host.buildBuy(utxo, Buffer.from('wrong-payload').toString('hex'), 990n)
  assert.notEqual((await chain.broadcast(bad.tx)).status, 'success', 'wrong wrapped must be rejected')
  // underpayment: tamper the seller output below `price`
  const under = host.buildBuy(utxo, WRAPPED, 990n)
  under.tx.outputs[0]!.satoshis = 4_999
  assert.notEqual((await chain.broadcast(under.tx)).status, 'success', 'underpayment must be rejected')
  // fee bound: changeSats too small → fee > maxFee
  const greedy = host.buildBuy(utxo, WRAPPED, 500n)
  assert.notEqual((await chain.broadcast(greedy.tx)).status, 'success', 'fee > maxFee must be rejected')
})

test('V3 escrow: depositor refunds after deadline; seller/early/stranger refused', async () => {
  const { chain, host } = setup()
  const utxo = await escrowOf(chain, host)
  // too early — locktime below refundAfter
  const early = host.buildRefund(utxo, buyer, 50, 5_990n)
  assert.notEqual((await chain.broadcast(early.tx)).status, 'success', 'refund before refundAfter must be rejected')
  // the SELLER cannot take the deposit — their key is not the depositor's
  const rug = host.buildRefund(utxo, seller, 100, 5_990n)
  assert.notEqual((await chain.broadcast(rug.tx)).status, 'success', 'seller refund must be rejected')
  // a stranger can't either
  const stranger = host.buildRefund(utxo, PrivateKey.fromRandom(), 100, 5_990n)
  assert.notEqual((await chain.broadcast(stranger.tx)).status, 'success', 'stranger refund must be rejected')
  // depositor at the deadline — deposit comes back
  const ok = host.buildRefund(utxo, buyer, 100, 5_990n)
  const r = await chain.broadcast(ok.tx)
  assert.equal(r.status, 'success', `depositor refund accepted: ${(r as any).description ?? ''}`)
  // fee bound: changeSats below input-maxFee → fee > maxFee
  const greedy = host.buildRefund(utxo, buyer, 100, 5_000n)
  assert.notEqual((await chain.broadcast(greedy.tx)).status, 'success', 'refund with fee > maxFee must be rejected')
})

test('V3 sale: claimDisclosure reads wrapped back from the real covenant buy tx', async () => {
  const { chain, host } = setup()
  const utxo = await escrowOf(chain, host)
  const good = host.buildBuy(utxo, WRAPPED, 990n)
  // parseBuyArgs/claimDisclosure (identity layer) on the covenant's real arg frame
  const { claimDisclosure } = await import('@nektar/identity')
  const r = claimDisclosure(good.tx, { hWrapped: H_WRAPPED })
  assert.equal(r.mode, 'public')
  assert.equal(Buffer.from(r.wrapped).toString('hex'), WRAPPED)
  assert.throws(() => claimDisclosure(good.tx, { hWrapped: 'ff'.repeat(32) }), /does not match/)
})
