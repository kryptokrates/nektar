// MamemCheckpointBond — GOAL-v7 V2: the checkpoint chain lives IN the
// contract state, so conflicting on-chain checkpoints are impossible by
// construction (prevention, not detection — closes VER-007's on-chain side).
// Manual covenant like MamemFeeVault: state = 40 bytes after OP_RETURN in
// scriptCode: n(8LE) ‖ lastRoot(32B).
//   commit:  outputs = [continuation(bondSats), change→operatorPkh]
//   close:   outputs = [pay→operatorPkh, change→operatorPkh], locktime-gated
// Punitive slashing of off-chain signed evidence needs checkDataSig — not in
// runar-lang 0.4.6 (documented deferral, upstream request filed).
import {
  SmartContract,
  assert, checkSig, checkPreimage, extractOutputHash, extractScriptCode,
  extractAmount, extractLocktime,
  hash160, hash256, cat, substr, len, num2bin, bin2num, toByteString,
  type PubKey, type Sig, type SigHashPreimage, type Sha256, type ByteString
} from 'runar-lang'

export class MamemCheckpointBond extends SmartContract {
  readonly operatorPkh: ByteString
  readonly bondReleaseHeight: bigint
  readonly maxFee: bigint

  constructor (operatorPkh: ByteString, bondReleaseHeight: bigint, maxFee: bigint) {
    super()
    this.operatorPkh = operatorPkh
    this.bondReleaseHeight = bondReleaseHeight
    this.maxFee = maxFee
  }

  private p2pkh (pkh: ByteString): ByteString {
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  private out (sats: bigint, script: ByteString): ByteString {
    // output serialization = value(8LE) + varint(scriptLen) + script
    const sl = len(script)
    const slB = sl < 253n ? num2bin(sl, 1n) : cat(toByteString('fd'), num2bin(sl, 2n))
    return cat(num2bin(sats, 8n), cat(slB, script))
  }

  /** append the next checkpoint — only n+1 ever continues, so the on-chain
   *  history cannot fork. */
  public commit (txPreimage: SigHashPreimage, sig: Sig, pubKey: PubKey,
    newRoot: Sha256, bondSats: bigint, changeSats: bigint) {
    assert(checkPreimage(txPreimage))
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    const code = extractScriptCode(txPreimage)
    const cl = len(code)
    const n = bin2num(substr(code, cl - 40n, 8n))
    const fee = extractAmount(txPreimage) - bondSats - changeSats
    assert(fee <= this.maxFee)
    const newCode = cat(substr(code, 0n, cl - 40n), cat(num2bin(n + 1n, 8n), newRoot))
    const outs = cat(this.out(bondSats, newCode), this.out(changeSats, this.p2pkh(this.operatorPkh)))
    assert(hash256(outs) === extractOutputHash(txPreimage))
  }

  /** release the bond to the operator once the release height is reached. */
  public close (txPreimage: SigHashPreimage, sig: Sig, pubKey: PubKey, changeSats: bigint) {
    assert(checkPreimage(txPreimage))
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    assert(extractLocktime(txPreimage) >= this.bondReleaseHeight)
    const fee = extractAmount(txPreimage) - changeSats
    assert(fee <= this.maxFee)
    const outs = this.out(changeSats, this.p2pkh(this.operatorPkh))
    assert(hash256(outs) === extractOutputHash(txPreimage))
  }
}
