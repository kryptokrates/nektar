import { StatefulSmartContract, assert, hash160, checkSig, toByteString, cat, type Sig, type PubKey, type ByteString, extractLocktime, extractAmount } from 'runar-lang'

// Covenant-enforced fee vault (idiomatic StatefulSmartContract):
// every spend may pay at most `payAmount` to `destPkh`, subject to
// per-tx and rolling-window caps, keeping the remainder in the contract.
// The window transition is supplied as (newSpent, newStart) and validated
// unconditionally — ANF state inference requires straight-line mutation.
// The residual (real change + miner fee) is bounded by maxFee + 1 sat:
// the injected change output's destination is caller-set, so we bound its
// AMOUNT — change-leakage beyond maxFee+1 sat is consensus-impossible.
export class MamemFeeVault extends StatefulSmartContract {
  destPkh: ByteString // 20B hash160 of the only payable address
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

  public spend (sig: Sig, pubKey: PubKey, payAmount: bigint, newSpent: bigint, newStart: bigint, contSats: bigint) {
    assert(hash160(pubKey) === this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const lt = extractLocktime(this.txPreimage)
    const expired = lt >= this.windowStart + this.windowBlocks
    assert((expired && newSpent === payAmount && newStart === lt) ||
      (!expired && newSpent === this.spentInWindow + payAmount && newStart === this.windowStart))
    this.spentInWindow = newSpent
    this.windowStart = newStart
    assert(this.spentInWindow <= this.windowCap)
    assert(payAmount <= this.perTxCap)
    assert(extractAmount(this.txPreimage) - contSats - payAmount <= this.maxFee + 1n)
    this.addOutput(contSats, this.destPkh, this.ownerPkh, this.perTxCap, this.windowCap, this.windowBlocks, this.maxFee, this.spentInWindow, this.windowStart)
    this.addRawOutput(payAmount, cat(toByteString('76a914'), cat(this.destPkh, toByteString('88ac'))))
  }
}
