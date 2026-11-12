// §14 committed derivation path: canonical serialisation, topological order,
// step linking (fromStep), edge commitments, negation refusal.
import test from 'node:test'
import assert from 'node:assert/strict'
import { dsha256, utf8, concat, toHex } from '@nektar/core'
import { parseProgram, Datalog, derivationPath, PathError, factCanonical, nodeHash, edgeHash, stepEdge, pathDigest } from '../src/index.js'

const PROG = `
@id r-allow
allowed(P, F) :- member(P, verified), fragment(F).
@id r-permit
permit(P, F) :- allowed(P, F), cleared(P).
`
const EDB = [
  { pred: 'member', args: ['a', 'verified'] },
  { pred: 'fragment', args: ['s1'] },
  { pred: 'cleared', args: ['a'] }
]

const run = (edb = EDB) => new Datalog(parseProgram(PROG).rules).run(edb)

test('§14 path: ordered steps, fromStep links, node/edge hashes', () => {
  const p = derivationPath(run(), 'permit', ['a', 's1'])!
  assert.equal(p.steps.length, 2)
  const [s0, s1] = p.steps
  assert.equal(s0!.ruleId, 'r-allow')
  assert.deepEqual(s0!.output, { pred: 'allowed', args: ['a', 's1'] })
  assert.deepEqual(
    s0!.inputs.map((i) => i.fromStep),
    [null, null],
    'EDB premises are context leaves'
  )
  assert.equal(s1!.ruleId, 'r-permit')
  assert.deepEqual(
    s1!.inputs.map((i) => i.fromStep),
    [0, null],
    'allowed premise links to step 0'
  )
  assert.equal(toHex(p.conclusionH), toHex(nodeHash({ pred: 'permit', args: ['a', 's1'] })))
  // every fromStep strictly precedes its step (no skipped or reordered steps)
  for (const [i, s] of p.steps.entries()) for (const inp of s.inputs) assert.ok(inp.fromStep === null || inp.fromStep < i)
})

test('§14 commitments: edge = dsha256(h_in‖…‖τ‖h_out); path digest folds edges', () => {
  const p = derivationPath(run(), 'permit', ['a', 's1'])!
  const tau = (id: string) => dsha256(utf8(`tau:${id}`)) // test stand-in for the rule's DAG node hash
  const e0 = stepEdge(p.steps[0]!, tau('r-allow'))
  assert.equal(
    toHex(e0),
    toHex(
      edgeHash(
        [nodeHash({ pred: 'member', args: ['a', 'verified'] }), nodeHash({ pred: 'fragment', args: ['s1'] })],
        tau('r-allow'),
        nodeHash({ pred: 'allowed', args: ['a', 's1'] })
      )
    )
  )
  const d = pathDigest(p.steps, tau)
  const expect = dsha256(concat(dsha256(concat(new Uint8Array(32), e0)), stepEdge(p.steps[1]!, tau('r-permit'))))
  assert.equal(toHex(d), toHex(expect))
  // deterministic across EDB shuffles (canonical provenance)
  const shuffled = derivationPath(run([...EDB].reverse()), 'permit', ['a', 's1'])!
  assert.equal(toHex(pathDigest(shuffled.steps, tau)), toHex(d))
})

test('§14 serialisation: factCanonical / nodeHash are fixed', () => {
  assert.equal(factCanonical({ pred: 'member', args: ['a', 'verified'] }), 'fact:member(a,verified)')
  assert.equal(toHex(nodeHash({ pred: 'x', args: ['y'] })), toHex(dsha256(utf8('fact:x(y)'))))
})

test('§14 path refuses: non-entailed, base-fact conclusion, negated premise', () => {
  assert.equal(derivationPath(run(), 'permit', ['b', 's1']), null, 'not entailed → null')
  assert.throws(() => derivationPath(run(), 'member', ['a', 'verified']), PathError, 'base fact is not a derivation')
  const { rules } = parseProgram('@id r\ndeny(P) :- member(P, verified), not cleared(P).')
  const facts = new Datalog(rules).run([{ pred: 'member', args: ['a', 'verified'] }])
  assert.ok(facts.has('deny(a)'))
  assert.throws(() => derivationPath(facts, 'deny', ['a']), /not ZK-provable/, 'negation cannot be proven pointwise')
})

test('§14 diamond: a shared premise is one step, referenced twice', () => {
  const { rules } = parseProgram(`
@id r-base
base(P) :- member(P, verified).
@id r-pair
pair(P) :- base(P), member(P, verified).
`)
  const facts = new Datalog(rules).run([{ pred: 'member', args: ['a', 'verified'] }])
  const p = derivationPath(facts, 'pair', ['a'])!
  assert.equal(p.steps.length, 2)
  assert.equal(p.steps[1]!.inputs[0]!.fromStep, 0)
})
