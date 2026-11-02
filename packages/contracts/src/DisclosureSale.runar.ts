// DisclosureSale — GOAL-v7 V3: atomic payment-for-disclosure escrow.
// buy() reveals `wrapped` on-chain in the SAME tx that pays the seller —
// payment and disclosure settle atomically, no escrow party needed.
// `wrapped` is a DisclosureEnvelope ciphertext (BRC-42 to the buyer), so
// on-chain exposure is safe; or plaintext {field,salt,path} for paid
// public disclosure. Stateless terminal contract: both methods spend the
// listing UTXO once, so it cannot be double-sold.
//   buy:    outputs = [pay→sellerPkh (price), change→buyerPkh]
//   refund: outputs = [pay→sellerPkh], seller-signed, locktime-gated
import {
  SmartContract,
  assert, checkSig, checkPreimage, extractOutputHash, extractAmount, extractLocktime,
  sha256, hash160, hash256, cat, len, num2bin, toByteString,
  type PubKey, type Sig, type SigHashPreimage, type Sha256, type ByteString
} from 'runar-lang'

export class DisclosureSale extends SmartContract {
  readonly sellerPkh: ByteString
  readonly price: bigint
  readonly hWrapped: Sha256
  readonly refundAfter: bigint
  readonly maxFee: bigint

  constructor (sellerPkh: ByteString, price: bigint, hWrapped: Sha256, refundAfter: bigint, maxFee: bigint) {
    super()
    this.sellerPkh = sellerPkh
    this.price = price
    this.hWrapped = hWrapped
    this.refundAfter = refundAfter
    this.maxFee = maxFee
  }

  private p2pkh (pkh: ByteString): ByteString {
    // see MamemFeeVault — buildP2PKH is declared but not compilable in 0.4.6
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  private out (sats: bigint, script: ByteString): ByteString {
    // tx output envelope = value(8LE) + varint(scriptLen) + script — the
    // serialization hashOutputs commits to (runar-lang has no output-builder
    // builtin for stateless contracts in 0.4.6).
    const sl = len(script)
    const slB = sl < 253n ? num2bin(sl, 1n) : cat(num2bin(253n, 1n), num2bin(sl, 2n))
    return cat(num2bin(sats, 8n), cat(slB, script))
  }

  /** the spend itself reveals `wrapped` — bound to the committed sha256. */
  public buy (wrapped: ByteString, buyerPkh: ByteString, changeSats: bigint, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(sha256(wrapped) === this.hWrapped)
    assert(hash256(cat(
      this.out(this.price, this.p2pkh(this.sellerPkh)),
      this.out(changeSats, this.p2pkh(buyerPkh)))) === extractOutputHash(txPreimage))
  }

  /** seller reclaims an unsold listing after the deadline. */
  public refund (sig: Sig, pubKey: PubKey, changeSats: bigint, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(hash160(pubKey) === this.sellerPkh)
    assert(checkSig(sig, pubKey))
    assert(extractLocktime(txPreimage) >= this.refundAfter)
    assert(extractAmount(txPreimage) - changeSats <= this.maxFee)
    assert(hash256(this.out(changeSats, this.p2pkh(this.sellerPkh))) === extractOutputHash(txPreimage))
  }
}
