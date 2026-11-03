import { StatefulSmartContract, assert, hash160, checkSig, cat, toByteString, type Sig, type PubKey, type ByteString, extractLocktime, extractAmount } from 'runar-lang'

// NektarCheckpointBond — GOAL-v7 V2 (idiomatic StatefulSmartContract):
// the checkpoint chain lives IN the contract state, so conflicting on-chain
// checkpoints are impossible by construction (prevention, not detection —
// closes VER-007's on-chain side).
//   commit: n += 1, lastRoot = newRoot, continuation carries the bond
//   close:  terminal — pays the bond to operatorPkh after release height
// The residual (injected change + miner fee) is bounded by maxFee + 1 sat
// on both methods — change-leakage is a consensus-bounded no-op.
// Punitive slashing of off-chain signed evidence needs checkDataSig — not in
// runar-lang 0.4.6 (documented deferral, upstream request filed).
export class NektarCheckpointBond extends StatefulSmartContract {
  operatorPkh: ByteString
  bondReleaseHeight: bigint
  maxFee: bigint
  n: bigint
  lastRoot: ByteString

  constructor (operatorPkh: ByteString, bondReleaseHeight: bigint, maxFee: bigint) {
    super(operatorPkh, bondReleaseHeight, maxFee)
    this.operatorPkh = operatorPkh
    this.bondReleaseHeight = bondReleaseHeight
    this.maxFee = maxFee
    this.n = 0n
    this.lastRoot = toByteString('')
  }

  private p2pkh (pkh: ByteString): ByteString {
    // see NektarFeeVault — buildP2PKH is declared but not compilable in 0.4.6
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  /** append the next checkpoint — only n+1 ever continues, so the on-chain
   *  history cannot fork. */
  public commit (sig: Sig, pubKey: PubKey, newRoot: ByteString, bondSats: bigint) {
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    this.n += 1n
    this.lastRoot = newRoot
    assert(extractAmount(this.txPreimage) - bondSats <= this.maxFee + 1n)
    this.addOutput(bondSats, this.operatorPkh, this.bondReleaseHeight, this.maxFee, this.n, this.lastRoot)
  }

  /** release the bond to the operator once the release height is reached. */
  public close (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    assert(extractLocktime(this.txPreimage) >= this.bondReleaseHeight)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, this.p2pkh(this.operatorPkh))
  }
}
