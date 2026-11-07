/**
 * RFC 6962 Merkle roots and audit paths over leaves of any length: the twin
 * of the Go package commit's byte-leaf functions (HashLeaf, RootOfBytes,
 * RootOfLeafHashes, RootOfSubtrees, ProveBytes, ProveLeafHashes,
 * ProveSubtrees, VerifyBytes, PathLen, RootAt, VerifyAt, Builder, SegmentRoot
 * and SegmentWriter). A leaf hashes as SHA-256(0x00 || leaf), an interior node
 * as SHA-256(0x01 || left || right), and the empty tree's root is SHA-256 of
 * no bytes.
 *
 * Indices and sizes are numbers, held to safe integers: a tree of up to
 * 2^53 leaves is described exactly, with arithmetic rather than 32-bit
 * bitwise operators.
 */
import { Hash } from '@bsv/sdk'

/** CommitError's codes: `index` is an index outside the tree, `size` a size or segment size out of range, `path` a compact path whose length does not match its index and size. */
export type CommitErrorCode = 'index' | 'size' | 'path'

export class CommitError extends Error {
  constructor(readonly code: CommitErrorCode, detail: string) {
    super(`commit: ${code}: ${detail}`)
    this.name = 'CommitError'
  }
}

function digest(...parts: Uint8Array[]): Uint8Array {
  const h = new Hash.SHA256()
  for (const p of parts) h.update(p)
  return Uint8Array.from(h.digest())
}

const leafPrefix = Uint8Array.of(0x00)
const nodePrefix = Uint8Array.of(0x01)

/** SHA-256(0x00 || leaf). */
export function hashLeaf(leaf: Uint8Array): Uint8Array {
  return digest(leafPrefix, leaf)
}

/** SHA-256(0x01 || left || right). */
export function hashNode(left: Uint8Array, right: Uint8Array): Uint8Array {
  return digest(nodePrefix, left, right)
}

/** SHA-256 of no bytes: the empty tree's root. */
export function emptyRoot(): Uint8Array {
  return digest()
}

function split(n: number): number {
  let k = 1
  while (k * 2 < n) k *= 2
  return k
}

function nodes(h: readonly Uint8Array[]): Uint8Array {
  if (h.length === 1) return h[0]!
  const k = split(h.length)
  return hashNode(nodes(h.slice(0, k)), nodes(h.slice(k)))
}

/** One sibling of an audit path; `left` when it is the left input of the node hash. */
export interface Step {
  hash: Uint8Array
  left: boolean
}

function proveNodes(h: readonly Uint8Array[], index: number): Step[] {
  if (h.length === 1) return []
  const k = split(h.length)
  if (index < k) return [...proveNodes(h.slice(0, k), index), { hash: nodes(h.slice(k)), left: false }]
  return [...proveNodes(h.slice(k), index - k), { hash: nodes(h.slice(0, k)), left: true }]
}

function checkIndex(index: number, size: number): void {
  if (!Number.isSafeInteger(index) || !Number.isSafeInteger(size) || index < 0 || index >= size) {
    throw new CommitError('index', `${index} in a tree of ${size}`)
  }
}

/** The root over leaves of any length, in order. */
export function rootOfBytes(leaves: readonly Uint8Array[]): Uint8Array {
  return rootOfLeafHashes(leaves.map(hashLeaf))
}

/** The root over leaves already hashed with hashLeaf, in order. */
export function rootOfLeafHashes(hashes: readonly Uint8Array[]): Uint8Array {
  return hashes.length === 0 ? emptyRoot() : nodes(hashes)
}

/**
 * The RFC 6962 split over subtree roots taken as nodes. It is the root over
 * the subtrees' leaves only when every subtree but the last covers the same
 * power-of-two number of leaves and the last at most that many. Throws
 * CommitError `size` for no subtrees.
 */
export function rootOfSubtrees(roots: readonly Uint8Array[]): Uint8Array {
  if (roots.length === 0) throw new CommitError('size', 'no subtree roots')
  return nodes(roots)
}

/** The audit path of leaves[index], leaf to root. */
export function proveBytes(leaves: readonly Uint8Array[], index: number): Step[] {
  return proveLeafHashes(leaves.map(hashLeaf), index)
}

/** The audit path of the leaf at index, from the leaf hashes alone. */
export function proveLeafHashes(hashes: readonly Uint8Array[], index: number): Step[] {
  checkIndex(index, hashes.length)
  return proveNodes(hashes, index)
}

/** The path of roots[index] up to rootOfSubtrees(roots). */
export function proveSubtrees(roots: readonly Uint8Array[], index: number): Step[] {
  checkIndex(index, roots.length)
  return proveNodes(roots, index)
}

function equal(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

/** Recomputes the root from a leaf and its sided path. */
export function verifyBytes(leaf: Uint8Array, path: readonly Step[], root: Uint8Array): boolean {
  let cur = hashLeaf(leaf)
  for (const s of path) cur = s.left ? hashNode(s.hash, cur) : hashNode(cur, s.hash)
  return equal(cur, root)
}

const odd = (x: number): boolean => x % 2 === 1
const half = (x: number): number => Math.floor(x / 2)

/** The audit path's length for leaf index in a tree of size leaves. Throws CommitError `index`. */
export function pathLength(index: number, size: number): number {
  checkIndex(index, size)
  let n = 0
  let fn = index
  let sn = size - 1
  while (sn !== 0) {
    if (odd(fn) || fn === sn) {
      while (!odd(fn) && fn !== 0) {
        fn = half(fn)
        sn = half(sn)
      }
    }
    fn = half(fn)
    sn = half(sn)
    n++
  }
  return n
}

/**
 * The root a compact audit path reproduces (Go's RootAt): leafHash at index
 * in a tree of size leaves, its siblings leaf to root without sides, each
 * side following from index and size by RFC 9162 section 2.1.3.2. Throws
 * CommitError `index` for an index outside the tree and `path` for a path
 * longer or shorter than pathLength.
 */
export function rootAt(leafHash: Uint8Array, index: number, size: number, siblings: readonly Uint8Array[]): Uint8Array {
  checkIndex(index, size)
  let fn = index
  let sn = size - 1
  let r = leafHash
  for (const p of siblings) {
    if (sn === 0) throw new CommitError('path', `${siblings.length} siblings for index ${index} of ${size}`)
    if (odd(fn) || fn === sn) {
      r = hashNode(p, r)
      while (!odd(fn) && fn !== 0) {
        fn = half(fn)
        sn = half(sn)
      }
    } else {
      r = hashNode(r, p)
    }
    fn = half(fn)
    sn = half(sn)
  }
  if (sn !== 0) throw new CommitError('path', `${siblings.length} siblings for index ${index} of ${size}`)
  return r
}

/**
 * The compact check of RFC 9162 section 2.1.3.2: rootAt reproduces root. A
 * path of the wrong length and an index outside the tree fail.
 */
export function verifyAt(leafHash: Uint8Array, index: number, size: number, siblings: readonly Uint8Array[], root: Uint8Array): boolean {
  try {
    return equal(rootAt(leafHash, index, size, siblings), root)
  } catch (e) {
    if (e instanceof CommitError) return false
    throw e
  }
}

/** A root over leaves added one at a time, holding one hash per set bit of the count. */
export class RootBuilder {
  private stack: Uint8Array[] = []
  private sizes: number[] = []
  private n = 0

  /** Appends a leaf already hashed with hashLeaf. */
  add(leafHash: Uint8Array): void {
    this.stack.push(leafHash)
    this.sizes.push(1)
    this.n++
    while (this.stack.length >= 2 && this.sizes[this.sizes.length - 1] === this.sizes[this.sizes.length - 2]) {
      const right = this.stack.pop()!
      const left = this.stack.pop()!
      const s = this.sizes.pop()!
      this.sizes.pop()
      this.stack.push(hashNode(left, right))
      this.sizes.push(s * 2)
    }
  }

  /** Appends a leaf of any length. */
  addBytes(leaf: Uint8Array): void {
    this.add(hashLeaf(leaf))
  }

  /** The number of leaves added. */
  size(): number {
    return this.n
  }

  /** The root over the leaves so far; more may be added afterwards. */
  root(): Uint8Array {
    const stack = this.stack
    if (stack.length === 0) return emptyRoot()
    let r = stack[stack.length - 1]!
    for (let i = stack.length - 2; i >= 0; i--) r = hashNode(stack[i]!, r)
    return r
  }

  /** An independent copy, to add to without changing this one. */
  clone(): RootBuilder {
    const c = new RootBuilder()
    c.stack = [...this.stack]
    c.sizes = [...this.sizes]
    c.n = this.n
    return c
  }
}

/** segmentRoot over content written in pieces, holding at most one segment. */
export class SegmentWriter {
  private buf: Uint8Array
  private fill = 0
  private readonly b = new RootBuilder()

  constructor(private readonly segment: number) {
    if (!Number.isSafeInteger(segment) || segment <= 0) throw new CommitError('size', `segment size ${segment}`)
    this.buf = new Uint8Array(segment)
  }

  write(p: Uint8Array): void {
    let off = 0
    while (off < p.length) {
      const k = Math.min(this.segment - this.fill, p.length - off)
      this.buf.set(p.subarray(off, off + k), this.fill)
      this.fill += k
      off += k
      if (this.fill === this.segment) {
        this.b.addBytes(this.buf)
        this.fill = 0
      }
    }
  }

  /** The segments so far, a partial last one counted. */
  segments(): number {
    return this.b.size() + (this.fill > 0 ? 1 : 0)
  }

  /** The root over the content so far, a partial last segment included as it is. */
  root(): Uint8Array {
    if (this.fill === 0) return this.b.root()
    const c = this.b.clone()
    c.addBytes(this.buf.subarray(0, this.fill))
    return c.root()
  }
}

/** The root over content cut into segments of `segment` bytes, the last unpadded. */
export function segmentRoot(content: Uint8Array, segment: number): Uint8Array {
  const w = new SegmentWriter(segment)
  w.write(content)
  return w.root()
}
