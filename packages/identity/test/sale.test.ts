// GOAL-v7 V3 host layer: offer → (covenant buy tx) → claim roundtrip.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction, UnlockingScript } from '@bsv/sdk'
import { toHex, sha256 } from '@nektar/core'
import {
  offerDisclosure, wrapSaleValue, plainSaleValue,
  parseBuyArgs, claimDisclosure
} from '../src/sale.js'

const seller = PrivateKey.fromRandom()
const buyer = PrivateKey.fromRandom()
const sellerPub = seller.toPublicKey().toString()
const buyerPub = buyer.toPublicKey().toString()
const VALUE = new TextEncoder().encode('{"field":"role","salt":"ab12","value":"admin"}')

/** fake the buy tx's unlocking pushes: opSig‖wrapped‖change‖pre‖00
 *  (depositorPkh is pinned in the escrow script, not an arg) */
function fakeBuyUnlock (wrapped: Uint8Array): string {
  const push = (b: Uint8Array) => b.length === 0 ? '00' : (b.length <= 0x4b ? b.length.toString(16).padStart(2, '0') : '4c' + b.length.toString(16).padStart(2, '0')) + toHex(b)
  return push(new Uint8Array(71)) + push(wrapped) + push(new Uint8Array([0xa0, 0x0f])) + push(new Uint8Array(181)) + '00'
}

test('sale: offer→wrap→claim roundtrip (private BRC-42)', () => {
  const { wrapped, hWrapped } = wrapSaleValue(seller, buyerPub, VALUE)
  assert.equal(hWrapped, toHex(sha256(wrapped)))
  // parse the unlock pushes back out
  const unlockHex = fakeBuyUnlock(wrapped)
  const args = parseBuyArgs(unlockHex)
  assert.deepEqual([...args.wrapped], [...wrapped])
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
  const r = claimDisclosure(tx, { hWrapped, buyerKey: buyer, sellerPub })
  assert.equal(r.mode, 'private')
  assert.deepEqual([...r.value], [...VALUE])
})

test('sale: public disclosure unwraps as-is; tampered hWrapped rejected', () => {
  const { wrapped, hWrapped } = plainSaleValue(VALUE)
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(fakeBuyUnlock(wrapped)) })
  const r = claimDisclosure(tx, { hWrapped })
  assert.equal(r.mode, 'public')
  assert.deepEqual([...r.value], [...VALUE])
  assert.throws(() => claimDisclosure(tx, { hWrapped: 'ff'.repeat(32) }), /does not match/)
})

test('sale: offerDisclosure produces a valid covenant cfg + committed wrapped', () => {
  const offer = offerDisclosure({
    sellerKey: seller, value: VALUE, price: 3_000n,
    refundAfter: 1_000_000n, maxFee: 10n, buyerPub
  })
  assert.equal(offer.cfg.hWrapped.length, 64)
  assert.equal(offer.cfg.sellerPkh.length, 40)
  assert.equal(offer.cfg.price, 3_000n)
  // wrapped round-trips through the claim path (the real-tx variant lives in
  // @nektar/anchor's sale.test.ts — anchor already depends on this package)
  const unlockHex = fakeBuyUnlock(fromHexForTest(offer.wrapped))
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
  const r = claimDisclosure(tx, { hWrapped: offer.cfg.hWrapped, buyerKey: buyer, sellerPub })
  assert.equal(r.mode, 'private')
  assert.deepEqual([...r.value], [...VALUE])
})
function fromHexForTest (s: string): Uint8Array { return new Uint8Array(s.match(/.{2}/g)!.map(b => parseInt(b, 16))) }
