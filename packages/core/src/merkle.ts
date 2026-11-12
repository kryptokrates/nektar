// Batch Merkle tree — double-SHA-256. Vector-identical to
// anchorchain/packages/merkle: leaf = dsha256(item), internal node =
// dsha256(left | right), odd level self-pairs, all internal byte order.
import { dsha256, concat, eq, type Bytes } from './hash.js'

export interface MerkleProof {
  index: number
  siblings: Bytes[]
}

export interface MerkleTree {
  root: Bytes
  levels: Bytes[][]
}

export function hashLeaf(item: Bytes): Bytes {
  return dsha256(item)
}

export function hashNode(left: Bytes, right: Bytes): Bytes {
  return dsha256(concat(left, right))
}

function nextLevel(level: Bytes[]): Bytes[] {
  const next: Bytes[] = []
  for (let i = 0; i < level.length; i += 2) {
    const l = level[i]!
    const r = i + 1 < level.length ? level[i + 1]! : l
    next.push(hashNode(l, r))
  }
  return next
}

export function buildTree(leaves: Bytes[]): MerkleTree {
  if (leaves.length === 0) throw new Error('EmptyLeaves: no leaves')
  const levels: Bytes[][] = [leaves.slice()]
  let cur = levels[0]!
  while (cur.length > 1) {
    cur = nextLevel(cur)
    levels.push(cur)
  }
  return { root: cur[0]!, levels }
}

export function merkleRoot(leaves: Bytes[]): Bytes {
  return buildTree(leaves).root
}

export function merkleProof(leaves: Bytes[], index: number): MerkleProof {
  if (index < 0 || index >= leaves.length) {
    throw new Error(`IndexOutOfRange: index ${index}`)
  }
  const tree = buildTree(leaves)
  const siblings: Bytes[] = []
  let pos = index
  for (let level = 0; level < tree.levels.length - 1; level++) {
    const nodes = tree.levels[level]!
    siblings.push(pos % 2 === 0 ? (pos + 1 < nodes.length ? nodes[pos + 1]! : nodes[pos]!) : nodes[pos - 1]!)
    pos = Math.floor(pos / 2)
  }
  return { index, siblings }
}

export function reconstructRoot(leaf: Bytes, proof: MerkleProof): Bytes {
  let cur = leaf
  let idx = proof.index
  for (const sib of proof.siblings) {
    cur = (idx & 1) === 0 ? hashNode(cur, sib) : hashNode(sib, cur)
    idx = idx >> 1
  }
  return cur
}

export function verifyProof(leaf: Bytes, proof: MerkleProof, root: Bytes): boolean {
  return eq(reconstructRoot(leaf, proof), root)
}

export function heightForLeafCount(leafCount: number): number {
  if (leafCount <= 1) return 0
  let height = 0
  let size = 1
  while (size < leafCount) {
    size *= 2
    height++
  }
  return height
}
