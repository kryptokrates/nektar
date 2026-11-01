// DIS-002 proof sharding (GOAL-v6 S5): L_ZKDis v2 subset proofs checked
// against real MamemAnchor transactions on the mock chain — every check of
// verifySubsetAnchored — plus MamemMemory.proveSubset/verifySubset end to end
// and the MAMEM_PROOF_SHARDING opt-out.
import test from 'node:test'
import assert from 'node:assert/strict'
import { MockChain, MockFunder } from '@mamem/chain'
import { AnchorChain } from '@mamem/anchor'
import { decodeMamemAnchor } from '@mamem/contracts'
import { dsha256, utf8, merkleRoot } from '@mamem/core'
import { buildSubsetProof, encodeZkdis, verifySubsetAnchored } from '@mamem/zk'
import { MamemMemory, callTool } from '../src/index.js'

const leaves = (n: number) => Array.from({ length: n }, (_, i) => dsha256(utf8(`rec-${i}`)))
const subsetBytes = (n: number, idx: number[]) => encodeZkdis(buildSubsetProof(leaves(n), idx))

async function anchoredBatch (R: Uint8Array, size: number, mine = true) {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(6); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('dis', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(size, R)
  const tx = ac.getHead()!.tx
  if (mine) { chain.mine(1); ac.confirm(tx.id('hex'), chain.merklePathOf(tx.id('hex'))) }
  return { chain, tx, shard: decodeMamemAnchor(tx.outputs[0]!.lockingScript.toHex())!.shard }
}

test('subset proof verifies against a real anchor whose R is the batch root (records across the shard boundary)', async () => {
  const { chain, tx, shard } = await anchoredBatch(merkleRoot(leaves(70)), 70)
  const res = await verifySubsetAnchored(subsetBytes(70, [1, 63, 64, 69]), tx, chain, { shard })
  assert.ok(res.ok, JSON.stringify(res))
  assert.equal(res.anchorTxid, tx.id('hex'))
  assert.deepEqual(res.indices, [1, 63, 64, 69])
})

test('subset proof NEGATIVES (each check fails closed with a specific reason)', async () => {
  const good = await anchoredBatch(merkleRoot(leaves(70)), 70)
  const bytes = () => subsetBytes(70, [1, 63, 64])
  const v = (b: Uint8Array, a = good, exp: any = { shard: good.shard }) => verifySubsetAnchored(b, a.tx, a.chain, exp)
  const cases: Array<[string, Promise<{ ok: boolean, reason?: string }>, RegExp]> = [
    ['anchor of another batch (R differs)', (async () => { const o = await anchoredBatch(merkleRoot(leaves(71)), 71); return v(bytes(), o, { shard: o.shard }) })(), /not the anchored R/],
    ['anchored size differs', (async () => { const o = await anchoredBatch(merkleRoot(leaves(70)), 71); return v(bytes(), o, { shard: o.shard }) })(), /size/],
    ['anchor of another shard', v(bytes(), good, { shard: '00'.repeat(20) }), /another shard/],
    ['anchor not canonical', v(bytes(), good, { shard: good.shard, isCanonical: () => false }), /canonical/],
    ['anchor not mined', (async () => { const o = await anchoredBatch(merkleRoot(leaves(70)), 70, false); return v(bytes(), o, { shard: o.shard }) })(), /NotMined/],
    ['anchor not SPV-valid on this header chain', verifySubsetAnchored(bytes(), good.tx, new MockChain(), { shard: good.shard }), /BEEFInvalid|NotMined/],
    ['tampered disclosed leaf', (() => { const b = bytes(); b[60] ^= 1; return v(b) })(), /reconstructed|disagree|codec/],
    ['truncated bytes', v(bytes().slice(0, 60)), /codec/]
  ]
  for (const [name, p, re] of cases) {
    const r = await p
    assert.equal(r.ok, false, name)
    assert.match(String(r.reason), re, `${name}: ${r.reason}`)
  }
})

test('MamemMemory: proveSubset + verifySubset over its own anchored batch; MAMEM_PROOF_SHARDING=0 → disabled', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(20); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const mem = new MamemMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 100 } })
  for (let i = 0; i < 70; i++) await mem.remember('s', `rec-${i}`)
  await mem.anchorShard('s')
  chain.mine(1); mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const seqs = [1, 63, 64, 69]
  const sub = await mem.proveSubset('s', seqs)
  assert.ok(sub.ok, JSON.stringify(sub))
  assert.deepEqual(sub.indices, seqs)
  const ok = await mem.verifySubset('s', sub.bytes)
  assert.ok(ok.ok, JSON.stringify(ok))
  assert.deepEqual(ok.seqs, seqs, 'the verifier recovers WHICH records were disclosed')
  assert.equal(ok.size, 70)
  // through the MCP tool layer: hex in → verified verdict + statement
  const viaTool = await callTool(mem, 'proveSubset', { shard: 's', seqs: [0, 1] })
  assert.ok(viaTool.lzkdis)
  const ver = await callTool(mem, 'verifySubset', { shard: 's', bytes: viaTool.lzkdis })
  assert.equal(ver.ok, true, JSON.stringify(ver))
  assert.match(ver.statement, /not truth/)
  // negatives at the memory layer
  const miss = await mem.proveSubset('s', [999]); assert.equal(miss.ok, false); if (!miss.ok) assert.match(miss.reason, /no record/)
  const dup = await mem.proveSubset('s', [0, 0]); assert.equal(dup.ok, false); if (!dup.ok) assert.match(dup.reason, /duplicate/)
  // a second batch anchors; a subset spanning both batches is refused
  await mem.remember('s', 'later-1'); await mem.remember('s', 'later-2')
  await mem.anchorShard('s'); chain.mine(1); mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const span = await mem.proveSubset('s', [0, 70]); assert.equal(span.ok, false); if (!span.ok) assert.match(span.reason, /multiple batches/)
  assert.match(String((await mem.verifySubset('s', sub.bytes.slice(0, 20))).reason), /codec/)
  const foreign = subsetBytes(5, [1])
  assert.match(String((await mem.verifySubset('s', foreign)).reason), /no anchor|not the anchored/)
  // flag off → 'disabled' (the patent opt-out for cautious operators)
  process.env.MAMEM_PROOF_SHARDING = '0'
  try {
    const off1 = await mem.proveSubset('s', [0]); assert.equal(off1.ok, false); if (!off1.ok) assert.equal(off1.reason, 'disabled')
    assert.equal((await mem.verifySubset('s', sub.bytes)).reason, 'disabled')
    assert.equal((await callTool(mem, 'proveSubset', { shard: 's', seqs: [0] })).reason, 'disabled')
    assert.equal((await callTool(mem, 'verifySubset', { shard: 's', bytes: 'aa' })).reason, 'disabled')
  } finally { delete process.env.MAMEM_PROOF_SHARDING }
})
