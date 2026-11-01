import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { MockChain, MockFunder } from '@mamem/chain'
import { canonicalizePolicySet } from '@mamem/policy'
import { signBinding, answerChallenge, issueChallenge, issueClearance, proveAccess, type ZkTranscript } from '@mamem/identity'
import { MamemMemory, type MamemMemoryOptions, type ZkClearance } from '../src/memory.js'
import { callTool } from '../src/server.js'

function setup (opts: MamemMemoryOptions = {}) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(50)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
  const settle = (shard: string) => { chain.mine(1); const l = mem.tailLink(shard); if (l) mem.confirmAnchor(shard, l.txid) }
  return { chain, funder, mem, settle }
}
/** a caller that answers a challenge issued by `mem` (the verifier) */
function callerOf (mem: MamemMemory, key = PrivateKey.fromRandom()) {
  const nonce = mem.issueChallenge()
  return { key, identity: { pubkey: key.toPublicKey().toString(), binding: signBinding(key, 'agent', '00'.repeat(32), 0), challengeNonce: nonce, challengeSig: answerChallenge(key, nonce) } }
}
const tr = (fragmentId: string, fill = 7): ZkTranscript => ({ queryId: `q${fill}`, fragmentId, nonce: new Uint8Array(32).fill(fill) })

test('mcp memory: remember → anchor → prove (verified)', async () => {
  const { mem, settle } = setup()
  await mem.remember('manual', 'drain port 4 torque 45Nm')
  const r = await mem.remember('manual', 'inspect belt every 10h')
  assert.ok(r.anchored, 'batch of 2 auto-anchored')
  settle('manual')
  const { bundle, result } = await mem.prove('manual', 0)
  assert.ok(bundle)
  assert.deepEqual(result, { ok: true })
})

test('mcp memory: recall returns answer receipt; plaintext + encrypted modes', async () => {
  const { mem, settle } = setup()
  await mem.remember('s', 'fact one')
  const { items, receipt } = await mem.recall('s')
  // S6.1: an unanchored PLAINTEXT record is withheld like an encrypted one —
  // it never earned 'verified' from anchor presence alone, and now it is not
  // served at all until SPV verification passes
  assert.equal(items[0]!.text, undefined)
  assert.equal(items[0]!.unverifiable, true)
  assert.match(items[0]!.reason!, /verify: NotAnchored/)
  assert.equal(receipt.checkResult, 'unverifiable') // not anchored yet
  assert.equal(receipt.modelId, 'test-model-1')
  // once anchored and mined the same plaintext record is served + verified
  await mem.anchorShard('s'); settle('s')
  const after = await mem.recall('s')
  assert.equal(after.items[0]!.text, 'fact one')
  assert.equal(after.items[0]!.unverifiable, undefined)
  assert.equal(after.receipt.checkResult, 'verified')
  const seed = PrivateKey.fromRandom()
  const enc = setup({ agentSeed: seed, maxLevel: 1 })
  await enc.mem.remember('s', 'classified fact')
  // decrypt requires the record to be anchored AND verified (KEY-008)
  await enc.mem.anchorShard('s'); enc.settle('s')
  assert.equal((await enc.mem.recall('s')).items[0]!.text, 'classified fact', 'decrypts with seed after verify')
  // unanchored encrypted record is never decrypted
  const enc2 = setup({ agentSeed: seed })
  await enc2.mem.remember('s', 'unanchored secret')
  const rec3 = await enc2.mem.recall('s')
  assert.equal(rec3.items[0]!.text, undefined)
  assert.equal(rec3.items[0]!.unverifiable, true)
})

test('S6.1 regression: plaintext items are SPV-verified like encrypted ones (broken anchor → unverifiable)', async () => {
  const { mem, settle } = setup()
  await mem.remember('s', 'fact one'); await mem.remember('s', 'fact two') // anchored batch of 2
  settle('s')
  const good = await mem.recall('s')
  assert.equal(good.receipt.checkResult, 'verified', 'unencrypted anchored records verify')
  assert.deepEqual(good.items.map(i => i.text), ['fact one', 'fact two'])
  // break the anchor: the BUMP disappears → every returned record is marked
  // unverifiable with a reason, none is served, the receipt is honest
  const txid = mem.tailLink('s')!.txid
  ;(mem as any).anchorTxs.get(txid)!.merklePath = undefined
  const bad = await mem.recall('s')
  assert.equal(bad.receipt.checkResult, 'unverifiable')
  for (const i of bad.items) {
    assert.equal(i.unverifiable, true)
    assert.equal(i.text, undefined)
    assert.match(i.reason!, /^verify: /)
  }
})

test('regression: encrypted records with identical provenance are NOT deduplicated (hdp binds plaintext)', async () => {
  const { mem } = setup({ agentSeed: PrivateKey.fromRandom(), batchPolicy: { maxLeaves: 2 } })
  const prov = { source: 's', time: 1_700_000_000 }
  const a = await mem.remember('e', 'first secret', prov)
  const b = await mem.remember('e', 'second secret', prov)
  assert.equal(b.duplicate, false)
  assert.notEqual(a.hdp, b.hdp)
  assert.ok(b.anchored, 'batch of 2 closes and anchors')
  assert.equal((await mem.remember('e', 'second secret', prov)).duplicate, true, 'a true duplicate is still deduplicated')
})

test('§15 modality: committed into H(d‖p) and the provenance signature; invalid kind refused', async () => {
  const { mem } = setup()
  const prov = { source: 's', time: 1_700_000_000 }
  const plain = await mem.remember('m', 'rule text', prov)
  const modal = await mem.remember('m', 'rule text', { ...prov, modality: { kind: 'temporal' as const, qualifier: 'as of Block 105432, Reg X includes Clause Y' } })
  assert.notEqual(plain.hdp, modal.hdp, 'modality changes the provenance binding hash')
  // the originator signature binds the modality too — tampering invalidates it
  const { signProvenance, verifyProvenance } = await import('@mamem/log')
  const key = PrivateKey.fromRandom()
  const d = new TextEncoder().encode('x')
  const p = signProvenance(key, d, { source: 's', time: 1, signature: new Uint8Array(0), modelId: 'm', modality: { kind: 'deontic', qualifier: 'obligatory under Reg X' } })
  assert.ok(verifyProvenance(key.toPublicKey().toString(), d, p))
  assert.ok(!verifyProvenance(key.toPublicKey().toString(), d, { ...p, modality: { kind: 'deontic', qualifier: 'forbidden under Reg X' } }), 'tampered qualifier fails')
  assert.ok(!verifyProvenance(key.toPublicKey().toString(), d, { ...p, modality: undefined }), 'stripped modality fails')
  // invalid kinds are refused at ingest
  await assert.rejects(() => mem.remember('m', 'y', { modality: { kind: 'bogus', qualifier: 'q' } as any }), /modality must be/)
  await assert.rejects(() => mem.remember('m', 'y', { modality: { kind: 'deontic', qualifier: '' } as any }), /modality must be/)
  // the MCP remember tool passes modality through
  const viaTool = await callTool(mem, 'remember', { shard: 'm', data: 'rule text', modality: { kind: 'epistemic', qualifier: 'believed at t=5' } })
  assert.notEqual(viaTool.hdp, plain.hdp)
})

test('§18 terminal commitment: every receipt is signed; tampering breaks verification', async () => {
  const { mem, settle } = setup()
  await mem.remember('s', 'fact one')
  await mem.anchorShard('s'); settle('s')
  const { receipt } = await mem.recall('s')
  assert.ok(receipt.signature && receipt.signerPub && receipt.timestamp, 'receipt carries σ, signer and τ')
  const { verifyAnswerReceipt } = await import('../src/memory.js')
  assert.deepEqual(verifyAnswerReceipt(receipt), { ok: true })
  assert.equal(verifyAnswerReceipt({ ...receipt, answer: receipt.answer + ' (edited)' }).ok, false, 'edited answer fails')
  assert.equal(verifyAnswerReceipt({ ...receipt, checkResult: 'verified' as const, fragmentRefs: [] }).ok, false, 'edited trace fails')
  assert.equal(verifyAnswerReceipt({ ...receipt, signature: undefined }).ok, false, 'unsigned fails')
  assert.equal(verifyAnswerReceipt({ ...receipt, signature: 'zz' }).ok, false, 'malformed fails')
})

test('mcp memory: tools dispatch + prove NotAnchored is honest', async () => {
  const { mem } = setup()
  const rr = await callTool(mem, 'remember', { shard: 'x', data: 'via tool' })
  assert.equal(rr.seq, 0)
  assert.deepEqual((await mem.prove('x', 0)).result, { ok: false, reason: 'NotAnchored' })
})

test('ZKA: clearance gate — trusted authority, holder-bound, scoped, single-use, level-covering', async () => {
  const authority = PrivateKey.fromRandom()
  const { mem, settle } = setup({ agentSeed: PrivateKey.fromRandom(), maxLevel: 2, clearanceAuthorities: [authority.toPublicKey().toString()] })
  await mem.remember('s', 'classified fact', undefined, 2)
  await mem.anchorShard('s'); settle('s')
  const { key, identity } = callerOf(mem)
  const cred = issueClearance(authority, 3n, identity.pubkey)
  const read = async (zk: ZkClearance, id = identity) => (await mem.recall('s', { seq: 0, zk, identity: id })).items[0]!
  // valid: ℓ=3 proves ≥ 2 for s/0 → served
  const ok = await read({ commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, tr('s/0', 1))!, transcript: tr('s/0', 1), requiredLevel: 2n })
  assert.equal(ok.text, 'classified fact')
  // replay of the same transcript → refused
  const replay = await read({ commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, tr('s/0', 1))!, transcript: tr('s/0', 1), requiredLevel: 2n })
  assert.match(replay.reason!, /replayed/)
  // proof covers level 1 only → level-2 record withheld
  const low = await read({ commitment: cred.commitment, proof: proveAccess(cred, 1n, 8, tr('s/0', 2))!, transcript: tr('s/0', 2), requiredLevel: 1n })
  assert.equal(low.text, undefined); assert.match(low.reason!, /proven level 1 < 2/)
  // transcript scoped to another fragment → refused
  const scoped = await read({ commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, tr('s/9', 3))!, transcript: tr('s/9', 3), requiredLevel: 2n })
  assert.match(scoped.reason!, /transcript scope s\/9 != s\/0/)
  // self-issued clearance (untrusted authority) → refused
  const self = issueClearance(key, 9n, identity.pubkey)
  const untrusted = await read({ commitment: self.commitment, proof: proveAccess(self, 2n, 8, tr('s/0', 4))!, transcript: tr('s/0', 4), requiredLevel: 2n })
  assert.match(untrusted.reason!, /untrusted authority/)
  // someone else's credential presented by this caller → refused (holder-bound)
  const other = issueClearance(authority, 3n, PrivateKey.fromRandom().toPublicKey().toString())
  const stolen = await read({ commitment: other.commitment, proof: proveAccess(other, 2n, 8, tr('s/0', 5))!, transcript: tr('s/0', 5), requiredLevel: 2n })
  assert.match(stolen.reason!, /holder is not the verified caller/)
  // no verified identity → refused even with a valid proof
  const anon = await read({ commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, tr('s/0', 6))!, transcript: tr('s/0', 6), requiredLevel: 2n }, { ...identity, challengeNonce: issueChallenge(), challengeSig: answerChallenge(key, issueChallenge()) })
  assert.match(anon.reason!, /verified caller identity/)
  // no proof at all → level-2 record withheld
  assert.match((await mem.recall('s', { seq: 0, identity })).items[0]!.reason!, /zkaccess required/)
})

test('ZKA: plaintext level>0 records are gated too; instances without trusted authorities refuse', async () => {
  const { mem } = setup()
  await mem.remember('p', 'plain but level 2', undefined, 2)
  const { identity } = callerOf(mem)
  const cred = issueClearance(PrivateKey.fromRandom(), 5n, identity.pubkey)
  const r = (await mem.recall('p', { seq: 0, identity, zk: { commitment: cred.commitment, proof: proveAccess(cred, 2n, 8, tr('p/0'))!, transcript: tr('p/0'), requiredLevel: 2n } })).items[0]!
  assert.equal(r.text, undefined)
  assert.match(r.reason!, /no trusted clearance authority configured/)
  assert.equal((await mem.recall('p')).items[0]!.text, undefined, 'no proof, no plaintext')
})

test('Cedar gates recall — verified identity, ZK-proven level, hash-mismatch; challenge must be issued here', async () => {
  const ps = canonicalizePolicySet('p1', `
permit(principal is Agent, action == Action::"read", resource is Fragment)
when { resource.level <= principal.level };
`)
  const authority = PrivateKey.fromRandom()
  const base = { policy: { policyText: ps.text, anchoredPolicySetHash: ps.hash }, clearanceAuthorities: [authority.toPublicKey().toString()] }
  const { mem, settle } = setup(base)
  await mem.remember('s', 'fact two', undefined, 2)
  await mem.anchorShard('s'); settle('s') // S6.1: plaintext is served only after SPV verify
  const { key, identity } = callerOf(mem)
  assert.equal((await mem.recall('s', { identity })).receipt.cedar, 'deny', 'verified identity, no proven level → DENY')
  // a nonce the caller chose (not issued by the instance) authenticates nothing
  const own = issueChallenge()
  assert.equal((await mem.recall('s', { identity: { ...identity, challengeNonce: own, challengeSig: answerChallenge(key, own) } })).receipt.cedar, 'deny')
  // binding for a different pubkey than the one answering → no principal
  assert.equal((await mem.recall('s', { identity: { ...identity, binding: signBinding(PrivateKey.fromRandom(), 'x', '00'.repeat(32), 0) } })).receipt.cedar, 'deny')
  // a FORGED proof claiming level 5 must not raise the Cedar level (old bypass)
  const cred = issueClearance(authority, 5n, identity.pubkey)
  const forged = await mem.recall('s', { seq: 0, identity, zk: { commitment: cred.commitment, proof: proveAccess(cred, 1n, 8, tr('s/0', 1))!, transcript: tr('s/0', 1), requiredLevel: 5n } })
  assert.equal(forged.receipt.cedar, 'deny'); assert.equal(forged.items[0]!.text, undefined)
  // proven level 3 → Cedar ALLOW + served
  const ok = await mem.recall('s', { seq: 0, identity, zk: { commitment: cred.commitment, proof: proveAccess(cred, 3n, 8, tr('s/0', 2))!, transcript: tr('s/0', 2), requiredLevel: 3n } })
  assert.equal(ok.receipt.cedar, 'allow'); assert.equal(ok.items[0]!.text, 'fact two')
  // tampered instance policy (hash mismatch vs anchored) → refused
  const tampered = setup({ ...base, policy: { policyText: 'permit(principal,action,resource);', anchoredPolicySetHash: ps.hash } })
  await tampered.mem.remember('s', 'x')
  assert.equal((await tampered.mem.recall('s', { identity: callerOf(tampered.mem).identity })).receipt.cedar, 'hash-mismatch')
})

test('mcp tools: challenge / rotate / revoke / authorize receipts (incl. DENY)', async () => {
  const policyText = 'permit(principal, action == Action::"read", resource); permit(principal, action == Action::"write", resource);'
  const { mem } = setup({ batchPolicy: { maxLeaves: 1 }, policy: { policyText, anchoredPolicySetHash: canonicalizePolicySet('i', policyText).hash } })
  await callTool(mem, 'remember', { shard: 't', data: 'x' })
  await callTool(mem, 'anchor', { shard: 't' })
  const rot = await callTool(mem, 'rotate', { shard: 't', newOwnerWif: PrivateKey.fromRandom().toWif() })
  assert.ok(typeof rot.txid === 'string' && rot.receipt.decision === 'allow', 'rotate returns receipt')
  const revoked = PrivateKey.fromRandom().toPublicKey().toString()
  const rev = await callTool(mem, 'revoke', { pkHex: revoked })
  assert.ok(rev.revoked === true && rev.smtRoot.length === 64, 'revoke returns smtRoot')
  const { deriveKey } = await import('@mamem/identity')
  assert.ok(mem.revocations.has(deriveKey(Buffer.from(revoked, 'hex'))), 'SMT keyed by derive_key(pubkey)')
  await assert.rejects(() => callTool(mem, 'revoke', { pkHex: 'aa' }), /33-byte/)
  assert.equal((await callTool(mem, 'authorize', { action: 'read' })).receipt.decision, 'deny', 'unverified principal')
  const key = PrivateKey.fromRandom()
  const { nonce } = await callTool(mem, 'challenge', {})
  const identity = { pubkey: key.toPublicKey().toString(), binding: signBinding(key, 'agent', '00'.repeat(32), 0), challengeNonce: nonce, challengeSig: answerChallenge(key, nonce) }
  assert.equal((await callTool(mem, 'authorize', { action: 'read', identity })).receipt.decision, 'allow', 'verified principal')
})

test('mcp tool: automaton deploy → step → halt; off-δ step refused; honest statement', async () => {
  const { mem } = setup()
  const def = { Q: ['draft', 'review', 'published'], Sigma: ['submit', 'approve'], delta: [[0, 0, 1], [1, 1, 2]], q0: 0, F: [2], O: [] }
  const out = 'ab'.repeat(32)
  const d = await callTool(mem, 'automaton', { op: 'deploy', id: 'doc', def })
  assert.deepEqual([d.q, d.n, d.halted], [0, 0, false])
  assert.match(d.statement, /says nothing about whether the output content is true/)
  await assert.rejects(() => callTool(mem, 'automaton', { op: 'step', id: 'doc', sym: 1, to: 2, outHash: out }), /not in δ/)
  await callTool(mem, 'automaton', { op: 'step', id: 'doc', sym: 0, to: 1, outHash: out })
  const s2 = await callTool(mem, 'automaton', { op: 'step', id: 'doc', sym: 1, to: 2, outHash: out })
  assert.deepEqual([s2.q, s2.n], [2, 2])
  const h = await callTool(mem, 'automaton', { op: 'halt', id: 'doc' })
  assert.equal(h.halted, true)
  assert.equal((await callTool(mem, 'automaton', { op: 'state', id: 'doc' })).txid, h.txid)
})
