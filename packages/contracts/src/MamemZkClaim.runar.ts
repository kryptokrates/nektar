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
  assert, checkSig, hash160, hash256, cat, toByteString, len, num2bin,
  extractLocktime, extractAmount, extractHashPrevouts, extractOutpoint,
  type Sig, type PubKey, type ByteString, type Sha256
} from 'runar-lang'

export class MamemZkClaim extends StatefulSmartContract {
  // state (addOutput arg order must match declaration order).
  // NOTE: proverPkh/maxFee/window/stages are protocol constants kept in
  // state — never assigned after construction. `readonly` constructor
  // slots splice args into the code section, which shifts the emitted
  // codeSeparatorIndex and breaks state deserialization in 0.4.6.
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
  proverPkh: ByteString
  maxFee: bigint
  window: bigint             // response window per bisection step
  stages: bigint             // stage count of the verified program
  verdictCode: ByteString    // ClaimVerdict artifact codePart — settle emits it

  constructor (status: bigint, receiptHash: Sha256, journalRoot: ByteString, journalSize: bigint, queryId: ByteString, bondSats: bigint, deadline: bigint, responseDeadline: bigint, challengerPkh: ByteString, bisectLo: bigint, bisectHi: bigint, midCommit: Sha256, proverPkh: ByteString, maxFee: bigint, window: bigint, stages: bigint, verdictCode: ByteString) {
    super(status, receiptHash, journalRoot, journalSize, queryId, bondSats, deadline, responseDeadline, challengerPkh, bisectLo, bisectHi, midCommit, proverPkh, maxFee, window, stages, verdictCode)
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
    this.verdictCode = verdictCode
  }

  /** HAPPY PATH: window expired unchallenged → ClaimVerdict covenant +
   *  bond back. Output[0] is a ClaimVerdict UTXO (verdictCode ‖ 6a ‖
   *  len-prefixed state) — consumers pin its outpoint as their input1;
   *  its own script enforces the payee payout when spent. */
  public settle (sig: Sig, pubKey: PubKey, payoutSats: bigint, verdictSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 0n)
    assert(extractLocktime(this.txPreimage) >= this.deadline)
    const e1 = cat(num2bin(len(this.proverPkh), 1n), this.proverPkh)
    const e2 = cat(num2bin(len(this.queryId), 1n), this.queryId)
    const e3 = cat(num2bin(len(this.journalRoot), 1n), this.journalRoot)
    const e4 = cat(num2bin(len(this.receiptHash), 1n), this.receiptHash)
    const vstate = cat(e4, cat(e3, cat(e2, e1)))
    this.addRawOutput(verdictSats, cat(this.verdictCode, cat(toByteString('6a'), vstate)))
    assert(extractAmount(this.txPreimage) - payoutSats - verdictSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
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
    this.addOutput(this.bondSats, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit, this.proverPkh, this.maxFee, this.window, this.stages, this.verdictCode)
  }

  /** prover posts the midpoint state commitment for the current range. */
  public reveal (sig: Sig, pubKey: PubKey, mid: Sha256) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 1n || this.status === 2n)
    assert(this.bisectHi - this.bisectLo > 1n)
    this.status = 2n
    this.midCommit = mid
    this.addOutput(this.bondSats, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit, this.proverPkh, this.maxFee, this.window, this.stages, this.verdictCode)
  }

  /** challenger picks the divergent half after a reveal. */
  public bisect (sig: Sig, pubKey: PubKey, takeUpper: bigint) {
    assert(hash160(pubKey) === this.challengerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(this.midCommit !== toByteString('0000000000000000000000000000000000000000000000000000000000000000'))
    // NOTE: temps referenced inside ternary branches are dropped by the
    // 0.4.6 stack scheduler ("t31 not found") — use pure arithmetic
    // selection instead (takeUpper ∈ {0,1}: x + s·(y−x)).
    assert(takeUpper === 0n || takeUpper === 1n)
    const lo0 = this.bisectLo
    const hi0 = this.bisectHi
    const mid = (lo0 + hi0) / 2n
    const rem = (mid - lo0) + takeUpper * (hi0 + lo0 - 2n * mid)
    this.bisectLo = lo0 + takeUpper * (mid - lo0)
    this.bisectHi = mid + takeUpper * (hi0 - mid)
    this.status = rem > 1n ? 1n : 2n
    this.midCommit = '0000000000000000000000000000000000000000000000000000000000000000' as Sha256
    this.responseDeadline = extractLocktime(this.txPreimage) + this.window
    this.addOutput(this.bondSats, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit, this.proverPkh, this.maxFee, this.window, this.stages, this.verdictCode)
  }

  /** LEAF: prover proves the single disputed stage by co-spending the
   *  stage UTXO in THIS tx — enforced by pinning prevouts:
   *  sha256(prevouts) == hashPrevouts(preimage) AND prevouts ==
   *  ourOutpoint ‖ stageOutpoint. The stage contract's script itself
   *  checks the disputed transition under consensus. */
  public proveStage (sig: Sig, pubKey: PubKey, prevouts: ByteString, stageOutpoint: ByteString, payoutSats: bigint, stageSats: bigint, stageOutScript: ByteString) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(this.bisectHi - this.bisectLo <= 1n)
    assert(hash256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === cat(extractOutpoint(this.txPreimage), stageOutpoint))
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n + stageSats)
    // The stage's continuation output goes FIRST (its addOutput emits @out0);
    // the stage's own addRawOutput(postSats, postScript) must re-emit our
    // payout as its post-output so both contracts enumerate the same tx
    // outputs for the hashOutputs check.
    this.addRawOutput(stageSats, stageOutScript)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }

  /** challenger went silent mid-game → prover sweeps. */
  public claimTimeout (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 1n)
    assert(extractLocktime(this.txPreimage) >= this.responseDeadline)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }

  /** prover went silent mid-game → challenger sweeps BOTH bonds. */
  public slashTimeout (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.challengerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(extractLocktime(this.txPreimage) >= this.responseDeadline)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.challengerPkh, toByteString('88ac'))))
  }
}
