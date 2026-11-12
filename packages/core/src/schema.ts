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
  /** §15 modality: an explicit deontic/epistemic/temporal qualifier
   *  ("as of Block 105432, Reg X includes Clause Y"), committed into H(d‖p)
   *  and into the originator signature digest. */
  modality?: { kind: 'deontic' | 'epistemic' | 'temporal'; qualifier: string }
}

export interface Fragment {
  d: Bytes // plaintext data (unencrypted form; never leaves the writer)
  p: Provenance
  hdp?: Bytes // H(d|p), computed if absent
  E?: Bytes // ciphertext (AES-256-GCM output from @nektar/crypto); empty if plaintext mode
}

function u32le(n: number): Bytes {
  const b = Buffer.alloc(4)
  b.writeUInt32LE(n)
  return new Uint8Array(b)
}

export function field(b: Bytes): Bytes {
  return concat(u32le(b.length), b)
}

export function serializeProvenance(p: Provenance): Bytes {
  // modality is a TRAILING optional field: fragments without it serialize
  // byte-identically to the pre-modality layout, so existing hashes stand.
  return concat(
    field(utf8(p.source)),
    u64le(p.time),
    field(p.signature),
    field(utf8(p.modelId)),
    ...(p.modality ? [field(utf8(JSON.stringify(p.modality)))] : [])
  )
}

export function serializeFragment(f: Fragment): Bytes {
  return concat(field(f.d), field(serializeProvenance(f.p)), field(f.hdp ?? hdp(f)), field(f.E ?? new Uint8Array(0)))
}

/** COM-003: what the originator signs — the data and every provenance field
 *  except the signature itself */
export function provenanceDigest(d: Bytes, p: Provenance): Bytes {
  return dsha256(concat(TAG_PROV_SIG, field(d), serializeProvenance({ ...p, signature: new Uint8Array(0) })))
}

/** inverse of field(): read a u32le-prefixed blob. */
function takeField(b: Bytes, off: number): { v: Bytes; next: number } {
  if (off + 4 > b.length) throw new Error('parse: truncated field header')
  const len = (b[off]! | (b[off + 1]! << 8) | (b[off + 2]! << 16) | (b[off + 3]! << 24)) >>> 0
  if (off + 4 + len > b.length) throw new Error('parse: truncated field body')
  return { v: b.slice(off + 4, off + 4 + len), next: off + 4 + len }
}

/** inverse of serializeProvenance — source|time|sig|modelId[|modality]. */
export function parseProvenance(b: Bytes): Provenance {
  const src = takeField(b, 0)
  if (src.next + 8 > b.length) throw new Error('parse: truncated time')
  const tb = b.slice(src.next, src.next + 8)
  let time = 0n
  for (let i = 7; i >= 0; i--) time = (time << 8n) | BigInt(tb[i]!)
  const sig = takeField(b, src.next + 8)
  const mid = takeField(b, sig.next)
  const p: Provenance = {
    source: new TextDecoder().decode(src.v),
    time: Number(time),
    signature: sig.v,
    modelId: new TextDecoder().decode(mid.v)
  }
  if (mid.next < b.length) {
    const mod = takeField(b, mid.next)
    if (mod.next !== b.length) throw new Error('parse: trailing bytes')
    p.modality = JSON.parse(new TextDecoder().decode(mod.v))
  }
  return p
}

/** inverse of serializeFragment — d|prov|hdp|E. */
export function parseFragment(b: Bytes): Fragment {
  const d = takeField(b, 0)
  const pv = takeField(b, d.next)
  const hd = takeField(b, pv.next)
  const ev = takeField(b, hd.next)
  if (ev.next !== b.length) throw new Error('parse: trailing bytes')
  return {
    d: d.v,
    p: parseProvenance(pv.v),
    ...(hd.v.length ? { hdp: hd.v } : {}),
    ...(ev.v.length ? { E: ev.v } : {})
  }
}

/** H(d|p) — provenance binding hash */
export function hdp(f: Pick<Fragment, 'd' | 'p'>): Bytes {
  return sha256(concat(TAG_PROV, f.d, serializeProvenance(f.p)))
}

/** H(E) — ciphertext hash (empty ciphertext hashes as sha256 of empty) */
export function eh(f: Fragment): Bytes {
  return sha256(f.E ?? new Uint8Array(0))
}

/**
 * Batch leaf for a fragment:
 *   leaf = H(TAG_LEAF | H(d|p) | H(E(d)))
 * The merkle tree then hashes the leaf with dsha256 (anchorchain convention),
 * so the tree leaf input is this 32-byte value.
 */
export function computeLeaf(f: Fragment): Bytes {
  return sha256(concat(TAG_LEAF, f.hdp ?? hdp(f), eh(f)))
}

/** Identity hash for dedup: H(d|p). */
export function identityHash(f: Fragment): Bytes {
  return f.hdp ?? hdp(f)
}

export { eq }
