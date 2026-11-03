import test from 'node:test'
import assert from 'node:assert'
import { AORGraph, PolicyDAG, verifyDerivation, instanceOf, entails, ruleCanonical } from '../src/index.js'
import { PrivateKey } from '@bsv/sdk'
import { parseProgram } from '@nektar/datalog'
import { dsha256, utf8 } from '@nektar/core'

const rules = () => parseProgram(`@id r1
spec(S,V) :- requires(P,V), applies(S,P).`).rules

test('POL-003: rule-body commitment — changing a body changes the root', () => {
  const a = new PolicyDAG([{ name: 'org', rules: rules() }])
  const b = new PolicyDAG([{ name: 'org', rules: parseProgram(`@id r1
spec(S,V) :- requires(P,V).`).rules }])
  assert.notEqual(Buffer.from(a.policyRoot()).toString('hex'), Buffer.from(b.policyRoot()).toString('hex'), 'body change changes policy root')
})

test('POL-006: query + verifyDerivation; tampered rule, swapped premise, other root', () => {
  const dag = new PolicyDAG([{ name: 'org', rules: rules() }])
  const edb = [{ pred: 'requires', args: ['t', '45'] }, { pred: 'applies', args: ['p4', 't'] }]
  const q = dag.query(edb, 'spec', ['p4', '45'])!
  assert.ok(verifyDerivation(q, dag, edb).ok, 'valid derivation verifies')
  // tampered rule text: a query whose tree claims a different fact
  const bad = { ...q, tree: { ...q.tree, fact: 'spec(p4, 99)' } }
  const r2 = verifyDerivation(bad, dag, edb)
  assert.ok(!r2.ok && /not entailed|mismatch/.test(r2.reason!), 'tampered step rejected')
  // proof against a different root
  const other = { ...q, root: 'ff'.repeat(32) }
  const r3 = verifyDerivation(other, dag, edb)
  assert.ok(!r3.ok && /different policy root/.test(r3.reason!), 'other root rejected')
})

// (AOR-006 GraphAnchor test lives in @nektar/mcp — it needs NektarMemory and
// graph→mcp is not a workspace dependency)

test('INS-006: partial entailment under R_q', () => {
  const cat = parseProgram(`@id r1
spec(S,V) :- requires(P,V), applies(S,P).
@id r2
checked(S,V) :- spec(S,V).`).rules
  const g = new AORGraph(cat)
  const e1 = g.append({ kind: 'episode', phi: 'requires(t, 45)', t: { logPos: 1 }, context: ['m'], sigma: [] }, [])
  const e2 = g.append({ kind: 'episode', phi: 'applies(p4, t)', t: { logPos: 2 }, context: ['m'], sigma: [] }, [])
  const inst = instanceOf(g, ['m'])
  const full = entails(g, inst, 'spec(p4, 45)', new Set(['r1', 'r2']), cat)
  assert.ok(full.entailed, 'spec entailed under full R')
  const none = entails(g, inst, 'spec(p4, 45)', new Set(['r2']), cat)
  assert.ok(!none.entailed, 'spec NOT entailed under subset without r1')
})

test('AOR-008a: currentView boundary — before, at, after refinement t', () => {
  const g = new AORGraph(rules())
  const e1 = g.append({ kind: 'episode', phi: 'requires(t, 45)', t: { logPos: 1 }, context: ['m'], sigma: [] }, [])
  const e2 = g.append({ kind: 'episode', phi: 'applies(p4, t)', t: { logPos: 2 }, context: ['m'], sigma: [] }, [])
  const d = g.append({ kind: 'derived', phi: 'spec(p4, 45)', t: { logPos: 3 }, context: ['m'], sigma: [] }, [e1.h, e2.h], 'r1')
  const e3 = g.append({ kind: 'episode', phi: 'requires(t, 55)', t: { logPos: 4 }, context: ['m'], sigma: [] }, [])
  g.append({ kind: 'refinement', phi: 'spec(p4, 55)', t: { logPos: 5 }, context: ['m'], sigma: [], refines: d.h }, [e3.h, e2.h], 'r1')
  assert.ok(g.currentView(4).some(v => v.phi === 'spec(p4, 45)'), 'before t=5 old spec valid')
  assert.ok(!g.currentView(5).some(v => v.phi === 'spec(p4, 45)'), 'AT t=5 old spec invalidated')
  assert.ok(!g.currentView(100).some(v => v.phi === 'spec(p4, 45)'), 'after t=5 still invalidated')
  assert.ok(g.history(d.h)!.phi === 'spec(p4, 45)', 'vertex retained, never mutated')
})
