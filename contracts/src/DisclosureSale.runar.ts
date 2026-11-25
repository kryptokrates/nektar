// DisclosureSale — GOAL-v7 V3: atomic payment-for-disclosure escrow,
// the BIP-199 HTLC shape adapted to a covenant-pinned output set.
// The BUYER funds the escrow; `buy` reveals `wrapped` on-chain in the
// SAME tx that pays the seller — payment and disclosure settle atomically.
// If the seller never reveals, the DEPOSITOR (buyer) reclaims the escrow
// after `refundAfter` — the seller can never take a deposit without
// revealing. `wrapped` is a DisclosureEnvelope ciphertext (BRC-42 to the
// buyer), so on-chain exposure is safe; or plaintext {field,salt,path}
// for paid public disclosure. Stateless terminal contract: both methods
// spend the escrow once.
//   buy:    outputs = [pay→sellerPkh (price), change→depositorPkh]
//   refund: outputs = [pay→depositorPkh], depositor-signed, locktime-gated
import {
  SmartContract,
  assert, checkSig, checkPreimage, extractOutputHash, extractAmount, extractLocktime,
  extractHashPrevouts, extractOutpoint,
  sha256, hash160, hash256, cat, len, num2bin, toByteString,
  type PubKey, type Sig, type SigHashPreimage, type Sha256, type ByteString
} from 'runar-lang'

export class DisclosureSale extends SmartContract {
  readonly sellerPkh: ByteString
  readonly price: bigint
  readonly hWrapped: Sha256
  readonly refundAfter: bigint
  readonly maxFee: bigint
  readonly depositorPkh: ByteString
  readonly verdictOutpoint: ByteString

  constructor (sellerPkh: ByteString, price: bigint, hWrapped: Sha256, refundAfter: bigint, maxFee: bigint, depositorPkh: ByteString, verdictOutpoint: ByteString) {
    super()
    this.sellerPkh = sellerPkh
    this.price = price
    this.hWrapped = hWrapped
    this.refundAfter = refundAfter
    this.maxFee = maxFee
    this.depositorPkh = depositorPkh
    this.verdictOutpoint = verdictOutpoint
  }

  private p2pkh (pkh: ByteString): ByteString {
    // see NektarFeeVault — buildP2PKH is declared but not compilable in 0.4.6
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

  /** the spend itself reveals `wrapped` — bound to the committed sha256.
   *  Change is pinned to the DEPOSITOR (post-reveal mempool snipers can't
   *  redirect it) and the fee is bounded (the deposit can't be burned). */
  public buy (wrapped: ByteString, changeSats: bigint, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(sha256(wrapped) === this.hWrapped)
    assert(extractAmount(txPreimage) - this.price - changeSats <= this.maxFee)
    assert(hash256(cat(
      this.out(this.price, this.p2pkh(this.sellerPkh)),
      this.out(changeSats, this.p2pkh(this.depositorPkh)))) === extractOutputHash(txPreimage))
  }

  /** GOAL-v9 consumer hook: same atomic buy, but GATED on a settled
   *  claim — input1 must be the ClaimVerdict UTXO the depositor
   *  committed at escrow-deploy (`verdictOutpoint` baked at construction,
   *  i.e. the sale is created after the claim settles). The verdict's
   *  own script then enforces the payee payout — pay-iff-settled. */
  public buyGated (wrapped: ByteString, changeSats: bigint, prevouts: ByteString, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(sha256(wrapped) === this.hWrapped)
    // exactly two inputs, input1 = the committed verdict UTXO
    assert(hash256(prevouts) === extractHashPrevouts(txPreimage))
    assert(prevouts === cat(extractOutpoint(txPreimage), this.verdictOutpoint))
    assert(extractAmount(txPreimage) - this.price - changeSats <= this.maxFee)
    assert(hash256(cat(
      this.out(this.price, this.p2pkh(this.sellerPkh)),
      this.out(changeSats, this.p2pkh(this.depositorPkh)))) === extractOutputHash(txPreimage))
  }

  /** the DEPOSITOR reclaims an unrevealed escrow after the deadline —
   *  BIP-199 refund leg: the seller can never take the deposit. */
  public refund (sig: Sig, pubKey: PubKey, changeSats: bigint, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(hash160(pubKey) === this.depositorPkh)
    assert(checkSig(sig, pubKey))
    assert(extractLocktime(txPreimage) >= this.refundAfter)
    assert(extractAmount(txPreimage) - changeSats <= this.maxFee)
    assert(hash256(this.out(changeSats, this.p2pkh(this.depositorPkh))) === extractOutputHash(txPreimage))
  }
}
