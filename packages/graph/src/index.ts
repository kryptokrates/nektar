// @mamem/graph — AORG (append-only reasoning graph), P11.
// Vertex v=(kind, phi, σ, t, context, h); edges carry a rule tag, committed.
// Enforced at append: acyclicity (premises must exist), justifiability (rule
// derives φ via @mamem/datalog), consistency (no contradiction with ancestors
// unless refinement), traceability (derived vertices bottom out in episodes).
// Refinements invalidate bi-temporally — nothing is ever mutated or deleted.
export * from './policy.js'
export * from './instance.js'
export * from './anchor.js'
export * from './ontology.js'
import { dsha256, utf8 } from '@mamem/core'
import { Datalog, type Fact, type Literal, type Rule } from '@mamem/datalog'

export class GraphError extends Error {}

export type VertexKind = 'episode' | 'axiom' | 'empirical' | 'derived' | 'refinement'
export interface VertexTime { logPos: number, height?: number }
export interface Vertex {
  kind: VertexKind
  phi: string // canonical literal for non-episode kinds; verbatim text for episode
  t: VertexTime
  context: string[] // C_i context vector
  sigma: string[] // signature hexes over H(phi‖t‖context)
  h: string // dsha256(phi‖σ‖t‖context)
  refines?: string // set on refinement vertices
}
export interface Edge { from: string, to: string, rule: string, e: string }
const cat = (...xs: string[]) => xs.join('|')

function vhash (phi: string, sigma: string[], t: VertexTime, context: string[]): string {
  const ser = `${phi}|${sigma.join(',')}|${t.logPos}:${t.height ?? ''}|${context.join(',')}`
  return Buffer.from(dsha256(utf8(ser))).toString('hex')
}
export function signMaterial (phi: string, t: VertexTime, context: string[]): Uint8Array {
  return dsha256(utf8(`${phi}|${t.logPos}:${t.height ?? ''}|${context.join(',')}`))
}

export class AORGraph {
  readonly vertices = new Map<string, Vertex>()
  readonly edges: Edge[] = []
  private catalogue: Map<string, Rule>
  private invalidAt = new Map<string, number>()
  constructor (catalogue: Rule[]) {
    this.catalogue = new Map(catalogue.map(r => [r.id, r]))
  }

  private ancestors (h: string, out = new Set<string>()): Set<string> {
    if (out.has(h)) return out
    out.add(h)
    for (const e of this.edges) if (e.to === h) this.ancestors(e.from, out)
    return out
  }
  private contradicts (a: string, b: string): boolean {
    return (a === `neg_${b}`) || (b === `neg_${a}`) ||
      (a.startsWith('permit(') && b.startsWith('deny(') && a.slice(7) === b.slice(4)) ||
      (b.startsWith('permit(') && a.startsWith('deny(') && b.slice(7) === a.slice(4))
  }

  append (v: Omit<Vertex, 'h'>, premises: string[], ruleId?: string): Vertex {
    // acyclicity: premises must exist, no self-ref
    for (const p of premises) {
      if (!this.vertices.has(p)) throw new GraphError(`unknown premise ${p.slice(0, 12)}`)
      if (v.phi && this.ancestors(p).has(vhash(v.phi, v.sigma, v.t, v.context))) {
        // computed below — checked after h assignment instead
      }
    }
    const h = vhash(v.phi, v.sigma, v.t, v.context)
    if (this.vertices.has(h)) throw new GraphError('duplicate vertex')
    if (premises.includes(h)) throw new GraphError('self-cycle rejected')
    for (const p of premises) if (this.ancestors(p).has(h)) throw new GraphError('cycle rejected')

    const vertex: Vertex = { ...v, h }
    if (v.kind === 'derived' || v.kind === 'refinement') {
      // consistency first: a claim that contradicts an ancestor is rejected
      // with the contradiction reason (unless it is a refinement)
      if (v.kind !== 'refinement') {
        const anc = new Set<string>(premises)
        for (const p of premises) for (const a of this.ancestors(p)) anc.add(a)
        for (const a of anc) {
          const av = this.vertices.get(a)!
          if (this.contradicts(v.phi, av.phi)) {
            throw new GraphError(`contradiction with ancestor ${a.slice(0, 12)} (${av.phi})`)
          }
        }
      }
      // justifiability: rule must exist and derive phi from premises
      const rule = ruleId && this.catalogue.get(ruleId)
      if (!rule) throw new GraphError(`unjustified: rule '${ruleId}' not in catalogue`)
      const dl = new Datalog([rule, ...this.catalogue.values()])
      const premFacts: Fact[] = premises.map(p => {
        const pv = this.vertices.get(p)!
        const lit = pv.phi.match(/^([a-z][a-zA-Z0-9_-]*)\((.*)\)$/)!
        return { pred: lit[1]!, args: lit[2]!.split(',').map(a => a.trim()) }
      })
      const facts = dl.run(premFacts)
      const goal = v.phi.match(/^([a-z][a-zA-Z0-9_-]*)\((.*)\)$/)!
      if (!facts.has(`${goal[1]}(${goal[2]!.split(',').map(a => a.trim()).join(',')})`)) {
        throw new GraphError(`unjustified: rule '${ruleId}' does not derive '${v.phi}'`)
      }
      // traceability: the transitive closure over this vertex's premises must
      // bottom out in episode/axiom/empirical and reach ≥1 episode (AOR-008b)
      if (v.kind === 'derived') {
        const closure = new Set<string>(premises)
        for (const p of premises) for (const a of this.ancestors(p)) closure.add(a)
        let episode = false
        for (const a of closure) {
          const av = this.vertices.get(a)!
          if (av.kind === 'episode') episode = true
          if (!['episode', 'axiom', 'empirical', 'derived', 'refinement'].includes(av.kind)) throw new GraphError('untraceable ancestor kind')
        }
        if (!episode) throw new GraphError('traceability: derived vertex has no episode ancestor')
      }
    }
    // refinement: must reference a vertex it refines
    if (v.kind === 'refinement' && !v.refines) throw new GraphError('refinement vertex missing refines')
    this.vertices.set(h, vertex)
    for (const p of premises) {
      this.edges.push({ from: p, to: h, rule: ruleId ?? 'assert', e: Buffer.from(dsha256(utf8(cat(p, h, ruleId ?? 'assert')))).toString('hex') })
    }
    // bi-temporal invalidation for refinements
    if (v.kind === 'refinement' && v.refines) this.invalidAt.set(v.refines, v.t.logPos)
    return vertex
  }

  /** valid facts at logical time t (refinements invalidate from invalid_at) */
  currentView (atT = Infinity): Vertex[] {
    return [...this.vertices.values()].filter(v => { const iv = this.invalidAt.get(v.h); return iv === undefined || iv > atT })
  }
  history (h: string): Vertex | undefined { return this.vertices.get(h) }

  /** subgraph scoping G_i^k: vertices sharing ≥1 context with Ψ, closed under edges */
  instance (psi: string[]): Set<string> {
    const out = new Set<string>()
    for (const v of this.vertices.values()) {
      if (v.context.some(c => psi.includes(c))) out.add(v.h)
    }
    // closure: include ancestors of selected vertices
    for (const h of [...out]) for (const a of this.ancestors(h)) out.add(a)
    return out
  }

  /** Metanet-compatible head export (AOR-011) */
  metanetHead (nodeKey: string, parentTxid: string, txid: string) {
    return { protocol: 'meta', nodeKey, parentTxid, txid }
  }

  /** reverse citation index hook (STO-005): h → citing vertex hashes */
  citing (h: string): string[] { return this.edges.filter(e => e.from === h).map(e => e.to) }
}
