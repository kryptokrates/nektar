import test from 'node:test'
import assert from 'node:assert'
import { parseProgram, ParseError, Datalog, CycleError, SafetyError, lint } from '../src/index.js'

const PROG = `
@id r-clear
cleared(A, L) :- clearance(A, L).
@id r-deleg
cleared(A, L) :- delegates(B, A), clearance(B, L).
@id r-allow
allowed(A, F) :- cleared(A, L), level(F, L), not revoked(A).
`

test('parser: facts, rules, @id, negation', () => {
  const p = parseProgram(`revoked(agent-c). ${PROG}`)
  assert.equal(p.facts.length, 1)
  assert.equal(p.rules.length, 3)
  assert.equal(p.rules[0]!.id, 'r-clear')
  assert.equal(p.rules[2]!.body[2]!.neg, true)
  assert.throws(() => parseProgram('p(X) :- q(X)'), /unterminated/, 'unterminated rejected')
  assert.throws(() => parseProgram('p(?X).'), ParseError, 'bad term rejected')
})

test('safety: unsafe head + negated vars rejected', () => {
  assert.throws(() => new Datalog([{ id: 'r', head: { pred: 'p', args: ['Y'], neg: false }, body: [{ pred: 'q', args: ['X'], neg: false }] }]), SafetyError, 'unsafe head var')
  assert.throws(() => new Datalog([{ id: 'r', head: { pred: 'p', args: ['A'], neg: false }, body: [{ pred: 'q', args: ['A'], neg: true }] }]), SafetyError, 'unsafe negated var')
})

test('stratification: negation cycle throws CycleError', () => {
  assert.throws(() => new Datalog([{ id: 'x', head: { pred: 'p', args: ['A'], neg: false }, body: [{ pred: 'p', args: ['A'], neg: true }] }]), CycleError)
  assert.throws(() => new Datalog(parseProgram('@id a\np(X) :- d(X), not q(X).\n@id b\nq(X) :- d(X), not p(X).').rules), CycleError, 'mutual negation cycle')
  assert.doesNotThrow(() => new Datalog(parseProgram('@id a\np(X) :- q(X).\n@id b\nq(X) :- p(X).').rules), 'positive mutual recursion is stratifiable')
})

test('evaluate + provenance + determinism across shuffled input order', () => {
  const { rules } = parseProgram(PROG)
  const edb = [
    { pred: 'clearance', args: ['agent-b', '2'] },
    { pred: 'delegates', args: ['lead', 'agent-b'] },
    { pred: 'clearance', args: ['lead', '3'] },
    { pred: 'level', args: ['frag-1', '2'] },
    { pred: 'revoked', args: ['agent-c'] }
  ]
  const dl = new Datalog(rules)
  const h = [0, 1, 2].map(() => dl.derivHash(dl.run([...edb].sort(() => 0.5 - Math.random()))))
  assert.equal(h[0], h[1]); assert.equal(h[1], h[2], 'derivHash deterministic')
  const facts = dl.run(edb)
  const d = dl.derivation(facts, 'allowed', ['agent-b', 'frag-1'])!
  assert.equal(d.ruleId, 'r-allow')
  assert.ok(d.premises.includes('NOT revoked(agent-b)'), 'negated premise recorded')
  assert.ok(dl.derivation(facts, 'allowed', ['agent-c', 'frag-1']) === null, 'revoked principal not derived')
  // partial entailment: r-deleg excluded → cleared(agent-b,3) gone
  const subset = dl.run(edb, new Set(['r-clear', 'r-allow']))
  assert.equal(subset.has('cleared(agent-b,3)'), false, 'rule subset restricts entailment')
})

test('semi-naive: recursive closure equals the reference fixpoint; provenance independent of rule order', () => {
  const src = `@id base
reach(X, Y) :- edge(X, Y).
@id step
reach(X, Z) :- reach(X, Y), edge(Y, Z).`
  const edges = Array.from({ length: 30 }, (_, i) => ({ pred: 'edge', args: [`n${i}`, `n${(i * 7 + 3) % 30}`] }))
  const got = new Datalog(parseProgram(src).rules).run(edges)
  // reference: naive transitive closure
  const adj = new Map<string, string[]>()
  for (const e of edges) adj.set(e.args[0]!, [...(adj.get(e.args[0]!) ?? []), e.args[1]!])
  const ref = new Set<string>()
  for (const s of adj.keys()) {
    const seen = new Set<string>(); const stack = [...(adj.get(s) ?? [])]
    while (stack.length) { const n = stack.pop()!; if (seen.has(n)) continue; seen.add(n); ref.add(`reach(${s},${n})`); stack.push(...(adj.get(n) ?? [])) }
  }
  const reach = [...got.keys()].filter(k => k.startsWith('reach('))
  assert.deepEqual(new Set(reach), ref, 'closure identical to reference')
  const reversed = parseProgram(src).rules.reverse()
  const dl1 = new Datalog(parseProgram(src).rules), dl2 = new Datalog(reversed)
  assert.equal(dl1.derivHash(dl1.run(edges)), dl2.derivHash(dl2.run([...edges].reverse())), 'canonical provenance')
})

test('linter: undefined predicate, unused rule, unsafe var', () => {
  const { rules, facts } = parseProgram(`p(A) :- q(A).`)
  const diags = lint(rules, facts)
  assert.ok(diags.some(d => d.code === 'undefined-predicate'), 'undefined predicate flagged')
  assert.ok(diags.some(d => d.code === 'unused-rule'), 'unused rule flagged')
  const bad = lint([{ id: 'r', head: { pred: 'p', args: ['Y'], neg: false }, body: [{ pred: 'q', args: ['X'], neg: false }] }], [])
  assert.ok(bad.some(d => d.code === 'unsafe-variable'), 'unsafe var flagged')
})
