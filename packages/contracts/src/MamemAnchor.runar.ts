// MamemAnchor — stateful Rúnar contract enforcing the mamem successor rule
// on-chain (OP_PUSH_TX continuation), v3: the state carries ONLY Wright's
// commitments (n, size, R_n, R'_n) plus ownership — no ZK companion root.
// ZK membership is proven over R_n itself (ADR-020, STARK over SHA-256).
//   shard = hash160(genesisOwner) — immutable, embedded in code.
//   append(sig, pubKey, newR, newSize):
//     hash160(pubKey) == ownerPkh AND checkSig(sig, pubKey) AND continuation
//     carries n+1, size=newSize, R=newR, Rp' = hash256(TAG‖Rp‖newR‖size_le8),
//     same ownerPkh.
//   rotate(sig, pubKey, newOwnerPkh):
//     same checks; continuation changes ONLY ownerPkh → newOwnerPkh.
import {
  SmartContract, StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat,
  type PubKey, type Sig, type Sha256, type ByteString
} from 'runar-lang'

export class MamemAnchor extends StatefulSmartContract {
  // mutable state (addOutput order below must match declaration order).
  // genesisOwner is never modified by any method — it stays in state and
  // shard = hash160(genesisOwner) is stable forever.
  genesisOwner: PubKey
  n: bigint
  size: bigint
  R: Sha256
  Rp: Sha256
  tagRprime: ByteString
  ownerPkh: ByteString // 20B = hash160(current owner pubkey)

  constructor (genesisOwner: PubKey, ownerPkh: ByteString, n: bigint, size: bigint, R: Sha256, Rp: Sha256, tagRprime: ByteString) {
    super(genesisOwner, n, size, R, Rp, tagRprime, ownerPkh)
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
    this.addOutput(1n, this.genesisOwner, this.n + 1n, newSize, newR, newRp, this.tagRprime, this.ownerPkh)
  }

  public rotate (sig: Sig, pubKey: PubKey, newOwnerPkh: ByteString) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    this.addOutput(1n, this.genesisOwner, this.n, this.size, this.R, this.Rp, this.tagRprime, newOwnerPkh)
  }
}
