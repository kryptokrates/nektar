// §9 / GOAL-v6 S4 — OWL 2 RL/QL axioms compiled to Datalog.
// Mini vocabulary: transitive subclass chain, domain/range, inverseOf,
// symmetric, transitive property, propertyChain(2), sameAs + equivalences,
// allDifferent/differentIndividuals, disjointWith, class/property assertions,
// negativeObjectAssertion → conflict, anchored-axiom entailment, §14 plug.
import test from 'node:test'
import assert from 'node:assert/strict'
import { toHex } from '@mamem/core'
import { nodeHash } from '@mamem/datalog'
import { commitOntology, compileOwl, entail, owlAxiomHash, type OwlAxiom } from '../src/index.js'

const V: OwlAxiom[] = [
  { kind: 'subClassOf', sub: 'Cryptographer', sup: 'Scientist' },        // 0
  { kind: 'subClassOf', sub: 'Scientist', sup: 'Professional' },         // 1
  { kind: 'subClassOf', sub: 'Professional', sup: 'Person' },            // 2
  { kind: 'equivalentClass', a: 'Worker', b: 'Professional' },           // 3
  { kind: 'disjointWith', a: 'Person', b: 'Organization' },              // 4
  { kind: 'domain', prop: 'advises', cls: 'Scientist' },                 // 5
  { kind: 'range', prop: 'advises', cls: 'Person' },                     // 6
  { kind: 'inverseOf', p: 'advises', q: 'advisedBy' },                   // 7
  { kind: 'symmetric', prop: 'collaboratesWith' },                       // 8
  { kind: 'transitive', prop: 'ancestorOf' },                            // 9
  { kind: 'propertyChain', chain: ['worksAt', 'locatedIn'], sup: 'basedIn' }, // 10
  { kind: 'subPropertyOf', sub: 'mentors', sup: 'advises' },             // 11
  { kind: 'equivalentProperty', a: 'knows', b: 'acquaintedWith' },       // 12
  { kind: 'sameAs', a: 'alice', b: 'alice_l2' },                         // 13
  { kind: 'differentIndividuals', a: 'alice', b: 'bob' },                // 14
  { kind: 'allDifferent', members: ['carol', 'dave', 'erin'] },          // 15
  { kind: 'classAssertion', cls: 'Cryptographer', ind: 'alice' },        // 16
  { kind: 'propertyAssertion', prop: 'advises', from: 'alice', to: 'bob' },   // 17
  { kind: 'propertyAssertion', prop: 'collaboratesWith', from: 'alice', to: 'carol' }, // 18
  { kind: 'propertyAssertion', prop: 'ancestorOf', from: 'alice', to: 'dave' },        // 19
  { kind: 'propertyAssertion', prop: 'ancestorOf', from: 'dave', to: 'erin' },         // 20
  { kind: 'propertyAssertion', prop: 'worksAt', from: 'alice', to: 'lab' },            // 21
  { kind: 'propertyAssertion', prop: 'locatedIn', from: 'lab', to: 'zurich' },         // 22
  { kind: 'propertyAssertion', prop: 'mentors', from: 'alice', to: 'bob' },            // 23
  { kind: 'propertyAssertion', prop: 'knows', from: 'alice', to: 'fred' },             // 24
  { kind: 'propertyAssertion', prop: 'advises', from: 'carol', to: 'dave' }            // 25
]
const has = (r: { facts: Map<string, unknown> }, k: string) => r.facts.has(k)
const ent = (axioms: OwlAxiom[], pred: string, args: string[]) =>
  entail(axioms, [], { pred, args }).entailed

test('OWL 2 RL: the mini vocabulary entails the profile primitives', () => {
  const r = entail(V, [], { pred: 'inst', args: ['alice', 'person'] })
  assert.ok(r.entailed && !r.conflict && r.used === V.length && r.rejected.length === 0)
  // transitive subclass chain + equivalentClass
  assert.ok(has(r, 'inst(alice,scientist)') && has(r, 'inst(alice,professional)') && has(r, 'inst(alice,person)'))
  assert.ok(has(r, 'inst(alice,worker)'), 'Worker ≡ Professional propagates membership')
  assert.ok(has(r, 'subclass(cryptographer,person)'), 'subclass chain materialised')
  // domain / range
  assert.ok(has(r, 'inst(carol,scientist)'), 'domain: advises(carol,dave) → carol:Scientist')
  assert.ok(has(r, 'inst(dave,person)') && has(r, 'inst(bob,person)'), 'range: advisee is a Person')
  // inverseOf, symmetric, transitive, propertyChain, subPropertyOf, equivalentProperty
  assert.ok(has(r, 't(bob,advisedby,alice)'), 'inverseOf')
  assert.ok(has(r, 't(carol,collaborateswith,alice)'), 'symmetric')
  assert.ok(has(r, 't(alice,ancestorof,erin)'), 'transitive via dave')
  assert.ok(has(r, 't(alice,basedin,zurich)'), 'propertyChain worksAt·locatedIn ⊑ basedIn')
  assert.ok(has(r, 't(alice,advises,bob)') && has(r, 'subprop(mentors,advises)'), 'subPropertyOf')
  assert.ok(has(r, 't(alice,acquaintedwith,fred)'), 'equivalentProperty')
  // sameAs: identity substitution over inst + both triple positions
  assert.ok(has(r, 'inst(alice_l2,cryptographer)') && has(r, 't(alice_l2,advises,bob)') && has(r, 'same(alice_l2,alice)'))
  // caller EDB participates (and is name-normalised) without being committed
  assert.ok(ent(V, 'inst', ['eve', 'Scientist']) === false)
  const withEdb = entail(V, [{ pred: 't', args: ['Eve', 'advises', 'Alice'] }], { pred: 'inst', args: ['eve', 'scientist'] })
  assert.ok(withEdb.entailed && withEdb.conflict === false, 'runtime triple → domain entailment')
})

test('OWL 2 RL: contradictions derive the conflict() marker the verifier rejects', () => {
  // NegativeObjectPropertyAssertion vs an asserted AND entailed triple
  const npa: OwlAxiom = { kind: 'negativeObjectAssertion', prop: 'advises', from: 'alice', to: 'bob' }
  const r1 = entail([...V, npa], [], { pred: 'inst', args: ['alice', 'person'] })
  assert.ok(r1.conflict, 'asserted/derived t(alice,advises,bob) + ¬advises(alice,bob) → conflict')
  assert.ok(ent([...V, npa], 'conflict', []), 'conflict() itself is queryable')
  // sameAs vs differentIndividuals
  assert.ok(entail([...V, { kind: 'sameAs', a: 'alice', b: 'bob' }], [], { pred: 'x', args: [] }).conflict, 'same+diff → conflict')
  // self-inequality is inconsistent on its own
  assert.ok(entail([{ kind: 'differentIndividuals', a: 'x', b: 'x' }], [], { pred: 'x', args: [] }).conflict, 'diff(x,x) → conflict')
  // disjointWith: alice is Person (derived) + Organization (asserted)
  const r4 = entail([...V, { kind: 'classAssertion', cls: 'Organization', ind: 'alice' }], [], { pred: 'x', args: [] })
  assert.ok(r4.conflict, 'Person ⊓ Organization = ⊥')
  // a foreign (unanchored) negative assertion cannot manufacture a conflict
  const anchored = new Set(commitOntology(V).axiomHashes)
  const r5 = entail([...V, npa], [], { pred: 'inst', args: ['alice', 'person'] }, h => anchored.has(h))
  assert.ok(!r5.conflict && r5.rejected.includes(owlAxiomHash(npa)), 'unanchored negation is void ab initio')
})

test('entailment only from anchored axioms — foreign/unanchored axioms are void', () => {
  const ont = commitOntology(V)
  const all = new Set(ont.axiomHashes)
  const q = { pred: 'inst', args: ['alice', 'professional'] }
  assert.ok(ont.entail([], q, h => all.has(h)).entailed, 'all axioms anchored → entailed')
  // a mid-chain axiom dropped → the chain is void, exactly the v5 semantics
  const minus = new Set(ont.axiomHashes.filter((_, i) => i !== 1)) // Scientist ⊑ Professional
  const r = ont.entail([], q, h => minus.has(h))
  assert.ok(!r.entailed, 'unanchored SubClassOf breaks entailment')
  assert.deepEqual(r.rejected, [owlAxiomHash(V[1]!)])
  // a FOREIGN axiom (never committed) cannot extend the ontology
  const foreign: OwlAxiom = { kind: 'subClassOf', sub: 'Person', sup: 'Agent' }
  const r2 = entail([...V, foreign], [], { pred: 'inst', args: ['alice', 'agent'] }, h => all.has(h))
  assert.ok(!r2.entailed, 'foreign axiom rejected')
  assert.deepEqual(r2.rejected, [owlAxiomHash(foreign)])
  // anchoring it works — the predicate, not the axiom set, gates entailment
  assert.ok(entail([...V, foreign], [], { pred: 'inst', args: ['alice', 'agent'] }).entailed)
})

test('commitOntology: deterministic root; rules + facts + axioms committed under it', () => {
  const ont = commitOntology(V)
  assert.equal(commitOntology([...V]).root, ont.root, 'same axiom list → same root')
  assert.match(ont.root, /^[0-9a-f]{64}$/)
  for (const h of ont.axiomHashes) assert.ok(ont.dag.nodes.has(h), 'every axiom is a committed node')
  // compiled EDB facts are committed fact nodes (§14 committedIndex inputs)
  assert.ok(ont.dag.nodes.has(toHex(nodeHash({ pred: 'inst', args: ['alice', 'cryptographer'] }))))
  assert.ok(ont.dag.nodes.has(toHex(nodeHash({ pred: 'subclass', args: ['scientist', 'professional'] }))))
  // compiled rules are committed rule nodes with canonical per-axiom ids
  for (const r of ont.rules) assert.ok(ont.dag.ruleNode.has(r.id))
  const c = compileOwl(V)
  assert.ok(c.rules.some(r => r.id === 'owlrl:subClassOf#0' && r.body.length === 1))
  assert.ok(c.rules.some(r => r.id === 'owlrl:propertyChain#10' && r.body.length === 2 && r.head.args.join(',') === 'X0,basedin,X2'))
  assert.ok(c.rules.some(r => r.id === 'owlrl:inverseOf#7:1') && c.rules.some(r => r.id === 'owlrl:inverseOf#7:2'), 'inverseOf emits both directions')
})

test('§14 path: every step rule and context premise is committed under the ontology root', () => {
  const ont = commitOntology(V)
  const res = ont.entail([], { pred: 'inst', args: ['alice', 'person'] })
  assert.ok(res.entailed && res.path, 'a committed derivation path exists')
  assert.ok(res.path!.steps.every(s => /^owlrl:/.test(s.ruleId)), 'canonical owlrl rule ids')
  for (const s of res.path!.steps) {
    assert.ok(ont.dag.ruleNode.has(s.ruleId), `τ for ${s.ruleId} committed in the DAG`)
    for (const i of s.inputs) {
      if (i.fromStep === null) {
        assert.ok(ont.dag.nodes.has(toHex(nodeHash(i.fact))), `context premise ${i.fact.pred}(${i.fact.args.join(',')}) is a committed node`)
      }
    }
  }
  // a base-fact goal has no derivation path — it attests as a committed node
  const base = ont.entail([], { pred: 'inst', args: ['alice', 'cryptographer'] })
  assert.ok(base.entailed && base.path === null, 'EDB goal: attest the fact node, not a path')
})
