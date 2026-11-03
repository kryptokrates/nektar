// Sparse Merkle tree — faithful TS port of idattr-smt (identity-attribution
// crates/idattr-smt/src/lib.rs), 2^256 keyspace, depth 256, domain-tagged
// single-SHA-256 hashing, lazy empty subtrees, compressed proofs.
import { sha256, type Bytes } from '@nektar/core'

export type SmtHash = Bytes // 32 B
export type SmtKey = Bytes  // 32 B — path from root to leaf, MSB-first
export const SMT_DEPTH = 256
export const EMPTY_LEAF: SmtHash = new Uint8Array(32)

const LEAF_DOMAIN = Uint8Array.of(0x00)
const NODE_DOMAIN = Uint8Array.of(0x01)
const eqH = (a: Bytes, b: Bytes) => a.length === b.length && a.every((x, i) => x === b[i])

export function hashLeaf (key: SmtKey, value: SmtHash): SmtHash {
  return sha256(new Uint8Array([0x00, ...key, ...value]))
}
export function hashNode (left: SmtHash, right: SmtHash): SmtHash {
  return sha256(new Uint8Array([0x01, ...left, ...right]))
}

const emptyTable: SmtHash[] = (() => {
  const t: SmtHash[] = new Array(SMT_DEPTH + 1).fill(EMPTY_LEAF)
  for (let d = SMT_DEPTH - 1; d >= 0; d--) t[d] = hashNode(t[d + 1]!, t[d + 1]!)
  return t
})()
export function emptySubtree (depth: number): SmtHash { return emptyTable[depth]! }
/** registry key from identity bytes: SHA-256(0x02 ‖ id) — idattr_smt::derive_key */
export function deriveKey (identity: Bytes): SmtKey { return sha256(new Uint8Array([0x02, ...identity])) }
export function emptyRoot (): SmtHash { return emptySubtree(0) }

const bit = (key: SmtKey, i: number) => (key[i >> 3]! >> (7 - (i & 7))) & 1
const truncate = (key: SmtKey, depth: number): SmtKey => {
  const out = new Uint8Array(32)
  const full = depth >> 3
  out.set(key.subarray(0, full))
  const rem = depth & 7
  if (rem !== 0) out[full] = key[full]! & (0xff << (8 - rem))
  return out
}

export interface SmtProof {
  key: SmtKey
  /** non-empty siblings as [depth_of_child, sibling_hash]; root = depth 0 */
  siblings: Array<[number, SmtHash]>
}
export type Leaf = { present: true, value: SmtHash } | { present: false }

export function reconstructRoot (proof: SmtProof, leaf: Leaf): SmtHash {
  const map = new Map<number, SmtHash>(proof.siblings.map(([d, h]) => [d, h]))
  let cur = leaf.present ? hashLeaf(proof.key, leaf.value) : EMPTY_LEAF
  for (let d = SMT_DEPTH - 1; d >= 0; d--) {
    const sib = map.get(d + 1) ?? emptySubtree(d + 1)
    cur = bit(proof.key, d) === 0 ? hashNode(cur, sib) : hashNode(sib, cur)
  }
  return cur
}
export function verifyProof (proof: SmtProof, leaf: Leaf, root: SmtHash): boolean {
  return eqH(reconstructRoot(proof, leaf), root)
}

export class Smt {
  private nodes = new Map<string, SmtHash>()
  len = 0
  private key (depth: number, path: SmtKey) { return `${depth}:${Buffer.from(path).toString('hex')}` }
  private get (depth: number, path: SmtKey): SmtHash {
    return this.nodes.get(this.key(depth, path)) ?? emptySubtree(depth)
  }
  private set (depth: number, path: SmtKey, value: SmtHash): void {
    const k = this.key(depth, path)
    if (eqH(value, emptySubtree(depth))) this.nodes.delete(k)
    else this.nodes.set(k, value)
  }
  root (): SmtHash { return this.get(0, new Uint8Array(32)) }
  insert (key: SmtKey, value: SmtHash): SmtHash {
    const leaf = hashLeaf(key, value)
    const present = this.nodes.has(this.key(SMT_DEPTH, key))
    this.set(SMT_DEPTH, key, leaf)
    if (!present) this.len++
    this.recomputePath(key)
    return this.root()
  }
  remove (key: SmtKey): SmtHash {
    if (this.nodes.has(this.key(SMT_DEPTH, key))) this.len--
    this.nodes.delete(this.key(SMT_DEPTH, key))
    this.recomputePath(key)
    return this.root()
  }
  private recomputePath (key: SmtKey): void {
    let cur = this.get(SMT_DEPTH, key)
    for (let d = SMT_DEPTH - 1; d >= 0; d--) {
      const sib = this.get(d + 1, truncateKeySibling(key, d + 1))
      cur = bit(key, d) === 0 ? hashNode(cur, sib) : hashNode(sib, cur)
      this.set(d, truncate(key, d), cur)
    }
  }
  prove (key: SmtKey): SmtProof {
    // ascending depth — wire-identical to idattr_smt::Smt::prove
    const siblings: Array<[number, SmtHash]> = []
    for (let d = 0; d < SMT_DEPTH; d++) {
      const sibPath = truncateKeySibling(key, d + 1)
      const sib = this.get(d + 1, sibPath)
      if (!eqH(sib, emptySubtree(d + 1))) siblings.push([d + 1, sib])
    }
    return { key: Uint8Array.from(key), siblings }
  }
  /** key is revoked iff it is enrolled (proof of inclusion at the value). */
  has (key: SmtKey): boolean { return this.nodes.has(this.key(SMT_DEPTH, key)) }
}
// sibling path at depth d = key with bit d-1 flipped, truncated to d bits
function truncateKeySibling (key: SmtKey, depth: number): SmtKey {
  const t = truncate(key, depth)
  const b = depth - 1
  t[b >> 3] = t[b >> 3]! ^ (1 << (7 - (b & 7)))
  return t
}
export { truncateKeySibling as siblingPath }
