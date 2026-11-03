import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { dsha256, utf8, computeLeaf, merkleRoot, merkleProof, toHex } from '@nektar/core'
import { MockChain, MockFunder } from '@nektar/chain'
import { AnchorChain } from '@nektar/anchor'
import { buildProofBundle, verifyFragment, type AnchorLink } from '../src/bundle.js'

const frag = (s: string) => ({
  d: utf8(s),
  p: { source: 'manual', time: 1700000000, signature: new Uint8Array(64), modelId: 'm1' }
})

async function scenario () {
  const chain = new MockChain()
  const funder = new MockFunder(); funder.topup(10)
  for (const u of (funder as any).utxos as Array<{ tx: import('@bsv/sdk').Transaction }>) {
    chain.mempool.set(u.tx.id('hex'), u.tx)
  }
  chain.mine(1)
  const ac = new AnchorChain('s1', PrivateKey.fromRandom(), funder, funder.keyPair, chain)
  const leaves = [frag('oil change'), frag('belt inspection')].map(computeLeaf)
  const R = merkleRoot(leaves)
  const g = await ac.anchor(leaves.length, R)
  chain.mine(1)
  ac.confirm(g.tx.id('hex'), g.tx.merklePath)
  const link0: AnchorLink = { txid: g.tx.id('hex'), outputIndex: 0, n: 0, size: 2, R: toHex(R), Rp: toHex(g.head.data.Rp) }
  const bundle = buildProofBundle({
    shard: 's1', leaf: leaves[0]!, leafIndex: 0, siblings: merkleProof(leaves, 0).siblings,
    batchN: 0, batchSize: leaves.length, batchR: R,
    anchorTx: g.tx, chainTail: [link0]
  })
  return { chain, bundle, ac, leaves, R }
}

test('verify: valid bundle verifies offline against headers', async () => {
  const { chain, bundle } = await scenario()
  const res = await verifyFragment(bundle, chain)
  assert.deepEqual(res, { ok: true, settled: true, anchorState: 'mined' })
})

test('verify: tampered leaf rejected', async () => {
  const { chain, bundle } = await scenario()
  bundle.leaf = toHex(dsha256(utf8('tampered')))
  const res = await verifyFragment(bundle, chain)
  assert.equal(res.ok, false)
  assert.equal((res as any).reason, 'LeafPathMismatch')
})

test('verify: broken anchor chain detected', async () => {
  const { chain, bundle } = await scenario()
  bundle.chainTail[0]!.Rp = toHex(dsha256(utf8('fake')))
  const res = await verifyFragment(bundle, chain)
  assert.equal(res.ok, false)
  assert.equal((res as any).reason, 'ChainInconsistent')
})

test('verify: unmined anchor still verifies locally (settled=false)', async () => {
  const { bundle } = await scenario()
  // strip the BUMP inside the beef: rebuild tx without merklePath
  const { Transaction } = await import('@bsv/sdk')
  const tx = Transaction.fromAtomicBEEF(bundle.anchorBEEF)
  tx.merklePath = undefined as any
  bundle.anchorBEEF = tx.toAtomicBEEF()
  const res = await verifyFragment(bundle, new MockChain())
  // mining is settlement, not validity: leaf path + anchor fields + chain
  // consistency all verify without a BUMP — the result reports unsettled
  assert.equal(res.ok, true)
  assert.equal((res as any).settled, false)
  assert.equal((res as any).anchorState, 'broadcast')
})

// --- leaf-count binding (ADR-002) + tail authentication negatives ---
test('verify: leafIndex >= size rejected', async () => {
  const { chain, bundle } = await scenario()
  bundle.leafIndex = bundle.size
  const r = await verifyFragment(bundle, chain)
  assert.equal(r.ok, false); assert.equal((r as any).reason, 'BadBundle')
})

test('verify: negative leafIndex rejected', async () => {
  const { chain, bundle } = await scenario()
  bundle.leafIndex = -1
  const r = await verifyFragment(bundle, chain)
  assert.equal(r.ok, false)
})

test('verify: path one extra sibling rejected', async () => {
  const { chain, bundle } = await scenario()
  bundle.path = [...bundle.path, bundle.path[0]!]
  const r = await verifyFragment(bundle, chain)
  assert.equal(r.ok, false); assert.equal((r as any).reason, 'BadBundle')
})

test('verify: path one missing sibling rejected', async () => {
  const { chain, bundle } = await scenario()
  bundle.path = bundle.path.slice(0, -1)
  const r = await verifyFragment(bundle, chain)
  assert.equal(r.ok, false); assert.equal((r as any).reason, 'BadBundle')
})

test('verify: odd-batch duplicate-last-leaf claim rejected', async () => {
  // 3 leaves: merkleProof(leaves,2) path includes the self-paired sibling.
  // Claiming index = size (3) must fail binding even though the sibling exists.
  const chain2 = new MockChain()
  const f2 = new MockFunder(); f2.topup(10)
  for (const u of (f2 as any).utxos as Array<{ tx: import('@bsv/sdk').Transaction }>) {
    chain2.mempool.set(u.tx.id('hex'), u.tx)
  }
  chain2.mine(1)
  const { PrivateKey: PK } = await import('@bsv/sdk')
  const { AnchorChain: AC } = await import('@nektar/anchor')
  const { merkleProof: mp, merkleRoot: mr, computeLeaf: cl } = await import('@nektar/core')
  const ac = new AC('odd', PK.fromRandom(), f2, f2.keyPair, chain2)
  const leaves = ['a', 'b', 'c'].map(s => cl({ d: utf8(s), p: { source: 'm', time: 1, signature: new Uint8Array(0), modelId: 'm' } }))
  const R = mr(leaves)
  const g = await ac.anchor(3, R)
  chain2.mine(1)
  const b = buildProofBundle({
    shard: 'odd', leaf: leaves[2]!, leafIndex: 3, // claim index = size!
    siblings: mp(leaves, 2).siblings, batchN: 0, batchSize: 3, batchR: R,
    anchorTx: g.tx, chainTail: [{ txid: g.tx.id('hex'), outputIndex: 0, n: 0, size: 3, R: toHex(R), Rp: toHex(g.head.data.Rp) }]
  })
  const r = await verifyFragment(b, chain2)
  assert.equal(r.ok, false)
})

test('verify: tampered middle tail link with recomputed self-consistent tail fails', async () => {
  const { chain, bundle } = await scenario()
  // recompute Rp of last link so tail is self-consistent but differs from on-chain
  const { chainRp: crp } = await import('@nektar/core')
  const mid = bundle.chainTail[0]!
  const fakeR = dsha256(utf8('middle'))
  mid.R = toHex(fakeR)
  mid.Rp = toHex(crp(new Uint8Array(32), fakeR, mid.size))
  const r = await verifyFragment(bundle, chain)
  assert.equal(r.ok, false); assert.equal((r as any).reason, 'ChainInconsistent')
})

test('verify: last tail entry differing from on-chain fields rejected', async () => {
  const { chain, bundle } = await scenario()
  for (const mutate of [
    (t: any) => { t.n = 7 },
    (t: any) => { t.size = 99 },
    (t: any) => { t.R = toHex(dsha256(utf8('x'))) },
    (t: any) => { t.Rp = toHex(dsha256(utf8('y'))) },
    (t: any) => { t.txid = '00'.repeat(32) }
  ]) {
    const b2 = structuredClone(bundle)
    mutate(b2.chainTail[b2.chainTail.length - 1])
    const r = await verifyFragment(b2, chain)
    assert.equal(r.ok, false, JSON.stringify(mutate))
  }
})
