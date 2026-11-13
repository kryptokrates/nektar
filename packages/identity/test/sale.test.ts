// GOAL-v7 V3 host layer: offer → (covenant buy tx) → claim roundtrip.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction, UnlockingScript } from '@bsv/sdk'
import { toHex, sha256 } from '@nektar/core'
import {
  offerDigest,
  offerDisclosure,
  wrapSaleValue,
  plainSaleValue,
  parseBuyArgs,
  parsePushes,
  claimDisclosure,
  publicSaleQuote,
  saleQuoteWire,
  verifyOfferSig
} from '../src/sale.js'

const seller = PrivateKey.fromRandom()
const buyer = PrivateKey.fromRandom()
const sellerPub = seller.toPublicKey().toString()
const buyerPub = buyer.toPublicKey().toString()
const VALUE = new TextEncoder().encode('{"field":"role","salt":"ab12","value":"admin"}')

/** fake the buy tx's unlocking pushes: opSig‖wrapped‖change‖pre‖00
 *  (depositorPkh is pinned in the escrow script, not an arg) */
function fakeBuyUnlock(wrapped: Uint8Array): string {
  const push = (b: Uint8Array) =>
    b.length === 0 ? '00' : (b.length <= 0x4b ? b.length.toString(16).padStart(2, '0') : '4c' + b.length.toString(16).padStart(2, '0')) + toHex(b)
  return push(new Uint8Array(71)) + push(wrapped) + push(new Uint8Array([0xa0, 0x0f])) + push(new Uint8Array(181)) + '00'
}

test('sale: offer→wrap→claim roundtrip (private BRC-42)', async () => {
  const { wrapped, hWrapped } = await wrapSaleValue(seller, buyerPub, VALUE)
  assert.equal(hWrapped, toHex(sha256(wrapped)))
  // parse the unlock pushes back out
  const unlockHex = fakeBuyUnlock(wrapped)
  const args = parseBuyArgs(unlockHex)
  assert.deepEqual([...args.wrapped], [...wrapped])
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
  const r = await claimDisclosure(tx, { hWrapped, buyerKey: buyer, sellerPub })
  assert.equal(r.mode, 'private')
  assert.deepEqual([...r.value], [...VALUE])
})

test('sale: public disclosure unwraps as-is; tampered hWrapped rejected', async () => {
  const { wrapped, hWrapped } = plainSaleValue(VALUE)
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(fakeBuyUnlock(wrapped)) })
  const r = await claimDisclosure(tx, { hWrapped })
  assert.equal(r.mode, 'public')
  assert.deepEqual([...r.value], [...VALUE])
  await assert.rejects(() => claimDisclosure(tx, { hWrapped: 'ff'.repeat(32) }), /does not match/)
})

test('sale: offerDisclosure produces a valid covenant cfg + committed wrapped', async () => {
  const offer = await offerDisclosure({
    sellerKey: seller,
    value: VALUE,
    price: 3_000n,
    refundAfter: 1_000_000n,
    maxFee: 10n,
    buyerPub
  })
  assert.equal(offer.cfg.hWrapped.length, 64)
  assert.equal(offer.cfg.sellerPkh.length, 40)
  assert.equal(offer.cfg.price, 3_000n)
  // wrapped round-trips through the claim path (the real-tx variant lives in
  // @nektar/anchor's sale.test.ts — anchor already depends on this package)
  const unlockHex = fakeBuyUnlock(fromHexForTest(offer.wrapped))
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
  const r = await claimDisclosure(tx, { hWrapped: offer.cfg.hWrapped, buyerKey: buyer, sellerPub })
  assert.equal(r.mode, 'private')
  assert.deepEqual([...r.value], [...VALUE])
})
test('sale: public quote carries no reveal credential yet still verifies', async () => {
  const offer = await offerDisclosure({
    sellerKey: seller,
    value: VALUE,
    price: 500n,
    refundAfter: 1_000_000n,
    maxFee: 10n,
    buyerPub,
    expiresAt: 700_000n
  })
  const q = publicSaleQuote(offer)
  assert.ok(!('wrapped' in q), 'public quote has no wrapped')
  assert.ok(JSON.stringify(saleQuoteWire(q)).indexOf(offer.wrapped) === -1, 'wire quote never contains the credential')
  assert.ok(verifyOfferSig(q, buyerPub), 'quote signature verifies without wrapped')
  const mutated = { ...q, cfg: { ...q.cfg, price: 1n } }
  assert.ok(!verifyOfferSig(mutated, buyerPub), 'mutated price rejected')
  const wire = JSON.parse(JSON.stringify(saleQuoteWire(q)))
  assert.equal(typeof wire.cfg.price, 'number')
  assert.ok(verifyOfferSig({ ...q, cfg: { ...q.cfg, price: BigInt(wire.cfg.price) } }, buyerPub), 'wire representation verifies')
})
test('sale: wire quote carries payload mode (delivered can out-run the quote)', () => {
  // regression: when the buyer's `delivered` landed before the out-of-band
  // `quote` message, the claim ran with offerMode undefined — a key-mode
  // payload parsed as a fragment and a good sale false-disputed. The signed
  // quote must self-describe the payload encoding.
  const offer = { cfg: { sellerPkh: 'aa'.repeat(20), price: 1n, hWrapped: 'bb'.repeat(32), refundAfter: 1n, maxFee: 1n }, wrapped: 'dd', sellerPub: '02' + 'ab'.repeat(32), offerSig: 'cc', mode: 'key' as const }
  const wire = JSON.parse(JSON.stringify(saleQuoteWire(publicSaleQuote(offer))))
  assert.equal(wire.mode, 'key')
  const noMode = { ...offer, mode: undefined }
  const wire2 = JSON.parse(JSON.stringify(saleQuoteWire(publicSaleQuote(noMode))))
  assert.ok(!('mode' in wire2), 'unset mode stays absent on the wire')
})
test('sale: wallet-custody quote sig (verbatim digest) verifies too', async () => {
  // the DigestSigner seam emits two wire forms for the same attestation:
  // wallet custody signs the digest verbatim (hashToDirectlySign → ECDSA
  // over sha256(digest)); key custody goes through PrivateKey.sign which
  // hashes once more (ECDSA over sha256(sha256(digest))). The test's
  // offerDisclosure above covers key custody; here we reproduce the wallet
  // form — PrivateKey.sign hashes its arg once, so sign(digest) == ECDSA
  // over sha256(digest).
  const offer = await offerDisclosure({
    sellerKey: seller,
    value: VALUE,
    price: 500n,
    refundAfter: 1_000_000n,
    maxFee: 10n,
    buyerPub,
    expiresAt: 700_000n
  })
  const q = publicSaleQuote(offer)
  const digest = offerDigest(q.cfg, buyerPub, '', q.expiresAt ?? 0)
  const walletForm = { ...q, offerSig: seller.sign(Array.from(digest)).toDER('hex') as string }
  assert.ok(verifyOfferSig(walletForm, buyerPub), 'verbatim-digest (wallet custody) form verifies')
})
function fromHexForTest(s: string): Uint8Array {
  return new Uint8Array(s.match(/.{2}/g)!.map((b) => parseInt(b, 16)))
}

test('parsePushes: small-integer opcodes (OP_1NEGATE, OP_1..OP_16) read as their minimal number pushes', () => {
  const p = parsePushes('00' + '4f' + '51' + '52' + '60' + '02abcd')
  assert.deepEqual(
    p.map((b) => Buffer.from(b).toString('hex')),
    ['', '81', '01', '02', '10', 'abcd']
  )
  assert.throws(() => parsePushes('76'), /non-push opcode/)
})
