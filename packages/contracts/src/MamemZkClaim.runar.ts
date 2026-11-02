// MamemZkClaim — GOAL-v9 §4.5: optimistic ZK verdict claim+bond covenant.
// The prover locks a bond behind the claim "receiptHash verifies against
// (journalRoot,journalSize) at queryId". Nobody challenges before
// `deadline` → `settle` emits the verdict + returns the bond (happy
// path ≈ 2 txs, fractions of a cent). Anyone can `challenge` before the
// deadline → the claim enters a bisection dispute (status 1→2): the
// prover alternately `reveal`s a mid-state commitment and the challenger
// `bisect`s into the divergent half, isolating ONE disputed stage in
// ~log2(stages)≈5 rounds. At the leaf (hi−lo≤1) the prover must
// `proveStage` — co-spending the disputed `Poseidon2Stage` UTXO in the
// SAME tx, enforced on-chain via hashPrevouts; the stage contract's own
// script is the judge of the step (its asserts run under consensus).
// Timeouts break deadlocks: silent challenger → `claimTimeout` (prover
// sweeps), silent prover → `slashTimeout` (challenger sweeps both bonds).
//   status: 0=open, 1=disputed(awaiting reveal), 2=bisecting/leaf
//   STAGE-1b TODO: bind the leaf's stage-script template hash on-chain
//   (today the challenger inspects the co-spent stage UTXO off-chain;
//   the co-spend requirement itself is already enforced by Script).
import {
  StatefulSmartContract,
  assert, checkSig, hash160, sha256, cat, toByteString,
  extractLocktime, extractAmount, extractHashPrevouts, extractOutpoint,
  type Sig, type PubKey, type ByteString, type Sha256
} from 'runar-lang'

export class MamemZkClaim extends StatefulSmartContract {
  // mutable state (addOutput arg order must match declaration order)
  status: bigint
  receiptHash: Sha256
  journalRoot: ByteString
  journalSize: bigint
  queryId: ByteString
  bondSats: bigint
  deadline: bigint
  responseDeadline: bigint
  challengerPkh: ByteString
  bisectLo: bigint
  bisectHi: bigint
  midCommit: Sha256
  // constants (constructor-baked, not part of state)
  readonly proverPkh: ByteString
  readonly maxFee: bigint
  readonly window: bigint      // response window per bisection step
  readonly stages: bigint      // stage count of the verified program

  constructor (status: bigint, receiptHash: Sha256, journalRoot: ByteString, journalSize: bigint, queryId: ByteString, bondSats: bigint, deadline: bigint, responseDeadline: bigint, challengerPkh: ByteString, bisectLo: bigint, bisectHi: bigint, midCommit: Sha256, proverPkh: ByteString, maxFee: bigint, window: bigint, stages: bigint) {
    super(status, receiptHash, journalRoot, journalSize, queryId, bondSats, deadline, responseDeadline, challengerPkh, bisectLo, bisectHi, midCommit)
    this.status = status
    this.receiptHash = receiptHash
    this.journalRoot = journalRoot
    this.journalSize = journalSize
    this.queryId = queryId
    this.bondSats = bondSats
    this.deadline = deadline
    this.responseDeadline = responseDeadline
    this.challengerPkh = challengerPkh
    this.bisectLo = bisectLo
    this.bisectHi = bisectHi
    this.midCommit = midCommit
    this.proverPkh = proverPkh
    this.maxFee = maxFee
    this.window = window
    this.stages = stages
  }

  private p2pkh (pkh: ByteString): ByteString {
    // see MamemCheckpointBond — buildP2PKH is declared but not compilable in 0.4.6
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  /** HAPPY PATH: window expired unchallenged → verdict + bond back.
   *  Verdict output binds (receiptHash ‖ journalRoot ‖ queryId) on-chain
   *  — consumers reference the settled claim spend. */
  public settle (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 0n)
    assert(extractLocktime(this.txPreimage) >= this.deadline)
    this.addRawOutput(1n, cat(toByteString('6a'), cat(this.receiptHash, cat(this.journalRoot, this.queryId))))
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, this.p2pkh(this.proverPkh))
  }

  /** ANYONE challenges before the deadline (sig proves they control the
   *  key; their bond rides on the same UTXO economics — the challenge
   *  tx must fund the continuation). Starts the dispute. */
  public challenge (sig: Sig, pubKey: PubKey) {
    assert(checkSig(sig, pubKey))
    assert(this.status === 0n)
    assert(extractLocktime(this.txPreimage) < this.deadline)
    this.status = 1n
    this.challengerPkh = hash160(pubKey)
    this.bisectLo = 0n
    this.bisectHi = this.stages
    this.responseDeadline = extractLocktime(this.txPreimage) + this.window
    this.addOutput(this.bondSats, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit)
  }

  /** prover posts the midpoint state commitment for the current range. */
  public reveal (sig: Sig, pubKey: PubKey, mid: Sha256) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 1n || this.status === 2n)
    assert(this.bisectHi - this.bisectLo > 1n)
    this.status = 2n
    this.midCommit = mid
    this.addOutput(this.bondSats, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit)
  }

  /** challenger picks the divergent half after a reveal. */
  public bisect (sig: Sig, pubKey: PubKey, takeUpper: bigint) {
    assert(hash160(pubKey) === this.challengerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(this.midCommit !== toByteString('0000000000000000000000000000000000000000000000000000000000000000'))
    const mid = (this.bisectLo + this.bisectHi) / 2n
    if (takeUpper === 1n) { this.bisectLo = mid } else { this.bisectHi = mid }
    this.midCommit = '0000000000000000000000000000000000000000000000000000000000000000' as Sha256
    this.responseDeadline = extractLocktime(this.txPreimage) + this.window
    this.status = this.bisectHi - this.bisectLo > 1n ? 1n : 2n
    this.addOutput(this.bondSats, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit)
  }

  /** LEAF: prover proves the single disputed stage by co-spending the
   *  stage UTXO in THIS tx — enforced by pinning prevouts:
   *  sha256(prevouts) == hashPrevouts(preimage) AND prevouts ==
   *  ourOutpoint ‖ stageOutpoint. The stage contract's script itself
   *  checks the disputed transition under consensus. */
  public proveStage (sig: Sig, pubKey: PubKey, prevouts: ByteString, stageOutpoint: ByteString, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(this.bisectHi - this.bisectLo <= 1n)
    assert(sha256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === cat(extractOutpoint(this.txPreimage), stageOutpoint))
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, this.p2pkh(this.proverPkh))
  }

  /** challenger went silent mid-game → prover sweeps. */
  public claimTimeout (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 1n)
    assert(extractLocktime(this.txPreimage) >= this.responseDeadline)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, this.p2pkh(this.proverPkh))
  }

  /** prover went silent mid-game → challenger sweeps BOTH bonds. */
  public slashTimeout (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.challengerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(extractLocktime(this.txPreimage) >= this.responseDeadline)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, this.p2pkh(this.challengerPkh))
  }
}
