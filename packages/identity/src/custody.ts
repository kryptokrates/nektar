// Custody (IDN-008) — port of anchorchain custody to @bsv/sdk primitives:
//  - Shamir secret sharing over GF(n) (secp256k1 order) + reconstruction;
//  - threshold Schnorr (Lagrange-weighted partial sigs, key never assembled);
//  - KeyCustodian lifecycle (genesis|rotation|revocation hash chain, anchorable head).
import { PrivateKey, PublicKey, BigNumber, Curve, Point } from '@bsv/sdk'
import { dsha256, concat, utf8 } from '@nektar/core'

const N = BigInt(new Curve().n.toString())
const mod = (a: bigint) => ((a % N) + N) % N
const add = (a: bigint, b: bigint) => mod(a + b)
const mul = (a: bigint, b: bigint) => mod(a * b)
const sub = (a: bigint, b: bigint) => mod(a - b)
const inv = (a: bigint): bigint => {
  let [t, nt, r, nr] = [0n, 1n, N, mod(a)]
  while (nr !== 0n) {
    const q = r / nr
    ;[t, nt] = [nt, t - q * nt]
    ;[r, nr] = [nr, r - q * nr]
  }
  return mod(t)
}
const reduce = (b: Uint8Array) => mod(BigInt('0x' + Buffer.from(b).toString('hex')))
const rand = (): bigint => {
  for (;;) {
    const s = reduce(crypto.getRandomValues(new Uint8Array(32)))
    if (s !== 0n) return s
  }
}
const privOf = (s: bigint) => PrivateKey.fromHex(s.toString(16).padStart(64, '0'))
const pubOf = (s: bigint) => {
  const p = privOf(s).toPublicKey() as any
  return new Point(p.x, p.y)
}

export interface Share {
  x: bigint
  y: bigint
}
export function splitSecret(secret: bigint, threshold: number, numShares: number): Share[] {
  if (threshold < 1 || threshold > numShares) throw new Error(`threshold ${threshold} not in [1,${numShares}]`)
  const coeffs = [mod(secret)]
  for (let i = 1; i < threshold; i++) coeffs.push(rand())
  const shares: Share[] = []
  for (let i = 1; i <= numShares; i++) {
    const x = BigInt(i)
    let acc = 0n
    for (let j = coeffs.length - 1; j >= 0; j--) acc = add(mul(acc, x), coeffs[j]!)
    shares.push({ x, y: acc })
  }
  return shares
}
export function reconstruct(shares: Share[]): bigint {
  let s = 0n
  for (const sj of shares) {
    let num = 1n,
      den = 1n
    for (const sm of shares) {
      if (sm === sj) continue
      num = mul(num, sm.x)
      den = mul(den, sub(sm.x, sj.x))
    }
    s = add(s, mul(sj.y, mul(num, inv(den))))
  }
  return s
}

// --- threshold Schnorr -------------------------------------------------------
export interface SchnorrSig {
  R: string
  s: bigint
} // R = compressed point hex
const G = () => new Curve().g
export const challengeOf = (R: string, P: string, msg: Uint8Array) =>
  reduce(dsha256(concat(utf8('threshold-schnorr/'), Buffer.from(R, 'hex') as Uint8Array, Buffer.from(P, 'hex') as Uint8Array, msg)))
export function lagrangeCoefficient(signingSet: bigint[], j: bigint): bigint {
  let num = 1n,
    den = 1n
  for (const m of signingSet) {
    if (m === j) continue
    num = mul(num, m)
    den = mul(den, sub(m, j))
  }
  return mul(num, inv(den))
}
/** partial sig s_j = k_j + e·λ_j·x_j — each party sees only its share. */
export function partialSign(share: Share, nonceK: bigint, e: bigint, signingSet: bigint[]): bigint {
  const lam = lagrangeCoefficient(signingSet, share.x)
  return mod(nonceK + mul(e, mul(lam, share.y)))
}
export function verifySchnorr(pubHex: string, msg: Uint8Array, sig: SchnorrSig): boolean {
  const Pp = PublicKey.fromString(pubHex) as any
  const Rp = PublicKey.fromString(sig.R) as any
  const P = new Point(Pp.x, Pp.y)
  const R = new Point(Rp.x, Rp.y)
  const e = challengeOf(sig.R, pubHex, msg)
  const lhs = G().mul(new BigNumber(sig.s.toString()))
  const rhs = R.add(P.mul(new BigNumber(e.toString())))
  return (lhs as any).eq(rhs)
}
/** aggregate partial sigs over the signing set: s = Σ s_j, R = Σ R_j. */
export function aggregateSchnorr(partials: bigint[], reveals: bigint[]): SchnorrSig {
  let s = 0n,
    R: Point | undefined
  for (const k of reveals) {
    const p = pubOf(k)
    R = R === undefined ? p : (R as any).add(p)
  }
  for (const sj of partials) s = add(s, sj)
  return { R: Buffer.from((R as any).encode(true)).toString('hex'), s }
}

// --- custodian ---------------------------------------------------------------
export type LifecycleKind = 'genesis' | 'rotation' | 'revocation'
export interface LifecycleEvent {
  kind: LifecycleKind
  publicKeyHex: string
  time: bigint
  prevHashHex: string
  hashHex: string
}
const ZERO = '00'.repeat(32)
const evHash = (kind: LifecycleKind, pub: string, t: bigint, prevHex: string) => {
  const t8 = new Uint8Array(8)
  new DataView(t8.buffer).setBigUint64(0, BigInt(t), true)
  return Buffer.from(dsha256(concat(utf8('custody/' + kind + '/'), utf8(pub), t8, Buffer.from(prevHex, 'hex') as Uint8Array))).toString('hex')
}
export class KeyCustodian {
  private pub: string
  private revoked = false
  private events: LifecycleEvent[] = []
  private lastHash = ZERO
  private constructor(secret: bigint, time: bigint) {
    this.pub = privOf(secret).toPublicKey().toString()
    this.append('genesis', time)
  }
  static generate(threshold: number, n: number, time: bigint, secret?: bigint) {
    const s = secret ?? rand()
    return { custodian: new KeyCustodian(s, time), shares: splitSecret(s, threshold, n) }
  }
  private append(kind: LifecycleKind, time: bigint): void {
    const h = evHash(kind, this.pub, time, this.lastHash)
    this.events.push({ kind, publicKeyHex: this.pub, time, prevHashHex: this.lastHash, hashHex: h })
    this.lastHash = h
  }
  publicKeyHex(): string {
    return this.pub
  }
  isRevoked(): boolean {
    return this.revoked
  }
  lifecycle(): LifecycleEvent[] {
    return this.events.map((e) => ({ ...e }))
  }
  headHashHex(): string {
    return this.lastHash
  }
  /** sign via reconstruction — only if the shares rebuild THIS key. */
  sign(shareSubset: Share[], msgHash: Uint8Array): string {
    if (this.revoked) throw new Error('key revoked')
    if (shareSubset.length === 0) throw new Error('no shares')
    const key = reconstruct(shareSubset)
    if (privOf(key).toPublicKey().toString() !== this.pub) throw new Error('shares do not reconstruct the current key')
    return privOf(key).sign(Array.from(msgHash)).toDER('hex') as string
  }
  rotate(threshold: number, n: number, time: bigint, secret?: bigint): Share[] {
    if (this.revoked) throw new Error('key revoked')
    const s = secret ?? rand()
    this.pub = privOf(s).toPublicKey().toString()
    this.append('rotation', time)
    return splitSecret(s, threshold, n)
  }
  revoke(time: bigint): void {
    if (this.revoked) return
    this.revoked = true
    this.append('revocation', time)
  }
}
export function verifyLifecycle(events: LifecycleEvent[]): boolean {
  if (events.length === 0 || events[0]!.kind !== 'genesis') return false
  let prev = ZERO
  for (const e of events) {
    if (e.prevHashHex !== prev) return false
    if (evHash(e.kind, e.publicKeyHex, e.time, e.prevHashHex) !== e.hashHex) return false
    prev = e.hashHex
  }
  return true
}
