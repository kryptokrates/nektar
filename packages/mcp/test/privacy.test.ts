// Private shards (remember …, private:true): the owner-declared flag is
// monotone + persisted; plain merkle citations are refused fail-closed, and a
// private cite only survives groundedAnswer when a ZK membership proof
// verifies — committed as {private:true, hdp, anchorTxid} so the shard name
// never reaches the anchored receipt. Prover-side tools are priced above
// answers; the verify side stays free (a verifier never pays to check).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { MockChain, MockFunder } from '@nektar/chain'
import { NektarMemory, type NektarMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { verifyReceipt } from '../src/receipt.js'
import { _testReset, priceFor, PAID_TOOLS } from '../src/pay.js'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

function setup(opts: NektarMemoryOptions = {}) {
  const chain = new MockChain()
  chain.verifyScripts = true
  const funder = new MockFunder()
  funder.topup(20)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, dataDir: mkdtempSync(join(tmpdir(), 'nektar-priv-')), ...opts })
  const settle = (shard: string) => {
    chain.mine(1)
    const l = mem.tailLink(shard)
    if (l) mem.confirmAnchor(shard, l.txid)
  }
  return { chain, mem, settle }
}

test('private shards: the flag is declared at write time and is monotone', async () => {
  const { mem } = setup()
  const r = await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  assert.equal(r.private, true)
  assert.equal(mem.shardPrivate('hr'), true)
  // private:false on an already-private shard is refused — never silent downgrade
  await assert.rejects(() => callTool(mem, 'remember', { shard: 'hr', data: 'x', private: false }), /monotone/)
  // a later write that omits the flag keeps the shard private
  await callTool(mem, 'remember', { shard: 'hr', data: 'headcount 12' })
  assert.equal(mem.shardPrivate('hr'), true)
  // and a shard never declared private stays public
  await callTool(mem, 'remember', { shard: 'pub', data: 'open fact' })
  assert.equal(mem.shardPrivate('pub'), false)
})

test('private flag persists across a restart (stateStore round-trip)', async () => {
  let saved: any
  const store = {
    load: () => saved,
    save: (s: any) => {
      saved = JSON.parse(JSON.stringify(s))
    }
  }
  const chain = new MockChain()
  const f = new MockFunder()
  f.topup(2)
  const m1 = new NektarMemory(chain, f, f.keyPair, { stateStore: store })
  m1.setShardPrivate('hr')
  assert.equal(saved.flags.hr.private, true, 'flag written to the persisted state')
  const m2 = new NektarMemory(chain, f, f.keyPair, { stateStore: store })
  assert.equal(m2.shardPrivate('hr'), true, 'flag restored after restart')
  // an old state file without a flags field restores safely (defaults public)
  saved.flags = undefined
  const m3 = new NektarMemory(chain, f, f.keyPair, { stateStore: store })
  assert.equal(m3.shardPrivate('hr'), false)
})

test('private shards: recall reads them for the owner but marks the policy', async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  const r = await callTool(mem, 'recall', { shard: 'hr' })
  assert.equal(r.items[0].text, 'salary band E5 = 120k', 'owner still reads the record')
  assert.equal(r.shardPrivate, true, 'the policy travels with the answer')
})

test('private shards: plain merkle citations are refused (prove)', async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  const r = await callTool(mem, 'prove', { shard: 'hr', seq: 0 })
  assert.equal(r.ok, false)
  assert.match(r.reason, /private.*ZK|ZK.*membership/i)
  // public shards keep the ordinary path
  await callTool(mem, 'remember', { shard: 'pub', data: 'open fact' })
  await mem.anchorShard('pub')
  settle('pub')
  const p = await callTool(mem, 'prove', { shard: 'pub', seq: 0 })
  assert.ok(p.bundle || p.result?.ok, 'public shard still proves plainly')
})

/** force the STARK prover absent for the duration of a test (auto-prove
 *  attempts must fail fast + fail closed, not depend on the docker image) */
function noProver(): () => void {
  const prev = process.env.NEKTAR_STARK_PROVER
  process.env.NEKTAR_STARK_PROVER = '/nonexistent-nektar-prover'
  return () => {
    prev === undefined ? delete process.env.NEKTAR_STARK_PROVER : (process.env.NEKTAR_STARK_PROVER = prev)
  }
}

test('private cites: groundedAnswer strips them when the proof cannot be produced — nothing leaks', async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  const restore = noProver()
  const r = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'salary band E5 = 120k', cites: [{ shard: 'hr', seq: 0 }] }] })
  restore()
  assert.equal(r.committed, false, 'no surviving claim → no receipt')
  assert.equal(r.kept.length, 0)
  assert.match(r.flagged[0].reason, /private shard.*ZK membership proof/i)
})

test('private cites: AUTO-PROVE — groundedAnswer keeps a plain private cite when the prover runs', { timeout: 300_000 }, async (t) => {
  if (!process.env.NEKTAR_STARK_PROVE) {
    t.skip('set NEKTAR_STARK_PROVE=1 to run the real STARK prover')
    return
  }
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  // no zk supplied — the service self-issues a queryId, proves, verifies, redacts
  const r = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'salary band E5 = 120k', cites: [{ shard: 'hr', seq: 0 }] }] })
  assert.equal(r.committed, true, `auto-proved cite should survive: ${JSON.stringify(r.flagged)}`)
  const ref = r.receipt.fragmentRefs[0]
  assert.equal(ref.private, true)
  assert.equal(ref.shard, undefined, 'shard name redacted')
  assert.match(ref.proofCommit, /^[0-9a-f]{64}$/)
  assert.ok(!JSON.stringify(r.receipt).includes('"hr"'))
  // and the committed proof is a REAL artifact a verifier re-runs
  assert.ok((mem.getProof(ref.proofCommit)?.length ?? 0) > 100_000, 'real STARK receipt stored')
  const v = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v.checks.find((c) => c.name === 'fragment (private)')?.ok, true)
})

test('private cites: a verified ZK proof keeps the claim; the shard name is redacted from the anchored receipt', async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  // stub the service-side ZK verify — the STARK machinery itself is covered by
  // zk-integration.test.ts; this test covers the privacy POLICY wiring.
  const orig = mem.verifyMembership.bind(mem)
  const origRv = mem.reverifyMembershipBytes.bind(mem)
  ;(mem as any).verifyMembership = async () => ({ ok: true, statement: 'stub' })
  const r = await callTool(mem, 'groundedAnswer', {
    claims: [{ text: 'salary band E5 = 120k', cites: [{ shard: 'hr', seq: 0, zk: { queryId: 'aa'.repeat(32), proof: 'deadbeef' } }] }]
  })
  ;(mem as any).verifyMembership = orig
  assert.equal(r.committed, true)
  const ref = r.receipt.fragmentRefs[0]
  assert.equal(ref.private, true)
  assert.equal(ref.shard, undefined, 'the shard name never enters the committed ref')
  assert.match(ref.hdp, /^[0-9a-f]{64}$/, 'the committed fragment hash stays')
  assert.match(ref.anchorTxid, /^[0-9a-f]{64}$/, 'the committed anchor stays')
  assert.match(ref.proofCommit, /^[0-9a-f]{64}$/, 'the proof commitment is committed')
  assert.ok(!JSON.stringify(r.receipt).includes('"hr"'), 'the receipt never names the shard')
  // the committed proof is stored + integrity-pinned by its sha256
  assert.deepEqual([...mem.getProof(ref.proofCommit)!], [...Buffer.from('deadbeef', 'hex')], 'proof fetchable by commit')
  // independent verify: the private cite is re-checked, not trusted
  ;(mem as any).reverifyMembershipBytes = async () => ({ ok: true, anchorTxid: ref.anchorTxid })
  const v = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v.ok, true)
  const priv = v.checks.find((c) => c.name === 'fragment (private)')
  assert.equal(priv!.ok, true)
  assert.match(priv!.detail, /ZK proof re-verified.*shard withheld/)
  // a FAILED re-verify is reported failed — fail-closed, not a green label
  ;(mem as any).reverifyMembershipBytes = async () => ({ ok: false, reason: 'stark invalid' })
  const v2 = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v2.ok, false)
  assert.match(v2.checks.find((c) => c.name === 'fragment (private)')!.detail, /re-verify failed: stark invalid/)
  // a committed proof that is not fetchable fails closed too
  ;(mem as any).reverifyMembershipBytes = origRv
  const origGet = mem.getProof.bind(mem)
  ;(mem as any).getProof = () => undefined
  const v3 = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v3.ok, false)
  assert.match(v3.checks.find((c) => c.name === 'fragment (private)')!.detail, /not fetchable.*cannot re-verify/)
  ;(mem as any).getProof = origGet
})

test('private cites: a FAILED ZK proof strips the claim too (fail-closed)', async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  const orig = mem.verifyMembership.bind(mem)
  ;(mem as any).verifyMembership = async () => ({ ok: false, reason: 'stark invalid', statement: 'stub' })
  const r = await callTool(mem, 'groundedAnswer', {
    claims: [{ text: 'salary band E5 = 120k', cites: [{ shard: 'hr', seq: 0, zk: { queryId: 'aa'.repeat(32), proof: 'deadbeef' } }] }]
  })
  ;(mem as any).verifyMembership = orig
  assert.equal(r.committed, false)
  assert.match(r.flagged[0].reason, /private shard.*ZK membership proof/i)
})

test('private cites: commitReceipt redacts the shard name the same way — and demands the ZK proof', async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await mem.anchorShard('hr')
  settle('hr')
  // no zk → auto-prove is attempted; with the prover absent it refuses (fail-closed)
  const restore = noProver()
  await assert.rejects(() => callTool(mem, 'commitReceipt', { answer: 'x', fragmentRefs: [{ shard: 'hr', seq: 0 }] }), /auto-proven/)
  restore()
  const orig = mem.verifyMembership.bind(mem)
  ;(mem as any).verifyMembership = async () => ({ ok: true, statement: 'stub' })
  const r = await callTool(mem, 'commitReceipt', { answer: 'x', fragmentRefs: [{ shard: 'hr', seq: 0, zk: { queryId: 'aa'.repeat(32), proof: 'deadbeef' } }] })
  ;(mem as any).verifyMembership = orig
  const ref = r.receipt.fragmentRefs[0]
  assert.equal(ref.private, true)
  assert.equal(ref.shard, undefined)
  assert.match(ref.proofCommit, /^[0-9a-f]{64}$/, 'the committed proof is fetchable for re-verification')
  assert.ok(!JSON.stringify(r.receipt).includes('"hr"'))
})

test('zk prover tools are priced when payments are on; the verify side stays free', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  process.env.NEKTAR_OPERATOR_WIF = PrivateKey.fromRandom().toWif([0xef])
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    _testReset()
  })
  assert.equal(priceFor('proveMembership'), 500, 'proving defaults to the ZK tier')
  assert.equal(priceFor('groundedAnswer'), 250)
  assert.equal(priceFor('anchor'), 100)
  const { mem } = setup()
  for (const tool of ['proveMembership', 'proveSubset', 'proveDerivation', 'attestMembership']) {
    const r = await callTool(mem, tool, {})
    assert.equal(r.ok, false, `${tool} must 402 before it runs`)
    assert.equal(r.accepts[0].amount, '500', `${tool} bills at the prover tier`)
  }
  for (const tool of [
    'zkQuery',
    'verifyMembership',
    'verifyCombined',
    'verifySubset',
    'verifyDerivation',
    'verifyAttestation',
    'prove',
    'remember',
    'recall',
    'verifyReceipt'
  ]) {
    assert.ok(!PAID_TOOLS.has(tool), `${tool} stays free — a verifier never pays to check`)
  }
})

test('private cites end-to-end: a REAL STARK keeps the private cite', { skip: process.env.NEKTAR_STARK_PROVE !== '1' }, async () => {
  const { mem, settle } = setup()
  await callTool(mem, 'remember', { shard: 'hr', data: 'salary band E5 = 120k', private: true })
  await callTool(mem, 'remember', { shard: 'hr', data: 'headcount 12' })
  await mem.anchorShard('hr')
  settle('hr')
  const q = mem.issueZkQuery('answer-consumer')
  const { bytes } = await mem.proveMembership('hr', 0, q.queryId)
  const proof = Buffer.from(bytes).toString('hex')
  const r = await callTool(mem, 'groundedAnswer', {
    claims: [{ text: 'salary band E5 = 120k', cites: [{ shard: 'hr', seq: 0, zk: { queryId: q.queryId, proof } }] }]
  })
  assert.equal(r.committed, true)
  const ref = r.receipt.fragmentRefs[0]
  assert.equal(ref.private, true)
  assert.equal(ref.shard, undefined)
  // the REAL STARK re-runs at verifyReceipt — independently, via the tombstone
  // ledger (the queryId was consumed at commit), not by trusting the service
  assert.match(ref.proofCommit, /^[0-9a-f]{64}$/)
  assert.deepEqual([...mem.getProof(ref.proofCommit)!], [...bytes], 'committed proof bytes round-trip')
  const v = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v.ok, true)
  assert.match(v.checks.find((c) => c.name === 'fragment (private)')!.detail, /ZK proof re-verified/)
})
