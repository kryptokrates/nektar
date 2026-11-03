// MamemGrothClaim — GOAL-v9 §0e: VALIDITY-rail claim covenant.
// Unlike MamemZkClaim (optimistic: bond + watcher window + bisection), this
// claim can ONLY settle by co-spending a pinned Groth16-verifier UTXO in
// the same tx. The verifier's locking script is the nChain-zkscript
// generated Groth16 verifier — when input1 executes, the proof is checked
// under consensus. No proof that verifies → no spend → no settlement.
// Watcher-free: settlement validity does not depend on anyone being awake.
//
//   settleValid pins prevouts == ourOutpoint ‖ verifierOutpoint via
//   hashPrevouts (same technique as MamemZkClaim.proveStage), so input1 IS
//   the committed verifier UTXO and nothing else can be smuggled in.
//
// Statement binding: the verifier's locking script carries a doc-push
// prefix `<statement-root> <index> OP_2DROP` so the bound statement is
// publicly readable from the deployed verifier tx. journalRoot here is the
// semantic claim; consumers cross-check doc-push == journalRoot off-chain
// (the outpoint pin itself is consensus-enforced).
//
//   One-shot: the UTXO settles once; no status field, no continuation.
import {
  StatefulSmartContract,
  assert, checkSig, hash160, hash256, cat, toByteString, len, num2bin,
  extractHashPrevouts, extractOutpoint, extractAmount,
  type Sig, type PubKey, type ByteString, type Sha256
} from 'runar-lang'

export class MamemGrothClaim extends StatefulSmartContract {
  receiptHash: Sha256         // sha256 of the certified statement payload
  journalRoot: ByteString     // semantic statement (mirrors verifier doc-push)
  journalSize: bigint
  queryId: ByteString
  proverPkh: ByteString       // payout recipient; sig gates settlement timing
  verifierOutpoint: ByteString // 36B — pinned co-spend input1 (the judge)
  maxFee: bigint
  verdictCode: ByteString     // ClaimVerdict artifact codePart — emitted on settle

  constructor (receiptHash: Sha256, journalRoot: ByteString, journalSize: bigint, queryId: ByteString, proverPkh: ByteString, verifierOutpoint: ByteString, maxFee: bigint, verdictCode: ByteString) {
    super(receiptHash, journalRoot, journalSize, queryId, proverPkh, verifierOutpoint, maxFee, verdictCode)
    this.receiptHash = receiptHash
    this.journalRoot = journalRoot
    this.journalSize = journalSize
    this.queryId = queryId
    this.proverPkh = proverPkh
    this.verifierOutpoint = verifierOutpoint
    this.maxFee = maxFee
    this.verdictCode = verdictCode
  }

  /** VALIDITY settle: input1 must be the committed verifier UTXO. Its
   *  Groth16 locking script runs under consensus inside THIS tx — the tx
   *  (and hence the verdict) is only valid iff the proof verifies.
   *  Outputs: [ClaimVerdict UTXO @out0, payout @out1]; the verifier's
   *  sats may subsidize payout+fee (verifierSats). */
  public settleValid (sig: Sig, pubKey: PubKey, prevouts: ByteString, verifierSats: bigint, verdictSats: bigint, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(len(this.verifierOutpoint) === 36n)
    assert(hash256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === cat(extractOutpoint(this.txPreimage), this.verifierOutpoint))
    const e1 = cat(num2bin(len(this.proverPkh), 1n), this.proverPkh)
    const e2 = cat(num2bin(len(this.queryId), 1n), this.queryId)
    const e3 = cat(num2bin(len(this.journalRoot), 1n), this.journalRoot)
    const e4 = cat(num2bin(len(this.receiptHash), 1n), this.receiptHash)
    const vstate = cat(e4, cat(e3, cat(e2, e1)))
    this.addRawOutput(verdictSats, cat(this.verdictCode, cat(toByteString('6a'), vstate)))
    assert(extractAmount(this.txPreimage) - payoutSats - verdictSats <= this.maxFee + 1n + verifierSats)
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(this.proverPkh, toByteString('88ac'))))
  }
}
