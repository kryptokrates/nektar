// GOAL-v7 V3: paid-disclosure MCP tools — env gating + the full two-party
// BIP-199 flow on a shared MockChain (real covenant script eval):
// seller offer → buyer escrow → seller complete (reveal+paid) → buyer claim.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { NektarMemory } from '../src/memory.js'
import { visibleTools, callTool } from '../src/server.js'

const SALE_TOOLS = ['sale']

function setup() {
  const chain = (await stackChain(t))!
  const mk = (opts: any = {}) => {
    const funder = StackFunder.dry()
    funder.topup(50)
    return new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
  }
  return { chain, seller: mk(), buyer: mk(), mk }
}

test('paid disclosure tools: hidden + refused unless NEKTAR_PAID_DISCLOSURE is set', async (t) => {
  const _w = await setup(t)
  if (_w === undefined) return
  const { seller } = _w
  process.env.NEKTAR_TOOL_PROFILE = 'operator'
  try {
    delete process.env.NEKTAR_PAID_DISCLOSURE
    const vis = new Set(visibleTools().map((t) => t.name as string))
    for (const n of SALE_TOOLS) assert.ok(!vis.has(n), `${n} must be hidden when unset`)
    await assert.rejects(() => callTool(seller, 'sale', { op: 'offer', value: 'aa', price: 100, refundAfter: 1, maxFee: 10 }), /paid disclosure is disabled/)
    process.env.NEKTAR_PAID_DISCLOSURE = '1'
    try {
      const vis2 = new Set(visibleTools().map((t) => t.name as string))
      for (const n of SALE_TOOLS) assert.ok(vis2.has(n), `${n} must be listed when enabled`)
    } finally {
      delete process.env.NEKTAR_PAID_DISCLOSURE
    }
  } finally {
    delete process.env.NEKTAR_TOOL_PROFILE
  }
})

test('paid disclosure: two-agent escrow — seller reveals+collects, buyer claims', async () => {
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  try {
    const _w = await setup(t)
    if (_w === undefined) return
    const { chain, seller, buyer } = _w
    const buyerPub = buyer.operatorKey().toPublicKey().toString()
    const sellerPub = seller.operatorKey().toPublicKey().toString()
    const VALUE = Buffer.from('{"field":7,"salt":"aa","path":[]}')
    // 1. seller offers (private wrap to the buyer)
    const offer = await callTool(seller, 'sale', { op: 'offer', value: VALUE.toString('hex'), price: 3_000, refundAfter: 1_000_000, maxFee: 10, buyerPub })
    assert.equal(offer.cfg.hWrapped.length, 64)
    assert.ok(offer.offerSig && offer.sellerPub, "the offer must carry the seller's delegation signature")
    // 2. buyer funds the escrow — verifies the signed offer first
    const esc = await callTool(buyer, 'sale', { op: 'escrow', cfg: offer.cfg, amount: 6_000, offer: { sellerPub: offer.sellerPub, offerSig: offer.offerSig } })
    assert.equal(esc.accepted, true, `escrow funded: ${esc.reason ?? ''}`)
    assert.ok(esc.escrow.depositorPkh, 'buyer completes cfg with their pkh')
    // a MITM-swapped cfg is refused before any funding
    await assert.rejects(
      () =>
        callTool(buyer, 'sale', {
          op: 'escrow',
          cfg: { ...offer.cfg, price: 999_999 },
          amount: 6_000,
          offer: { sellerPub: offer.sellerPub, offerSig: offer.offerSig }
        }),
      /offerSig does not verify/
    )
    // 3. seller completes — reveals wrapped, collects price
    const done = await callTool(seller, 'sale', { op: 'complete', cfg: esc.escrow, utxo: esc.utxo, wrapped: offer.wrapped, changeSats: 2_990 })
    assert.equal(done.accepted, true, `buy accepted: ${done.reason ?? ''}`)
    assert.equal(done.sellerSats, 3_000)
    // 4. buyer claims from the real buy tx
    const tx = chain.mempool.get(done.txid)!
    const claim = await callTool(buyer, 'sale', { op: 'claim', txHex: tx.toHex(), hWrapped: offer.cfg.hWrapped, sellerPub })
    assert.equal(claim.mode, 'private')
    assert.equal(claim.valueHex, VALUE.toString('hex'), 'unwrapped payload round-trips')
    // 5. wrong commitment is refused
    await assert.rejects(() => callTool(buyer, 'sale', { op: 'claim', txHex: tx.toHex(), hWrapped: 'ff'.repeat(32) }), /does not match/)
  } finally {
    delete process.env.NEKTAR_PAID_DISCLOSURE
  }
})

test('paid disclosure: depositor refunds an unrevealed escrow after deadline', async (t) => {
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  try {
    const _w = await setup(t)
    if (_w === undefined) return
    const { seller, buyer } = _w
    const offer = await callTool(seller, 'sale', { op: 'offer', value: 'deadbeef', price: 1_000, refundAfter: 100, maxFee: 10 })
    const esc = await callTool(buyer, 'sale', { op: 'escrow', cfg: offer.cfg, amount: 2_000 })
    assert.equal(esc.accepted, true)
    // seller never completes — buyer reclaims at the deadline
    const ref = await callTool(buyer, 'sale', { op: 'refund', cfg: esc.escrow, utxo: esc.utxo, lockTime: 100, changeSats: 1_990 })
    assert.equal(ref.accepted, true, `refund accepted: ${ref.reason ?? ''}`)
    assert.equal(ref.refundSats, 1_990)
    // a refund before the deadline is refused by the covenant
    const esc2 = await callTool(buyer, 'sale', { op: 'escrow', cfg: offer.cfg, amount: 2_000 })
    const early = await callTool(buyer, 'sale', { op: 'refund', cfg: esc2.escrow, utxo: esc2.utxo, lockTime: 50, changeSats: 1_990 })
    assert.notEqual(early.accepted, true, 'early refund must be refused')
  } finally {
    delete process.env.NEKTAR_PAID_DISCLOSURE
  }
})

test('paid disclosure: saleOfferMemory binds the payload to an anchored leaf; claim verifies it + provenance σ', async (t) => {
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  try {
    const _w = await setup(t)
    if (_w === undefined) return
    const { chain, seller, buyer } = _w
    const sellerPub = seller.operatorKey().toPublicKey().toString()
    // §16: the sold record carries a verifiable provenance signature
    const { signProvenance } = await import('@nektar/log')
    const { utf8 } = await import('@nektar/core')
    const p0 = { source: 'agent', time: 1_700_000_000, signature: new Uint8Array(0), modelId: 'test-model-1' }
    const signed = signProvenance(seller.operatorKey(), utf8('the sold secret fact'), p0)
    await seller.remember('vault', 'the sold secret fact', { ...p0, signature: signed.signature, sourcePub: sellerPub })
    const buyerPub = buyer.operatorKey().toPublicKey().toString()
    const offer = await callTool(seller, 'sale', { op: 'offerMemory', shard: 'vault', seq: 0, price: 500, refundAfter: 1_000_000, maxFee: 10, buyerPub })
    assert.equal(offer.commitment.leaf.length, 64)
    assert.ok(
      offer.commitment.proof?.path !== undefined || offer.commitment.anchorTxid === null,
      'commitment should carry the inclusion path when the record is anchored'
    )
    const esc = await callTool(buyer, 'sale', {
      op: 'escrow',
      cfg: offer.cfg,
      amount: 1_000,
      offer: { sellerPub: offer.sellerPub, offerSig: offer.offerSig, commitment: offer.commitment }
    })
    const done = await callTool(seller, 'sale', { op: 'complete', cfg: esc.escrow, utxo: esc.utxo, wrapped: offer.wrapped, changeSats: 490 })
    assert.equal(done.accepted, true, `buy accepted: ${done.reason ?? ''}`)
    const tx = chain.mempool.get(done.txid)!
    const claim = await callTool(buyer, 'sale', {
      op: 'claim',
      txHex: tx.toHex(),
      hWrapped: offer.cfg.hWrapped,
      sellerPub,
      offerMode: offer.mode,
      commitment: offer.commitment
    })
    assert.equal(claim.mode, 'private')
    assert.equal(claim.verified, true, 'the bought bytes recompute to the anchored leaf')
    assert.equal(claim.authorVerified, true, 'the provenance σ verifies against the recorded author key')
    // a tampered commitment fails the leaf check
    const bad = await callTool(buyer, 'sale', {
      op: 'claim',
      txHex: tx.toHex(),
      hWrapped: offer.cfg.hWrapped,
      sellerPub,
      commitment: { ...offer.commitment, leaf: 'ff'.repeat(32) }
    })
    assert.equal(bad.verified, false)
  } finally {
    delete process.env.NEKTAR_PAID_DISCLOSURE
  }
})

test('paid disclosure: encrypted records sell the FRAGMENT KEY (paper f_i shape); buyer decrypts + verifies', async (t) => {
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  try {
    const _w = await setup(t)
    if (_w === undefined) return
    const { chain, mk } = _w
    // agentSeed enables §7 encryption; maxLevel 0 keeps writes at level 0
    const seller = mk({ agentSeed: PrivateKey.fromRandom(), maxLevel: 0 })
    const buyer = mk()
    const buyerPub = buyer.operatorKey().toPublicKey().toString()
    const sellerPub = seller.operatorKey().toPublicKey().toString()
    const r = await seller.remember('vault', 'encrypted memory for sale', {}, 0)
    assert.equal(r.seq, 0, `remember: ${JSON.stringify(r)}`)
    const offer = await callTool(seller, 'sale', { op: 'offerMemory', shard: 'vault', seq: 0, price: 500, refundAfter: 1_000_000, maxFee: 10, buyerPub })
    assert.equal(offer.mode, 'key', 'encrypted record must sell the fragment key, not plaintext')
    const esc = await callTool(buyer, 'sale', {
      op: 'escrow',
      cfg: offer.cfg,
      amount: 1_000,
      offer: { sellerPub: offer.sellerPub, offerSig: offer.offerSig, commitment: offer.commitment }
    })
    const done = await callTool(seller, 'sale', { op: 'complete', cfg: esc.escrow, utxo: esc.utxo, wrapped: offer.wrapped, changeSats: 490 })
    assert.equal(done.accepted, true, `buy accepted: ${done.reason ?? ''}`)
    const tx = chain.mempool.get(done.txid)!
    const claim = await callTool(buyer, 'sale', {
      op: 'claim',
      txHex: tx.toHex(),
      hWrapped: offer.cfg.hWrapped,
      sellerPub,
      offerMode: 'key',
      commitment: offer.commitment
    })
    assert.equal(claim.decrypted, true, 'the buyer decrypts with the sold fragment key')
    assert.equal(claim.verified, true, 'decrypted fragment recomputes to the anchored leaf')
    assert.equal(Buffer.from(claim.valueHex, 'hex').toString(), 'encrypted memory for sale')
  } finally {
    delete process.env.NEKTAR_PAID_DISCLOSURE
  }
})

test('GOAL-v8 D9/D10: expired offers + past-deadline escrows are refused before funding', async (t) => {
  process.env.NEKTAR_PAID_DISCLOSURE = '1'
  try {
    const _w = await setup(t)
    if (_w === undefined) return
    const { chain, seller, buyer } = _w
    await chain.mine(5) // push the mock tip to height 5
    const buyerPub = buyer.operatorKey().toPublicKey().toString()
    // D9 — offer expiry is inside the signed digest and enforced at escrow time
    const expired = await callTool(seller, 'sale', { op: 'offer', value: 'aa', price: 100, refundAfter: 1_000_000, maxFee: 10, buyerPub, expiresAt: 3 })
    assert.equal(expired.expiresAt, 3)
    await assert.rejects(
      () =>
        callTool(buyer, 'sale', {
          op: 'escrow',
          cfg: expired.cfg,
          amount: 2_000,
          offer: { sellerPub: expired.sellerPub, offerSig: expired.offerSig, expiresAt: expired.expiresAt }
        }),
      /offer expired/,
      'a lapsed offer cannot be funded'
    )
    // a fresh horizon is accepted AND the expiry is inside the signature
    const fresh = await callTool(seller, 'sale', { op: 'offer', value: 'aa', price: 100, refundAfter: 1_000_000, maxFee: 10, buyerPub, expiresAt: 50 })
    const esc = await callTool(buyer, 'sale', {
      op: 'escrow',
      cfg: fresh.cfg,
      amount: 2_000,
      offer: { sellerPub: fresh.sellerPub, offerSig: fresh.offerSig, expiresAt: fresh.expiresAt }
    })
    assert.equal(esc.accepted, true)
    // tampering with the expiry after signing breaks offerSig (it is digest-bound)
    await assert.rejects(
      () =>
        callTool(buyer, 'sale', {
          op: 'escrow',
          cfg: fresh.cfg,
          amount: 2_000,
          offer: { sellerPub: fresh.sellerPub, offerSig: fresh.offerSig, expiresAt: 9999 }
        }),
      /offerSig does not verify/,
      'a substituted expiry invalidates the signed offer'
    )
    // D10 — a refundAfter that is already past makes the escrow instantly refundable
    const stale = await callTool(seller, 'sale', { op: 'offer', value: 'aa', price: 100, refundAfter: 0, maxFee: 10, buyerPub })
    await assert.rejects(
      () => callTool(buyer, 'sale', { op: 'escrow', cfg: stale.cfg, amount: 2_000 }),
      /refundAfter is not in the future/,
      'a past locktime is refused at funding'
    )
    // a timestamp-based deadline in the past is refused the same way
    const pastTs = await callTool(seller, 'sale', {
      op: 'offer',
      value: 'aa',
      price: 100,
      refundAfter: 1_000_000_000,
      maxFee: 10,
      buyerPub // 2001-09-09
    })
    await assert.rejects(() => callTool(buyer, 'sale', { op: 'escrow', cfg: pastTs.cfg, amount: 2_000 }), /refundAfter is not in the future/)
  } finally {
    delete process.env.NEKTAR_PAID_DISCLOSURE
  }
})
