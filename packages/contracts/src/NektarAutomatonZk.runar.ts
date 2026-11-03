// NektarAutomatonZk — AUT-005 + optional Schnorr-PoK delegation gate.
// Same automaton logic as NektarAutomaton, but `stepZk` replaces the owner
// signature with a Schnorr proof of knowledge of the discrete log of the
// committed delegate point `delegateP`:
//   e = sha256(R ‖ delegateP ‖ num2bin8(n) ‖ num2bin8(to)) — binding the
//   proof to THIS step counter and successor state (no replay at another n).
//   assert sha256(...)=num2bin(e,32) ∧ ecMulGen(s) == ecAdd(R, ecMul(delegateP,e))
import {
  SmartContract, StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, cat, len, sha256,
  ecMulGen, ecAdd, ecMul, ecOnCurve,
  type PubKey, type Sig, type Sha256, type ByteString, type Point
} from 'runar-lang'

export class NektarAutomatonZk extends StatefulSmartContract {
  q: bigint
  deltaRoot: Sha256
  ownerPkh: ByteString
  n: bigint
  lastOut: ByteString
  finalRoot: Sha256
  halted: bigint
  delegateP: Point // committed delegate point — the prover must know its dlog

  constructor (q: bigint, deltaRoot: Sha256, ownerPkh: ByteString, n: bigint, lastOut: ByteString, finalRoot: Sha256, halted: bigint, delegateP: Point) {
    super(q, deltaRoot, ownerPkh, n, lastOut, finalRoot, halted, delegateP)
    this.q = q
    this.deltaRoot = deltaRoot
    this.ownerPkh = ownerPkh
    this.n = n
    this.lastOut = lastOut
    this.finalRoot = finalRoot
    this.halted = halted
    this.delegateP = delegateP
  }

  private fold (h: ByteString, sib: ByteString, dir: bigint): ByteString {
    return dir == 1n ? hash256(cat(sib, h)) : hash256(cat(h, sib))
  }

  // Schnorr-PoK gate: the caller proves knowledge of dlog(delegateP) bound to
  // this n and successor to — no owner signature required.
  public stepZk (commitR: Point, s: bigint, e: bigint,
    from: bigint, sym: bigint, to: bigint, outHash: ByteString,
    sib0: ByteString, sib1: ByteString, sib2: ByteString, sib3: ByteString,
    dir: bigint) {
    assert(this.halted == 0n)
    assert(ecOnCurve(commitR))
    assert(ecOnCurve(this.delegateP))
    // challenge binds (R, P, n, to) — a replayed (R,s) at a different n fails
    const challenge: ByteString = sha256(cat(cat(commitR, this.delegateP), cat(num2bin(this.n, 8n), num2bin(to, 8n))))
    assert(challenge == num2bin(e, 32n))
    assert(ecMulGen(s) == ecAdd(commitR, ecMul(this.delegateP, e)))
    assert(len(outHash) == 32n)
    assert(from == this.q)
    const h0: ByteString = cat(num2bin(from, 8n), cat(num2bin(sym, 8n), num2bin(to, 8n)))
    const h1: ByteString = this.fold(h0, sib0, dir % 2n)
    const h2: ByteString = this.fold(h1, sib1, (dir / 2n) % 2n)
    const h3: ByteString = this.fold(h2, sib2, (dir / 4n) % 2n)
    const h4: ByteString = this.fold(h3, sib3, (dir / 8n) % 2n)
    assert(h4 == this.deltaRoot)
    this.addOutput(1n, to, this.deltaRoot, this.ownerPkh, this.n + 1n, outHash, this.finalRoot, 0n, this.delegateP)
  }

  // owner-signed path remains available alongside stepZk
  public step (sig: Sig, pubKey: PubKey,
    from: bigint, sym: bigint, to: bigint, outHash: ByteString,
    sib0: ByteString, sib1: ByteString, sib2: ByteString, sib3: ByteString,
    dir: bigint) {
    assert(hash160(pubKey) == this.ownerPkh)
    assert(checkSig(sig, pubKey))
    assert(this.halted == 0n)
    assert(len(outHash) == 32n)
    assert(from == this.q)
    const h0: ByteString = cat(num2bin(from, 8n), cat(num2bin(sym, 8n), num2bin(to, 8n)))
    const h1: ByteString = this.fold(h0, sib0, dir % 2n)
    const h2: ByteString = this.fold(h1, sib1, (dir / 2n) % 2n)
    const h3: ByteString = this.fold(h2, sib2, (dir / 4n) % 2n)
    const h4: ByteString = this.fold(h3, sib3, (dir / 8n) % 2n)
    assert(h4 == this.deltaRoot)
    this.addOutput(1n, to, this.deltaRoot, this.ownerPkh, this.n + 1n, outHash, this.finalRoot, 0n, this.delegateP)
  }

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
    this.addOutput(1n, this.q, this.deltaRoot, this.ownerPkh, this.n + 1n, this.lastOut, this.finalRoot, 1n, this.delegateP)
  }
}
