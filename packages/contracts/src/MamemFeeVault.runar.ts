// MamemFeeVault — spend caps enforced by the locking script itself (V1,
// GOAL-v7). Unlike the app-level SpendGuard, these limits are consensus
// rules: no code path (and no stolen-but-signing key) can spend the vault
// anywhere except `destPkh`, above `perTxCap`, above the rolling window cap
// (per `windowBlocks` via nLocktime), or with a fee above `maxFee`.
//
// Manual covenant (OP_PUSH_TX): the contract takes txPreimage as an explicit
// param and verifies ALL outputs by hand — the compiler's auto change output
// can't be constrained, so we don't use StatefulSmartContract machinery.
// Mutable state = 16 bytes appended after OP_RETURN inside the locking
// script: spentInWindow(8LE) ‖ windowStart(8LE), read via scriptCode.
//   Outputs (exactly three, order fixed):
//     [0] self-continuation  contSats → this script with updated state
//     [1] payment            payAmount → P2PKH(destPkh)
//     [2] change             changeSats → P2PKH(ownerPkh)  (operator only)
import {
  SmartContract,
  assert, checkSig, checkPreimage, extractOutputHash, extractScriptCode,
  extractAmount, extractLocktime,
  sha256, hash160, hash256, cat, substr, len, num2bin, bin2num,
  type PubKey, type Sig, type SigHashPreimage, type Sha256, type ByteString
} from 'runar-lang'

export class MamemFeeVault extends SmartContract {
  readonly destPkh: ByteString
  readonly ownerPkh: ByteString
  readonly perTxCap: bigint
  readonly windowCap: bigint
  readonly windowBlocks: bigint
  readonly maxFee: bigint

  constructor (destPkh: ByteString, ownerPkh: ByteString, perTxCap: bigint, windowCap: bigint, windowBlocks: bigint, maxFee: bigint) {
    super()
    this.destPkh = destPkh
    this.ownerPkh = ownerPkh
    this.perTxCap = perTxCap
    this.windowCap = windowCap
    this.windowBlocks = windowBlocks
    this.maxFee = maxFee
  }

  private p2pkh (pkh: ByteString): ByteString {
    return cat(num2bin(0x76a9n, 2n), cat(pkh, num2bin(0x88acn, 2n)))
  }

  private out (sats: bigint, script: ByteString): ByteString {
    const sl = len(script)
    const slB = sl < 253n ? num2bin(sl, 1n) : cat(num2bin(0xfdn, 1n), num2bin(sl, 2n))
    return cat(num2bin(sats, 8n), cat(slB, script))
  }

  public spend (txPreimage: SigHashPreimage, sig: Sig, pubKey: PubKey,
    payAmount: bigint, contSats: bigint, changeSats: bigint) {
    assert(checkPreimage(txPreimage))
    assert(hash160(pubKey) === this.ownerPkh)
    assert(checkSig(sig, pubKey))

    // mutable state = last 16 bytes of scriptCode: spent(8)‖windowStart(8)
    const code = extractScriptCode(txPreimage)
    const cl = len(code)
    const spentInWindow = bin2num(substr(code, cl - 16n, 8n))
    const windowStart = bin2num(substr(code, cl - 8n, 8n))

    const lt = extractLocktime(txPreimage)
    let newSpent = spentInWindow + payAmount
    let newStart = windowStart
    if (lt >= windowStart + this.windowBlocks) {
      newSpent = payAmount
      newStart = lt
    }
    assert(newSpent <= this.windowCap)
    assert(payAmount <= this.perTxCap)

    const fee = extractAmount(txPreimage) - payAmount - contSats - changeSats
    assert(fee <= this.maxFee)

    const newCode = cat(substr(code, 0n, cl - 16n), cat(num2bin(newSpent, 8n), num2bin(newStart, 8n)))
    const outs = cat(
      this.out(contSats, newCode),
      cat(this.out(payAmount, this.p2pkh(this.destPkh)), this.out(changeSats, this.p2pkh(this.ownerPkh)))
    )
    assert(hash256(outs) === extractOutputHash(txPreimage))
  }
}
