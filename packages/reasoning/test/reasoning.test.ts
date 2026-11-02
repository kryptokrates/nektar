import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey } from '@bsv/sdk'
import { MockChain, MockFunder, type ChainTracker } from '@mamem/chain'
import { MamemMemory } from '@mamem/mcp'
import { parseProgram } from '@mamem/datalog'
import { PolicyDAG } from '@mamem/graph'
import {
  ground, modelIdentity, commitReceipt, CitationIndex, onRevision, timeBounds,
  buildAuditBundle, verifyAuditBundle, hashOf, type FragmentFetch, type Claim
} from '../src/index.js'

const RULES = parseProgram(`@id r-spec
spec(S, V) :- requires(P, V), applies(S, P).`).rules

/** one anchored manual batch [requires(torque,45), applies(port4,torque)] + helpers */
async function world () {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(20); for (const t of funder.parents) chain.registerParent(t)
  const sk = PrivateKey.fromRandom()
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, signingKey: sk })
  const settle = (shard: string) => { chain.mine(1); const l = mem.tailLink(shard); if (l) mem.confirmAnchor(shard, l.txid) }
  const r0 = await mem.remember('manual', 'requires(torque, 45).')
  const r1 = await mem.remember('manual', 'applies(port4, torque).')
  settle('manual')
  const fetch: FragmentFetch = async ref => {
    const rec = (await mem.records(ref.shard)).find(r => r.seq === ref.seq)
    if (!rec) return { verified: false }
    return { verified: (await mem.prove(ref.shard, ref.seq)).result.ok === true, text: new TextDecoder().decode(rec.fragment.d) }
  }
  return { chain, mem, sk, settle, fetch, promises: [r0.promise, r1.promise] }
}
const m = (seq: number) => ({ shard: 'manual', seq })

test('AGT-003 grounding: quote, entailed, uncited, unverifiable, not-entailed; flag vs remove', async () => {
  const w = await world()
  await w.mem.remember('manual', 'requires(torque, 99).') // pending — never anchored
  const claims: Claim[] = [
    { text: 'requires(torque,   45)', cites: [m(0)] },
    { text: 'port 4 is torqued to 45', literal: 'spec(port4, 45)', cites: [m(0), m(1)] },
    { text: 'the pump is blue', cites: [] },
    { text: 'requires(torque, 99)', cites: [m(2)] },
    { text: 'port 4 is torqued to 99', literal: 'spec(port4, 99)', cites: [m(0), m(1)] }
  ]
  const r = await ground(claims, w.fetch, { rules: RULES })
  assert.deepEqual(r.claims.map(c => c.reason), ['grounded:quote', 'grounded:entailed', 'uncited', 'citation-unverifiable', 'not-entailed'])
  assert.equal(r.kept.length, 5, 'flag mode keeps all claims')
  assert.equal(r.flagged.length, 3)
  const rm = await ground(claims, w.fetch, { rules: RULES, mode: 'remove' })
  assert.deepEqual(rm.kept.map(c => c.text), [claims[0]!.text, claims[1]!.text], 'remove mode drops ungrounded claims')
  assert.equal((await ground(claims, w.fetch, { rules: RULES })).reportHash, r.reportHash, 'report hash deterministic')
  assert.notEqual(rm.reportHash, r.reportHash)
})

test('AGT-011/012 model identity + committed, anchored receipt', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'mamem-'))
  const wp = join(dir, 'weights.gguf'); writeFileSync(wp, Buffer.alloc(3 * 65536 + 17, 7))
  const local = await modelIdentity({ kind: 'local', weightsPath: wp, name: 'qwen-test' })
  assert.deepEqual(local, { kind: 'local', weightsSha256: createHash('sha256').update(Buffer.alloc(3 * 65536 + 17, 7)).digest('hex'), name: 'qwen-test' })
  await assert.rejects(() => modelIdentity({ kind: 'api', provider: 'anthropic', release: '' }), /provider \+ release/)
  const api = await modelIdentity({ kind: 'api', provider: 'anthropic', release: 'claude-x-2026-01-01' })
  const w = await world()
  const base = { answer: 'torque 45', fragmentRefs: [m(0)], checkResult: 'verified' as const, modelId: api, timestamp: 1_700_000_000 }
  const a = await commitReceipt(base)
  assert.equal(a.receiptHash, (await commitReceipt({ ...base })).receiptHash, 'deterministic')
  assert.notEqual(a.receiptHash, (await commitReceipt({ ...base, modelId: local })).receiptHash, 'model identity is bound')
  assert.match(a.receipt.truthNote, /do not show the record is true/)
  const anchored = await commitReceipt(base, w.mem)
  w.settle('__receipts__')
  assert.ok(anchored.anchor?.txid, 'receipt anchored')
  assert.equal((await w.mem.prove('__receipts__', anchored.anchor!.seq)).result.ok, true)
})

test('AOR-008c/AGT-006 correction loop: revision withdraws a claim, correction anchored', async () => {
  const w = await world()
  const idx = new CitationIndex(join(mkdtempSync(join(tmpdir(), 'mamem-')), 'cit.json'))
  const claims: Claim[] = [
    { text: 'port 4 is torqued to 45', literal: 'spec(port4, 45)', cites: [m(0), m(1)] },
    { text: 'applies(port4, torque)', cites: [m(1)] }
  ]
  idx.add({ receiptHash: 'r1', claims })
  idx.add({ receiptHash: 'r2', claims: [claims[1]!] })
  assert.deepEqual(idx.citing(m(0)).map(a => a.receiptHash), ['r1'])
  assert.deepEqual(idx.citing(m(1)).map(a => a.receiptHash).sort(), ['r1', 'r2'])
  // the manual is revised: torque is now 55
  await w.mem.remember('manual', 'requires(torque, 55).')
  await w.mem.remember('manual', 'note(revision, 2).')
  w.settle('manual')
  const older = await w.mem.prove('manual', 0)
  assert.equal(older.result.ok, true, `older batch still provable after a newer anchor: ${JSON.stringify(older.result)}`)
  const corr = await onRevision(idx, m(0), m(2), w.fetch, { rules: RULES, mem: w.mem })
  assert.equal(corr.length, 1)
  assert.equal(corr[0]!.stillGrounded, false)
  assert.deepEqual(corr[0]!.withdrawn, ['port 4 is torqued to 45'])
  w.settle('__corrections__')
  assert.equal((await w.mem.prove('__corrections__', corr[0]!.anchor!.seq)).result.ok, true, 'correction anchored + provable')
  assert.equal(new CitationIndex(idx['file']).citing(m(1)).length, 2, 'index persists')
})

test('AGT-005 consensus time: existence-by the block time; unmined/forged refused', async () => {
  const w = await world()
  const tx = w.mem.anchorTx(w.mem.tailLink('manual')!.txid)!
  const tb = await timeBounds(tx, w.chain)
  const hdr = await w.chain.headerAtHeight(tb.blockHeight)
  assert.equal(tb.notAfter, hdr!.time)
  assert.ok(tb.mtp <= tb.notAfter)
  assert.match(tb.statement, /no later than/)
  const lying = { currentHeight: async () => await w.chain.currentHeight(), headerAtHeight: async (h: number) => await w.chain.headerAtHeight(h), isValidRootForHeight: async () => false } as unknown as ChainTracker
  await assert.rejects(() => timeBounds(tx, lying), /does not verify/)
  const { Transaction } = await import('@bsv/sdk')
  await assert.rejects(() => timeBounds(Transaction.fromHex(tx.toHex()), w.chain), /not mined/)
})

test('AGT-007 audit bundle verifies offline; 4 tamper cases rejected with reasons', async () => {
  const w = await world()
  await w.mem.remember('manual', 'requires(torque, 55).'); await w.mem.remember('manual', 'z.')
  w.settle('manual')
  const b0 = (await w.mem.prove('manual', 0)).bundle!
  const b1 = (await w.mem.prove('manual', 1)).bundle!
  const b2 = (await w.mem.prove('manual', 2)).bundle!
  const dag = new PolicyDAG([{ name: 'org', rules: parseProgram(`@id pol
permit(P, read, R) :- member(P, verified), fragment(R).`).rules }])
  const edb = [{ pred: 'member', args: ['agent-b', 'verified'] }, { pred: 'fragment', args: ['manual-0'] }]
  const derivation = dag.query(edb, 'permit', ['agent-b', 'read', 'manual-0'])!
  const { receipt } = await commitReceipt({ answer: 'port 4: 45', fragmentRefs: [m(0), m(1)], checkResult: 'verified', modelId: { kind: 'api', provider: 'p', release: 'r1' }, timestamp: 1 })
  const pub = w.sk.toPublicKey().toString()
  const bundle = buildAuditBundle({
    receipt,
    fragments: [{ ref: m(0), bundle: b0 }, { ref: m(1), bundle: b1 }],
    signers: [{ ref: m(0), pubkey: pub, promise: w.promises[0]! }, { ref: m(1), pubkey: pub, promise: w.promises[1]! }],
    policy: { edb, derivation }
  })
  const json = JSON.parse(JSON.stringify(bundle)) // offline: travels as plain JSON
  assert.deepEqual(await verifyAuditBundle(json, w.chain, { dag }), { ok: true, reasons: [] })
  const clone = () => JSON.parse(JSON.stringify(bundle))
  const t1 = clone(); t1.receipt.answer = 'port 4: 99'
  assert.deepEqual((await verifyAuditBundle(t1, w.chain, { dag })).reasons, ['receipt hash mismatch'])
  const t2 = clone(); t2.fragments[0].bundle = JSON.parse(JSON.stringify(b2))
  assert.ok((await verifyAuditBundle(t2, w.chain, { dag })).reasons.some(r => /signer promise does not bind fragment manual\/0/.test(r)), 'swapped fragment')
  const t3 = clone(); t3.policy.edb = [{ pred: 'member', args: ['agent-c', 'verified'] }, edb[1]]
  assert.ok((await verifyAuditBundle(t3, w.chain, { dag })).reasons.some(r => r.startsWith('policy derivation:')), 'forged derivation')
  const badHeaders = { ...w.chain, currentHeight: () => w.chain.currentHeight(), headerAtHeight: async (h: number) => await w.chain.headerAtHeight(h), isValidRootForHeight: async () => false } as unknown as ChainTracker
  assert.ok((await verifyAuditBundle(clone(), badHeaders, { dag })).reasons.some(r => /fragment manual\/0: (BEEFInvalid|NotMined)/.test(r)), 'wrong headers')
  assert.ok((await verifyAuditBundle(clone(), w.chain)).reasons.includes('policy derivation present but verifier holds no authoritative policy DAG'))
  assert.equal(hashOf(receipt), bundle.receiptHash)
})

test('GOAL-v8 D8: anchored revocation record convicts a signer — no local isRevoked needed', async () => {
  const w = await world()
  const b0 = (await w.mem.prove('manual', 0)).bundle!
  const b1 = (await w.mem.prove('manual', 1)).bundle!
  const { receipt } = await commitReceipt({ answer: 'port 4: 45', fragmentRefs: [m(0), m(1)], checkResult: 'verified', modelId: { kind: 'api', provider: 'p', release: 'r1' }, timestamp: 1 })
  const pub = w.sk.toPublicKey().toString()
  // revoke the signer's key — the __revocation__ record + its anchor are the
  // third-party-verifiable evidence (revocationProof exposes them)
  w.mem.revoke(pub)
  const rr = await w.mem.remember('__revocation__', `revoke:${pub}:root=aa`)
  await w.mem.anchorShard('__revocation__'); w.settle('__revocation__')
  const rev = await w.mem.revocationProof(pub)
  assert.ok(rev && rev.anchored && rev.bundle, 'revocation record is anchored + provable')
  assert.equal(rev.seq, rr.seq)
  assert.ok(rev.data.startsWith(`revoke:${pub}`))
  const base = () => buildAuditBundle({
    receipt,
    fragments: [{ ref: m(0), bundle: b0 }, { ref: m(1), bundle: b1 }],
    signers: [{ ref: m(0), pubkey: pub, promise: w.promises[0]! }, { ref: m(1), pubkey: pub, promise: w.promises[1]! }]
  })
  // without the carried evidence and without a local checker the bundle verifies
  // (the verifier simply has no revocation source)
  assert.equal((await verifyAuditBundle(base(), w.chain)).ok, true)
  // with the anchored revocation record the signer is convicted even though the
  // verifier holds no isRevoked view — chain data alone convicts
  const withRev = { ...base(), revocations: [{ pubkey: pub, seq: rev.seq, data: rev.data, prov: rev.prov, E: rev.E, leaf: rev.leaf, bundle: rev.bundle! }] }
  const vr = await verifyAuditBundle(withRev, w.chain)
  assert.ok(vr.reasons.some(r => /key revoked \(anchored\)/.test(r)), `anchored revocation convicts: ${vr.reasons.join('; ')}`)
  // a record that does not name the revoked pubkey is rejected as evidence
  const wrongKey = { ...base(), revocations: [{ pubkey: PrivateKey.fromRandom().toPublicKey().toString(), seq: rev.seq, data: rev.data, prov: rev.prov, E: rev.E, leaf: rev.leaf, bundle: rev.bundle! }] }
  assert.ok((await verifyAuditBundle(wrongKey, w.chain)).reasons.some(r => /does not bind this pubkey/.test(r)))
  // an inclusion proof for a DIFFERENT record is not evidence — the leaf
  // recompute (data→leaf) fails before the SPV check is even reached
  const badBundle = { ...base(), revocations: [{ pubkey: pub, seq: rev.seq, data: rev.data, prov: rev.prov, E: rev.E, leaf: b1.leaf, bundle: b1 }] }
  assert.ok((await verifyAuditBundle(badBundle, w.chain)).reasons.some(r => /does not recompute the proven leaf/.test(r)))
  // and a genuinely wrong proof (leaf matches but bundle fails) is refused too
  const badProof = { ...base(), revocations: [{ pubkey: pub, seq: rev.seq, data: rev.data, prov: rev.prov, E: rev.E, leaf: rev.leaf, bundle: { ...rev.bundle!, leaf: 'ff'.repeat(32) } }] }
  assert.ok((await verifyAuditBundle(badProof, w.chain)).reasons.some(r => /inclusion proof failed/.test(r)))
})

test('GOAL-v8 D4: anchored rotation chain — third party replays pk(0)→pk(i) from chain data', async () => {
  const { verifyAnchoredRotationChain } = await import('../src/index.js')
  const w = await world()
  const k1 = PrivateKey.fromRandom()
  await w.mem.rotateShard('manual', k1)
  w.settle('__identity__')
  const proof = await w.mem.rotationChainProof('manual')
  assert.ok(proof && proof.certs.length === 1, 'one anchored cert')
  assert.equal(proof.certs[0]!.cert.pubkey, k1.toPublicKey().toString())
  assert.ok(proof.certs[0]!.anchored, 'cert is SPV-verifiably anchored')
  const v = await verifyAnchoredRotationChain(proof, w.chain)
  assert.deepEqual(v.reasons, [])
  assert.equal(v.activePub, k1.toPublicKey().toString(), 'verified chain resolves the rotated key')
  // negative: a cert swapped for an unanchored record breaks the chain link
  const forged = {
    genesisPub: proof.genesisPub,
    certs: [{ ...proof.certs[0]!, cert: { ...proof.certs[0]!.cert, pubkey: PrivateKey.fromRandom().toPublicKey().toString() } }]
  }
  const vf = await verifyAnchoredRotationChain(forged, w.chain)
  assert.ok(!vf.ok && vf.reasons.some(r => /payload mismatch|do not link/.test(r)), `forged cert refused: ${vf.reasons.join('; ')}`)
  // negative: dropping the bundle leaves the cert unprovable
  const noBundle = { genesisPub: proof.genesisPub, certs: [{ ...proof.certs[0]!, bundle: undefined }] }
  const vn = await verifyAnchoredRotationChain(noBundle, w.chain)
  assert.ok(!vn.ok && vn.reasons.some(r => /no inclusion bundle|do not link/.test(r)))
})
