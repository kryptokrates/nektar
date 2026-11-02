// @mamem/reasoning — P15 agent-side reasoning over verified memory:
//   ground()           AGT-003  every claim must be entailed by a VERIFIED cited fragment
//   modelIdentity()    AGT-011  local weights hash / API provider release
//   commitReceipt()    AGT-012  answer ‖ refs ‖ check ‖ model ‖ time ‖ grounding, committed
//   CitationIndex +    AOR-008c / STO-005 / AGT-006  reverse index; a revision re-grounds
//   onRevision()                every citing answer and publishes linked corrections
//   timeBounds()       AGT-005  consensus time: existence no later than the block time
//   build/verifyAuditBundle  AGT-007  output → fragments → signers → policy path → anchors
// Proofs establish inclusion + integrity + existence-by-time — never truth.
import { createHash } from 'node:crypto'
import { createReadStream, existsSync, readFileSync, writeFileSync } from 'node:fs'
import { PublicKey, Signature, PrivateKey, type Transaction } from '@bsv/sdk'
import { dsha256, utf8, toHex, fromHex, concat, computeLeaf, TAG_PROMISE, TAG_DELEGATION } from '@mamem/core'
import { Datalog, parseLiteral, parseProgram, ParseError, type Fact, type Rule } from '@mamem/datalog'
import { verifyFragment, type ProofBundle } from '@mamem/verify'
import { verifyDerivation, type PolicyDAG } from '@mamem/graph'
import type { ChainTracker } from '@mamem/chain'

export * from './temporal.js'

export const TRUTH_NOTE = 'Proofs show a cited record was committed, is unaltered and existed by its block time; they do not show the record is true.'

/** canonical JSON: sorted keys, no whitespace — the only form that is hashed */
export function canonicalJson (v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonicalJson).join(',')}]`
  if (v && typeof v === 'object') {
    return `{${Object.keys(v).sort().filter(k => (v as Record<string, unknown>)[k] !== undefined)
      .map(k => `${JSON.stringify(k)}:${canonicalJson((v as Record<string, unknown>)[k])}`).join(',')}}`
  }
  return JSON.stringify(v)
}
export const hashOf = (v: unknown): string => toHex(dsha256(utf8(canonicalJson(v))))

// ── AGT-003 grounding ─────────────────────────────────────────────────────
export interface FragmentRef { shard: string, seq: number }
export interface Claim { text: string, literal?: string, cites: FragmentRef[] }
export type GroundReason = 'grounded:quote' | 'grounded:entailed' | 'uncited' | 'citation-unverifiable' | 'not-entailed'
export interface GroundedClaim extends Claim { grounded: boolean, reason: GroundReason }
export interface GroundingReport { claims: GroundedClaim[], kept: Claim[], flagged: GroundedClaim[], mode: 'flag' | 'remove', reportHash: string }
export type FragmentFetch = (ref: FragmentRef) => Promise<{ verified: boolean, text?: string }>

const norm = (s: string) => s.toLowerCase().replace(/\s+/g, ' ').trim()
const refKey = (r: FragmentRef) => `${r.shard}/${r.seq}`

/** facts carried by a verified fragment: its text parsed as L_pol facts, if it is one */
function factsOf (text: string): Fact[] {
  try { return parseProgram(text).facts } catch (e) { if (e instanceof ParseError) return []; throw e }
}

export async function ground (claims: Claim[], fetch: FragmentFetch, opts: { mode?: 'flag' | 'remove', rules?: Rule[] } = {}): Promise<GroundingReport> {
  const mode = opts.mode ?? 'flag'
  const out: GroundedClaim[] = []
  for (const c of claims) {
    if (c.cites.length === 0) { out.push({ ...c, grounded: false, reason: 'uncited' }); continue }
    const verified = (await Promise.all(c.cites.map(fetch))).filter(f => f.verified && f.text !== undefined).map(f => f.text!)
    if (verified.length === 0) { out.push({ ...c, grounded: false, reason: 'citation-unverifiable' }); continue }
    if (verified.some(t => norm(t).includes(norm(c.text)))) { out.push({ ...c, grounded: true, reason: 'grounded:quote' }); continue }
    if (c.literal) {
      const goal = parseLiteral(c.literal)
      const derived = new Datalog(opts.rules ?? []).run(verified.flatMap(factsOf))
      if (derived.has(`${goal.pred}(${goal.args.join(',')})`)) { out.push({ ...c, grounded: true, reason: 'grounded:entailed' }); continue }
    }
    out.push({ ...c, grounded: false, reason: 'not-entailed' })
  }
  const flagged = out.filter(c => !c.grounded)
  const kept = mode === 'remove' ? out.filter(c => c.grounded) : out
  const strip = ({ text, literal, cites }: Claim): Claim => ({ text, literal, cites })
  return { claims: out, kept: kept.map(strip), flagged, mode, reportHash: hashOf({ mode, claims: out }) }
}

// ── AGT-011 / AGT-012 model identity + receipts ──────────────────────────
export type ModelIdentity =
  | { kind: 'local', weightsSha256: string, name?: string }
  | { kind: 'api', provider: string, release: string }

export async function modelIdentity (m: { kind: 'local', weightsPath: string, name?: string } | { kind: 'api', provider: string, release: string }): Promise<ModelIdentity> {
  if (m.kind === 'api') {
    if (!m.provider || !m.release) throw new Error('api model identity needs provider + release identifier')
    return { kind: 'api', provider: m.provider, release: m.release }
  }
  const h = createHash('sha256')
  await new Promise<void>((resolve, reject) => {
    createReadStream(m.weightsPath).on('data', c => h.update(c)).on('end', () => resolve()).on('error', reject)
  })
  return { kind: 'local', weightsSha256: h.digest('hex'), name: m.name }
}

export interface Receipt {
  answer: string
  fragmentRefs: FragmentRef[]
  checkResult: 'verified' | 'unverifiable' | 'failed'
  modelId: ModelIdentity
  timestamp: number
  groundingReportHash?: string
  truthNote: string
  /** §16 delegation map μ_i: sub-decisions asserted by delegating agents.
   *  signature = Sign_delegator(H(TAG_DELEGATION‖delegator‖decision‖answer)) */
  delegations?: Array<{ delegator: string, decision: string, signature: string }>
}

/** §16: delegating agent A′ signs authorship over a sub-decision bound to the answer */
export function makeDelegation (key: PrivateKey, decision: string, answer: string): { delegator: string, decision: string, signature: string } {
  const delegator = key.toPublicKey().toString()
  const msg = dsha256(concat(TAG_DELEGATION, utf8(delegator), utf8(decision), utf8(answer)))
  return { delegator, decision, signature: key.sign([...msg]).toDER('hex') as string }
}

/** verify every entry of a receipt's delegation map against its stated delegator */
export function verifyReceiptDelegations (receipt: Pick<Receipt, 'answer' | 'delegations'>): { ok: boolean, reasons: string[] } {
  const reasons: string[] = []
  for (const d of receipt.delegations ?? []) {
    const msg = dsha256(concat(TAG_DELEGATION, utf8(d.delegator), utf8(d.decision), utf8(receipt.answer)))
    let ok = false
    try { ok = PublicKey.fromString(d.delegator).verify([...msg], Signature.fromDER(d.signature, 'hex')) } catch (e) { reasons.push(`delegation for "${d.decision.slice(0, 32)}": malformed key/signature (${(e as Error).message})`); continue }
    if (!ok) reasons.push(`delegation for "${d.decision.slice(0, 32)}": signature does not bind decision+answer to the delegator`)
  }
  return { ok: reasons.length === 0, reasons }
}
/** structural seam onto MamemMemory (no package cycle) */
export interface AnchorMem {
  remember: (shard: string, data: string) => Promise<{ seq: number }>
  anchorShard: (shard: string) => Promise<{ txid: string, n: number } | null>
}

export async function commitReceipt (r: Omit<Receipt, 'truthNote'>, mem?: AnchorMem): Promise<{ receipt: Receipt, receiptHash: string, anchor?: { shard: string, seq: number, txid?: string } }> {
  const receipt: Receipt = { ...r, truthNote: TRUTH_NOTE }
  const receiptHash = hashOf(receipt)
  if (!mem) return { receipt, receiptHash }
  const { seq } = await mem.remember('__receipts__', `receipt:${receiptHash}`)
  const a = await mem.anchorShard('__receipts__')
  return { receipt, receiptHash, anchor: { shard: '__receipts__', seq, txid: a?.txid } }
}

// ── AOR-008c / STO-005 / AGT-006 citation index + correction loop ─────────
export interface CitedAnswer { receiptHash: string, claims: Claim[] }

export class CitationIndex {
  private byRef = new Map<string, Set<string>>()
  private answers = new Map<string, CitedAnswer>()
  constructor (private file?: string) {
    if (file && existsSync(file)) {
      const j = JSON.parse(readFileSync(file, 'utf8')) as { answers: CitedAnswer[] }
      for (const a of j.answers) this.index(a, false)
    }
  }
  add (a: CitedAnswer): void { this.index(a, true) }
  private index (a: CitedAnswer, persist: boolean): void {
    this.answers.set(a.receiptHash, a)
    for (const c of a.claims) for (const r of c.cites) {
      const k = refKey(r)
      if (!this.byRef.has(k)) this.byRef.set(k, new Set())
      this.byRef.get(k)!.add(a.receiptHash)
    }
    if (persist && this.file) writeFileSync(this.file, JSON.stringify({ answers: [...this.answers.values()] }))
  }
  /** STO-005: receipts whose answers cite `ref` */
  citing (ref: FragmentRef): CitedAnswer[] {
    return [...(this.byRef.get(refKey(ref)) ?? [])].map(h => this.answers.get(h)!)
  }
}

export interface Correction {
  receiptHash: string
  oldRef: FragmentRef
  newRef: FragmentRef
  regrounded: GroundingReport
  stillGrounded: boolean
  /** AGT-006: claims grounded before but not after the revision */
  withdrawn: string[]
  correctionHash: string
  anchor?: { seq: number, txid?: string }
}

export async function onRevision (
  idx: CitationIndex, oldRef: FragmentRef, newRef: FragmentRef, fetch: FragmentFetch,
  opts: { rules?: Rule[], mem?: AnchorMem } = {}
): Promise<Correction[]> {
  const out: Correction[] = []
  for (const a of idx.citing(oldRef)) {
    const swapped = a.claims.map(c => ({ ...c, cites: c.cites.map(r => refKey(r) === refKey(oldRef) ? newRef : r) }))
    const before = await ground(a.claims, fetch, { rules: opts.rules })
    const after = await ground(swapped, fetch, { rules: opts.rules })
    const withdrawn = after.claims.filter((c, i) => !c.grounded && before.claims[i]!.grounded).map(c => c.text)
    const body = { receiptHash: a.receiptHash, oldRef, newRef, regroundedHash: after.reportHash, withdrawn }
    const corr: Correction = { receiptHash: a.receiptHash, oldRef, newRef, regrounded: after, stillGrounded: after.flagged.length === 0, withdrawn, correctionHash: hashOf(body) }
    if (opts.mem) {
      const { seq } = await opts.mem.remember('__corrections__', `correction:${corr.correctionHash}:${canonicalJson(body)}`)
      corr.anchor = { seq, txid: (await opts.mem.anchorShard('__corrections__'))?.txid }
    }
    out.push(corr)
  }
  return out
}

// ── AGT-005 consensus time ────────────────────────────────────────────────
export interface TimeBounds { blockHeight: number, notAfter: number, mtp: number, statement: string }

/** existence-by: the record's anchor is in block h, so it existed no later
 *  than header(h).time; MTP(h) = median of the 11 header times ending at h. */
export async function timeBounds (anchorTx: Transaction, tracker: ChainTracker): Promise<TimeBounds> {
  const mp = anchorTx.merklePath
  if (!mp) throw new Error('timeBounds: anchor not mined (no BUMP)')
  if (!(await mp.verify(anchorTx.id('hex'), tracker))) throw new Error('timeBounds: BUMP does not verify against the header chain')
  const h = mp.blockHeight
  const head = await tracker.headerAtHeight(h)
  if (!head) throw new Error(`timeBounds: no header at height ${h}`)
  const times: number[] = []
  for (let i = h; i > Math.max(0, h - 11); i--) { const x = await tracker.headerAtHeight(i); if (x) times.push(x.time) }
  times.sort((a, b) => a - b)
  return { blockHeight: h, notAfter: head.time, mtp: times[Math.floor(times.length / 2)]!, statement: 'existed no later than the block time; says nothing about when it was created' }
}

// ── AGT-007 audit bundle ──────────────────────────────────────────────────
export interface AuditBundle {
  version: 1
  receipt: Receipt
  receiptHash: string
  fragments: Array<{ ref: FragmentRef, bundle: ProofBundle }>
  /** inclusion promises (TAG_PROMISE‖shard‖seq_be32‖leaf) by the log signer */
  signers: Array<{ ref: FragmentRef, pubkey: string, promise: { seq: number, leaf: string, signature: string } }>
  /** §19: anchored revocation evidence — `__revocation__` shard records with
   *  their inclusion bundles, so a third party verifies "key X was revoked"
   *  against chain data, not an operator's opaque local list. `data`/`prov`/`E`
   *  let the verifier recompute `leaf = computeLeaf({d,p,E})` — the evidence
   *  binds the revocation TEXT to the proven leaf, not just any inclusion. */
  revocations?: Array<{ pubkey: string, seq: number, data: string, prov?: import('@mamem/core').Provenance, E?: Uint8Array, leaf: string, bundle: ProofBundle }>
  /** JSON-portable: Merkle siblings travel as hex */
  policy?: { edb: Fact[], derivation: PortableDerivation }
}
type Derivation = Parameters<typeof verifyDerivation>[0]
export interface PortableDerivation { root: string, tree: unknown, proofs: Array<{ h: string, proof: { index: number, siblings: string[] } }> }

export function buildAuditBundle (a: Omit<AuditBundle, 'version' | 'receiptHash' | 'policy'> & { policy?: { edb: Fact[], derivation: Derivation } }): AuditBundle {
  const policy = a.policy && {
    edb: a.policy.edb,
    derivation: { ...a.policy.derivation, proofs: a.policy.derivation.proofs.map(p => ({ h: p.h, proof: { index: p.proof.index, siblings: p.proof.siblings.map(toHex) } })) }
  }
  return { version: 1, ...a, policy, receiptHash: hashOf(a.receipt) }
}

/** Fully offline: needs block headers (tracker) and, when a policy decided,
 *  the verifier's OWN authoritative policy DAG (AGT-009) — never the bundle's. */
export async function verifyAuditBundle (b: AuditBundle, tracker: ChainTracker, opts: { dag?: PolicyDAG, isRevoked?: (pubkeyHex: string) => boolean } = {}): Promise<{ ok: boolean, reasons: string[] }> {
  const reasons: string[] = []
  if (hashOf(b.receipt) !== b.receiptHash) reasons.push('receipt hash mismatch')
  // §19: revoked signers/delegators fail the bundle. Two conviction paths —
  // the verifier's local isRevoked (operator view) AND anchored revocation
  // records carried in the bundle itself (third-party-verifiable: the
  // `__revocation__` record must verify against the chain to count).
  const revoked = opts.isRevoked ?? (() => false)
  const anchoredRevoked = new Set<string>()
  for (const r of b.revocations ?? []) {
    // 1. the revocation TEXT must recompute the proven leaf — an inclusion
    //    proof for a different record is not evidence
    try {
      const recomputed = toHex(computeLeaf({ d: utf8(r.data), p: r.prov!, E: r.E }))
      if (recomputed !== r.leaf) { reasons.push(`revocation record for ${r.pubkey.slice(0, 16)}…: data does not recompute the proven leaf`); continue }
    } catch { reasons.push(`revocation record for ${r.pubkey.slice(0, 16)}…: malformed provenance`); continue }
    // 2. that leaf must be SPV-verifiably anchored
    const v = await verifyFragment(r.bundle, tracker)
    if (!v.ok) { reasons.push(`revocation record for ${r.pubkey.slice(0, 16)}…: inclusion proof failed (${(v as { reason?: string }).reason ?? 'unknown'})`); continue }
    // 3. the record must name THIS pubkey
    if (!r.data.startsWith(`revoke:${r.pubkey}`)) { reasons.push(`revocation record for ${r.pubkey.slice(0, 16)}…: record does not bind this pubkey`); continue }
    anchoredRevoked.add(r.pubkey)
  }
  for (const s of b.signers) {
    if (anchoredRevoked.has(s.pubkey)) reasons.push(`signer ${refKey(s.ref)}: key revoked (anchored)`)
    else if (revoked(s.pubkey)) reasons.push(`signer ${refKey(s.ref)}: key revoked (unanchored local view)`)
  }
  for (const d of b.receipt.delegations ?? []) {
    if (anchoredRevoked.has(d.delegator)) reasons.push(`delegation for "${d.decision.slice(0, 32)}": delegator key revoked (anchored)`)
    else if (revoked(d.delegator)) reasons.push(`delegation for "${d.decision.slice(0, 32)}": delegator key revoked (unanchored local view)`)
  }
  const cited = new Set(b.receipt.fragmentRefs.map(refKey))
  const bundled = new Set(b.fragments.map(f => refKey(f.ref)))
  for (const k of cited) if (!bundled.has(k)) reasons.push(`no proof bundle for cited fragment ${k}`)
  for (const f of b.fragments) {
    if (!cited.has(refKey(f.ref))) reasons.push(`bundle for uncited fragment ${refKey(f.ref)}`)
    const r = await verifyFragment(f.bundle, tracker)
    if (!r.ok) reasons.push(`fragment ${refKey(f.ref)}: ${r.reason}`)
  }
  for (const s of b.signers) {
    const f = b.fragments.find(x => refKey(x.ref) === refKey(s.ref))
    const p = s.promise
    if (!f || p.seq !== s.ref.seq || p.leaf !== f.bundle.leaf) { reasons.push(`signer promise does not bind fragment ${refKey(s.ref)}`); continue }
    const msg = concat(TAG_PROMISE, utf8(s.ref.shard), Uint8Array.of(p.seq >>> 24, p.seq >>> 16, p.seq >>> 8, p.seq), fromHex(p.leaf))
    let ok = false
    try { ok = PublicKey.fromString(s.pubkey).verify([...msg], Signature.fromDER(p.signature, 'hex')) } catch (e) { reasons.push(`signer ${refKey(s.ref)}: malformed key/signature (${(e as Error).message})`); continue }
    if (!ok) reasons.push(`signer ${refKey(s.ref)}: inclusion-promise signature invalid`)
  }
  if (b.receipt.delegations?.length) reasons.push(...verifyReceiptDelegations(b.receipt).reasons)
  if (b.policy) {
    if (!opts.dag) reasons.push('policy derivation present but verifier holds no authoritative policy DAG')
    else {
      const pd = b.policy.derivation
      const d = verifyDerivation({ ...pd, proofs: pd.proofs.map(p => ({ h: p.h, proof: { index: p.proof.index, siblings: p.proof.siblings.map(fromHex) } })) }, opts.dag, b.policy.edb)
      if (!d.ok) reasons.push(`policy derivation: ${d.reason}`)
    }
  }
  return { ok: reasons.length === 0, reasons }
}
