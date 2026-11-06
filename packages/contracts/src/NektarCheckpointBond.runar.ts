import { StatefulSmartContract, assert, hash160, checkSig, cat, toByteString, type Sig, type PubKey, type ByteString, extractLocktime, extractAmount } from 'runar-lang'

// NektarCheckpointBond — GOAL-v7 V2 (idiomatic StatefulSmartContract):
// the checkpoint chain lives IN the contract state, so conflicting on-chain
// checkpoints are impossible by construction (prevention, not detection —
// closes VER-007's on-chain side).
//   commit: n += 1, lastRoot = newRoot, deadline = nLockTime + epochLen,
//           continuation carries the bond — MUST land before `deadline`
//   seize:  no signature — once deadline passes ANYONE may claim the bond;
//           "no checkpoint in the window" is provable by consensus nLockTime
//           alone (§12 ϕ→_tψ SLA), no checkDataSig needed: evidence-based
//           slashing remains deferred upstream, deadline-slashing is not.
//   close:  terminal — pays the bond to operatorPkh after release height
// The residual (injected change + miner fee) is bounded by maxFee + 1 sat
// on all methods — change-leakage is a consensus-bounded no-op.
export class NektarCheckpointBond extends StatefulSmartContract {
  operatorPkh: ByteString
  bondReleaseHeight: bigint
  maxFee: bigint
  epochLen: bigint
  n: bigint
  lastRoot: ByteString
  deadline: bigint
  seized: bigint

  constructor (operatorPkh: ByteString, bondReleaseHeight: bigint, maxFee: bigint, epochLen: bigint) {
    super(operatorPkh, bondReleaseHeight, maxFee, epochLen)
    this.operatorPkh = operatorPkh
    this.bondReleaseHeight = bondReleaseHeight
    this.maxFee = maxFee
    this.epochLen = epochLen
    this.n = 0n
    this.lastRoot = toByteString('')
    this.deadline = bondReleaseHeight // the first commit must land before release
    this.seized = 0n
  }

  private p2pkh (pkh: ByteString): ByteString {
    // see NektarFeeVault — buildP2PKH is declared but not compilable in 0.4.6
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  /** append the next checkpoint — only n+1 ever continues, so the on-chain
   *  history cannot fork. Must land BEFORE the current deadline: nLockTime
   *  is consensus-enforced (cannot exceed real height), so the operator
   *  cannot stretch the SLA window by lying about time. Committing early
   *  only tightens the operator's own window — self-harm, never a cheat.
   *  newDeadline is caller-computed (vault arg pattern): runar 0.4.6
   *  miscompiles builtin-call results assigned into state slots, so the
   *  deadline arithmetic happens off-chain and is only CHECKED here. */
  public commit (sig: Sig, pubKey: PubKey, newRoot: ByteString, bondSats: bigint, newDeadline: bigint) {
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    assert(this.seized == 0n)
    assert(extractLocktime(this.txPreimage) < this.deadline)
    assert(newDeadline - extractLocktime(this.txPreimage) == this.epochLen)
    this.n += 1n
    this.lastRoot = newRoot
    this.deadline = newDeadline
    assert(extractAmount(this.txPreimage) - bondSats <= this.maxFee + 1n)
    this.addOutput(bondSats, this.operatorPkh, this.bondReleaseHeight, this.maxFee, this.epochLen, this.n, this.lastRoot, this.deadline, this.seized)
  }

  /** permissionless slashing: anyone may claim a bond whose deadline passed
   *  without a commit. `payTo` is the full locking script the seizer chooses.
   *  The 1-sat husk keeps the continuation (runar 0.4.6 parses state only
   *  when addOutput is present) and is inert — commits require seized=0. */
  public seize (payTo: ByteString, payoutSats: bigint) {
    assert(this.seized == 0n)
    assert(extractLocktime(this.txPreimage) >= this.deadline)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.seized = 1n
    this.addRawOutput(payoutSats, payTo)
    this.addOutput(1n, this.operatorPkh, this.bondReleaseHeight, this.maxFee, this.epochLen, this.n, this.lastRoot, this.deadline, this.seized)
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
