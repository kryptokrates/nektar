// GOAL-v5: the remaining paper gaps — §8 delta encoding, §12 temporal logic,
// §19 epoch-bound authorship, §9 memory as law (ontology / jurisdiction / precedent).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { diff, apply, revert, decodeDelta, encodeDelta, DeltaError, utf8 } from '@mamem/core'
import { MockChain, MockFunder } from '@mamem/chain'
import { signProvenance } from '@mamem/log'
import { makeRotationCert, verifyAuthorship } from '@mamem/identity'
import { always, eventually, obligation, causalClosure, type TimedFact } from '@mamem/reasoning'
import { axiomHash, ontologyEntails, Jurisdictions, validByPrecedent, type Axiom } from '@mamem/graph'
import { MamemMemory } from '@mamem/mcp'

function world () {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(20); for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const settle = (s: string) => { chain.mine(1); const l = mem.tailLink(s); if (l) mem.confirmAnchor(s, l.txid) }
  return { mem, settle }
}

test('§8 delta: deterministic, invertible, unicode-safe; tampered Δ rejected', () => {
  for (const [a, b] of [['torque 45Nm', 'torque 55Nm'], ['', 'new'], ['same', 'same'], ['abc', ''], ['naïve ✓ 🙂 x', 'naïve ✗ 🙂 y']] as const) {
    const d = diff(a, b)
    assert.equal(apply(a, d), b); assert.equal(revert(b, d), a)
    assert.deepEqual(decodeDelta(encodeDelta(d)), d)
  }
  assert.throws(() => apply('torque 99Nm', diff('torque 45Nm', 'torque 55Nm')), DeltaError, 'Δ applied to a different base')
  assert.throws(() => decodeDelta('[1,"x"]'), DeltaError)
})

test('§8 delta: revisions stored as Δ; every version materialises from proven links', async () => {
  const { mem, settle } = world()
  const versions = ['Manual rev1: torque 45Nm, belt 10h', 'Manual rev2: torque 55Nm, belt 10h', 'Manual rev3: torque 55Nm, belt 12h', 'Manual rev4: torque 55Nm, belt 12h, oil 5W30', 'Manual rev5: torque 50Nm, belt 12h, oil 5W30']
  await mem.remember('man', versions[0]!)
  for (let i = 1; i < versions.length; i++) await mem.revise('man', i - 1, versions[i]!)
  await mem.remember('man', 'filler'); settle('man')
  const stored = (await mem.records('man')).map(r => Buffer.from(r.fragment.d).toString())
  assert.ok(stored.slice(1, 5).every(s => s.startsWith('delta:')), 'revisions are deltas, not full copies')
  assert.ok(stored[4]!.length < versions[4]!.length + 16)
  for (let i = 0; i < versions.length; i++) assert.equal(await mem.materialize('man', i), versions[i], `version ${i}`)
  // 0-conf semantics: the auto-anchored (broadcast, unmined) batch IS provable
  // locally — mining is settlement, not validity — so the links materialize
  await mem.remember('man', 'broadcast base')
  await mem.revise('man', 6, 'broadcast base v2')
  assert.equal(await mem.materialize('man', 7), 'broadcast base v2', 'unmined-but-anchored link is provable')
  // a genuinely unanchored record (batch open, nothing broadcast) still refuses
  await mem.remember('man', 'pending base')
  await assert.rejects(() => mem.materialize('man', 8), /not provable/, 'unproven link refused')
})

test('§12 temporal: □_t, ◇_(t1,t2), obligations with deadlines, causal closure', () => {
  const F: TimedFact[] = [
    { phi: 'incident(pump2)', t: 100 }, { phi: 'repaired(pump2)', t: 160 },
    { phi: 'incident(pump3)', t: 200 },
    { phi: 'certified(op1)', t: 50, withdrawnAt: 300 }
  ]
  assert.ok(always(F, 'certified(op1)', 250)); assert.ok(!always(F, 'certified(op1)', 300), 'withdrawn by t=300'); assert.ok(!always(F, 'certified(op1)', 40))
  assert.ok(eventually(F, 'repaired(pump2)', 100, 200)); assert.ok(!eventually(F, 'repaired(pump2)', 0, 150))
  const sla = obligation(F.map(f => ({ ...f, phi: f.phi.replace(/\(.*/, '') })), 'incident', 'repaired', 100, 400)
  assert.equal(sla.ok, false); assert.deepEqual(sla.violations.map((v: { trigger: TimedFact }) => v.trigger.t), [200], 'pump3 incident unrepaired past its deadline')
  assert.equal(obligation(F.map(f => ({ ...f, phi: f.phi.replace(/\(.*/, '') })), 'incident', 'repaired', 100, 250).ok, true, 'open obligation is not yet a violation')
  assert.deepEqual(causalClosure([{ conclusion: 'fit(op1)', t: 260, cites: [F[3]!] }]), { ok: true })
  assert.match(causalClosure([{ conclusion: 'pump3 ok', t: 150, cites: [F[2]!] }]).reason!, /cites "incident\(pump3\)" anchored at t=200/)
})

test('§19 epoch binding: authorship survives rotation; wrong epoch / after expiry refused', async () => {
  const k0 = PrivateKey.fromRandom(); const k1 = PrivateKey.fromRandom(); const k2 = PrivateKey.fromRandom()
  const g = k0.toPublicKey().toString()
  const c1 = { ...makeRotationCert(k0, k1.toPublicKey().toString(), 1000, 100), index: 1 }
  const c2 = { ...makeRotationCert(k1, k2.toPublicKey().toString(), 2000, 100), index: 2 }
  const certs = [c1, c2]
  assert.deepEqual(verifyAuthorship(g, 0, 900, g, certs), { ok: true })
  assert.deepEqual(verifyAuthorship(g, 0, 1050, g, certs), { ok: true }, 'inside the deprecation horizon')
  assert.match(verifyAuthorship(g, 0, 1100, g, certs).reason!, /outside epoch-0 window/, 'old key used after expiry')
  assert.match(verifyAuthorship(g, 1, 1500, g, certs).reason!, /not the epoch-1 key/)
  assert.deepEqual(verifyAuthorship(k1.toPublicKey().toString(), 1, 1500, g, certs), { ok: true })
  assert.match(verifyAuthorship(k1.toPublicKey().toString(), 1, 900, g, certs).reason!, /outside/, 'before certification')
  // through MamemMemory: a signed write keeps its epoch and verifies after later rotations
  const { mem } = world()
  const p = signProvenance(k1, utf8('epoch-1 statement'), { source: 'op', time: 1500, signature: new Uint8Array(0), modelId: 'm' })
  await mem.remember('a', 'epoch-1 statement', { ...p, sourcePub: k1.toPublicKey().toString(), epoch: 1 })
  assert.deepEqual(await mem.authorship('a', 0, g, certs), { ok: true })
  assert.match((await mem.authorship('a', 0, g, [c1])).reason ?? 'ok', /ok/, 'still valid with a shorter chain')
  await mem.remember('a', 'unsigned note')
  assert.match((await mem.authorship('a', 1, g, certs)).reason!, /no verified author epoch/)
})

test('§9 memory as law: anchored DL entailment, jurisdiction graphs, precedent', () => {
  const ax: Axiom[] = [
    { kind: 'SubClassOf', sub: 'Cryptographer', sup: 'Scientist' },
    { kind: 'SubClassOf', sub: 'Scientist', sup: 'Professional' },
    { kind: 'ClassAssertion', cls: 'Cryptographer', ind: 'alice' }
  ]
  const all = new Set(ax.map(axiomHash))
  assert.ok(ontologyEntails(ax, { pred: 'instance', args: ['alice', 'professional'] }, h => all.has(h)).entailed, 'C ⊑ D via an anchored subclass chain')
  const missing = new Set([axiomHash(ax[0]!), axiomHash(ax[2]!)])
  const r = ontologyEntails(ax, { pred: 'instance', args: ['alice', 'professional'] }, h => missing.has(h))
  assert.equal(r.entailed, false, 'an unanchored axiom breaks the chain (void ab initio)')
  assert.deepEqual(r.rejected, [axiomHash(ax[1]!)])
  const J = new Jurisdictions()
  J.add('EU', [...ax, { kind: 'Rule', text: '@id eu-export\npermitted(X) :- instance(X, professional), licensed(X, eu).' }, { kind: 'ObjectPropertyAssertion', prop: 'licensed', from: 'alice', to: 'eu' }])
  J.add('US', [...ax, { kind: 'Rule', text: '@id us-export\npermitted(X) :- instance(X, professional), cleared(X, us).' }])
  const yes = () => true
  assert.ok(J.permissible('alice', 'EU', yes).entailed)
  assert.equal(J.permissible('alice', 'US', yes).entailed, false, 'same facts, different regulatory code')
  assert.equal(J.permissible('alice', 'UK', yes).entailed, false, 'unknown jurisdiction permits nothing')
  const precedent: Axiom[] = [
    { kind: 'ClassAssertion', cls: 'ApprovedTransfer', ind: 'case_2024_17' },
    { kind: 'Rule', text: '@id prec\npermitted(X) :- same_facts(X, Y), instance(Y, approvedtransfer).' },
    { kind: 'ObjectPropertyAssertion', prop: 'same_facts', from: 'case_2026_03', to: 'case_2024_17' }
  ]
  assert.ok(validByPrecedent(precedent, 'case_2026_03', yes).entailed, 'new action entailed by anchored precedent')
  assert.equal(validByPrecedent(precedent, 'case_2026_04', yes).entailed, false, 'no precedent → not valid')
  const unanchoredPrecedent = new Set(precedent.slice(1).map(axiomHash))
  assert.equal(validByPrecedent(precedent, 'case_2026_03', h => unanchoredPrecedent.has(h)).entailed, false, 'precedent must itself be anchored')
})

test('§16 delegation map: sub-decisions signed by delegating agents; tampering fails', async () => {
  const { makeDelegation, verifyReceiptDelegations, commitReceipt } = await import('@mamem/reasoning')
  const sub = PrivateKey.fromRandom()
  const answer = 'approved transfer of EUR 500'
  const receipt = {
    answer, fragmentRefs: [], checkResult: 'verified' as const,
    modelId: { kind: 'local' as const, weightsSha256: 'ab'.repeat(32), name: 'm' },
    timestamp: 1_700_000_000,
    delegations: [makeDelegation(sub, 'compliance check passed', answer)]
  }
  assert.deepEqual(verifyReceiptDelegations(receipt), { ok: true, reasons: [] })
  // a delegation over a different decision or answer fails
  assert.equal(verifyReceiptDelegations({ ...receipt, answer: 'approved transfer of EUR 500000' }).ok, false, 'delegation does not carry to a different answer')
  const forged = { ...receipt, delegations: [{ delegator: PrivateKey.fromRandom().toPublicKey().toString(), decision: 'compliance check passed', signature: receipt.delegations[0]!.signature }] }
  assert.equal(verifyReceiptDelegations(forged).ok, false, 'sig moved to another delegator fails')
  // commitReceipt anchors the receipt (with its delegation map) into __receipts__
  const { mem } = world()
  const { receiptHash } = await commitReceipt(receipt, mem)
  assert.equal(receiptHash.length, 64)
})
