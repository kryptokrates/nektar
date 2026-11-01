// K_i fragment schema and canonical serialization.
// Fragment = (d, p, H(d|p), E(d)) — data, provenance, binding hash, ciphertext.
// Canonical encoding: length-prefixed fields (u32le length | bytes), field
// order fixed. Deterministic for vectors and dedup.
import { sha256, dsha256, concat, utf8, u64le, eq, type Bytes } from './hash.js'
import { TAG_LEAF, TAG_PROV, TAG_PROV_SIG } from './domain.js'

export interface Provenance {
  source: string
  time: number // unix seconds
  signature: Bytes // may be empty
  modelId: string
}

export interface Fragment {
  d: Bytes // plaintext data (unencrypted form; never leaves the writer)
  p: Provenance
  hdp?: Bytes // H(d|p), computed if absent
  E?: Bytes // ciphertext (AES-256-GCM output from @mamem/crypto); empty if plaintext mode
}

function u32le (n: number): Bytes {
  const b = Buffer.alloc(4)
  b.writeUInt32LE(n)
  return new Uint8Array(b)
}

export function field (b: Bytes): Bytes {
  return concat(u32le(b.length), b)
}

export function serializeProvenance (p: Provenance): Bytes {
  return concat(
    field(utf8(p.source)),
    u64le(p.time),
    field(p.signature),
    field(utf8(p.modelId))
  )
}

export function serializeFragment (f: Fragment): Bytes {
  return concat(field(f.d), field(serializeProvenance(f.p)), field(f.hdp ?? hdp(f)), field(f.E ?? new Uint8Array(0)))
}

/** COM-003: what the originator signs — the data and every provenance field
 *  except the signature itself */
export function provenanceDigest (d: Bytes, p: Provenance): Bytes {
  return dsha256(concat(TAG_PROV_SIG, field(d), serializeProvenance({ ...p, signature: new Uint8Array(0) })))
}

/** H(d|p) — provenance binding hash */
export function hdp (f: Pick<Fragment, 'd' | 'p'>): Bytes {
  return sha256(concat(TAG_PROV, f.d, serializeProvenance(f.p)))
}

/** H(E) — ciphertext hash (empty ciphertext hashes as sha256 of empty) */
export function eh (f: Fragment): Bytes {
  return sha256(f.E ?? new Uint8Array(0))
}

/**
 * Batch leaf for a fragment:
 *   leaf = H(TAG_LEAF | H(d|p) | H(E(d)))
 * The merkle tree then hashes the leaf with dsha256 (anchorchain convention),
 * so the tree leaf input is this 32-byte value.
 */
export function computeLeaf (f: Fragment): Bytes {
  return sha256(concat(TAG_LEAF, f.hdp ?? hdp(f), eh(f)))
}

/** Identity hash for dedup: H(d|p). */
export function identityHash (f: Fragment): Bytes {
  return f.hdp ?? hdp(f)
}

export { eq }
