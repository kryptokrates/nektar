import { test } from 'node:test'
import assert from 'node:assert/strict'
import { hashLeaf, buildTree, merkleRoot, merkleProof, reconstructRoot, verifyProof, heightForLeafCount } from '../src/merkle.js'
import { dsha256, toHex, fromHex, eq } from '../src/hash.js'

const leaf = (n: number) => hashLeaf(Uint8Array.of(n))

test('merkle: odd self-pairing and proof round-trip (anchorchain parity)', () => {
  const a = leaf(1), b = leaf(2), c = leaf(3)
  const root3 = merkleRoot([a, b, c])
  const tree = buildTree([a, b, c])
  assert.ok(eq(tree.root, root3))
  for (let size = 1; size <= 33; size++) {
    const leaves = Array.from({ length: size }, (_, i) => hashLeaf(dsha256(Uint8Array.of(size, i))))
    const root = merkleRoot(leaves)
    for (let idx = 0; idx < size; idx++) {
      const proof = merkleProof(leaves, idx)
      assert.equal(verifyProof(leaves[idx]!, proof, root), true)
      assert.equal(proof.siblings.length, heightForLeafCount(size))
    }
  }
})

test('merkle: tamper detection', () => {
  const leaves = Array.from({ length: 12 }, (_, i) => leaf(i + 1))
  const root = merkleRoot(leaves)
  const proof = merkleProof(leaves, 5)
  assert.equal(verifyProof(leaves[6]!, proof, root), false)
  assert.equal(verifyProof(leaves[5]!, { index: 9, siblings: proof.siblings }, root), false)
  const bad = Uint8Array.from(root); bad[0]! ^= 0xff
  assert.equal(verifyProof(leaves[5]!, proof, bad), false)
})

// anchorchain parity vector: BSV mainnet block 181 (2 txids, root from the block)
test('merkle: vector-identical with anchorchain (block 181)', () => {
  const txids = [
    '8347cee4a1cb5ad1bb0d92e86e6612dbf6cfc7649c9964f210d4069b426e720a',
    'a16f3ce4dd5deb92d98ef5cf8afeaf0775ebca408f708b2146c4fb42b41e14be'
  ]
  // anchorchain uses internal byte order = reversed display hex
  const leaves = txids.map(t => Uint8Array.from(fromHex(t)).reverse())
  const root = merkleRoot(leaves)
  const display = toHex(Uint8Array.from(root).reverse())
  assert.equal(display, 'ed92b1db0b3e998c0a4351ee3f825fd5ac6571ce50c050b4b45df015092a6c36')
})
