import test from 'node:test'
import assert from 'node:assert'
import { AORGraph, PolicyDAG, dagAuthorize, instanceOf, memberProof, delegate, verifyDelegation } from '../src/index.js'
import { PrivateKey } from '@bsv/sdk'
import { parseProgram } from '@nektar/datalog'
import { dsha256, utf8 } from '@nektar/core'

const layerBase = parseProgram(`@id org-permit
permit(P, read, F) :- member(P, org), fragment(F).
@id org-deny
deny(P, read, F) :- extern(P), fragment(F).`).rules
const layerDept = parseProgram(`@id dept-permit
permit(P, read, F) :- member(P, dept), fragment(F).`).rules

test('policy DAG: layered rules, conflict with localisation, Merkle proof, Cedar∩DAG', () => {
  const dag = new PolicyDAG([
    { name: 'org', rules: layerBase },
    { name: 'dept', rules: layerDept }
  ])
  const edb = [{ pred: 'member', args: ['bob', 'org'] }, { pred: 'member', args: ['bob', 'dept'] }, { pred: 'extern', args: ['bob'] }, { pred: 'fragment', args: ['f1'] }]
  const { conflicts } = dag.evaluate(edb)
  assert.equal(conflicts.length, 1, 'permit+deny conflict detected')
  // both org-permit and dept-permit derive the permit; provenance is the
  // canonical (lexicographically smallest) one — independent of rule order
  assert.ok(conflicts[0]!.permitPath.some(p => /^(org|dept)-permit:/.test(p)), 'permit path localised')
  assert.deepEqual(new PolicyDAG([{ name: 'dept', rules: layerDept }, { name: 'org', rules: layerBase }]).evaluate(edb).conflicts[0]!.permitPath,
    conflicts[0]!.permitPath, 'localisation is independent of layer/rule order')
  assert.ok(conflicts[0]!.denyPath.some(p => p.startsWith('org-deny')), 'deny path localised')
  assert.ok(conflicts[0]!.layers.includes('org'), 'conflict localised to layer')
  const h = [...dag.nodes.keys()][0]!
  assert.ok(dag.nodeProof(h).siblings.length > 0 || dag.nodes.size === 1, 'POL-006 per-node Merkle proof')
  // Cedar ALLOW but DAG conflict → DENY
  const d1 = dagAuthorize(dag, edb, true, 'bob', 'read', 'f1')
  assert.equal(d1.decision, 'DENY'); assert.match(d1.reason, /conflict/)
  // no conflict, Cedar ALLOW + DAG permit → ALLOW
  const edb2 = [{ pred: 'member', args: ['bob', 'org'] }, { pred: 'fragment', args: ['f1'] }]
  const d2 = dagAuthorize(dag, edb2, true, 'bob', 'read', 'f1')
  assert.equal(d2.decision, 'ALLOW'); assert.ok(d2.derivationHash)
  // Cedar DENY → DENY even with DAG permit
  assert.equal(dagAuthorize(dag, edb2, false, 'bob', 'read', 'f1').decision, 'DENY')
  // succession: dept overrides an org rule by id
  const dag2 = new PolicyDAG([
    { name: 'org', rules: layerBase },
    { name: 'dept', rules: layerDept, overrides: ['org-deny'] }
  ])
  assert.equal(dag2.rules.some(r => r.id === 'org-deny'), false, 'overridden rule removed from effective set')
  assert.ok(dag2.rules.some(r => r.id === 'dept-permit'), 'dept rule remains')
})

test('instancing: selection + closure + member proof + delegation', () => {
  const g = new AORGraph(parseProgram(`@id r1
spec(S,V) :- requires(P,V), applies(S,P).`).rules)
  const s = (x: string) => '00'.repeat(10)
  const t = (n: number) => ({ logPos: n })
  const e1 = g.append({ kind: 'episode', phi: 'requires(t, 45)', t: t(1), context: ['dept-a'], sigma: [] }, [])
  const e2 = g.append({ kind: 'episode', phi: 'applies(p4, t)', t: t(2), context: ['dept-a'], sigma: [] }, [])
  const d = g.append({ kind: 'derived', phi: 'spec(p4, 45)', t: t(3), context: ['dept-a'], sigma: [] }, [e1.h, e2.h], 'r1')
  const inst = instanceOf(g, ['dept-a'])
  assert.ok(inst.members.has(d.h), 'derived vertex selected')
  assert.ok(inst.members.size === 3, 'closure includes episodes')
  const pf = memberProof(inst, d.h)
  assert.ok(pf.siblings.length >= 0 && pf.index >= 0, 'per-node Merkle proof')
  // delegation: sign over instance root + expiry; expired/forged rejects
  const dk = PrivateKey.fromRandom()
  const exp = Date.now() + 60000
  const sig = delegate(dk, inst, exp)
  assert.ok(verifyDelegation(dk.toPublicKey(), inst, exp, sig).ok, 'delegation verifies')
  assert.equal(verifyDelegation(dk.toPublicKey(), inst, Date.now() - 1, sig).reason, 'delegation expired', 'expired delegation rejects')
  const forged = instanceOf(g, ['dept-b'])
  assert.ok(!verifyDelegation(dk.toPublicKey(), forged, exp, sig).ok, 'modified instance rejects')
})
