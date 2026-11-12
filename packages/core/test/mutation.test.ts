// TST-006: Merkle mutation suite. For every batch size 1..33 and every leaf,
// every single-node mutation of the proof (leaf, each sibling, index, claimed
// size, path length) must fail under the leaf-count-bound verification that
// verifyFragment applies (index < size, |path| = height(size)).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { merkleRoot, merkleProof, reconstructRoot, heightForLeafCount, dsha256, utf8, eq, type Bytes, type MerkleProof } from '../src/index.js'

// The path alone does NOT bind the leaf count (sizes 3 and 4 share height and,
// with odd self-pairing, the root). verifyFragment binds it by requiring the
// claimed size to equal the size committed in the on-chain anchor — modelled
// here by `anchoredSize`.
let anchoredSize = 0
function accept(leaf: Bytes, proof: MerkleProof, size: number, root: Bytes): boolean {
  if (size !== anchoredSize) return false
  if (!Number.isInteger(proof.index) || proof.index < 0 || proof.index >= size) return false
  if (proof.siblings.length !== heightForLeafCount(size)) return false
  return eq(reconstructRoot(leaf, proof), root)
}
const flip = (b: Bytes, i: number) => {
  const c = Uint8Array.from(b)
  c[i % c.length] = c[i % c.length] ^ 0x01
  return c
}

test('TST-006: every single-node mutation fails, sizes 1..33, every leaf', () => {
  let mutants = 0
  for (let size = 1; size <= 33; size++) {
    const leaves = Array.from({ length: size }, (_, i) => dsha256(utf8(`leaf-${size}-${i}`)))
    const root = merkleRoot(leaves)
    anchoredSize = size
    if (size === 3) {
      // document the hazard the anchored size closes
      const pr = merkleProof(leaves, 0)
      assert.ok(eq(reconstructRoot(leaves[0], pr), root) && heightForLeafCount(4) === heightForLeafCount(3), 'path alone cannot tell 3 from 4 leaves')
    }
    for (let i = 0; i < size; i++) {
      const pr = merkleProof(leaves, i)
      assert.ok(accept(leaves[i], pr, size, root), `honest proof size=${size} i=${i}`)
      const reject = (leaf: Bytes, p: MerkleProof, s: number, what: string) => {
        mutants++
        assert.equal(accept(leaf, p, s, root), false, `${what} size=${size} i=${i}`)
      }
      for (let byte = 0; byte < 32; byte += 11) reject(flip(leaves[i], byte), pr, size, `leaf byte ${byte}`)
      pr.siblings.forEach((_, k) => reject(leaves[i], { index: i, siblings: pr.siblings.map((s, j) => (j === k ? flip(s, k) : s)) }, size, `sibling ${k}`))
      for (let j = 0; j < size + 2; j++) if (j !== i) reject(leaves[i], { index: j, siblings: pr.siblings }, size, `index ${i}→${j}`)
      reject(leaves[i], { index: i, siblings: [...pr.siblings, root] }, size, 'extra sibling')
      if (pr.siblings.length) reject(leaves[i], { index: i, siblings: pr.siblings.slice(0, -1) }, size, 'missing sibling')
      for (const s of [size - 1, size + 1, size * 2]) if (s >= 1 && s !== size) reject(leaves[i], pr, s, `claimed size ${s}`)
      // the odd-level self-pair ("duplicate last leaf", CVE-2012-2459 shape) as a forged extra leaf
      if (size % 2 === 1 && size > 1)
        reject(leaves[size - 1], { index: size, siblings: merkleProof(leaves, size - 1).siblings }, size, 'duplicate-last-leaf index')
    }
  }
  assert.ok(mutants > 15_000, `mutants checked: ${mutants}`)
})
