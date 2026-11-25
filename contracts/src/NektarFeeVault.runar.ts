import { StatefulSmartContract, assert, hash160, checkSig, cat, toByteString, type Sig, type PubKey, type ByteString, extractLocktime, extractSequence, extractAmount } from 'runar-lang'

// NektarFeeVault — hardened rolling-window rate-limited fee vault.
// Enforces extractSequence < 0xffffffff and block-height locktime domain (< 500_000_000)
// so windowCap resets cannot be spoofed via SEQUENCE_FINAL or timestamp locktimes.
export class NektarFeeVault extends StatefulSmartContract {
  destPkh: ByteString
  ownerPkh: ByteString
  perTxCap: bigint
  windowCap: bigint
  windowBlocks: bigint
  maxFee: bigint
  spentInWindow: bigint
  windowStart: bigint

  constructor (destPkh: ByteString, ownerPkh: ByteString, perTxCap: bigint, windowCap: bigint, windowBlocks: bigint, maxFee: bigint) {
    super(destPkh, ownerPkh, perTxCap, windowCap, windowBlocks, maxFee)
    this.destPkh = destPkh
    this.ownerPkh = ownerPkh
    this.perTxCap = perTxCap
    this.windowCap = windowCap
    this.windowBlocks = windowBlocks
    this.maxFee = maxFee
    this.spentInWindow = 0n
    this.windowStart = 0n
  }

  private p2pkh (pkh: ByteString): ByteString {
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  public spend (sig: Sig, pubKey: PubKey, payAmount: bigint, newSpent: bigint, newStart: bigint, contSats: bigint) {
    assert(hash160(pubKey) === this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(payAmount >= 1n && contSats >= 1n)
    const expired = lt >= this.windowStart + this.windowBlocks
    assert((expired && newSpent === payAmount && newStart === lt) ||
      (!expired && newSpent === this.spentInWindow + payAmount && newStart === this.windowStart))
    this.spentInWindow = newSpent
    this.windowStart = newStart
    assert(this.spentInWindow <= this.windowCap)
    assert(payAmount <= this.perTxCap)
    assert(extractAmount(this.txPreimage) - contSats - payAmount <= this.maxFee + 1n)
    this.addOutput(contSats, this.destPkh, this.ownerPkh, this.perTxCap, this.windowCap, this.windowBlocks, this.maxFee, this.spentInWindow, this.windowStart)
    this.addRawOutput(payAmount, this.p2pkh(this.destPkh))
  }
}
