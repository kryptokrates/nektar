// GOAL-v7 V3: the DisclosureSale covenant under real node script eval —
// BIP-199 shape: the BUYER funds the escrow, the seller completes by revealing
// `wrapped`, the DEPOSITOR refunds after the deadline. Broadcasts go through
// the regtest node's own admission: real UTXOs, real signatures, real CLTV.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript, UnlockingScript } from '@bsv/sdk'
import { Hash } from '@bsv/sdk'
import { stackWorld, type StackChain, type StackFunder } from '@nektar/chain'
import { DisclosureSaleHost } from '../src/sale.js'

const seller = PrivateKey.fromRandom()
const sellerPkh = Buffer.from(seller.toPublicKey().toHash()).toString('hex')
const WRAPPED = Buffer.from('the-wrapped-payload-preimage-of-disclosure').toString('hex')
const H_WRAPPED = Buffer.from(Hash.sha256(Buffer.from(WRAPPED, 'hex'))).toString('hex')
// the seller's published offer — no depositor; the buyer completes it
const cfg = { sellerPkh, price: 5_000n, hWrapped: H_WRAPPED, refundAfter: 100n, maxFee: 10n }

type World = { chain: StackChain, funder: StackFunder, host: DisclosureSaleHost, buyer: PrivateKey }
async function setup (t: { skip: (m?: string) => void }): Promise<World | undefined> {
  const w = await stackWorld(t, 8, 20_000)
  if (w === undefined) return undefined
  const buyer = w.funder.keyPair
  const buyerPkh = Buffer.from(buyer.toPublicKey().toHash()).toString('hex')
  const host = new DisclosureSaleHost({ ...cfg, depositorPkh: buyerPkh })
  return { chain: w.chain, funder: w.funder, host, buyer }
}

/** a buyer-funded escrow utxo — a real wallet outpoint into the covenant */
async function escrowOf (w: World, deposit = 6_000) {
  const out = await w.funder.take(20_000)
  const { tx, utxo } = await w.host.buildEscrow([out], w.buyer, deposit)
  assert.equal((await w.chain.broadcast(tx)).status, 'success', 'escrow funding accepted')
  return utxo
}

test('V3 escrow: buyer funds, seller reveals+collects atomically; underpayment/reroute/fee refused', async t => {
  const w = await setup(t); if (w === undefined) return
  const { chain, host } = w
  const utxo = await escrowOf(w)
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

test('V3 escrow: depositor refunds after deadline; seller/early/stranger refused', async t => {
  const w = await setup(t); if (w === undefined) return
  const { chain, host, buyer } = w
  const utxo = await escrowOf(w)
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

test('V3 sale: claimDisclosure reads wrapped back from the real covenant buy tx', async t => {
  const w = await setup(t); if (w === undefined) return
  const { host } = w
  const utxo = await escrowOf(w)
  const good = host.buildBuy(utxo, WRAPPED, 990n)
  // parseBuyArgs/claimDisclosure (identity layer) on the covenant's real arg frame
  const { claimDisclosure } = await import('@nektar/identity')
  const r = claimDisclosure(good.tx, { hWrapped: H_WRAPPED })
  assert.equal(r.mode, 'public')
  assert.equal(Buffer.from(r.wrapped).toString('hex'), WRAPPED)
  assert.throws(() => claimDisclosure(good.tx, { hWrapped: 'ff'.repeat(32) }), /does not match/)
})

test('V3 escrow: funding fee follows the instance rate, not a constant', async () => {
  const buyer = PrivateKey.fromRandom()
  const buyerPkh = Buffer.from(buyer.toPublicKey().toHash()).toString('hex')
  const host = new DisclosureSaleHost({ ...cfg, depositorPkh: buyerPkh })
  const mkFunding = (sats: number) => {
    const f = new Transaction()
    // coinbase-shaped parent — a 1-byte unlock so the parent serializes; the
    // tx never hits the node, only the escrow's fee math reads it
    f.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromHex('51') })
    f.addOutput({
      satoshis: sats,
      lockingScript: LockingScript.fromHex(
        Buffer.concat([Buffer.from('76a914', 'hex'), Buffer.from(buyer.toPublicKey().toHash()), Buffer.from('88ac', 'hex')]).toString('hex')
      )
    })
    return f
  }
  const feeOf = async (rate: number) => {
    const { tx, feeSats } = await host.buildEscrow([{ tx: mkFunding(20_000), vout: 0, satoshis: 20_000 }], buyer, 6_000, undefined, rate)
    const paid = 20_000 - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
    assert.equal(feeSats, paid, 'reported fee equals actual input-output delta')
    return paid
  }
  const at100 = await feeOf(100)
  const bytes = (await host.buildEscrow([{ tx: mkFunding(20_000), vout: 0, satoshis: 20_000 }], buyer, 6_000, undefined, 100)).tx.toBinary().length
  assert.ok(at100 >= bytes * 0.1 - 2 && at100 <= bytes * 0.1 + 2, `100 sat/kB on ~${bytes}B tx → ${at100} sats`)
  const at1 = await feeOf(1)
  assert.ok(at1 >= 1 && at1 <= bytes * 0.001 + 1, `1 sat/kB → ${at1} sats (min 1)`)
  // insufficient funding still refuses
  await assert.rejects(() => host.buildEscrow([{ tx: mkFunding(5_000), vout: 0, satoshis: 5_000 }], buyer, 6_000, undefined, 1), /amount .* \+ fee/)
})
