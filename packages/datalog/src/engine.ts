// Stratified semi-naive Datalog evaluation with provenance.
import { sha256, utf8, toHex } from '@nektar/core'
import { isVar, type Fact, type Literal, type Rule } from './parse.js'

export class CycleError extends Error {}
export class SafetyError extends Error {}

export interface DerivedFact extends Fact {
  prov: { ruleId: string; premises: Literal[] }
}
export type Bindings = Record<string, string>
const key = (p: string, a: string[]) => `${p}(${a.join(',')})`

export class Datalog {
  readonly rules: Rule[]
  readonly strata: Map<string, number>
  constructor(rules: Rule[], edbPreds?: Set<string>) {
    this.rules = rules
    this.strata = this.stratify(rules)
    this.checkSafety(rules)
  }
  private isIdb(p: string): boolean {
    return this.rules.some((r) => r.head.pred === p)
  }

  /** every head var + every negated-literal var must appear in a positive body literal */
  private checkSafety(rules: Rule[]): void {
    for (const r of rules) {
      const bound = new Set<string>()
      for (const l of r.body) if (!l.neg) for (const a of l.args) if (isVar(a)) bound.add(a)
      for (const a of r.head.args) if (isVar(a) && !bound.has(a)) throw new SafetyError(`rule ${r.id}: unsafe head var ${a}`)
      for (const l of r.body) if (l.neg) for (const a of l.args) if (isVar(a) && !bound.has(a)) throw new SafetyError(`rule ${r.id}: unsafe negated var ${a}`)
    }
  }

  /** Standard stratification fixpoint: s(h) ≥ s(p) for a positive body
   *  literal, s(h) ≥ s(p)+1 for a negated one. Positive recursion is allowed;
   *  a stratum exceeding the IDB predicate count means a cycle through
   *  negation → CycleError. */
  private stratify(rules: Rule[]): Map<string, number> {
    const idb = new Set(rules.map((r) => r.head.pred))
    const s = new Map([...idb].map((p) => [p, 0]))
    for (let changed = true; changed;) {
      changed = false
      for (const r of rules) {
        for (const l of r.body) {
          if (!idb.has(l.pred)) continue
          const need = s.get(l.pred)! + (l.neg ? 1 : 0)
          if (s.get(r.head.pred)! < need) {
            if (need > idb.size) throw new CycleError(`negation cycle through ${r.head.pred}`)
            s.set(r.head.pred, need)
            changed = true
          }
        }
      }
    }
    return s
  }

  private subst(args: string[], b: Bindings): string[] {
    return args.map((a) => (isVar(a) ? b[a] : a))
  }

  private unify(lit: Literal, f: Fact, b: Bindings): Bindings | null {
    const nb = { ...b }
    for (let j = 0; j < lit.args.length; j++) {
      const a = lit.args[j]
      if (isVar(a)) {
        if (nb[a] !== undefined && nb[a] !== f.args[j]) return null
        nb[a] = f.args[j]!
      } else if (a !== f.args[j]) return null
    }
    return nb
  }
  /** body join; positive literal `deltaAt` draws from `delta`, all others from `all` (by-predicate indexes) */
  private *matchAll(
    body: Literal[],
    i: number,
    all: Map<string, Fact[]>,
    delta: Map<string, Fact[]> | null,
    deltaAt: number,
    b: Bindings
  ): Generator<Bindings> {
    if (i === body.length) {
      yield b
      return
    }
    const lit = body[i]
    if (lit.neg) {
      for (const f of all.get(lit.pred) ?? []) if (this.unify(lit, f, b)) return // a match exists → negated literal fails
      yield* this.matchAll(body, i + 1, all, delta, deltaAt, b)
      return
    }
    const src = (i === deltaAt && delta ? delta : all).get(lit.pred) ?? []
    for (const f of src) {
      const nb = this.unify(lit, f, b)
      if (nb) yield* this.matchAll(body, i + 1, all, delta, deltaAt, nb)
    }
  }

  /** Semi-naive bottom-up evaluation, stratum by stratum. Provenance is
   *  canonical (POL-005): a fact keeps the derivation from the earliest round
   *  in which it appears, ties broken by the lexicographically smallest
   *  (ruleId, premises) — independent of EDB order and rule order. */
  run(edb: Fact[], rulesSubset?: Set<string>): Map<string, DerivedFact> {
    const facts = new Map<string, DerivedFact>()
    const index = new Map<string, Fact[]>()
    const push = (m: Map<string, Fact[]>, f: Fact) => {
      const a = m.get(f.pred)
      if (a) a.push(f)
      else m.set(f.pred, [f])
    }
    for (const f of edb) {
      const k = key(f.pred, f.args)
      if (!facts.has(k)) {
        const d = { pred: f.pred, args: f.args, prov: { ruleId: 'edb', premises: [] } }
        facts.set(k, d)
        push(index, d)
      }
    }
    const rs = this.rules.filter((r) => !rulesSubset || rulesSubset.has(r.id))
    const provKey = (p: DerivedFact['prov']) => `${p.ruleId}|${p.premises.map((l) => `${l.neg ? '!' : ''}${l.pred}(${l.args.join(',')})`).join(',')}`
    const maxS = Math.max(0, ...this.strata.values())
    for (let s = 0; s <= maxS; s++) {
      const stratum = rs.filter((r) => (this.strata.get(r.head.pred) ?? 0) === s)
      let delta: Map<string, Fact[]> | null = null // null = first round: naive over everything
      for (;;) {
        const round = new Map<string, DerivedFact>()
        for (const r of stratum) {
          const positions = delta === null ? [-1] : r.body.flatMap((l, i) => (!l.neg && delta!.has(l.pred) ? [i] : []))
          for (const at of positions) {
            for (const b of this.matchAll(r.body, 0, index, delta, at, {})) {
              const args = this.subst(r.head.args, b)
              const k = key(r.head.pred, args)
              if (facts.has(k)) continue
              const prov = { ruleId: r.id, premises: r.body.map((l) => ({ pred: l.pred, args: this.subst(l.args, b), neg: l.neg })) }
              const prev = round.get(k)
              if (!prev || provKey(prov) < provKey(prev.prov)) round.set(k, { pred: r.head.pred, args, prov })
            }
          }
        }
        if (round.size === 0) break
        delta = new Map()
        for (const [k, f] of round) {
          facts.set(k, f)
          push(index, f)
          push(delta, f)
        }
      }
    }
    return facts
  }

  derivation(facts: Map<string, DerivedFact>, pred: string, args: string[]): { fact: string; ruleId: string; premises: string[] } | null {
    const f = facts.get(key(pred, args))
    if (!f) return null
    return { fact: key(pred, args), ruleId: f.prov.ruleId, premises: f.prov.premises.map((p) => `${p.neg ? 'NOT ' : ''}${p.pred}(${p.args.join(',')})`) }
  }

  /** deterministic hash over the sorted derivations */
  derivHash(facts: Map<string, DerivedFact>): string {
    const lines = [...facts.values()]
      .map(
        (f) =>
          `${f.pred}(${f.args.join(',')}) <- ${f.prov.ruleId}:${f.prov.premises.map((p) => (p.neg ? '!' : '') + p.pred + '(' + p.args.join(',') + ')').join(',')}`
      )
      .sort()
    return toHex(sha256(utf8(lines.join('\n'))))
  }
}
