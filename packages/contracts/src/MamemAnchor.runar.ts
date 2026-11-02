// MamemAnchor — stateful Rúnar contract enforcing the mamem successor rule
// on-chain (OP_PUSH_TX continuation), v3: the state carries ONLY Wright's
// commitments (n, size, R_n, R'_n) plus ownership — no ZK companion root.
// ZK membership is proven over R_n itself (ADR-020, STARK over SHA-256).
//   shard = hash160(genesisOwner) — immutable, embedded in code.
//   ANC-006 (GOAL-v6 S6.2): the state payload's FIRST push is the Metanet
//   flag 0x4D455441 ("META"). Flagged scripts decode alongside flag-absent
//   v3 anchors (decodeMamemAnchor accepts both artifacts).
//   append(sig, pubKey, newR, newSize):
//     hash160(pubKey) == ownerPkh AND checkSig(sig, pubKey) AND continuation
//     carries n+1, size=newSize, R=newR, Rp' = hash256(TAG‖Rp‖newR‖size_le8),
//     same ownerPkh.
//   rotate(sig, pubKey, newOwnerPkh):
//     same checks; continuation changes ONLY ownerPkh → newOwnerPkh.
import {
  SmartContract, StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat, len, toByteString,
  type PubKey, type Sig, type Sha256, type ByteString
} from 'runar-lang'

export class MamemAnchor extends StatefulSmartContract {
  // mutable state (addOutput order below must match declaration order).
  // metaFlag is the FIRST serialized push (the OP_RETURN data payload begins
  // with 0x4D455441 "META", the Metanet flag). genesisOwner is never modified
  // by any method — it stays in state and shard = hash160(genesisOwner) is
  // stable forever.
  metaFlag: ByteString
  genesisOwner: PubKey
  n: bigint
  size: bigint
  R: Sha256
  Rp: Sha256
  tagRprime: ByteString
  ownerPkh: ByteString // 20B = hash160(current owner pubkey)

  constructor (genesisOwner: PubKey, ownerPkh: ByteString, metaFlag: ByteString, n: bigint, size: bigint, R: Sha256, Rp: Sha256, tagRprime: ByteString) {
    super(metaFlag, genesisOwner, n, size, R, Rp, tagRprime, ownerPkh)
    this.metaFlag = metaFlag
    this.genesisOwner = genesisOwner
    this.ownerPkh = ownerPkh
    this.n = n
    this.size = size
    this.R = R
    this.Rp = Rp
    this.tagRprime = tagRprime
  }

  public append (sig: Sig, pubKey: PubKey, newR: Sha256, newSize: bigint) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const newRp: Sha256 = hash256(cat(this.tagRprime, cat(this.Rp, cat(newR, num2bin(newSize, 8n)))))
    this.addOutput(1n, this.metaFlag, this.genesisOwner, this.n + 1n, newSize, newR, newRp, this.tagRprime, this.ownerPkh)
  }

  public rotate (sig: Sig, pubKey: PubKey, newOwnerPkh: ByteString) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    this.addOutput(1n, this.metaFlag, this.genesisOwner, this.n, this.size, this.R, this.Rp, this.tagRprime, newOwnerPkh)
  }

  /** §4.5 anchor witness: co-spent as input1 of MamemZkClaim.settle.
   *  Re-emits the settle tx's [verdict, payout] outputs, constructing the
   *  ClaimVerdict script with THIS.R embedded at the journalRoot field —
   *  hashOutputs only validates if the emitted verdict carries our root.
   *  Consumes the anchor UTXO (terminal) — a continuing variant would need
   *  the claim to also enumerate the anchor continuation output. */
  public witnessSettle (sig: Sig, pubKey: PubKey, receiptHash: ByteString, queryId: ByteString, payeePkh: ByteString, verdictCode: ByteString, verdictSats: bigint, payoutSats: bigint, payoutPkh: ByteString) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const e1 = cat(num2bin(len(payeePkh), 1n), payeePkh)
    const e2 = cat(num2bin(len(queryId), 1n), queryId)
    const e3 = cat(num2bin(len(this.R), 1n), this.R)
    const e4 = cat(num2bin(len(receiptHash), 1n), receiptHash)
    const vstate = cat(e4, cat(e3, cat(e2, e1)))
    this.addRawOutput(verdictSats, cat(verdictCode, cat(toByteString('6a'), vstate)))
    this.addRawOutput(payoutSats, cat(toByteString('76a914'), cat(payoutPkh, toByteString('88ac'))))
  }
}
