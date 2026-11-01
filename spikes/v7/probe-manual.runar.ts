// V0 spike probe 3: stateless SmartContract with an explicit SigHashPreimage
// param — fully manual output verification (sCrypt-style covenant).
import {
  SmartContract,
  assert, checkSig, checkPreimage, extractOutputHash, extractScriptCode, extractAmount, extractLocktime,
  sha256, hash160, hash256, cat, substr, len, num2bin, bin2num,
  type PubKey, type Sig, type SigHashPreimage, type Sha256, type ByteString
} from 'runar-lang'

export class ProbeManual extends SmartContract {
  readonly destPkh: ByteString
  readonly ownerPkh: ByteString
  readonly perTxCap: bigint
  readonly windowCap: bigint
  readonly windowBlocks: bigint
  readonly maxFee: bigint
  readonly stateOffset: bigint // where mutable {spentInWindow,windowStart} sits in scriptCode

  constructor (destPkh: ByteString, ownerPkh: ByteString, perTxCap: bigint, windowCap: bigint, windowBlocks: bigint, maxFee: bigint, stateOffset: bigint) {
    super()
    this.destPkh = destPkh
    this.ownerPkh = ownerPkh
    this.perTxCap = perTxCap
    this.windowCap = windowCap
    this.windowBlocks = windowBlocks
    this.maxFee = maxFee
    this.stateOffset = stateOffset
  }

  private payScript (pkh: ByteString): ByteString {
    return cat(num2bin(0x76a9n, 2n), cat(pkh, num2bin(0x88acn, 2n)))
  }

  private out (sats: bigint, script: ByteString): ByteString {
    const sl = len(script)
    const slB = sl < 253n ? num2bin(sl, 1n) : cat(num2bin(0xfdn, 1n), num2bin(sl, 2n))
    return cat(num2bin(sats, 8n), cat(slB, script))
  }

  public spend (txPreimage: SigHashPreimage, sig: Sig, pubKey: PubKey,
    payAmount: bigint, changeSats: bigint, spentInWindow: bigint, windowStart: bigint) {
    assert(checkPreimage(txPreimage))
    assert(hash160(pubKey) === this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const lt = extractLocktime(txPreimage)
    const rolled = lt >= windowStart + this.windowBlocks
    const newSpent = rolled ? payAmount : spentInWindow + payAmount
    assert(newSpent <= this.windowCap)
    assert(payAmount <= this.perTxCap)
    // reconstruct the locking script with the updated state (substr swap at stateOffset)
    const code = extractScriptCode(txPreimage)
    const newState = cat(num2bin(newSpent, 8n), num2bin(rolled ? lt : windowStart, 8n))
    const newCode = cat(substr(code, 0n, this.stateOffset), cat(newState, substr(code, this.stateOffset + 16n, len(code))))
    const fee = extractAmount(txPreimage) - payAmount - changeSats - 1n
    assert(fee <= this.maxFee)
    const outs = cat(
      this.out(1n, newCode),
      cat(this.out(payAmount, this.payScript(this.destPkh)), this.out(changeSats, this.payScript(this.ownerPkh)))
    )
    assert(hash256(outs) === extractOutputHash(txPreimage))
  }
}
