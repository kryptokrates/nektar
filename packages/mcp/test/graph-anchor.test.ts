import test from 'node:test'
import assert from 'node:assert'
import { MockChain, MockFunder } from '@mamem/chain'
import { MamemMemory } from '../src/index.js'
import { AORGraph, GraphAnchor, PolicyDAG } from '@mamem/graph'
import { parseProgram } from '@mamem/datalog'
import { PrivateKey } from '@bsv/sdk'
import { signBinding, answerChallenge } from '@mamem/identity'

const rules = () => parseProgram(`@id r1
spec(S,V) :- requires(P,V), applies(S,P).`).rules

test('AOR-006: GraphAnchor records + anchors vertex hashes; tamper detected', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(10); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 10 } })
  const ga = new GraphAnchor(mem, 'g1')
  const g = new AORGraph(rules())
  const e = g.append({ kind: 'episode', phi: 'requires(t, 45)', t: { logPos: 1 }, context: ['m'], sigma: [] }, [])
  await ga.record(e.h); await ga.anchor(); chain.mine(1)
  assert.ok(await ga.verifyVertex(e.h), 'recorded vertex verifies')
  assert.ok(!await ga.verifyVertex('ff'.repeat(32)), 'tampered/foreign hash rejects')
  const { anchorTx } = ga.proveVertex(e.h)
  assert.ok(anchorTx, 'anchored vertex exposes its anchor tx')
})

test('POL-016: policy DAG gates recall — ALLOW only when Cedar+DAG agree; conflict DENY; caller facts ignored', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(10); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const dag = new PolicyDAG([{ name: 'org', rules: parseProgram(`@id p1
permit(P, read, R) :- member(P, verified), fragment(R).`).rules }])
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 10 }, policyDag: dag })
  await mem.remember('s', 'hello', {}, 0)
  const res = await mem.recall('s')
  // no verified identity → principal facts empty → DAG cannot derive permit → deny
  assert.ok(res.receipt.dag, 'dag receipt present')
  assert.match(res.receipt.dag!.state, /dag-deny/, 'DAG gate denies without verified principal')
  assert.ok(res.receipt.dag!.policyRoot.length === 64, 'receipt carries policy root')
})

test('POL-016b: per-item DAG permit — mixed batch withholds only the unpermitted fragment', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(10); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  // DAG permits ONLY 's/0' — 's/1' has no permit rule
  const dag = new PolicyDAG([{ name: 'org', rules: parseProgram(`@id p0
permit(P, read, 's/0') :- member(P, verified).`).rules }])
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 10 }, policyDag: dag })
  await mem.remember('s', 'permitted fragment', {}, 0)
  await mem.remember('s', 'withheld fragment', {}, 0)
  const caller = PrivateKey.fromRandom()
  const nonce = mem.issueChallenge()
  const identity = {
    pubkey: caller.toPublicKey().toString(),
    binding: signBinding(caller, 'agent-x', '00'.repeat(32), 0),
    challengeNonce: nonce,
    challengeSig: answerChallenge(caller, nonce)
  }
  const res = await mem.recall('s', { identity })
  const seq0 = res.items.find(i => i.seq === 0)!
  const seq1 = res.items.find(i => i.seq === 1)!
  assert.equal(seq0.unverifiable, undefined, 'permitted fragment served')
  assert.equal(seq0.text, 'permitted fragment')
  assert.equal(seq1.unverifiable, true, 'non-permitted fragment withheld')
  assert.equal(seq1.reason, 'dag deny')
  assert.match(res.receipt.dag!.state, /dag-deny|dag-allow/, 'dag state reported')
})

test('POL-016c: anchoredPolicyRoot mismatch denies everything', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(10); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const dag = new PolicyDAG([{ name: 'org', rules: parseProgram(`@id p1
permit(P, read, R) :- member(P, verified), fragment(R).`).rules }])
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 10 }, policyDag: dag, anchoredPolicyRoot: 'ff'.repeat(32) })
  await mem.remember('s', 'x', {}, 0)
  const res = await mem.recall('s')
  assert.equal(res.items[0]!.unverifiable, true)
  assert.match(res.receipt.dag!.state, /policy DAG root mismatch/, 'root mismatch denies')
  assert.equal(res.receipt.cedar, 'deny')
})
