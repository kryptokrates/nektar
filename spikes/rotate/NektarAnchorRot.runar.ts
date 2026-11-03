// NektarAnchorRot — rotation via ownerPkh (Ripemd160) in mutable state:
// avoids PubKey-in-state serialization entirely.
//   shard = hash160(genesisOwner) — immutable, embedded in code.
//   append(sig, pubKey, newR, newSize):
//     assert(hash160(pubKey) == this.ownerPkh); assert(checkSig(sig, pubKey))
//   rotate(sig, pubKey, newOwnerPkh): same checks, continuation carries
//     newOwnerPkh; all other state unchanged.
import {
  StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat,
  type PubKey, type Sig, type Sha256, type ByteString
} from 'runar-lang'

export class NektarAnchorRot extends StatefulSmartContract {
  readonly genesisOwner: PubKey
  // mutable state — addOutput arg order must match declaration order
  n: bigint
  size: bigint
  R: Sha256
  Rp: Sha256
  poseidonRoot: ByteString
  tagRprime: ByteString
  ownerPkh: ByteString // 20B, = hash160(current owner pubkey)
  constructor (
    genesisOwner: PubKey, ownerPkh: ByteString,
    n: bigint, size: bigint, R: Sha256, Rp: Sha256,
    poseidonRoot: ByteString, tagRprime: ByteString
  ) {
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

  public append (sig: Sig, pubKey: PubKey, newR: Sha256, newSize: bigint) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const newRp: Sha256 = hash256(cat(this.tagRprime, cat(this.Rp, cat(newR, num2bin(newSize, 8n)))))
    this.addOutput(1n, this.n + 1n, newSize, newR, newRp, this.poseidonRoot, this.tagRprime, this.ownerPkh)
  }

  public rotate (sig: Sig, pubKey: PubKey, newOwnerPkh: ByteString) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    this.addOutput(1n, this.n, this.size, this.R, this.Rp, this.poseidonRoot, this.tagRprime, newOwnerPkh)
  }
}
