// DisclosureSale — GOAL-v7 V4: hardened atomic payment-for-disclosure escrow.
// Security invariants enforced under consensus:
//   - F-05: buy/buyGated verify native BRC-369 keyed.Commitment(wrapped):
//           len(wrapped) === 32 AND sha256("metanet keyed content commitment v1" || wrapped) === hWrapped
//           so hWrapped === KeyCommitment directly under Bitcoin Script consensus.
//   - F-06: buy() asserts len(verdictOutpoint) === 0n so verdict-gated escrows cannot bypass buyGated().
//   - F-02: refund() enforces extractSequence < 0xffffffff AND matching locktime domain (< or >= 500_000_000).
//   - F-19: unsigned varint length encoding in out() + 20-byte PKH and positive changeSats checks.
import {
  SmartContract,
  assert, checkSig, checkPreimage, extractOutputHash, extractAmount, extractLocktime,
  extractSequence, extractHashPrevouts, extractOutpoint,
  sha256, hash160, hash256, cat, len, left, num2bin, toByteString,
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
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  private out (sats: bigint, script: ByteString): ByteString {
    const sl = len(script)
    const slB = sl < 253n ? left(num2bin(sl, 2n), 1n) : cat(toByteString('fd'), left(num2bin(sl, 4n), 2n))
    return cat(num2bin(sats, 8n), cat(slB, script))
  }

  /** Atomic ungated reveal: reveals the 32-byte BRC-369 content key `wrapped` whose
   *  BRC-369 keyed.Commitment(wrapped) equals `hWrapped`. Refused if verdictOutpoint is set. */
  public buy (wrapped: ByteString, changeSats: bigint, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(len(this.verdictOutpoint) === 0n)
    assert(len(this.sellerPkh) === 20n && len(this.depositorPkh) === 20n)
    assert(len(wrapped) === 32n)
    assert(sha256(cat(toByteString('6d6574616e6574206b6579656420636f6e74656e7420636f6d6d69746d656e74207631'), wrapped)) === this.hWrapped)
    assert(changeSats >= 1n)
    assert(extractAmount(txPreimage) - this.price - changeSats <= this.maxFee)
    assert(hash256(cat(
      this.out(this.price, this.p2pkh(this.sellerPkh)),
      this.out(changeSats, this.p2pkh(this.depositorPkh)))) === extractOutputHash(txPreimage))
  }

  /** Atomic ZK-verdict-gated reveal: requires input1 == committed ClaimVerdict outpoint. */
  public buyGated (wrapped: ByteString, changeSats: bigint, prevouts: ByteString, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(len(this.verdictOutpoint) === 36n)
    assert(len(this.sellerPkh) === 20n && len(this.depositorPkh) === 20n)
    assert(len(wrapped) === 32n)
    assert(sha256(cat(toByteString('6d6574616e6574206b6579656420636f6e74656e7420636f6d6d69746d656e74207631'), wrapped)) === this.hWrapped)
    assert(hash256(prevouts) === extractHashPrevouts(txPreimage))
    assert(prevouts === cat(extractOutpoint(txPreimage), this.verdictOutpoint))
    assert(changeSats >= 1n)
    assert(extractAmount(txPreimage) - this.price - changeSats <= this.maxFee)
    assert(hash256(cat(
      this.out(this.price, this.p2pkh(this.sellerPkh)),
      this.out(changeSats, this.p2pkh(this.depositorPkh)))) === extractOutputHash(txPreimage))
  }

  /** Depositor reclaims an unrevealed escrow after refundAfter (consensus locktime enforced). */
  public refund (sig: Sig, pubKey: PubKey, changeSats: bigint, txPreimage: SigHashPreimage) {
    assert(checkPreimage(txPreimage))
    assert(len(this.depositorPkh) === 20n)
    assert(hash160(pubKey) === this.depositorPkh)
    assert(checkSig(sig, pubKey))
    const lt = extractLocktime(txPreimage)
    assert(extractSequence(txPreimage) < 4294967295n)
    assert((this.refundAfter < 500000000n && lt < 500000000n) || (this.refundAfter >= 500000000n && lt >= 500000000n))
    assert(lt >= this.refundAfter)
    assert(changeSats >= 1n)
    assert(extractAmount(txPreimage) - changeSats <= this.maxFee)
    assert(hash256(this.out(changeSats, this.p2pkh(this.depositorPkh))) === extractOutputHash(txPreimage))
  }
}
