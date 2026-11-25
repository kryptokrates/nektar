// NektarZkClaim — GOAL-v9 §4.5: hardened optimistic ZK verdict claim+bond covenant.
// Security invariants enforced under consensus:
//   - F-01: responseDeadline uses the vault-arg pattern (newDeadline - lt === window)
//           so runar 0.4.6 emits OP_SUB + OP_NUMEQUAL (never OP_CAT).
//   - F-02: every locktime check enforces extractSequence < 0xffffffff AND lt < 500_000_000.
//   - F-03: proveStage pins stageCodeHash (hash256(stageCode) === this.stageCodeHash)
//           and len(stageOutpoint) === 36n.
//   - F-04: explicit turn states (0=open, 1=awaiting_reveal [prover], 2=awaiting_bisect
//           [challenger], 3=awaiting_leaf [prover]); claimTimeout sweeps on status==2,
//           slashTimeout sweeps on status==1||3; reveal resets responseDeadline and
//           rejects zero midCommit.
//   - F-19: fixed pushdata bounds before num2bin(len(x), 1n).
//   - F-20: challenge() locks 2*bondSats (matching challenger bond).
import {
  StatefulSmartContract,
  assert, checkSig, hash160, hash256, cat, toByteString, len, num2bin,
  extractLocktime, extractSequence, extractAmount, extractHashPrevouts, extractOutpoint,
  type Sig, type PubKey, type ByteString, type Sha256
} from 'runar-lang'

export class NektarZkClaim extends StatefulSmartContract {
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
  window: bigint
  stages: bigint
  verdictCode: ByteString
  anchorOutpoint: ByteString
  stageCodeHash: Sha256

  constructor (status: bigint, receiptHash: Sha256, journalRoot: ByteString, journalSize: bigint, queryId: ByteString, bondSats: bigint, deadline: bigint, responseDeadline: bigint, challengerPkh: ByteString, bisectLo: bigint, bisectHi: bigint, midCommit: Sha256, proverPkh: ByteString, maxFee: bigint, window: bigint, stages: bigint, verdictCode: ByteString, anchorOutpoint: ByteString, stageCodeHash: Sha256) {
    super(status, receiptHash, journalRoot, journalSize, queryId, bondSats, deadline, responseDeadline, challengerPkh, bisectLo, bisectHi, midCommit, proverPkh, maxFee, window, stages, verdictCode, anchorOutpoint, stageCodeHash)
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
    this.anchorOutpoint = anchorOutpoint
    this.stageCodeHash = stageCodeHash
  }

  public settle (sig: Sig, pubKey: PubKey, prevouts: ByteString, payoutSats: bigint, verdictSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 0n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt >= this.deadline)
    assert(len(this.anchorOutpoint) === 0n)
    assert(hash256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === extractOutpoint(this.txPreimage))
    assert(len(this.proverPkh) === 20n)
    assert(len(this.queryId) <= 75n)
    assert(len(this.journalRoot) === 32n)
    assert(len(this.receiptHash) === 32n)
    assert(verdictSats >= 1n && payoutSats >= 1n)
    const e1 = cat(num2bin(len(this.proverPkh), 1n), this.proverPkh)
    const e2 = cat(num2bin(len(this.queryId), 1n), this.queryId)
    const e3 = cat(num2bin(len(this.journalRoot), 1n), this.journalRoot)
    const e4 = cat(num2bin(len(this.receiptHash), 1n), this.receiptHash)
    const vstate = cat(e4, cat(e3, cat(e2, e1)))
    this.addRawOutput(verdictSats, cat(this.verdictCode, cat(toByteString('6a'), vstate)))
    assert(extractAmount(this.txPreimage) - payoutSats - verdictSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }

  public settleAnchored (sig: Sig, pubKey: PubKey, prevouts: ByteString, anchorSats: bigint, anchorScript: ByteString, payoutSats: bigint, verdictSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 0n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt >= this.deadline)
    assert(len(this.anchorOutpoint) === 36n)
    assert(hash256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === cat(extractOutpoint(this.txPreimage), this.anchorOutpoint))
    assert(len(this.proverPkh) === 20n)
    assert(len(this.queryId) <= 75n)
    assert(len(this.journalRoot) === 32n)
    assert(len(this.receiptHash) === 32n)
    assert(anchorSats >= 1n && verdictSats >= 1n && payoutSats >= 1n)
    this.addRawOutput(anchorSats, anchorScript)
    const e1 = cat(num2bin(len(this.proverPkh), 1n), this.proverPkh)
    const e2 = cat(num2bin(len(this.queryId), 1n), this.queryId)
    const e3 = cat(num2bin(len(this.journalRoot), 1n), this.journalRoot)
    const e4 = cat(num2bin(len(this.receiptHash), 1n), this.receiptHash)
    const vstate = cat(e4, cat(e3, cat(e2, e1)))
    this.addRawOutput(verdictSats, cat(this.verdictCode, cat(toByteString('6a'), vstate)))
    assert(extractAmount(this.txPreimage) - payoutSats - verdictSats - anchorSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }

  public challenge (sig: Sig, pubKey: PubKey, newDeadline: bigint) {
    assert(checkSig(sig, pubKey))
    assert(this.status === 0n)
    assert(this.stages >= 1n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt < this.deadline)
    assert(newDeadline - lt === this.window)
    assert(newDeadline < 500000000n)
    this.status = this.stages > 1n ? 1n : 3n
    this.challengerPkh = hash160(pubKey)
    this.bisectLo = 0n
    this.bisectHi = this.stages
    this.responseDeadline = newDeadline
    // Challenger must match prover's bondSats (total 2 * bondSats locked in continuation).
    this.addOutput(this.bondSats * 2n, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit, this.proverPkh, this.maxFee, this.window, this.stages, this.verdictCode, this.anchorOutpoint, this.stageCodeHash)
  }

  public reveal (sig: Sig, pubKey: PubKey, mid: Sha256, newDeadline: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 1n)
    assert(this.bisectHi - this.bisectLo > 1n)
    assert(mid !== toByteString('0000000000000000000000000000000000000000000000000000000000000000'))
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(newDeadline - lt === this.window)
    assert(newDeadline < 500000000n)
    this.status = 2n
    this.midCommit = mid
    this.responseDeadline = newDeadline
    this.addOutput(this.bondSats * 2n, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit, this.proverPkh, this.maxFee, this.window, this.stages, this.verdictCode, this.anchorOutpoint, this.stageCodeHash)
  }

  public bisect (sig: Sig, pubKey: PubKey, takeUpper: bigint, newDeadline: bigint) {
    assert(hash160(pubKey) === this.challengerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    assert(this.midCommit !== toByteString('0000000000000000000000000000000000000000000000000000000000000000'))
    assert(takeUpper === 0n || takeUpper === 1n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(newDeadline - lt === this.window)
    assert(newDeadline < 500000000n)
    const lo0 = this.bisectLo
    const hi0 = this.bisectHi
    const mid = (lo0 + hi0) / 2n
    const rem = (mid - lo0) + takeUpper * (hi0 + lo0 - 2n * mid)
    this.bisectLo = lo0 + takeUpper * (mid - lo0)
    this.bisectHi = mid + takeUpper * (hi0 - mid)
    this.status = rem > 1n ? 1n : 3n
    this.midCommit = '0000000000000000000000000000000000000000000000000000000000000000' as Sha256
    this.responseDeadline = newDeadline
    this.addOutput(this.bondSats * 2n, this.status, this.receiptHash, this.journalRoot, this.journalSize, this.queryId, this.bondSats, this.deadline, this.responseDeadline, this.challengerPkh, this.bisectLo, this.bisectHi, this.midCommit, this.proverPkh, this.maxFee, this.window, this.stages, this.verdictCode, this.anchorOutpoint, this.stageCodeHash)
  }

  public proveStage (sig: Sig, pubKey: PubKey, prevouts: ByteString, stageOutpoint: ByteString, payoutSats: bigint, stageSats: bigint, stageCode: ByteString, stageState: ByteString) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 3n)
    assert(this.bisectHi - this.bisectLo <= 1n)
    assert(len(stageOutpoint) === 36n)
    assert(hash256(stageCode) === this.stageCodeHash)
    assert(hash256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === cat(extractOutpoint(this.txPreimage), stageOutpoint))
    assert(payoutSats >= 1n && stageSats >= 1n)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(stageSats, cat(stageCode, cat(toByteString('6a'), stageState)))
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }

  /** challenger went silent on their bisect turn (status === 2n) → prover sweeps both bonds. */
  public claimTimeout (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 2n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt >= this.responseDeadline)
    assert(payoutSats >= 1n)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }

  /** prover went silent on their reveal (status === 1n) or proveStage (status === 3n) turn → challenger sweeps both bonds. */
  public slashTimeout (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.challengerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.status === 1n || this.status === 3n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt >= this.responseDeadline)
    assert(payoutSats >= 1n)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.challengerPkh, toByteString('88ac'))))
  }
}
