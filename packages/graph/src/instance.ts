// Instancing + delegation (INS-002..006): Instance(G,Ψ) selection + closure,
// per-node Merkle proof against an anchored root, delegation signature
// σ_A′ = Sign(H(G′root‖Ψ‖τ)), partial entailment under a rule subset.
import { dsha256, utf8, merkleRoot, merkleProof } from '@nektar/core'
/** Structural minimum of AORGraph this module needs — declared here (never
 *  imported from the barrel) so instance.ts has no edge back into index.ts
 *  and the package stays acyclic. AORGraph satisfies this shape. */
export interface GraphLike {
  instance: (psi: string[]) => Set<string>
  edges: Iterable<{ from: string; to: string }>
  vertices: Map<string, { kind: string; phi: string }>
}
import { PrivateKey, PublicKey, Signature } from '@bsv/sdk'
import { Datalog, type Fact, type Rule } from '@nektar/datalog'

export interface Instance {
  psi: string[]
  members: Set<string>
  root: Uint8Array // Merkle root over the instance's member hashes, sorted
}

/** INS-002/003: select nodes with C_i ∩ Ψ ≠ ∅, closed under derivability */
export function instanceOf(g: GraphLike, psi: string[]): Instance {
  const members = g.instance(psi) // AORG.instance already includes ancestor closure
  const hs = [...members].sort().map((h) => new Uint8Array(Buffer.from(h, 'hex')))
  const root = hs.length === 0 ? dsha256(utf8('empty-instance')) : merkleRoot(hs)
  return { psi, members, root }
}

/** INS-004: a Merkle proof for every node in the instance against its root */
export function memberProof(inst: Instance, h: string) {
  const hs = [...inst.members].sort()
  const i = hs.indexOf(h)
  if (i < 0) throw new Error('INS-004: vertex not in instance')
  return merkleProof(
    hs.map((x) => new Uint8Array(Buffer.from(x, 'hex'))),
    i
  )
}

/** INS-005: delegation σ = Sign(H(G′root ‖ Ψ ‖ τ)) — binds delegator + expiry */
export function delegate(key: PrivateKey, inst: Instance, expiry: number): string {
  const msg = dsha256(new Uint8Array([...inst.root, ...utf8(`|${inst.psi.join(',')}|${expiry}`)]))
  return key.sign(Array.from(msg)).toDER('hex') as string
}
export function verifyDelegation(pub: PublicKey, inst: Instance, expiry: number, sig: string, atTime = Date.now()): { ok: boolean; reason?: string } {
  if (atTime > expiry) return { ok: false, reason: 'delegation expired' }
  const msg = dsha256(new Uint8Array([...inst.root, ...utf8(`|${inst.psi.join(',')}|${expiry}`)]))
  let parsed: Signature
  try {
    parsed = Signature.fromDER(sig, 'hex')
  } catch {
    return { ok: false, reason: 'malformed DER signature' }
  }
  if (!pub.verify(Array.from(msg), parsed)) return { ok: false, reason: 'signature does not bind this instance root/Ψ/τ to the delegator' }
  return { ok: true }
}

/** INS-004: full instance verification — every member anchored (via
 * GraphAnchor's recorded refs), closure under derivability (every premise of
 * a member is itself a member), delegation valid + unexpired, and the signed
 * root recomputes from the member set. */
export function verifyInstance(
  g: GraphLike,
  inst: Instance,
  anchored: Map<string, { anchorTxid?: string }>, // h → GraphAnchor ref
  delegation: { pub: PublicKey; expiry: number; sig: string },
  atTime = Date.now()
): { ok: boolean; reason?: string } {
  // closure: every edge premise of a member must be a member
  for (const e of g.edges) {
    if (inst.members.has(e.to) && !inst.members.has(e.from)) {
      return { ok: false, reason: `closure violation: premise ${e.from.slice(0, 12)} missing from instance` }
    }
  }
  // every member must be anchored
  for (const h of inst.members) {
    if (!anchored.get(h)?.anchorTxid) return { ok: false, reason: `member ${h.slice(0, 12)} not anchored` }
  }
  // root must match the member set
  const hs = [...inst.members].sort().map((h) => new Uint8Array(Buffer.from(h, 'hex')))
  const root = hs.length === 0 ? dsha256(utf8('empty-instance')) : merkleRoot(hs)
  if (Buffer.from(root).toString('hex') !== Buffer.from(inst.root).toString('hex')) {
    return { ok: false, reason: 'instance root does not match its member set' }
  }
  const d = verifyDelegation(delegation.pub, inst, delegation.expiry, delegation.sig, atTime)
  if (!d.ok) return { ok: false, reason: `delegation: ${d.reason}` }
  return { ok: true }
}

/** INS-006: partial entailment restricted to a rule subset R_q — run the
 * datalog engine over the instance's fact vertices using ONLY rules in R_q.
 * A goal derivable under the full R but not under R_q is reported not
 * entailed, with the derivation when entailed. */
export function entails(g: GraphLike, inst: Instance, goal: string, rulesSubset: Set<string>, catalogue: Rule[]): { entailed: boolean; derivation: string[] } {
  const facts: Fact[] = [...inst.members].flatMap((h) => {
    const v = g.vertices.get(h)
    if (!v || v.kind === 'derived' || v.kind === 'refinement') return []
    const m = v.phi.match(/^([a-z][a-zA-Z0-9_-]*)\((.*)\)$/)
    return m ? [{ pred: m[1]!, args: m[2]!.split(',').map((a) => a.trim()) }] : []
  })
  const dl = new Datalog(catalogue.filter((r) => rulesSubset.has(r.id)))
  const out = dl.run(facts, rulesSubset)
  const m = goal.match(/^([a-z][a-zA-Z0-9_-]*)\((.*)\)$/)!
  const d = dl.derivation(
    out,
    m[1]!,
    m[2]!.split(',').map((a) => a.trim())
  )
  return { entailed: d !== null, derivation: d ? d.premises : [] }
}
