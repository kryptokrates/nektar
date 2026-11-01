// MamemAnchor — stateful Rúnar contract enforcing the mamem successor rule
// on-chain (OP_PUSH_TX continuation), v2 with key rotation:
//   shard = hash160(genesisOwner) — immutable, embedded in code.
//   append(sig, pubKey, newR, newSize, newPoseidonRoot):
//     hash160(pubKey) == ownerPkh AND checkSig(sig, pubKey) AND
//     len(newPoseidonRoot) ∈ {0, 32} AND continuation carries n+1, size=newSize,
//     R=newR, Rp' = hash256(TAG‖Rp‖newR‖size_le8), poseidonRoot=newPoseidonRoot,
//     same ownerPkh.
//     Empty poseidonRoot = legacy/opt-out batch without a ZK companion tree;
//     verifyMembershipAnchored rejects such anchors with 'no poseidonRoot'.
//   rotate(sig, pubKey, newOwnerPkh):
//     same checks; continuation changes ONLY ownerPkh → newOwnerPkh.
import {
  SmartContract, StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat, len,
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
  poseidonRoot: ByteString // reserved, empty for now
  tagRprime: ByteString
  ownerPkh: ByteString // 20B = hash160(current owner pubkey)

  constructor (genesisOwner: PubKey, ownerPkh: ByteString, n: bigint, size: bigint, R: Sha256, Rp: Sha256, poseidonRoot: ByteString, tagRprime: ByteString) {
    super(genesisOwner, n, size, R, Rp, poseidonRoot, tagRprime, ownerPkh)
    this.genesisOwner = genesisOwner
    this.ownerPkh = ownerPkh
    this.n = n
    this.size = size
    this.R = R
    this.Rp = Rp
    this.poseidonRoot = poseidonRoot
    this.tagRprime = tagRprime
  }

  public append (sig: Sig, pubKey: PubKey, newR: Sha256, newSize: bigint, newPoseidonRoot: ByteString) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    assert(len(newPoseidonRoot) == 32n || len(newPoseidonRoot) == 0n)
    const newRp: Sha256 = hash256(cat(this.tagRprime, cat(this.Rp, cat(newR, num2bin(newSize, 8n)))))
    this.addOutput(1n, this.genesisOwner, this.n + 1n, newSize, newR, newRp, newPoseidonRoot, this.tagRprime, this.ownerPkh)
  }

  public rotate (sig: Sig, pubKey: PubKey, newOwnerPkh: ByteString) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    this.addOutput(1n, this.genesisOwner, this.n, this.size, this.R, this.Rp, this.poseidonRoot, this.tagRprime, newOwnerPkh)
  }
}
