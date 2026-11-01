// Bulletproofs range proof — port of anchorchain/packages/privacy/bulletproofs.ts
// to @bsv/sdk. Logarithmic inner-product argument over secp256k1, Fiat-Shamir.
// Sound + ZK under DL in the ROM; no trusted setup; no post-quantum claim.
import { PublicKey, Point, BigNumber, Curve } from '@bsv/sdk'
import { dsha256, concat, utf8, type Bytes } from '@mamem/core'

const curve = new Curve()
const N = BigInt(curve.n.toString())
const mod = (a: bigint) => ((a % N) + N) % N
const add = (a: bigint, b: bigint) => mod(a + b)
const mul = (a: bigint, b: bigint) => mod(a * b)
const sub = (a: bigint, b: bigint) => mod(a - b)
const inv = (a: bigint): bigint => { let [t, nt, r, nr] = [0n, 1n, N, mod(a)]; while (nr !== 0n) { const q = r / nr; [t, nt] = [nt, t - q * nt]; [r, nr] = [nr, r - q * nr] } return mod(t) }
const rand = (): bigint => { for (;;) { const s = mod(BigInt('0x' + Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString('hex'))); if (s !== 0n) return s } }

export const BP_G = curve.g
export const BP_H = curve.g.mul(new BigNumber(BigInt('0x' + Buffer.from(dsha256(utf8('mamem/bp/H/v1'))).toString('hex')).toString()))
const ptFromHex = (hex: string): Point => { const q = PublicKey.fromString(hex) as any; return new Point(q.x, q.y) }
const ptHex = (p: Point): string => Buffer.from(p.encode(true)).toString('hex')
export const BP_U = hashToPoint('mamem/bp/U/v1')

export function hashToPoint (label: string): Point {
  for (let c = 0; c < 100000; c++) {
    const x = dsha256(utf8(`${label}:${c}`))
    for (const prefix of [0x02, 0x03]) {
      try { return ptFromHex(Buffer.from(Uint8Array.from([prefix, ...x])).toString('hex')) } catch { continue }
    }
  }
  throw new Error('hashToPoint: unreachable')
}
const genVector = (domain: string, n: number) => Array.from({ length: n }, (_, i) => hashToPoint(`${domain}/${i}`))

const challenge = (label: string, pts: Point[]): bigint => {
  const c = mod(BigInt('0x' + Buffer.from(dsha256(concat(utf8(label), ...pts.map(p => Buffer.from(p.encode(true)) as Uint8Array)))).toString('hex')))
  return c === 0n ? 1n : c
}
const mulP = (p: Point, s: bigint) => p.mul(new BigNumber(mod(s).toString()))
type Opt = Point | undefined
const addO = (a: Opt, b: Opt): Opt => a === undefined ? b : b === undefined ? a : a.add(b)
const smul = (s: bigint, p: Point): Opt => mod(s) === 0n ? undefined : mulP(p, s)
const msm = (ss: bigint[], ps: Point[]): Opt => { let acc: Opt; for (let i = 0; i < ss.length; i++) acc = addO(acc, smul(ss[i]!, ps[i]!)); return acc }
const need = (p: Opt): Point => { if (!p) throw new Error('unexpected identity'); return p }
const eqO = (a: Opt, b: Opt) => (a === undefined || b === undefined) ? a === b : a.eq(b)
const inner = (a: bigint[], b: bigint[]) => a.reduce((acc, x, i) => add(acc, mul(x, b[i]!)), 0n)
const powers = (b: bigint, n: number): bigint[] => { const o = [1n]; for (let i = 1; i < n; i++) o.push(mul(o[i - 1]!, b)); return o }
const hadamard = (a: bigint[], b: bigint[]) => a.map((x, i) => mul(x, b[i]!))
const isPow2 = (n: number) => n > 0 && (n & (n - 1)) === 0

export interface InnerProductProof { L: string[], R: string[], a: string, b: string }
export interface RangeProofBP { bits: number, A: string, S: string, T1: string, T2: string, taux: string, mu: string, tHat: string, ip: InnerProductProof }

function proveIP (gIn: Point[], hIn: Point[], U: Point, aIn: bigint[], bIn: bigint[], bind = ''): InnerProductProof {
  let g = gIn.slice(), h = hIn.slice(), a = aIn.slice(), b = bIn.slice()
  const L: Point[] = []; const R: Point[] = []; let round = 0
  while (a.length > 1) {
    const nn = a.length / 2
    const aLo = a.slice(0, nn), aHi = a.slice(nn), bLo = b.slice(0, nn), bHi = b.slice(nn)
    const gLo = g.slice(0, nn), gHi = g.slice(nn), hLo = h.slice(0, nn), hHi = h.slice(nn)
    const cL = inner(aLo, bHi), cR = inner(aHi, bLo)
    const Lj = need(addO(addO(msm(aLo, gHi), msm(bHi, hLo)), smul(cL, U)))
    const Rj = need(addO(addO(msm(aHi, gLo), msm(bLo, hHi)), smul(cR, U)))
    L.push(Lj); R.push(Rj)
    const u = challenge(`${bind}mamem/bp/ip/${round}`, [Lj, Rj]), uInv = inv(u)
    const gNext: Point[] = [], hNext: Point[] = [], aNext: bigint[] = [], bNext: bigint[] = []
    for (let i = 0; i < nn; i++) {
      gNext.push(need(addO(smul(uInv, gLo[i]!), smul(u, gHi[i]!))))
      hNext.push(need(addO(smul(u, hLo[i]!), smul(uInv, hHi[i]!))))
      aNext.push(add(mul(aLo[i]!, u), mul(aHi[i]!, uInv)))
      bNext.push(add(mul(bLo[i]!, uInv), mul(bHi[i]!, u)))
    }
    g = gNext; h = hNext; a = aNext; b = bNext; round++
  }
  return { L: L.map(ptHex), R: R.map(ptHex), a: a[0]!.toString(), b: b[0]!.toString() }
}

function verifyIP (gIn: Point[], hIn: Point[], U: Point, Pin: Point, proof: InnerProductProof, bind = ''): boolean {
  let g = gIn.slice(), h = hIn.slice(), P: Point = Pin
  for (let round = 0; round < proof.L.length; round++) {
    const Lj = ptFromHex(proof.L[round]!), Rj = ptFromHex(proof.R[round]!)
    const u = challenge(`${bind}mamem/bp/ip/${round}`, [Lj, Rj]), uInv = inv(u)
    const u2 = mul(u, u), uInv2 = mul(uInv, uInv)
    const nn = g.length / 2
    const gLo = g.slice(0, nn), gHi = g.slice(nn), hLo = h.slice(0, nn), hHi = h.slice(nn)
    const gNext: Point[] = [], hNext: Point[] = []
    for (let i = 0; i < nn; i++) {
      gNext.push(need(addO(smul(uInv, gLo[i]!), smul(u, gHi[i]!))))
      hNext.push(need(addO(smul(u, hLo[i]!), smul(uInv, hHi[i]!))))
    }
    g = gNext; h = hNext
    P = need(addO(addO(need(smul(u2, Lj)), P), need(smul(uInv2, Rj))))
  }
  const rhs = addO(addO(smul(BigInt(proof.a), g[0]!), smul(BigInt(proof.b), h[0]!)), smul(mul(BigInt(proof.a), BigInt(proof.b)), U))
  return eqO(P, rhs)
}

export function proveRangeBP (value: bigint, blinding: bigint, bits: number, bind = ''): { proof: RangeProofBP, commitment: Point } | null {
  if (!isPow2(bits) || bits > 256) return null
  const v = mod(value)
  if (v >= 1n << BigInt(bits)) return null
  const n = bits
  const gv = genVector('mamem/bp/g/v1', n)
  const hv = genVector('mamem/bp/h/v1', n)
  const U = BP_U
  const commitment = mulP(BP_G, v).add(mulP(BP_H, blinding))
  const aL = Array.from({ length: n }, (_, i) => (v >> BigInt(i)) & 1n)
  const aR = aL.map(b => sub(b, 1n))
  const alpha = rand()
  const A = need(addO(addO(smul(alpha, BP_H), msm(aL, gv)), msm(aR, hv)))
  const sL = Array.from({ length: n }, () => rand())
  const sR = Array.from({ length: n }, () => rand())
  const rho = rand()
  const S = need(addO(addO(smul(rho, BP_H), msm(sL, gv)), msm(sR, hv)))
  const y = challenge(`${bind}mamem/bp/y`, [A, S])
  const z = challenge(`${bind}mamem/bp/z`, [A, S])
  const yN = powers(y, n), twoN = powers(2n, n), oneN = Array.from({ length: n }, () => 1n)
  const z2 = mul(z, z)
  const l0 = aL.map(x => sub(x, z))
  const l1 = sL
  const r0 = vecAdd(hadamard(yN, vecAdd(aR, vecScale(z, oneN))), vecScale(z2, twoN))
  const r1 = hadamard(yN, sR)
  const t1 = add(inner(l0, r1), inner(l1, r0))
  const t2 = inner(l1, r1)
  const tau1 = rand(), tau2 = rand()
  const T1 = need(addO(smul(t1, BP_G), smul(tau1, BP_H)))
  const T2 = need(addO(smul(t2, BP_G), smul(tau2, BP_H)))
  const x = challenge(`${bind}mamem/bp/x`, [T1, T2])
  const x2 = mul(x, x)
  const l = vecAdd(l0, vecScale(x, l1))
  const r = vecAdd(r0, vecScale(x, r1))
  const tHat = inner(l, r)
  const taux = add(add(mul(tau2, x2), mul(tau1, x)), mul(z2, blinding))
  const mu = add(alpha, mul(rho, x))
  const yInv = inv(y), yInvN = powers(yInv, n)
  const hp = hv.map((p, i) => mulP(p, yInvN[i]!))
  const ip = proveIP(gv, hp, U, l, r, bind)
  return { proof: { bits, A: ptHex(A), S: ptHex(S), T1: ptHex(T1), T2: ptHex(T2), taux: taux.toString(), mu: mu.toString(), tHat: tHat.toString(), ip }, commitment }
}
function vecAdd (a: bigint[], b: bigint[]) { return a.map((x, i) => add(x, b[i]!)) }
function vecScale (s: bigint, a: bigint[]) { return a.map(x => mul(s, x)) }

export function verifyRangeBP (commitment: Point, proof: RangeProofBP, bind = ''): boolean {
  const n = proof.bits
  if (!isPow2(n) || n > 256) return false
  const gv = genVector('mamem/bp/g/v1', n)
  const hv = genVector('mamem/bp/h/v1', n)
  const U = BP_U
  const A = ptFromHex(proof.A), S = ptFromHex(proof.S), T1 = ptFromHex(proof.T1), T2 = ptFromHex(proof.T2)
  const y = challenge(`${bind}mamem/bp/y`, [A, S])
  const z = challenge(`${bind}mamem/bp/z`, [A, S])
  const x = challenge(`${bind}mamem/bp/x`, [T1, T2])
  const yN = powers(y, n), twoN = powers(2n, n), oneN = Array.from({ length: n }, () => 1n)
  const z2 = mul(z, z), z3 = mul(z2, z), x2 = mul(x, x)
  const delta = sub(mul(sub(z, z2), inner(oneN, yN)), mul(z3, inner(oneN, twoN)))
  const tHat = BigInt(proof.tHat), taux = BigInt(proof.taux), mu = BigInt(proof.mu)
  const lhs1 = need(addO(smul(tHat, BP_G), smul(taux, BP_H)))
  const rhs1 = need(addO(addO(addO(smul(z2, commitment), smul(delta, BP_G)), smul(x, T1)), smul(x2, T2)))
  if (!lhs1.eq(rhs1)) return false
  const yInvN = powers(inv(y), n)
  const hp = hv.map((p, i) => mulP(p, yInvN[i]!))
  // P = A + x·S + Σ (−z)·g_i + Σ (z·y^i + z²·2^i)·h'_i; then Pip = P − μ·H + tHat·U
  const gScalars = oneN.map(() => mod(-z))
  const hScalars = yN.map((yi, i) => add(mul(z, yi), mul(z2, twoN[i]!)))
  const P = need(addO(addO(addO(A, need(smul(x, S))), msm(gScalars, gv)), msm(hScalars, hp)))
  const Pip = need(addO(addO(P, smul(mod(-mu), BP_H)), smul(tHat, U)))
  return verifyIP(gv, hp, U, Pip, proof.ip, bind)
}
function msmPsum (pts: Point[]): Point { let acc = pts[0]!; for (let i = 1; i < pts.length; i++) acc = acc.add(pts[i]!); return acc }
