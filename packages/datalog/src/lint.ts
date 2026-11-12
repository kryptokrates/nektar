// POL-012 linter: diagnostics for L_pol programs.
import { type Fact, type Rule } from './parse.js'
import { Datalog, CycleError, SafetyError } from './engine.js'

export interface Diagnostic {
  severity: 'error' | 'warning'
  code: string
  message: string
}

export function lint(rules: Rule[], facts: Fact[]): Diagnostic[] {
  const out: Diagnostic[] = []
  const defined = new Set([...rules.map((r) => r.head.pred), ...facts.map((f) => f.pred)])
  const used = new Set<string>()
  for (const r of rules) for (const l of r.body) used.add(l.pred)
  // undefined predicates
  for (const r of rules)
    for (const l of r.body) {
      if (!defined.has(l.pred)) out.push({ severity: 'error', code: 'undefined-predicate', message: `${r.id}: body predicate '${l.pred}' is never defined` })
    }
  // unused rules (head never referenced and not a query target — heuristic warning)
  for (const r of rules) {
    if (!used.has(r.head.pred)) out.push({ severity: 'warning', code: 'unused-rule', message: `rule ${r.id} derives '${r.head.pred}' which nothing consumes` })
  }
  // safety + stratification
  try {
    new Datalog(rules)
  } catch (e) {
    const code = e instanceof CycleError ? 'unstratified-negation' : e instanceof SafetyError ? 'unsafe-variable' : 'error'
    out.push({ severity: 'error', code, message: String((e as Error).message) })
  }
  return out
}
