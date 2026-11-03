// NektarAutomaton — spike 0.2: stateful Rúnar contract enforcing a committed
// transition table δ on-chain. Table = 4 entries (from‖sym‖to) passed as flat
// args and hashed in-contract (slice() isn't in Rúnar 0.4.6).
import {
  SmartContract, StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat,
  type PubKey, type Sig, type Sha256, type ByteString
} from 'runar-lang'

export class NektarAutomaton extends StatefulSmartContract {
  q: bigint
  deltaRoot: Sha256 // hash256 of the 4-triple table (num2bin8 each)
  ownerPkh: ByteString // 20B = hash160(owner pubkey)
  n: bigint // step counter

  constructor (q: bigint, deltaRoot: Sha256, ownerPkh: ByteString, n: bigint) {
    super(q, deltaRoot, ownerPkh, n)
    this.q = q
    this.deltaRoot = deltaRoot
    this.ownerPkh = ownerPkh
    this.n = n
  }

  // e0..e11 = the 4 triples flattened: (e0,e1,e2),(e3,e4,e5),(e6,e7,e8),(e9,e10,e11)
  public step (sig: Sig, pubKey: PubKey,
    e0: bigint, e1: bigint, e2: bigint, e3: bigint, e4: bigint, e5: bigint,
    e6: bigint, e7: bigint, e8: bigint, e9: bigint, e10: bigint, e11: bigint,
    from: bigint, sym: bigint, to: bigint) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    // rebuild the encoded table and check the commitment
    const table: ByteString = cat(
      cat(num2bin(e0, 8n), cat(num2bin(e1, 8n), num2bin(e2, 8n))),
      cat(cat(num2bin(e3, 8n), cat(num2bin(e4, 8n), num2bin(e5, 8n))),
          cat(cat(num2bin(e6, 8n), cat(num2bin(e7, 8n), num2bin(e8, 8n))),
              cat(num2bin(e9, 8n), cat(num2bin(e10, 8n), num2bin(e11, 8n))))))
    assert(hash256(table) == this.deltaRoot)
    assert(from == this.q)
    const found: boolean =
      (from == e0 && sym == e1 && to == e2) ||
      (from == e3 && sym == e4 && to == e5) ||
      (from == e6 && sym == e7 && to == e8) ||
      (from == e9 && sym == e10 && to == e11)
    assert(found)
    this.addOutput(1n, to, this.deltaRoot, this.ownerPkh, this.n + 1n)
  }
}
