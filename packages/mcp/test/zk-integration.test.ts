import test, { after } from 'node:test'
import assert from 'node:assert'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { PrivateKey } from '@bsv/sdk'
import { MockChain, MockFunder } from '@mamem/chain'
import { MamemMemory } from '../src/index.js'
import {
  loadArtifacts, vkeyHashOf, MemoryNullifierStore, shutdownZk,
  verifyMembershipAnchored, verifyCombined, poseidon, PoseidonTree, leafCommit, blindingOf, proveMembership
} from '@mamem/zk'
import type { ZkQuery } from '@mamem/zk'

const ART = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'zk', 'artifacts')
const Q: ZkQuery = { nonce: new Uint8Array(16).fill(3), verifierId: 'auditor' }
after(async () => await shutdownZk()) // terminate snarkjs BN128 workers cleanly

async function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(30); for (const u of (funder as any).utxos) chain.registerParent(u.tx)
  const shardKey = PrivateKey.fromRandom()
  const mem = new MamemMemory(chain, funder, funder.keyPair, { maxLevel: 2, batchPolicy: { maxLeaves: 4 }, shardKeys: new Map([['s1', shardKey]]) })
  for (const d of ['m0', 'm1', 'm2', 'm3']) await mem.remember('s1', d, {}, 1)
  chain.mine(1)
  return { chain, funder, mem, shardKey }
}
const artifacts = loadArtifacts(ART)
const vkeyHash = vkeyHashOf(artifacts.vkey)

test('anchored membership: proof verifies via verifyMembershipAnchored against the live anchor', async () => {
  const { chain, mem } = await setup()
  const anchorTx = mem.anchorTx(mem.anchorHead('s1')!.tx.id('hex'))!
  const bytes = await mem.proveMembership('s1', 0, Q, mem.zkArtifactsDir())
  const { decodeMamemAnchor } = await import('@mamem/contracts')
  const shardId = decodeMamemAnchor(anchorTx.outputs[0].lockingScript.toHex())!.shard
  const res = await verifyMembershipAnchored(bytes, anchorTx, chain, Q, `${ART}/vkey.json`, new MemoryNullifierStore(), { shard: shardId })
  assert.ok(res.ok, `anchored verify: ${JSON.stringify(res)}`)
})

test('anchored membership negatives', async () => {
  const { chain, mem, shardKey } = await setup()
  const anchorTx = mem.anchorTx(mem.anchorHead('s1')!.tx.id('hex'))!
  const { decodeMamemAnchor } = await import('@mamem/contracts')
  const shardId = decodeMamemAnchor(anchorTx.outputs[0].lockingScript.toHex())!.shard
  const bytes = await mem.proveMembership('s1', 0, Q, mem.zkArtifactsDir())
  const fresh = () => new MemoryNullifierStore()
  const exp = { shard: shardId }

  // wrong shard: a different shard's anchor → 'anchor shard mismatch'
  const f2 = new MockFunder(); f2.topup(4); for (const u of (f2 as any).utxos) chain.registerParent(u.tx)
  const mem2 = new MamemMemory(chain, f2, f2.keyPair, { batchPolicy: { maxLeaves: 1 } })
  await mem2.remember('other', 'x', {}, 0)
  chain.mine(1)
  const otherAnchor = mem2.anchorTx(mem2.anchorHead('other')!.tx.id('hex'))!
  const wrongShard = await verifyMembershipAnchored(bytes, otherAnchor, chain, Q, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(wrongShard.ok, false); assert.equal(wrongShard.reason, 'anchor shard mismatch')

  // not on the canonical chain: a valid mamem anchor from a foreign chain
  const notCanon = await verifyMembershipAnchored(bytes, anchorTx, chain, Q, `${ART}/vkey.json`, fresh(), { shard: shardId, isCanonical: () => false })
  assert.equal(notCanon.ok, false); assert.equal(notCanon.reason, 'anchor not on canonical shard chain')

  // root of a batch never anchored (build a tree locally, prove against it)
  const p = await poseidon()
  const rec = (await mem.records('s1')).find(r => r.seq === 0)!
  const fakeTree = new PoseidonTree(p, [leafCommit(p, rec.leaf, blindingOf(shardKey, 's1#0')), 99n])
  const fakeBytes = await proveMembership({ leaf32: rec.leaf, blinding: blindingOf(shardKey, 's1#0'), index: 0, tree: fakeTree, query: Q, artifacts, vkeyHash })
  const unanchored = await verifyMembershipAnchored(fakeBytes, anchorTx, chain, Q, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(unanchored.ok, false); assert.equal(unanchored.reason, 'root != anchored poseidonRoot')

  // anchor tx not SPV-valid (chain that doesn't contain it)
  const otherChain = new MockChain()
  const spvFail = await verifyMembershipAnchored(bytes, anchorTx, otherChain, Q, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(spvFail.ok, false)
  assert.ok(/NotMined|BEEFInvalid/.test(String(spvFail.reason)), `expected SPV failure, got ${spvFail.reason}`)

  // tampered public signal: flip a root byte to another canonical value
  const tampered = bytes.slice(); tampered[37] ^= 0x01
  const tps = await verifyMembershipAnchored(tampered, anchorTx, chain, Q, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(tps.ok, false); assert.equal(tps.reason, 'root != anchored poseidonRoot')

  // tampered proof
  const tp = bytes.slice(); tp[133] ^= 0x01
  const tpRes = await verifyMembershipAnchored(tp, anchorTx, chain, Q, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(tpRes.ok, false); assert.equal(tpRes.reason, 'groth16 verify failed')

  // replayed nullifier
  const store = new MemoryNullifierStore()
  await verifyMembershipAnchored(bytes, anchorTx, chain, Q, `${ART}/vkey.json`, store, exp)
  const replay = await verifyMembershipAnchored(bytes, anchorTx, chain, Q, `${ART}/vkey.json`, store, exp)
  assert.equal(replay.ok, false); assert.equal(replay.reason, 'nullifier replay')

  // different queryId
  const otherQ = await verifyMembershipAnchored(bytes, anchorTx, chain, { nonce: Q.nonce, verifierId: 'other' }, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(otherQ.ok, false); assert.equal(otherQ.reason, 'queryId mismatch')

  // combined Π with mismatched nonce → verifyCombined false
  const { queryIdOf } = await import('@mamem/zk')
  assert.equal(verifyCombined(Q, { queryId: queryIdOf(Q).toString(), nonce: new Uint8Array(16).fill(1) }), false, 'mismatched nonce rejects')

  // wrong blinding → proof generates but root won't match → root mismatch reason
  const wrongB = await proveMembership({ leaf32: rec.leaf, blinding: 999n, index: 0, tree: fakeTree, query: Q, artifacts, vkeyHash })
  const wbRes = await verifyMembershipAnchored(wrongB, anchorTx, chain, Q, `${ART}/vkey.json`, fresh(), exp)
  assert.equal(wbRes.ok, false); assert.equal(wbRes.reason, 'root != anchored poseidonRoot')
})

test('verifyMembershipAnchored rejects an anchor with no poseidonRoot (legacy/opt-out)', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(4); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const { AnchorChain } = await import('@mamem/anchor')
  const { dsha256, utf8 } = await import('@mamem/core')
  const ac = new AnchorChain('legacy', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(1, dsha256(utf8('legacy batch'))) // default empty poseidonRoot
  const legacyTx = ac.getHead()!.tx
  chain.mine(1)
  const { decodeMamemAnchor } = await import('@mamem/contracts')
  const res2 = await verifyMembershipAnchored(new Uint8Array(397).fill(1), legacyTx, chain, Q, `${ART}/vkey.json`, new MemoryNullifierStore(), { shard: decodeMamemAnchor(legacyTx.outputs[0].lockingScript.toHex())!.shard })
  assert.equal(res2.ok, false)
  assert.equal(res2.reason, 'anchor has no poseidonRoot (legacy/opt-out batch)', 'empty poseidonRoot reason')
})
