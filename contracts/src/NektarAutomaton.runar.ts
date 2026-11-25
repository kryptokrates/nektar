// NektarAutomaton — AUT-005 full mode. Stateful Rúnar contract holding q of a
// deterministic finite automaton; each step spends the state UTXO and enforces
// δ on-chain (AUT-007: depth-4 Merkle membership over 24B transition leaves —
// no nested IF over the table itself).
//   step: owner sig + not halted + from==q + Merkle proof of (from‖sym‖to) vs
//         deltaRoot + continuation (q=to, n+1, lastOut=outHash).
//   halt: owner sig + not halted + q ∈ F (Merkle path vs finalRoot) → continuation
//         with halted=1; no further step is possible (the record is final).
// Both methods continue the state on purpose: in runar 0.4.6 a terminal
// method (no addOutput) does not parse state from the preimage and reads
// mutable fields from the constructor slots — i.e. the deploy-time q.
import {
  SmartContract, StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat, len,
  type PubKey, type Sig, type Sha256, type ByteString
} from 'runar-lang'

export class NektarAutomaton extends StatefulSmartContract {
  q: bigint
  deltaRoot: Sha256 // Merkle root over 24B leaf entries num2bin8(from)‖num2bin8(sym)‖num2bin8(to)
  ownerPkh: ByteString // 20B = hash160(owner pubkey)
  n: bigint // step counter
  lastOut: ByteString // 32B hash of last output event, empty at genesis
  finalRoot: Sha256 // Merkle root over 8B leaves num2bin8(q) for q ∈ F, padded to 16
  halted: bigint // 0 = running, 1 = halted in a final state

  constructor (q: bigint, deltaRoot: Sha256, ownerPkh: ByteString, n: bigint, lastOut: ByteString, finalRoot: Sha256, halted: bigint) {
    super(q, deltaRoot, ownerPkh, n, lastOut, finalRoot, halted)
    this.q = q
    this.deltaRoot = deltaRoot
    this.ownerPkh = ownerPkh
    this.n = n
    this.lastOut = lastOut
    this.finalRoot = finalRoot
    this.halted = halted
  }

  // fold a depth-4 Merkle path; dir=1 → sibling on the left, dir=0 → right
  private fold (h: ByteString, sib: ByteString, dir: bigint): ByteString {
    return dir == 1n ? hash256(cat(sib, h)) : hash256(cat(h, sib))
  }

  public step (sig: Sig, pubKey: PubKey,
    from: bigint, sym: bigint, to: bigint, outHash: ByteString,
    sib0: ByteString, sib1: ByteString, sib2: ByteString, sib3: ByteString,
    dir: bigint) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.halted == 0n)
    assert(len(outHash) == 32n)
    assert(from == this.q)
    // leaf = num2bin8(from)‖num2bin8(sym)‖num2bin8(to); fold 4 levels.
    // Distinct consts on purpose: runar 0.4.6 miscompiles a `let` reassigned
    // from a private-method result (the final read becomes an empty push).
    const h0: ByteString = cat(num2bin(from, 8n), cat(num2bin(sym, 8n), num2bin(to, 8n)))
    const h1: ByteString = this.fold(h0, sib0, dir % 2n)
    const h2: ByteString = this.fold(h1, sib1, (dir / 2n) % 2n)
    const h3: ByteString = this.fold(h2, sib2, (dir / 4n) % 2n)
    const h4: ByteString = this.fold(h3, sib3, (dir / 8n) % 2n)
    assert(h4 == this.deltaRoot)
    this.addOutput(1n, to, this.deltaRoot, this.ownerPkh, this.n + 1n, outHash, this.finalRoot, 0n)
  }

  // q ∈ F by a depth-4 Merkle path of the 8-byte leaf num2bin(q) against finalRoot
  public halt (sig: Sig, pubKey: PubKey,
    sib0: ByteString, sib1: ByteString, sib2: ByteString, sib3: ByteString, dir: bigint) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.halted == 0n)
    const f0: ByteString = num2bin(this.q, 8n)
    const f1: ByteString = this.fold(f0, sib0, dir % 2n)
    const f2: ByteString = this.fold(f1, sib1, (dir / 2n) % 2n)
    const f3: ByteString = this.fold(f2, sib2, (dir / 4n) % 2n)
    const f4: ByteString = this.fold(f3, sib3, (dir / 8n) % 2n)
    assert(f4 == this.finalRoot)
    this.addOutput(1n, this.q, this.deltaRoot, this.ownerPkh, this.n + 1n, this.lastOut, this.finalRoot, 1n)
  }
}
