// §9 memory as law: DL-Lite / OWL2-style axioms compiled to stratified
// Datalog. Entailment uses ONLY axioms whose anchors verify ("void ab initio"
// otherwise). Regulatory codes are jurisdiction-named graphs; precedent is a
// set of anchored past decisions from which a new action must be entailed.
import { Datalog, parseProgram, type Fact, type Rule } from '@nektar/datalog'
import { dsha256, utf8, toHex } from '@nektar/core'

export type Axiom =
  | { kind: 'ClassAssertion'; cls: string; ind: string }
  | { kind: 'SubClassOf'; sub: string; sup: string }
  | { kind: 'ObjectPropertyAssertion'; prop: string; from: string; to: string }
  | { kind: 'Rule'; text: string } // L_pol rule (e.g. Permitted(A) conditions)

export const axiomHash = (a: Axiom): string => toHex(dsha256(utf8(`nektar:axiom:v1|${JSON.stringify(a)}`)))
// IRIs/names → Datalog constants: case-insensitive, lower-case, safe characters
const lc = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9_-]/g, '_')
    .replace(/^_/, 'x_')

// DL core: instance propagation through the (transitive) subclass hierarchy
const DL_RULES = parseProgram(`@id dl-sub-trans
subclass(A, C) :- subclass(A, B), subclass(B, C).
@id dl-inst
instance(X, C) :- instance(X, B), subclass(B, C).`).rules

export function compile(axioms: Axiom[]): { facts: Fact[]; rules: Rule[] } {
  const facts: Fact[] = []
  const rules: Rule[] = []
  for (const a of axioms) {
    if (a.kind === 'ClassAssertion') facts.push({ pred: 'instance', args: [lc(a.ind), lc(a.cls)] })
    else if (a.kind === 'SubClassOf') facts.push({ pred: 'subclass', args: [lc(a.sub), lc(a.sup)] })
    else if (a.kind === 'ObjectPropertyAssertion') facts.push({ pred: lc(a.prop), args: [lc(a.from), lc(a.to)] })
    else rules.push(...parseProgram(a.text).rules)
  }
  return { facts, rules }
}

export interface EntailResult {
  entailed: boolean
  used: number
  rejected: string[]
}
/** π ⊨ q under the anchored axioms only; `anchored(hash)` must return true for an axiom to count */
export function ontologyEntails(axioms: Axiom[], goal: { pred: string; args: string[] }, anchored: (h: string) => boolean): EntailResult {
  const ok = axioms.filter((a) => anchored(axiomHash(a)))
  const rejected = axioms.filter((a) => !ok.includes(a)).map(axiomHash)
  const { facts, rules } = compile(ok)
  const derived = new Datalog([...DL_RULES, ...rules]).run(facts)
  return { entailed: derived.has(`${goal.pred}(${goal.args.map(lc).join(',')})`), used: ok.length, rejected }
}

/** regulatory codes: G_r = axioms of jurisdiction r; Permissible(a) ⇔ G_r ⊨ permitted(a) */
export class Jurisdictions {
  private graphs = new Map<string, Axiom[]>()
  add(jurisdiction: string, axioms: Axiom[]): void {
    this.graphs.set(jurisdiction, [...(this.graphs.get(jurisdiction) ?? []), ...axioms])
  }
  permissible(action: string, jurisdiction: string, anchored: (h: string) => boolean): EntailResult {
    const g = this.graphs.get(jurisdiction)
    if (!g) return { entailed: false, used: 0, rejected: [] }
    return ontologyEntails(g, { pred: 'permitted', args: [action] }, anchored)
  }
}

/** precedent: NewAction(A) valid ⇔ P ⊨ Permitted(A), with P the anchored precedent decisions (+ shared rules) */
export function validByPrecedent(precedents: Axiom[], action: string, anchored: (h: string) => boolean): EntailResult {
  return ontologyEntails(precedents, { pred: 'permitted', args: [action] }, anchored)
}
