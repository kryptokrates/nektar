// NektarGrothClaim — GOAL-v9 §0e: hardened VALIDITY-rail claim covenant.
// Enforces:
//   - Exact 2-input co-spend pinning verifierOutpoint (36B) via hashPrevouts.
//   - F-19: strict length bounds on proverPkh/queryId/journalRoot/receiptHash before 1B num2bin.
//   - F-25: no unconstrained caller verifierSats on RHS of maxFee check; verdictSats >= 1 && payoutSats >= 1.
//   - F-18: paired with Go VerifyGrothClaimSettlement checking verifier VK script hash & doc-push == journalRoot.
import {
  StatefulSmartContract,
  assert, checkSig, hash160, hash256, cat, toByteString, len, num2bin,
  extractHashPrevouts, extractOutpoint, extractAmount,
  type Sig, type PubKey, type ByteString, type Sha256
} from 'runar-lang'

export class NektarGrothClaim extends StatefulSmartContract {
  receiptHash: Sha256
  journalRoot: ByteString
  journalSize: bigint
  queryId: ByteString
  proverPkh: ByteString
  verifierOutpoint: ByteString
  maxFee: bigint
  verdictCode: ByteString

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

  public settleValid (sig: Sig, pubKey: PubKey, prevouts: ByteString, verdictSats: bigint, payoutSats: bigint) {
    assert(hash160(pubKey) === this.proverPkh)
    assert(checkSig(sig, pubKey))
    assert(len(this.verifierOutpoint) === 36n)
    assert(hash256(prevouts) === extractHashPrevouts(this.txPreimage))
    assert(prevouts === cat(extractOutpoint(this.txPreimage), this.verifierOutpoint))
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
}
