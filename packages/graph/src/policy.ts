// Policy DAG (POL-003/004/006/008/009): versioned rule nodes committed as
// h_i = dsha256(canonical(rule)) incl. body+negations; derivation edges
// e_ij = (h_i,h_j,τ,H(h_i‖h_j‖τ)); layered graphs with override succession;
// conflict detection with full-path localisation; derivation paths with
// per-node Merkle proofs against the anchored policy root.
import { dsha256, utf8, merkleRoot, merkleProof, verifyProof, type MerkleProof } from '@mamem/core'
import { Datalog, type Rule, type Fact, type Literal } from '@mamem/datalog'

export class PolicyError extends Error {}

export interface PolicyLayer { name: string, rules: Rule[], /** rule ids superseded by this layer (v′ ≻ v) */ overrides?: string[] }
export type NodeKind = 'rule' | 'fact' | 'axiom' // 'axiom' nodes: committed OWL axiom canonicals (graph/owlrl)
export interface PolicyNode {
  kind: NodeKind
  h: string
  /** canonical serialisation committed to (rule: full id/layer/head/body; fact: literal) */
  canonical: string
  layer: string
  supersedes?: string // h of the overridden rule node (POL-009)
}
export interface PolicyEdge { from: string, to: string, rule: string, e: string }

const ehash = (a: string, b: string, t: string) => Buffer.from(dsha256(utf8(`${a}|${b}|${t}`))).toString('hex')
export function ruleCanonical (r: Rule, layer: string): string {
  return JSON.stringify({
    id: r.id, layer, version: 1,
    head: `${r.head.pred}(${r.head.args.join(',')})`,
    body: r.body.map(l => `${l.neg ? 'not ' : ''}${l.pred}(${l.args.join(',')})`).join(' ')
  })
}
export function factCanonical (f: Fact): string { return `fact:${f.pred}(${f.args.join(',')})` }

export class PolicyDAG {
  readonly nodes = new Map<string, PolicyNode>()
  readonly edges: PolicyEdge[] = []
  readonly layers: PolicyLayer[]
  /** effective (non-superseded) rules used by evaluation */
  readonly rules: Rule[]
  readonly ruleNode = new Map<string, string>() // ruleId → node h

  constructor (layers: PolicyLayer[]) {
    this.layers = layers
    const overriddenIds = new Set(layers.flatMap(l => l.overrides ?? []))
    this.rules = layers.flatMap(l => l.rules).filter(r => !overriddenIds.has(r.id))
    // POL-003: commit the FULL rule (id, version, layer, head, body incl. negation)
    for (const l of layers) for (const r of l.rules) {
      const canonical = ruleCanonical(r, l.name)
      const h = Buffer.from(dsha256(utf8(canonical))).toString('hex')
      this.nodes.set(h, { kind: 'rule', h, canonical, layer: l.name })
      this.ruleNode.set(r.id, h)
    }
    // POL-009: per-node supersedes — the overriding node records the exact h
    // of the overridden rule node; the overridden node stays (append-only)
    for (const l of layers) for (const oid of l.overrides ?? []) {
      const oh = this.ruleNode.get(oid)
      if (oh) {
        for (const r of l.rules) {
          const nh = this.ruleNode.get(r.id)!
          const n = this.nodes.get(nh)!
          if (!n.supersedes) { n.supersedes = oh; this.edges.push({ from: nh, to: oh, rule: 'Phi(override)', e: ehash(nh, oh, 'Phi') }) }
        }
      }
    }
  }

  /** POL-004: Merkle root over committed node hashes — anchored like a batch */
  policyRoot (): Uint8Array { return merkleRoot([...this.nodes.keys()].map(h => new Uint8Array(Buffer.from(h, 'hex')))) }

  /** POL-006: Merkle proof of a node hash against the policy root */
  nodeProof (h: string): MerkleProof {
    const hs = [...this.nodes.keys()]
    const i = hs.indexOf(h)
    if (i < 0) throw new PolicyError('node not in DAG')
    return merkleProof(hs.map(x => new Uint8Array(Buffer.from(x, 'hex'))), i)
  }

  private factNode (f: Fact, layer = 'edb'): PolicyNode {
    const canonical = factCanonical(f)
    const h = Buffer.from(dsha256(utf8(canonical))).toString('hex')
    if (!this.nodes.has(h)) this.nodes.set(h, { kind: 'fact', h, canonical, layer })
    return this.nodes.get(h)!
  }

  /** evaluate + conflict detection: full derivation PATHS of both permit and
   * deny reported for localisation (POL-008) */
  evaluate (edb: Fact[]): { facts: Map<string, { pred: string, args: string[], prov: { ruleId: string, premises: Literal[] } }>, conflicts: { claim: string, permitPath: string[], denyPath: string[], layers: string[] }[] } {
    const dl = new Datalog(this.rules)
    const facts = dl.run(edb)
    const conflicts: { claim: string, permitPath: string[], denyPath: string[], layers: string[] }[] = []
    const path = (k: string): string[] => {
      const out: string[] = []
      const seen = new Set<string>()
      const walk = (key: string) => {
        if (seen.has(key)) return
        seen.add(key)
        const f = facts.get(key)!
        out.push(`${f.prov.ruleId}:${f.pred}`)
        for (const p of f.prov.premises) {
          if (!p.neg && facts.has(`${p.pred}(${p.args.join(',')})`)) walk(`${p.pred}(${p.args.join(',')})`)
        }
      }
      walk(k)
      return out
    }
    const layerOf = (ruleId: string) => this.layers.find(l => l.rules.some(r => r.id === ruleId))?.name ?? '?'
    for (const k of facts.keys()) {
      if (k.startsWith('permit(')) {
        const dk = `deny(${k.slice(7)}`
        if (facts.has(dk)) {
          const permitPath = path(k), denyPath = path(dk)
          const layers = [...new Set([...permitPath, ...denyPath].map(s => layerOf(s.split(':')[0]!)))]
          conflicts.push({ claim: k.slice(7, -1), permitPath, denyPath, layers })
        }
      }
    }
    return { facts, conflicts }
  }

  /** POL-006: derivation path π_k + a Merkle proof for EVERY rule and fact
   * node used in the derivation, against the anchored policy root */
  query (edb: Fact[], pred: string, args: string[]) {
    const dl = new Datalog(this.rules)
    const facts = dl.run(edb)
    const root = facts.get(`${pred}(${args.join(',')})`)
    if (!root) return null
    // build the derivation tree, committing fact nodes + derivation edges
    const usedNodeHs = new Set<string>()
    const tree = (f: { pred: string, args: string[], prov: { ruleId: string, premises: Literal[] } }): any => {
      const fnode = this.factNode(f)
      usedNodeHs.add(fnode.h)
      const rh = this.ruleNode.get(f.prov.ruleId)
      if (rh) { usedNodeHs.add(rh); this.edges.push({ from: rh, to: fnode.h, rule: f.prov.ruleId, e: ehash(rh, fnode.h, f.prov.ruleId) }) }
      return {
        fact: `${f.pred}(${f.args.join(',')})`, node: fnode.h, rule: f.prov.ruleId,
        premises: f.prov.premises.map(p => {
          if (p.neg) return { not: `${p.pred}(${p.args.join(',')})` }
          const pf = facts.get(`${p.pred}(${p.args.join(',')})`)
          if (!pf) return { edb: `${p.pred}(${p.args.join(',')})` }
          const child = tree(pf)
          this.edges.push({ from: pf && this.factNode(pf).h, to: fnode.h, rule: f.prov.ruleId, e: ehash(this.factNode(pf).h, fnode.h, f.prov.ruleId) })
          return child
        })
      }
    }
    const t = tree(root)
    const proofs = [...usedNodeHs].map(h => ({ h, proof: this.nodeProof(h) }))
    return { root: Buffer.from(this.policyRoot()).toString('hex'), tree: t, proofs }
  }
}

/** verifyDerivation: recompute each node hash, check each Merkle proof, and
 * re-check every step is a valid application of the referenced rule */
export function verifyDerivation (
  q: { root: string, tree: any, proofs: { h: string, proof: MerkleProof }[] },
  dag: PolicyDAG, edb: Fact[]
): { ok: boolean, reason?: string } {
  // root must match the instance's DAG root — caller-supplied roots are not trusted
  const expected = Buffer.from(dag.policyRoot()).toString('hex')
  if (q.root !== expected) return { ok: false, reason: 'proof against a different policy root' }
  const dl = new Datalog(dag.rules)
  const facts = dl.run(edb)
  const check = (node: any): string | null => {
    const k = node.fact
    const f = facts.get(k)
    if (!f) return `derivation step ${k} not entailed by the rules`
    const rh = dag.ruleNode.get(f.prov.ruleId)
    if (rh && node.node !== undefined) {
      // recompute the fact node hash
      const canon = factCanonical(f)
      if (Buffer.from(dsha256(utf8(canon))).toString('hex') !== node.node) return `node hash mismatch for ${k}`
    }
    for (const p of node.premises ?? []) {
      if (typeof p === 'object' && p.fact) { const e = check(p); if (e) return e }
    }
    return null
  }
  const err = check(q.tree)
  if (err) return { ok: false, reason: err }
  // every proof must verify against the DAG root
  for (const { h, proof } of q.proofs) {
    if (!verifyProof(new Uint8Array(Buffer.from(h, 'hex')), proof, dag.policyRoot())) {
      return { ok: false, reason: `node ${h.slice(0, 12)} Merkle proof invalid` }
    }
  }
  return { ok: true }
}

// POL-016 receipt + Cedar∩DAG authorization helper (the instance-level
// integration in @mamem/mcp evaluates Cedar itself — this is the pure check).
export interface DagDecision {
  decision: 'ALLOW' | 'DENY'
  reason: string
  policyRoot: string // computed from the instance's own DAG — never caller-supplied
  derivationHash?: string
}

export function dagAuthorize (
  dag: PolicyDAG, edb: Fact[],
  cedarAllows: boolean,
  principal: string, action: string, resource: string
): DagDecision {
  const { facts, conflicts } = dag.evaluate(edb)
  const root = Buffer.from(dag.policyRoot()).toString('hex')
  if (conflicts.length > 0) {
    const c = conflicts[0]!
    return { decision: 'DENY', reason: `policy conflict on ${c.claim} (permit ${c.permitPath.join('>')}, deny ${c.denyPath.join('>')}, layers ${c.layers.join('/')})`, policyRoot: root }
  }
  const k = `permit(${principal},${action},${resource})`
  if (!cedarAllows) return { decision: 'DENY', reason: 'Cedar DENY', policyRoot: root }
  if (!facts.has(k)) return { decision: 'DENY', reason: `DAG does not derive ${k}`, policyRoot: root }
  const dl = new Datalog(dag.rules)
  return { decision: 'ALLOW', reason: 'Cedar ALLOW + DAG permit', policyRoot: root, derivationHash: dl.derivHash(facts) }
}
