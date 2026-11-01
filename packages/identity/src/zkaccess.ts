// ZKAccess (ZKA-001..006) — port of anchorchain/packages/privacy to @bsv/sdk:
// Pedersen clearance commitment C = ℓ·G + r·H issued by an authority (signed),
// and a bit-decomposition range proof that the agent holds ℓ ≥ ℓ_j (proven as
// ℓ−ℓ_j ≥ 0 over C·g^(−ℓ_j)), bound to a transcript (queryId ‖ fragmentId ‖
// verifier nonce). Honest label: Fiat-Shamir sigma range proof, linear in bits
// — not a logarithmic Bulletproof.
import { PrivateKey, PublicKey, Signature, Point, BigNumber, Curve } from '@bsv/sdk'
import { dsha256, concat, utf8, type Bytes } from '@mamem/core'
import { proveRangeBP, verifyRangeBP, type RangeProofBP, BP_H } from './bp.js'

const curve = new Curve()
export const G: Point = curve.g
/** H — shared nothing-up-my-sleeve generator (same base as the Bulletproof
 * module, so a Pedersen commitment verifies under either proof system). */
export const H: Point = BP_H

const N = BigInt(curve.n.toString())
const mod = (a: bigint) => ((a % N) + N) % N
const add = (a: bigint, b: bigint) => mod(a + b)
const mul = (a: bigint, b: bigint) => mod(a * b)
const sub = (a: bigint, b: bigint) => mod(a - b)
const rand = (): bigint => { for (;;) { const s = mod(BigInt('0x' + Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString('hex'))); if (s !== 0n) return s } }
const mulP = (p: Point, s: bigint) => p.mul(new BigNumber(mod(s).toString()))
const negP = (p: Point) => p.neg()
const ptHex = (p: Point) => Buffer.from(p.encode(true)).toString('hex')

// --- Pedersen commitment ------------------------------------------------------
export interface PedersenCommit { point: Point, hex: string }
export const commit = (v: bigint, r: bigint): Point => mulP(G, v).add(mulP(H, r))
export const commitHex = (v: bigint, r: bigint): string => ptHex(commit(v, r))
const subG = (p: Point) => p.add(negP(G))

// --- transcript ---------------------------------------------------------------
export interface ZkTranscript { queryId: string, fragmentId: string, nonce: Bytes }
export function transcriptLabel (t: ZkTranscript, extra: string): string {
  return `mamem/zkaccess/${t.queryId}/${t.fragmentId}/${Buffer.from(t.nonce).toString('hex')}/${extra}`
}
function challenge (label: string, pts: Point[]): bigint {
  return mod(BigInt('0x' + Buffer.from(dsha256(concat(utf8(label), ...pts.map(p => Buffer.from(p.encode(true)) as Uint8Array)))).toString('hex')))
}

// --- OR proof (CDS'94) --------------------------------------------------------
interface OrProof { a: string[], e: string[], s: string[] }
function proveOr (label: string, base: Point, statements: Point[], trueIdx: number, witness: bigint): OrProof {
  const m = statements.length
  const a: Point[] = new Array(m); const e: bigint[] = new Array(m); const s: bigint[] = new Array(m)
  let sumFake = 0n
  for (let j = 0; j < m; j++) {
    if (j === trueIdx) continue
    e[j] = rand(); s[j] = rand()
    a[j] = mulP(base, s[j]!).add(negP(mulP(statements[j]!, e[j]!)))
    sumFake = add(sumFake, e[j]!)
  }
  const k = rand()
  a[trueIdx] = mulP(base, k)
  const total = challenge(label, [base, ...statements, ...a])
  e[trueIdx] = sub(total, sumFake)
  s[trueIdx] = add(k, mul(e[trueIdx]!, witness))
  return { a: a.map(ptHex), e: e.map(x => x.toString()), s: s.map(x => x.toString()) }
}
function verifyOr (label: string, base: Point, statements: Point[], p: OrProof): boolean {
  const m = statements.length
  if (p.a.length !== m || p.e.length !== m || p.s.length !== m) return false
  const pts = p.a.map(h => { const q = PublicKey.fromString(h) as any; return new Point(q.x, q.y) })
  const total = challenge(label, [base, ...statements, ...pts])
  let sum = 0n
  for (const ej of p.e) sum = add(sum, BigInt(ej))
  if (mod(sum) !== mod(total)) return false
  for (let j = 0; j < m; j++) {
    const lhs = mulP(base, BigInt(p.s[j]!))
    const rhs = pts[j]!.add(mulP(statements[j]!, BigInt(p.e[j]!)))
    if (!lhs.eq(rhs)) return false
  }
  return true
}

// --- range proof (bit decomposition) ------------------------------------------
export interface RangeProof { bits: number, bitCommits: string[], bitProofs: OrProof[] }
export function proveRange (value: bigint, blinding: bigint, bits: number, t: ZkTranscript): RangeProof | null {
  if (bits <= 0 || bits > 64) return null
  const v = mod(value)
  if (v >= 1n << BigInt(bits)) return null
  const r = new Array<bigint>(bits)
  let weighted = 0n
  for (let i = 1; i < bits; i++) { r[i] = rand(); weighted = add(weighted, mul(r[i]!, 1n << BigInt(i))) }
  r[0] = sub(blinding, weighted)
  const bitCommits: Point[] = []; const bitProofs: OrProof[] = []
  for (let i = 0; i < bits; i++) {
    const bi = (v >> BigInt(i)) & 1n
    const ci = commit(bi, r[i]!)
    bitCommits.push(ci)
    bitProofs.push(proveOr(transcriptLabel(t, `bit/${i}`), H, [ci, subG(ci)], Number(bi), r[i]!))
  }
  return { bits, bitCommits: bitCommits.map(ptHex), bitProofs }
}
export function verifyRange (C: Point, proof: RangeProof, t: ZkTranscript): boolean {
  if (proof.bitCommits.length !== proof.bits || proof.bitProofs.length !== proof.bits) return false
  let acc: Point | undefined
  for (let i = 0; i < proof.bits; i++) {
    const q = PublicKey.fromString(proof.bitCommits[i]!) as any
    const ci = new Point(q.x, q.y)
    const term = mulP(ci, 1n << BigInt(i))
    acc = acc === undefined ? term : acc.add(term)
  }
  if (acc === undefined || !acc.eq(C)) return false
  for (let i = 0; i < proof.bits; i++) {
    const q = PublicKey.fromString(proof.bitCommits[i]!) as any
    const ci = new Point(q.x, q.y)
    if (!verifyOr(transcriptLabel(t, `bit/${i}`), H, [ci, subG(ci)], proof.bitProofs[i]!)) return false
  }
  return true
}

// --- ZKAccess: authority-issued clearance + proof of ℓ ≥ ℓ_j --------------------
/** PUBLIC part — all a verifier ever sees: authority, holder, C, authority sig.
 *  The level and blinding never leave the holder (that is the point of ZK). */
export interface ClearanceCommitment { pub: string, holder: string, hex: string, sig: string }
/** HOLDER secret: the commitment plus its opening (level, blinding). */
export interface ClearanceCredential { commitment: ClearanceCommitment, level: bigint, blinding: string }
const clearanceDigest = (hex: string, holder: string) =>
  Array.from(dsha256(concat(utf8('mamem/zkaccess/commit/v2/'), Buffer.from(hex, 'hex') as Uint8Array, Buffer.from(holder, 'hex') as Uint8Array)))
/** authority signs C = ℓ·G + r·H bound to the holder's pubkey. */
export function issueClearance (authority: PrivateKey, level: bigint, holderPub: string): ClearanceCredential {
  const blind = rand()
  const hex = commitHex(level, blind)
  const sig = authority.sign(clearanceDigest(hex, holderPub)).toDER('hex') as string
  return { commitment: { pub: authority.toPublicKey().toString(), holder: holderPub, hex, sig }, level, blinding: blind.toString() }
}
export function verifyClearanceSig (c: ClearanceCommitment): boolean {
  try {
    return PublicKey.fromString(c.pub).verify(clearanceDigest(c.hex, c.holder), Signature.fromDER(c.sig, 'hex'))
  } catch { return false }
}
/** prove ℓ − ℓ_j ≥ 0 over C·g^(−ℓ_j): the agent commits to (ℓ−ℓ_j) with the
 * SAME blinding minus the claimed level, so the range proof binds C.
 * Default = logarithmic Bulletproof; linear sigma kept as fallback (BP needs
 * pow2 bits and different generator bases). Transcript binds via t. */
export function proveAccess (c: ClearanceCredential, requiredLevel: bigint, bits: number, t: ZkTranscript, mode: 'bp' | 'sigma' = 'bp'): RangeProof | RangeProofBP | null {
  const blind = BigInt(c.blinding)
  const diff = c.level - requiredLevel
  if (diff < 0n) return null // a lower clearance cannot produce a valid proof
  if (mode === 'bp') {
    const r = proveRangeBP(diff, blind, bits, transcriptLabel(t, 'bp'))
    return r ? r.proof : null
  }
  return proveRange(diff, blind, bits, t)
}
/** verifier: authority sig on (C, holder) + range proof on C·g^(−ℓ_j) +
 *  transcript. `trustedAuthorities` pins who may issue clearance; `holder`
 *  must be the VERIFIED caller — both are mandatory in production paths. */
export function verifyAccess (c: ClearanceCommitment, requiredLevel: bigint, proof: RangeProof | RangeProofBP, t: ZkTranscript, opts: { trustedAuthorities?: string[], holder?: string } = {}): boolean {
  if (opts.trustedAuthorities && !opts.trustedAuthorities.includes(c.pub)) return false
  if (opts.holder !== undefined && opts.holder !== c.holder) return false
  if (!verifyClearanceSig(c)) return false
  const Cp = PublicKey.fromString(c.hex) as any
  const C = new Point(Cp.x, Cp.y)
  // the claim is about C·g^(−ℓ_j) — the commitment to (ℓ−ℓ_j)
  const Cshift = C.add(mulP(G, mod(-requiredLevel)))
  // the BP commitment uses BP_G/BP_H — verify with the matching verifier
  if ((proof as RangeProofBP).ip !== undefined) {
    // C·g^(−ℓ_j) under the shared bases (G,H) IS the BP commitment shape —
    // verifyRangeBP checks it directly; transcript is bound by verifyClearanceSig+c.
    return verifyRangeBP(Cshift, proof as RangeProofBP, transcriptLabel(t, 'bp'))
  }
  return verifyRange(Cshift, proof as RangeProof, t)
}
