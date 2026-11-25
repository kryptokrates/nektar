// NektarAutomatonZk — AUT-005 + hardened Schnorr-PoK delegation gate.
// F-22: Strong Fiat-Shamir challenge binds domain tag, R, delegateP, contract instance
//       (ownerPkh, deltaRoot), AND every transition parameter (n, from, sym, to, outHash).
// F-26: Unsigned scalar decoding via bin2num(cat(challenge, 0x00)) % EC_N and strict
//       1 <= e < EC_N, 1 <= s < EC_N canonicality bounds.
import {
  StatefulSmartContract,
  assert, checkSig, hash256, hash160, num2bin, bin2num, cat, len, sha256, toByteString,
  ecMulGen, ecAdd, ecMul, ecOnCurve, EC_N,
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
  delegateP: Point

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

  public stepZk (commitR: Point, s: bigint,
    from: bigint, sym: bigint, to: bigint, outHash: ByteString,
    sib0: ByteString, sib1: ByteString, sib2: ByteString, sib3: ByteString,
    dir: bigint) {
    assert(this.halted == 0n)
    assert(len(outHash) == 32n)
    assert(from == this.q)
    assert(ecOnCurve(commitR))
    assert(ecOnCurve(this.delegateP))
    assert(s >= 1n && s < EC_N)
    const domain: ByteString = toByteString('6e656b7461723a6175747a6b3a7632') // "nektar:autzk:v2"
    const ctxBytes: ByteString = cat(cat(domain, cat(commitR, this.delegateP)), cat(this.ownerPkh, this.deltaRoot))
    const stepBytes: ByteString = cat(cat(num2bin(this.n, 8n), num2bin(from, 8n)), cat(num2bin(sym, 8n), cat(num2bin(to, 8n), outHash)))
    const challenge: ByteString = sha256(cat(ctxBytes, stepBytes))
    const e: bigint = bin2num(cat(challenge, toByteString('00'))) % EC_N
    assert(e >= 1n && e < EC_N)
    assert(ecMulGen(s) == ecAdd(commitR, ecMul(this.delegateP, e)))
    const h0: ByteString = cat(num2bin(from, 8n), cat(num2bin(sym, 8n), num2bin(to, 8n)))
    const h1: ByteString = this.fold(h0, sib0, dir % 2n)
    const h2: ByteString = this.fold(h1, sib1, (dir / 2n) % 2n)
    const h3: ByteString = this.fold(h2, sib2, (dir / 4n) % 2n)
    const h4: ByteString = this.fold(h3, sib3, (dir / 8n) % 2n)
    assert(h4 == this.deltaRoot)
    this.addOutput(1n, to, this.deltaRoot, this.ownerPkh, this.n + 1n, outHash, this.finalRoot, 0n, this.delegateP)
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
