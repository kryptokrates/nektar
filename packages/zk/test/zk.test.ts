import test, { after } from 'node:test'
import assert from 'node:assert'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  poseidon, PoseidonTree, leafCommit, splitLeafHash, MAX_LEAVES,
  encodeZkMem, decodeZkMem, ZkCodecError, BN254_R, BN254_Q,
  queryIdOf, verifyCombined, MemoryNullifierStore,
  proveMembership, verifyMembership, loadArtifacts, vkeyHashOf, shutdownZk
} from '../src/index.js'

const ART = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'artifacts')
const Q = { nonce: new Uint8Array(16).fill(7), verifierId: 'verifier-x' }
after(async () => await shutdownZk()) // terminate snarkjs BN128 workers cleanly

test('PoseidonTree: deterministic root + path verifies against circuit structure', async () => {
  const p = await poseidon()
  const leaves = [11n, 22n, 33n, 44n, 55n]
  const t1 = new PoseidonTree(p, leaves)
  const t2 = new PoseidonTree(p, leaves)
  assert.equal(t1.root(), t2.root(), 'deterministic root')
  // manually recompute root for leaf 0 path
  const path = t1.path(0)
  let cur = leaves[0]
  for (let d = 0; d < 16; d++) {
    const [l, r] = path.bits[d] === 0 ? [cur, path.elements[d]] : [path.elements[d], cur]
    cur = BigInt(p.F.toObject(p([l, r])))
  }
  assert.equal(cur, t1.root(), 'path reconstructs root')
})

test('PoseidonTree: capacity error >65536', async () => {
  const p = await poseidon()
  assert.throws(() => new PoseidonTree(p, new Array(MAX_LEAVES + 1).fill(1n)), /capacity exceeded/)
})

test('L_ZKMem: round-trip + malformed cases', () => {
  const zm = {
    vkeyHash: new Uint8Array(32).fill(9),
    root: 123n, nullifier: 456n, queryId: 789n,
    pi_a: [1n, 2n] as [bigint, bigint],
    pi_b: [[3n, 4n], [5n, 6n]] as [[bigint, bigint], [bigint, bigint]],
    pi_c: [7n, 8n] as [bigint, bigint]
  }
  const enc = encodeZkMem(zm)
  const dec = decodeZkMem(enc)
  assert.deepEqual(dec, zm, 'round-trip')
  assert.throws(() => decodeZkMem(enc.subarray(0, enc.length - 1)), ZkCodecError, 'short rejected')
  assert.throws(() => decodeZkMem(new Uint8Array([...enc, 0])), ZkCodecError, 'long rejected')
  const badMagic = enc.slice(); badMagic[0] = 0x58
  assert.throws(() => decodeZkMem(badMagic), /bad magic/)
  const badVer = enc.slice(); badVer[4] = 2
  assert.throws(() => decodeZkMem(badVer), /bad version/)
  const nonCanon = enc.slice(); nonCanon.fill(0xff, 37, 69)
  assert.throws(() => decodeZkMem(nonCanon), /non-canonical/, 'non-canonical signal rejected')
})

test('L_ZKMem: coordinate in [r,q) accepted for pi_* but rejected for signals', () => {
  const inBetween = BN254_R + 1n // ≥ r, < q
  assert.ok(inBetween < BN254_Q)
  const zm = {
    vkeyHash: new Uint8Array(32), root: 1n, nullifier: 1n, queryId: 1n,
    pi_a: [inBetween, 1n] as [bigint, bigint],
    pi_b: [[1n, 1n], [1n, 1n]] as [[bigint, bigint], [bigint, bigint]],
    pi_c: [1n, 1n] as [bigint, bigint]
  }
  assert.doesNotThrow(() => decodeZkMem(encodeZkMem(zm)), 'pi_a coord in [r,q) OK')
  const bad = { ...zm, root: inBetween }
  assert.throws(() => encodeZkMem(bad), /out of range/, 'signal coord ≥ r rejected')
})

test('verifyCombined: same query passes; mismatched nonce and verifierId reject', () => {
  const qid = queryIdOf(Q)
  const t = { queryId: qid.toString(), nonce: Q.nonce }
  assert.ok(verifyCombined(Q, t), 'same query passes')
  assert.ok(!verifyCombined(Q, { ...t, nonce: new Uint8Array(16).fill(1) }), 'mismatched nonce rejects')
  const other = { nonce: Q.nonce, verifierId: 'verifier-y' }
  assert.ok(!verifyCombined(other, t), 'mismatched verifierId rejects')
})

test('prove → encode → decode → verify round-trip (end-to-end, real artifacts)', async () => {
  const artifacts = loadArtifacts(ART)
  const vkeyHash = vkeyHashOf(artifacts.vkey)
  const p = await poseidon()
  const ownerKey = { fragmentId: 'shard-x#0' } as any
  // deterministic leaves + blinding
  const leaf32 = new Uint8Array(32); leaf32[0] = 0xab
  const blinding = 424242n
  const leaves = [leafCommit(p, leaf32, blinding), 7n, 8n, 9n]
  const tree = new PoseidonTree(p, leaves)
  const bytes = await proveMembership({ leaf32, blinding, index: 0, tree, query: Q, artifacts, vkeyHash })
  const store = new MemoryNullifierStore()
  const res = await verifyMembership(bytes, { poseidonRoot: tree.root(), anchorTxid: 'aa'.repeat(32) }, Q, artifacts.vkey, store)
  assert.ok(res.ok, `verify ok: ${JSON.stringify(res)}`)
  // negatives
  const store2 = new MemoryNullifierStore()
  const wrongRoot = await verifyMembership(bytes, { poseidonRoot: tree.root() + 1n, anchorTxid: 'aa'.repeat(32) }, Q, artifacts.vkey, store2)
  assert.equal(wrongRoot.ok, false, 'wrong root rejected')
  assert.equal(wrongRoot.reason, 'root != anchored poseidonRoot')
  // tampered proof bytes
  const tampered = bytes.slice(); tampered[133] ^= 0x01
  const t2 = await verifyMembership(tampered, { poseidonRoot: tree.root(), anchorTxid: 'aa'.repeat(32) }, Q, artifacts.vkey, new MemoryNullifierStore())
  assert.equal(t2.ok, false, 'tampered proof rejected')
  assert.equal(t2.reason, 'groth16 verify failed', 'tampered proof reason')
  // replay: same proof against fresh store-but-nullifier-already-seen
  const replay = await verifyMembership(bytes, { poseidonRoot: tree.root(), anchorTxid: 'aa'.repeat(32) }, Q, artifacts.vkey, store)
  assert.equal(replay.ok, false, 'nullifier replay rejected')
  assert.equal(replay.reason, 'nullifier replay')
  // wrong blinding → proof generates but root won't match the tree built with right blinding
  const wrongBlindingBytes = await proveMembership({ leaf32, blinding: 999n, index: 0, tree, query: Q, artifacts, vkeyHash })
  const wb = await verifyMembership(wrongBlindingBytes, { poseidonRoot: tree.root(), anchorTxid: 'aa'.repeat(32) }, Q, artifacts.vkey, new MemoryNullifierStore())
  assert.equal(wb.ok, false, 'wrong blinding produces non-matching root → rejected')
  assert.equal(wb.reason, 'root != anchored poseidonRoot', 'wrong blinding reason')
})
