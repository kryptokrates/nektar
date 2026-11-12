// §9 / GOAL-v6 S4 — OWL 2 RL/QL axioms compiled to the L_pol Datalog engine.
// The paper asks only for the tractable profiles ("DL-Lite and OWL 2, which
// support tractable reasoning"); full OWL 2 DL is undecidable and stays out of
// scope. The input is a typed axiom list (JSON-serialisable — no RDF/XML
// parsing); every axiom compiles to committed EDB facts and/or Datalog rules
// with stable rule ids `owlrl:<kind>#<i>` (#i = the axiom's index in the
// committed set), so anchored axioms yield deterministic §14 derivations.
//
// Vocabulary (all constants lower-case normalised, same lc() as ontology.ts):
//   inst(X, C)           class membership          t(X, P, Y)  triple
//   subclass(C, D)       materialised hierarchy    subprop(P, Q)
//   same(A, B)           individual equality       diff(A, B)  inequality
//   neg_t(X, P, Y)       negative object assertion disjoint(C, D)
//   conflict()           contradiction marker — a derived conflict() means the
//                        ontology is inconsistent; callers/verifiers reject
//                        (mirrors the AORG `neg_` contradiction convention).
//
// RL rule coverage (W3C OWL 2 RL/RDF rule names):
//   covered — cax-sco (incl. transitive chain materialisation), cax-eqc1/eqc2,
//     cax-dw (→ conflict), prp-spo1 (+ transitive subprop closure), prp-spo2
//     (propertyChain, general k ≥ 2), prp-eqp1/eqp2, prp-dom, prp-rng,
//     prp-inv1/inv2, prp-symp, prp-trp, prp-adp (allDifferent expansion),
//     eq-sym, eq-trans, eq-rep-s/eq-rep-o (subject/object replacement) and
//     class-instance replacement, eq-diff (same+diff → conflict), plus
//     NegativeObjectPropertyAssertion inconsistency → conflict.
//   deferred — class expressions (intersectionOf/unionOf/complementOf,
//     someValuesFrom/allValuesFrom/hasValue/hasSelf/oneOf, cardinality),
//     prp-fp/prp-ifp (functional/inverse-functional properties), prp-key,
//     prp-irp/prp-asyp (irreflexive/asymmetric), reflexive properties,
//     eq-ref/eq-rep-p (reflexive sameAs, predicate-position replacement),
//     datatype rules (dt-*) — L_pol terms are atomic constants, no literals —
//     and owl:Thing/owl:Nothing universals.
import { Datalog, derivationPath, parseProgram, type DerivedFact, type DerivationPath, type Fact, type Literal, type Rule } from '@nektar/datalog'
import { dsha256, utf8, toHex } from '@nektar/core'
import { PolicyDAG, factCanonical, type NodeKind } from './policy.js'

export class OwlError extends Error {}

/** typed OWL 2 RL axiom list — itself a tiny JSON shape (no RDF parsing) */
export type OwlAxiom =
  | { kind: 'subClassOf'; sub: string; sup: string } // cax-sco
  | { kind: 'subPropertyOf'; sub: string; sup: string } // prp-spo1
  | { kind: 'domain'; prop: string; cls: string } // prp-dom
  | { kind: 'range'; prop: string; cls: string } // prp-rng
  | { kind: 'inverseOf'; p: string; q: string } // prp-inv1/inv2
  | { kind: 'symmetric'; prop: string } // prp-symp
  | { kind: 'transitive'; prop: string } // prp-trp
  | { kind: 'propertyChain'; chain: string[]; sup: string } // prp-spo2, chain ≥ 2
  | { kind: 'sameAs'; a: string; b: string } // eq-*
  | { kind: 'differentIndividuals'; a: string; b: string } // differentFrom
  | { kind: 'allDifferent'; members: string[] } // prp-adp
  | { kind: 'equivalentClass'; a: string; b: string } // cax-eqc1/eqc2
  | { kind: 'equivalentProperty'; a: string; b: string } // prp-eqp1/eqp2
  | { kind: 'disjointWith'; a: string; b: string } // cax-dw → conflict
  | { kind: 'classAssertion'; cls: string; ind: string } // ABox EDB
  | { kind: 'propertyAssertion'; prop: string; from: string; to: string } // ABox EDB
  | { kind: 'negativeObjectAssertion'; prop: string; from: string; to: string } // → conflict

// IRIs/names → Datalog constants: same normalisation as ontology.ts lc()
const lc = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9_-]/g, '_')
    .replace(/^_/, 'x_')

/** canonical axiom serialisation — fixed key order per kind, so the hash is
 *  independent of how the caller built the object */
export function owlAxiomCanonical(a: OwlAxiom): string {
  switch (a.kind) {
    case 'subClassOf':
    case 'subPropertyOf':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, sub: a.sub, sup: a.sup })}`
    case 'domain':
    case 'range':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, prop: a.prop, cls: a.cls })}`
    case 'inverseOf':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, p: a.p, q: a.q })}`
    case 'sameAs':
    case 'differentIndividuals':
    case 'equivalentClass':
    case 'equivalentProperty':
    case 'disjointWith':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, a: a.a, b: a.b })}`
    case 'symmetric':
    case 'transitive':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, prop: a.prop })}`
    case 'propertyChain':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, chain: a.chain, sup: a.sup })}`
    case 'allDifferent':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, members: a.members })}`
    case 'classAssertion':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, cls: a.cls, ind: a.ind })}`
    case 'propertyAssertion':
    case 'negativeObjectAssertion':
      return `owlaxiom:${JSON.stringify({ kind: a.kind, prop: a.prop, from: a.from, to: a.to })}`
  }
}
/** the per-axiom anchor token — what `axiom:<h>` records commit (v4 'law' shard
 *  pattern) and what the `anchored` predicate of `entail` is queried with */
export const owlAxiomHash = (a: OwlAxiom): string => toHex(dsha256(utf8(owlAxiomCanonical(a))))

const lit = (pred: string, ...args: string[]): Literal => ({ pred, args, neg: false })
const fact = (pred: string, ...args: string[]): Fact => ({ pred, args })
const rule = (id: string, head: Literal, body: Literal[]): Rule => ({ id, head, body })

// Fixed library: the rules that are NOT per-axiom — schema-relational closure
// (subclass/subprop transitivity), equality (sameAs is data, not a rule), and
// the inconsistency markers. Rule ids are the RL table names under `owlrl:`.
const OWLRL_LIB = parseProgram(`@id owlrl:cax-sco-trans
subclass(A, C) :- subclass(A, B), subclass(B, C).
@id owlrl:prp-spo-trans
subprop(A, C) :- subprop(A, B), subprop(B, C).
@id owlrl:eq-sym
same(X, Y) :- same(Y, X).
@id owlrl:eq-trans
same(X, Z) :- same(X, Y), same(Y, Z).
@id owlrl:eq-rep-cls
inst(Y, C) :- inst(X, C), same(X, Y).
@id owlrl:eq-rep-subj
t(Y, P, Z) :- t(X, P, Z), same(X, Y).
@id owlrl:eq-rep-obj
t(X, P, Z) :- t(X, P, Y), same(Y, Z).
@id owlrl:diff-sym
diff(Y, X) :- diff(X, Y).
@id owlrl:diff-irr-conflict
conflict() :- diff(X, X).
@id owlrl:npa-conflict
conflict() :- t(X, P, Y), neg_t(X, P, Y).
@id owlrl:eq-diff-conflict
conflict() :- same(X, Y), diff(X, Y).
@id owlrl:cax-dw-conflict
conflict() :- inst(X, C), inst(X, D), disjoint(C, D).`).rules

/** compile one axiom (index i in the committed set) to facts + rules.
 *  Rule-shaped axioms emit per-axiom rules `owlrl:<kind>#<i>` (and `#<i>:<n>`
 *  for multi-rule axioms) — τ commits the exact axiom to §14 derivations. */
function compileAxiom(a: OwlAxiom, i: number, facts: Fact[], rules: Rule[]): void {
  const id = (n?: number) => `owlrl:${a.kind}#${i}${n === undefined ? '' : `:${n}`}`
  switch (a.kind) {
    case 'subClassOf': // C ⊑ D: inst(x,C) → inst(x,D); subclass fact feeds chain materialisation
      facts.push(fact('subclass', lc(a.sub), lc(a.sup)))
      rules.push(rule(id(), lit('inst', 'X', lc(a.sup)), [lit('inst', 'X', lc(a.sub))]))
      break
    case 'subPropertyOf': // P ⊑ Q: t(x,P,y) → t(x,Q,y); subprop fact feeds transitivity
      facts.push(fact('subprop', lc(a.sub), lc(a.sup)))
      rules.push(rule(id(), lit('t', 'X', lc(a.sup), 'Y'), [lit('t', 'X', lc(a.sub), 'Y')]))
      break
    case 'domain': // t(x,P,y) → inst(x,C)
      rules.push(rule(id(), lit('inst', 'X', lc(a.cls)), [lit('t', 'X', lc(a.prop), 'Y')]))
      break
    case 'range': // t(x,P,y) → inst(y,C)
      rules.push(rule(id(), lit('inst', 'Y', lc(a.cls)), [lit('t', 'X', lc(a.prop), 'Y')]))
      break
    case 'inverseOf': // t(x,P,y) ⇔ t(y,Q,x) — both directions (prp-inv1 + prp-inv2)
      rules.push(rule(id(1), lit('t', 'Y', lc(a.q), 'X'), [lit('t', 'X', lc(a.p), 'Y')]))
      rules.push(rule(id(2), lit('t', 'Y', lc(a.p), 'X'), [lit('t', 'X', lc(a.q), 'Y')]))
      break
    case 'symmetric': // t(x,P,y) → t(y,P,x)
      rules.push(rule(id(), lit('t', 'Y', lc(a.prop), 'X'), [lit('t', 'X', lc(a.prop), 'Y')]))
      break
    case 'transitive': // t(x,P,y), t(y,P,z) → t(x,P,z)
      rules.push(rule(id(), lit('t', 'X', lc(a.prop), 'Z'), [lit('t', 'X', lc(a.prop), 'Y'), lit('t', 'Y', lc(a.prop), 'Z')]))
      break
    case 'propertyChain': {
      // t(x,P1,·),…,t(·,Pk,z) → t(x,Q,z), general k ≥ 2
      if (a.chain.length < 2) throw new OwlError('propertyChain needs ≥ 2 properties (subPropertyOf covers k = 1)')
      const k = a.chain.length
      rules.push(
        rule(
          id(),
          lit('t', 'X0', lc(a.sup), `X${k}`),
          a.chain.map((p, j) => lit('t', `X${j}`, lc(p), `X${j + 1}`))
        )
      )
      break
    }
    case 'sameAs': // individual equality — EDB; the eq-* library rewrites terms
      facts.push(fact('same', lc(a.a), lc(a.b)))
      break
    case 'differentIndividuals': // owl:differentFrom — EDB; diff-sym closes symmetry, diff(x,x) is inconsistent
      facts.push(fact('diff', lc(a.a), lc(a.b)))
      break
    case 'allDifferent': // prp-adp: AllDifferent(x₁…xₙ) expands to the n(n−1)/2 pairs
      for (let j = 0; j < a.members.length; j++) {
        for (let m = j + 1; m < a.members.length; m++) facts.push(fact('diff', lc(a.members[j]!), lc(a.members[m]!)))
      }
      break
    case 'equivalentClass': // C ≡ D — both directions, as rules AND schema facts
      facts.push(fact('subclass', lc(a.a), lc(a.b)), fact('subclass', lc(a.b), lc(a.a)))
      rules.push(rule(id(1), lit('inst', 'X', lc(a.b)), [lit('inst', 'X', lc(a.a))]))
      rules.push(rule(id(2), lit('inst', 'X', lc(a.a)), [lit('inst', 'X', lc(a.b))]))
      break
    case 'equivalentProperty': // P ≡ Q — both directions, as rules AND schema facts
      facts.push(fact('subprop', lc(a.a), lc(a.b)), fact('subprop', lc(a.b), lc(a.a)))
      rules.push(rule(id(1), lit('t', 'X', lc(a.b), 'Y'), [lit('t', 'X', lc(a.a), 'Y')]))
      rules.push(rule(id(2), lit('t', 'X', lc(a.a), 'Y'), [lit('t', 'X', lc(a.b), 'Y')]))
      break
    case 'disjointWith': // cax-dw: inst(x,C), inst(x,D) → conflict (both orders)
      facts.push(fact('disjoint', lc(a.a), lc(a.b)), fact('disjoint', lc(a.b), lc(a.a)))
      break
    case 'classAssertion': // ABox EDB
      facts.push(fact('inst', lc(a.ind), lc(a.cls)))
      break
    case 'propertyAssertion': // ABox EDB
      facts.push(fact('t', lc(a.from), lc(a.prop), lc(a.to)))
      break
    case 'negativeObjectAssertion': // ¬P(x,y) — EDB; npa-conflict marks inconsistency
      facts.push(fact('neg_t', lc(a.from), lc(a.prop), lc(a.to)))
      break
  }
}

function compilePairs(pairs: { a: OwlAxiom; i: number }[]): { facts: Fact[]; rules: Rule[] } {
  const facts: Fact[] = []
  const rules: Rule[] = [...OWLRL_LIB]
  for (const { a, i } of pairs) compileAxiom(a, i, facts, rules)
  return { facts, rules }
}

/** OWL 2 RL axioms → Datalog facts + rules. `i` in `owlrl:<kind>#<i>` is the
 *  axiom's position in this list, so a committed set's rule ids are stable. */
export function compileOwl(axioms: OwlAxiom[]): { facts: Fact[]; rules: Rule[] } {
  return compilePairs(axioms.map((a, i) => ({ a, i })))
}

export interface OwlEntailResult {
  entailed: boolean
  /** true when the closure derives conflict() — the ontology is inconsistent
   *  and every answer must be rejected by the caller/verifier */
  conflict: boolean
  /** the full derived closure (EDB + IDB, canonical provenance) */
  facts: Map<string, DerivedFact>
  /** the §14 committed derivation path for the query; null when not entailed
   *  or when the goal is itself a committed EDB fact (attest that as a
   *  committed node, not as a derivation) */
  path: DerivationPath | null
  used: number // anchored axioms consumed
  rejected: string[] // axiom hashes filtered out (foreign / unanchored)
}

/**
 * π ⊨ q under the OWL-compiled rules, restricted to axioms whose
 * `owlAxiomHash` the `anchored` predicate accepts — "void ab initio"
 * otherwise, exactly like ontologyEntails. `facts` are caller EDB (runtime
 * triples/predicates), axiom assertions are committed EDB. Query predicates:
 * inst(x, C), t(x, P, y), subclass(C, D), subprop(P, Q), same/diff, conflict().
 */
export function entail(
  axioms: OwlAxiom[],
  facts: Fact[],
  query: { pred: string; args: string[] },
  anchored: (h: string) => boolean = () => true
): OwlEntailResult {
  const ok: { a: OwlAxiom; i: number }[] = []
  const rejected: string[] = []
  axioms.forEach((a, i) => {
    const h = owlAxiomHash(a)
    if (anchored(h)) ok.push({ a, i })
    else rejected.push(h)
  })
  const { facts: ef, rules } = compilePairs(ok)
  const edb = [...ef, ...facts.map((f) => ({ pred: lc(f.pred), args: f.args.map(lc) }))]
  const derived = new Datalog(rules).run(edb)
  const hit = derived.get(`${lc(query.pred)}(${query.args.map(lc).join(',')})`)
  const conflict = derived.has('conflict()')
  const path = hit && hit.prov.ruleId !== 'edb' ? derivationPath(derived, hit.pred, hit.args) : null
  return { entailed: hit !== undefined, conflict, facts: derived, path, used: ok.length, rejected }
}

/**
 * A committed axiom set: the axioms themselves, their compiled rules and their
 * compiled EDB facts are all nodes of ONE PolicyDAG (layer 'owlrl'), so
 * `ont.root` is a POL-004-style root that anchors exactly like the policy DAG
 * root — write `dagroot:<root>` into a shard (expectedPolicyRoot already
 * accepts it) or configure it as `anchoredPolicyRoot`. Individual axioms can
 * additionally anchor as `axiom:<owlAxiomHash>` records (the v4 'law' flow).
 *
 * §14 plug-in: pass `ont.dag` as `opts.policyDag` to NektarMemory — every
 * compiled rule is a committed rule node (τ) and every axiom-compiled EDB fact
 * is a committed fact node, so `proveDerivation`/`verifyDerivation` attest OWL
 * derivations byte-identically. An `axiom` node additionally commits each
 * axiom's canonical form under the same root.
 */
export class CommittedOntology {
  readonly axioms: OwlAxiom[]
  readonly facts: Fact[] // compiled EDB (assertions + schema facts), committed
  readonly dag: PolicyDAG // rule nodes + committed fact/axiom nodes, layer 'owlrl'
  readonly axiomHashes: string[] // parallel to `axioms` — the anchor tokens
  constructor(axioms: OwlAxiom[]) {
    this.axioms = [...axioms]
    const c = compileOwl(this.axioms)
    this.facts = c.facts
    this.dag = new PolicyDAG([{ name: 'owlrl', rules: c.rules }])
    this.axiomHashes = this.axioms.map(owlAxiomHash)
    for (const f of this.facts) this.commitNode('fact', factCanonical(f))
    for (const [i, a] of this.axioms.entries()) this.commitNode('axiom', owlAxiomCanonical(a), this.axiomHashes[i]!)
  }
  get rules(): Rule[] {
    return this.dag.rules
  }
  /** the ontology root — anchors like the policy DAG root (POL-004) */
  get root(): string {
    return toHex(this.dag.policyRoot())
  }
  private commitNode(kind: NodeKind, canonical: string, h = toHex(dsha256(utf8(canonical)))): string {
    if (!this.dag.nodes.has(h)) this.dag.nodes.set(h, { kind, h, canonical, layer: 'owlrl' })
    return h
  }
  entail(facts: Fact[] = [], query: { pred: string; args: string[] }, anchored: (h: string) => boolean = () => true): OwlEntailResult {
    return entail(this.axioms, facts, query, anchored)
  }
}
export function commitOntology(axioms: OwlAxiom[]): CommittedOntology {
  return new CommittedOntology(axioms)
}
