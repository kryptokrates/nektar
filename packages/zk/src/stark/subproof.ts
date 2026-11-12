// L_ZKDis v2 — selective proof of existence for a SUBSET of an anchored
// batch's records (GOAL-v6 S5 "proof sharding"; DIS-002). nChain US 12,730,827
// B2 (data-reveal path over a hierarchical structure) is implemented under the
// Open BSV pledge — free use on BSV only, which is all nektar ever runs on;
// see NOTICE. NEKTAR_PROOF_SHARDING=0 opts the MCP tools out.
//
//   'ZKS2' ‖ ver(1) ‖ batchRoot(32) ‖ size(u32 LE) ‖ numShards(u32 LE) ‖
//     per shard, ascending shardIndex:
//       shardIndex(u32 LE) ‖ numLeaves(u16 LE) ‖
//         per leaf, ascending leafIndex:
//           leafIndex(u32 LE) ‖ leafHash(32) ‖ subpathLen(u8) ‖ subpathLen×32
//   ‖ topPathLen(u32 LE) ‖ topPathLen×32
//
// Virtual sharding: the batch tree itself supplies the shard structure —
// shard j is the 64-leaf subtree fold of leaves [64j, min(64j+64, size)),
// i.e. the tree's own level-6 node at index j (odd-position self-pairing
// included). The anchored root R_n is UNCHANGED. Every disclosed leaf's
// standard Merkle path is split at depth 6: a per-leaf subpath (≤6 siblings)
// folds the leaf up to its shard root, and ONE shared multiproof (topPath,
// deduplicated across shards) folds every shard root up to R_n.
import { buildTree, hashNode, eq, heightForLeafCount, type MerkleTree } from '@nektar/core'

export const SHARD_LEAVES = 64
export const SHARD_DEPTH = 6 // log2(SHARD_LEAVES): leafIndex >> 6 = shardIndex
export const ZKS2_MAGIC = Uint8Array.from([0x5a, 0x4b, 0x53, 0x32]) // "ZKS2"
export const ZKS2_VERSION = 2 // rev 2: GEN-007 algorithm id byte
/** GEN-007: 0x01 = the sha256d suite (defined once in codec.ts) */
export { ALG_SHA256D } from './codec.js'
import { ALG_SHA256D } from './codec.js'
const HEADER_V1 = 4 + 1 + 32 + 4 + 4
const HEADER = HEADER_V1 + 1 // + algorithm byte

export class ZksCodecError extends Error {}

export interface SubsetLeaf {
  index: number // leafIndex within the batch
  hash: Uint8Array // the tree leaf (computeLeaf output) — contents stay private
  subpath: Uint8Array[] // first min(SHARD_DEPTH, height) siblings of its standard path
}
export interface ShardProof {
  shardIndex: number // index >> 6, position in tree level 6
  leaves: SubsetLeaf[]
}
export interface ZkSubsetProof {
  /** GEN-007 algorithm suite id (sha256d); rev-1 wires imply it */
  algorithm?: number
  batchRoot: Uint8Array // R_n claimed for the anchored batch
  size: number // anchored batch leaf count
  shards: ShardProof[] // strictly ascending shardIndex
  topPath: Uint8Array[] // deduplicated multiproof for levels 6..height-1
}

const u32 = (v: number) => {
  const b = new Uint8Array(4)
  new DataView(b.buffer).setUint32(0, v, true)
  return b
}
const u16 = (v: number) => {
  const b = new Uint8Array(2)
  new DataView(b.buffer).setUint16(0, v, true)
  return b
}
const levelWidth = (size: number, level: number) => Math.ceil(size / 2 ** level)

/** siblings of `index` over the first `depth` levels of an already-built tree */
function subpathFromTree(tree: MerkleTree, index: number, depth: number): Uint8Array[] {
  const out: Uint8Array[] = []
  let pos = index
  for (let level = 0; level < depth; level++) {
    const nodes = tree.levels[level]
    out.push(pos % 2 === 0 ? (pos + 1 < nodes.length ? nodes[pos + 1] : nodes[pos]) : nodes[pos - 1])
    pos = Math.floor(pos / 2)
  }
  return out
}

/**
 * Top multiproof accounting, levels SHARD_DEPTH..height-1 over `size` leaves.
 * At each level the active nodes are the shard roots' ancestors; a node needs
 * a stored sibling hash iff its sibling is not itself active and it is not an
 * odd self-pair. `fetch` returns the sibling hash at a level+index (the
 * builder reads the tree; the canonical-size check passes null to count only).
 */
function topProofWalk(size: number, shardIndices: number[], fetch: ((level: number, index: number) => Uint8Array) | null): Uint8Array[] {
  const out: Uint8Array[] = []
  let active = [...shardIndices]
  for (let level = SHARD_DEPTH; level < heightForLeafCount(size); level++) {
    const width = levelWidth(size, level)
    const set = new Set(active)
    const next = new Set<number>()
    for (const a of active) {
      const sib = a ^ 1
      if (!set.has(sib) && !((a & 1) === 0 && a + 1 >= width)) out.push(fetch === null ? new Uint8Array() : fetch(level, sib))
      next.add(a >> 1)
    }
    active = [...next].sort((x, y) => x - y)
  }
  return out
}

/** the exact sibling count a canonical topPath carries for (size, shard set) */
function expectedTopSiblings(size: number, shardIndices: number[]): number {
  return topProofWalk(size, shardIndices, null).length
}

/**
 * Build a subset proof for `indices` of a batch whose leaves are `leaves`
 * (the same array the anchored root was folded from). Canonical: indices are
 * sorted, deduplicated and grouped by shard.
 */
export function buildSubsetProof(leaves: Uint8Array[], indices: number[]): ZkSubsetProof {
  const size = leaves.length
  if (size < 1) throw new ZksCodecError('empty tree')
  if (indices.length === 0) throw new ZksCodecError('subset must not be empty')
  if (new Set(indices).size !== indices.length) throw new ZksCodecError('duplicate leaf indices')
  const sorted = [...indices].sort((a, b) => a - b)
  for (const i of sorted) if (!Number.isInteger(i) || i < 0 || i >= size) throw new ZksCodecError(`leafIndex ${i} out of range`)
  const tree = buildTree(leaves)
  const subLen = Math.min(SHARD_DEPTH, tree.levels.length - 1)
  const groups = new Map<number, number[]>()
  for (const i of sorted) {
    const j = i >>> SHARD_DEPTH
    if (!groups.has(j)) groups.set(j, [])
    groups.get(j)!.push(i)
  }
  const shardIndices = [...groups.keys()].sort((a, b) => a - b)
  const shards: ShardProof[] = shardIndices.map((shardIndex) => ({
    shardIndex,
    leaves: groups.get(shardIndex)!.map((i) => ({ index: i, hash: leaves[i], subpath: subpathFromTree(tree, i, subLen) }))
  }))
  const topPath = topProofWalk(size, shardIndices, (level, sib) => tree.levels[level][sib])
  return { algorithm: ALG_SHA256D, batchRoot: tree.root, size, shards, topPath }
}

export function encodeZkdis(p: ZkSubsetProof): Uint8Array {
  const alg = p.algorithm ?? ALG_SHA256D
  if (alg !== ALG_SHA256D) throw new ZksCodecError(`unsupported algorithm ${alg}`)
  if (p.batchRoot.length !== 32) throw new ZksCodecError('batchRoot must be 32 bytes')
  if (!Number.isInteger(p.size) || p.size < 1 || p.size > 0xffffffff) throw new ZksCodecError('size out of range')
  const subLen = Math.min(SHARD_DEPTH, heightForLeafCount(p.size))
  const maxShard = (p.size - 1) >>> SHARD_DEPTH
  if (!Array.isArray(p.shards) || p.shards.length < 1) throw new ZksCodecError('numShards out of range')
  let prevShard = -1
  let total = HEADER + 4
  for (const sh of p.shards) {
    if (!Number.isInteger(sh.shardIndex) || sh.shardIndex < 0 || sh.shardIndex > maxShard) throw new ZksCodecError(`shardIndex ${sh.shardIndex} out of range`)
    if (sh.shardIndex <= prevShard) throw new ZksCodecError('shards not in canonical order')
    prevShard = sh.shardIndex
    const maxLeaves = Math.min(SHARD_LEAVES, p.size - sh.shardIndex * SHARD_LEAVES)
    if (!Array.isArray(sh.leaves) || sh.leaves.length < 1 || sh.leaves.length > maxLeaves) throw new ZksCodecError('numLeaves out of range')
    let prevLeaf = -1
    for (const l of sh.leaves) {
      if (!Number.isInteger(l.index) || l.index >>> SHARD_DEPTH !== sh.shardIndex || l.index >= p.size)
        throw new ZksCodecError(`leafIndex ${l.index} out of range`)
      if (l.index <= prevLeaf) throw new ZksCodecError('leaves not in canonical order')
      prevLeaf = l.index
      if (l.hash.length !== 32) throw new ZksCodecError('leafHash must be 32 bytes')
      if (l.subpath.length !== subLen) throw new ZksCodecError(`subpath must be ${subLen} siblings`)
      for (const s of l.subpath) if (s.length !== 32) throw new ZksCodecError('subpath sibling must be 32 bytes')
      total += 4 + 32 + 1 + subLen * 32
    }
    total += 6
  }
  const expectedTop = expectedTopSiblings(
    p.size,
    p.shards.map((s) => s.shardIndex)
  )
  if (p.topPath.length !== expectedTop) throw new ZksCodecError(`topPath must be ${expectedTop} siblings`)
  for (const s of p.topPath) if (s.length !== 32) throw new ZksCodecError('topPath sibling must be 32 bytes')
  total += p.topPath.length * 32
  const out = new Uint8Array(total)
  let o = 0
  const put = (b: Uint8Array) => {
    out.set(b, o)
    o += b.length
  }
  put(ZKS2_MAGIC)
  put(Uint8Array.of(ZKS2_VERSION))
  put(Uint8Array.of(alg))
  put(p.batchRoot)
  put(u32(p.size))
  put(u32(p.shards.length))
  for (const sh of p.shards) {
    put(u32(sh.shardIndex))
    put(u16(sh.leaves.length))
    for (const l of sh.leaves) {
      put(u32(l.index))
      put(l.hash)
      put(Uint8Array.of(l.subpath.length))
      for (const s of l.subpath) put(s)
    }
  }
  put(u32(p.topPath.length))
  for (const s of p.topPath) put(s)
  return out
}

export function decodeZkdis(b: Uint8Array): ZkSubsetProof {
  if (b.length < HEADER_V1 + 4) throw new ZksCodecError('truncated header')
  for (let i = 0; i < 4; i++) if (b[i] !== ZKS2_MAGIC[i]) throw new ZksCodecError('bad magic (not L_ZKDis v2)')
  const ver = b[4]
  if (ver !== 1 && ver !== ZKS2_VERSION) throw new ZksCodecError(`unsupported version ${ver}`)
  const off = ver === 1 ? 0 : 1 // rev 2 inserts the algorithm byte at index 5
  const alg = ver === 1 ? ALG_SHA256D : b[5]
  if (alg !== ALG_SHA256D) throw new ZksCodecError(`unsupported algorithm ${alg}`)
  const header = HEADER_V1 + off
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const size = dv.getUint32(37 + off, true)
  if (size < 1) throw new ZksCodecError('size must be >= 1')
  const numShards = dv.getUint32(41 + off, true)
  if (numShards < 1) throw new ZksCodecError('numShards must be >= 1')
  const subLen = Math.min(SHARD_DEPTH, heightForLeafCount(size))
  const maxShard = (size - 1) >>> SHARD_DEPTH
  let o = header
  const need = (n: number, what: string) => {
    if (o + n > b.length) throw new ZksCodecError(`truncated ${what}`)
  }
  const shards: ShardProof[] = []
  let prevShard = -1
  for (let s = 0; s < numShards; s++) {
    need(6, 'shard header')
    const shardIndex = dv.getUint32(o, true)
    const numLeaves = dv.getUint16(o + 4, true)
    o += 6
    if (shardIndex <= prevShard) throw new ZksCodecError('shards not in canonical order')
    prevShard = shardIndex
    if (shardIndex > maxShard) throw new ZksCodecError(`shardIndex ${shardIndex} out of range`)
    if (numLeaves < 1 || numLeaves > Math.min(SHARD_LEAVES, size - shardIndex * SHARD_LEAVES)) throw new ZksCodecError('numLeaves out of range')
    const leaves: SubsetLeaf[] = []
    let prevLeaf = -1
    for (let i = 0; i < numLeaves; i++) {
      need(4 + 32 + 1, 'leaf entry')
      const index = dv.getUint32(o, true)
      o += 4
      const hash = b.slice(o, o + 32)
      o += 32
      const sl = b[o]
      o += 1
      if (sl !== subLen) throw new ZksCodecError(`subpath must be ${subLen} siblings`)
      need(sl * 32, 'subpath')
      const subpath: Uint8Array[] = []
      for (let k = 0; k < sl; k++) {
        subpath.push(b.slice(o, o + 32))
        o += 32
      }
      if (index >>> SHARD_DEPTH !== shardIndex || index >= size) throw new ZksCodecError(`leafIndex ${index} out of range`)
      if (index <= prevLeaf) throw new ZksCodecError('leaves not in canonical order')
      prevLeaf = index
      leaves.push({ index, hash, subpath })
    }
    shards.push({ shardIndex, leaves })
  }
  need(4, 'topPath length')
  const topLen = dv.getUint32(o, true)
  o += 4
  const expectedTop = expectedTopSiblings(
    size,
    shards.map((s) => s.shardIndex)
  )
  if (topLen !== expectedTop) throw new ZksCodecError(`topPath must be ${expectedTop} siblings`)
  need(topLen * 32, 'topPath')
  const topPath: Uint8Array[] = []
  for (let k = 0; k < topLen; k++) {
    topPath.push(b.slice(o, o + 32))
    o += 32
  }
  if (o !== b.length) throw new ZksCodecError('trailing bytes')
  return { algorithm: alg, batchRoot: b.slice(5 + off, 37 + off), size, shards, topPath }
}

export type SubsetVerdict = { ok: true; root: Uint8Array; indices: number[] } | { ok: false; reason: string }

/**
 * Structural + cryptographic verification of a decoded subset proof: every
 * leaf's subpath must fold to the SAME shard root, and the shared top
 * multiproof must fold every shard root to `batchRoot`. The anchor binding
 * (batchRoot == anchored R, size == anchored size, SPV) is the caller's.
 */
export function verifySubsetProof(p: ZkSubsetProof): SubsetVerdict {
  const height = heightForLeafCount(p.size)
  const roots = new Map<number, Uint8Array>()
  const indices: number[] = []
  for (const sh of p.shards) {
    let shardRoot: Uint8Array | undefined
    for (const leaf of sh.leaves) {
      let cur: Uint8Array = leaf.hash
      let idx = leaf.index
      for (const sib of leaf.subpath) {
        cur = (idx & 1) === 0 ? hashNode(cur, sib) : hashNode(sib, cur)
        idx = idx >> 1
      }
      if (idx !== sh.shardIndex) return { ok: false, reason: `leaf ${leaf.index} does not fold to shard ${sh.shardIndex}` }
      if (shardRoot === undefined) shardRoot = cur
      else if (!eq(shardRoot, cur)) return { ok: false, reason: `leaves disagree on shard ${sh.shardIndex} root` }
      indices.push(leaf.index)
    }
    roots.set(sh.shardIndex, shardRoot!)
  }
  let active = new Map(roots)
  let cursor = 0
  for (let level = SHARD_DEPTH; level < height; level++) {
    const width = levelWidth(p.size, level)
    const next = new Map<number, Uint8Array>()
    for (const a of [...active.keys()].sort((x, y) => x - y)) {
      const h = active.get(a)!
      const sib = a ^ 1
      let node: Uint8Array
      if (active.has(sib)) {
        const other = active.get(sib)!
        node = (a & 1) === 0 ? hashNode(h, other) : hashNode(other, h)
      } else if ((a & 1) === 0 && a + 1 >= width) {
        node = hashNode(h, h) // odd-position self-pair, same fold as the batch tree
      } else {
        const s = p.topPath[cursor++]
        if (s === undefined) return { ok: false, reason: 'missing top-path sibling' }
        node = (a & 1) === 0 ? hashNode(h, s) : hashNode(s, h)
      }
      next.set(a >> 1, node)
    }
    active = next
  }
  if (cursor !== p.topPath.length) return { ok: false, reason: 'unused top-path siblings' }
  if (active.size !== 1 || !active.has(0)) return { ok: false, reason: 'shard roots do not converge to one batch root' }
  if (!eq(active.get(0)!, p.batchRoot)) return { ok: false, reason: 'reconstructed root is not the claimed batch root' }
  return { ok: true, root: active.get(0)!, indices }
}

/** decode L_ZKDis v2 and verify the multiproof; never throws */
export function verifyZkdis(bytes: Uint8Array): SubsetVerdict {
  let p: ZkSubsetProof
  try {
    p = decodeZkdis(bytes)
  } catch (e) {
    return { ok: false, reason: e instanceof ZksCodecError ? `codec: ${e.message}` : 'codec error' }
  }
  return verifySubsetProof(p)
}
