// chirp.ts — CHIRP/BRC-167 content addressing for the object plane.
//
// A stored artifact (shard pack, proof bundle, evidence) is split into the
// canonical CHIRP tree: every node and blob is PUT under the content-
// addressed key `chirp/<hex(sha256(object))>`, and the advertised handle is
// the stock `chirp://<identifier>` URL of the root hash. Any holder of the
// URL can reassemble the bytes from ANY store that carries the objects —
// verifyClosure refuses tampered or missing objects by construction, and the
// root's contentHash pins the reassembled bytes end to end. No operator
// origin or bucket layout is part of the address.
import { buildChirp, chirpURL, parseChirpURL, verifyClosure } from '@lightwebinc/bcommon'
import { sha256, toHex, concat } from '@nektar/core'
import type { S3ObjectStore } from './objectstore.js'
import { getObjectUnsigned } from './objectstore.js'

/** the content-addressed store key for one CHIRP object (node or blob) */
const chirpKey = (hashHex: string): string => `chirp/${hashHex}`

/** every object the tree for `bytes` produces, keyed for the store:
 *  `chirp/<hash>` → node/blob bytes (root first), plus the chirp:// URL. */
function chirpObjects(bytes: Uint8Array): { url: string; rootHash: string; objects: Map<string, Uint8Array> } {
  const built = buildChirp(bytes)
  const objects = new Map<string, Uint8Array>()
  objects.set(chirpKey(toHex(Uint8Array.from(built.rootHash))), built.root)
  for (const b of built.branches) objects.set(chirpKey(toHex(Uint8Array.from(sha256(b)))), b)
  for (const b of built.blobs ?? []) objects.set(chirpKey(toHex(Uint8Array.from(sha256(b)))), b)
  return { url: chirpURL(built.rootHash), rootHash: toHex(Uint8Array.from(built.rootHash)), objects }
}

/** The chirp:// URL `bytes` would resolve to, and the store key its root node
 *  lives under — no puts, so a retry can check the root exists before
 *  republishing (the object set is deterministic content-addressed data). */
export function chirpRefFor(bytes: Uint8Array): { url: string; rootKey: string } {
  const { url, rootHash } = chirpObjects(bytes)
  return { url, rootKey: chirpKey(rootHash) }
}

/** PUT every CHIRP object of `bytes` to the store; returns the chirp:// URL.
 *  Idempotent — the same bytes always produce the same keys. */
export async function storeChirp(store: S3ObjectStore, bytes: Uint8Array, bucket?: string): Promise<string> {
  const { url, objects } = chirpObjects(bytes)
  for (const [key, obj] of objects) await store.put(key, obj, 'application/octet-stream', bucket)
  return url
}

/** fetch + verify one object by hash — through a signed store client. */
const signedFetch =
  (store: S3ObjectStore, bucket?: string) =>
  async (hash: Uint8Array): Promise<Uint8Array> =>
    await store.get(chirpKey(toHex(Uint8Array.from(hash))), bucket)

/** fetch + verify one object by hash — unsigned GET against a gateway that
 *  serves public reads; throws on 403/404 so the caller can fall back. */
const unsignedFetch =
  (endpoint: string, bucket: string) =>
  async (hash: Uint8Array): Promise<Uint8Array> =>
    await getObjectUnsigned(endpoint, bucket, chirpKey(toHex(Uint8Array.from(hash))))

/** Reassemble the content a chirp:// URL names — every object's hash checked
 *  by the closure verification, and the blob order rebuilt end to end.
 *  `fetch` resolves a 32-byte hash to the object bytes it must equal. */
async function resolveChirp(url: string, fetch: (hash: Uint8Array) => Promise<Uint8Array> | Uint8Array): Promise<Uint8Array> {
  const rootHash = parseChirpURL(url)
  const got = new Map<string, Uint8Array>()
  const closure = await verifyClosure(rootHash, async (h) => {
    const b = await fetch(h)
    got.set(toHex(Uint8Array.from(h)), b)
    return b
  })
  return concat(...closure.blobs.map((c) => got.get(toHex(Uint8Array.from(c.hash)))!))
}

/** chirp:// resolution through a signed store client. */
async function fetchChirp(store: S3ObjectStore, url: string, bucket?: string): Promise<Uint8Array> {
  return await resolveChirp(url, signedFetch(store, bucket))
}

/** chirp:// resolution through unsigned GETs — the importer needs no
 *  credentials when the gateway serves the bucket publicly. */
async function fetchChirpUnsigned(endpoint: string, bucket: string, url: string): Promise<Uint8Array> {
  return await resolveChirp(url, unsignedFetch(endpoint, bucket))
}

/** The one object-ref resolution order every fetcher shares — packs,
 *  evidence bundles, receipts: a chirp:// ref resolves through the signed
 *  store first, then unsigned against the advertised endpoint; the keyed
 *  object is the compatibility fallback, signed then unsigned. Digest and
 *  size are checked wherever the ref pins them, so a wrong byte stream
 *  fails the read rather than the caller's parse. `st` may be undefined —
 *  unsigned paths alone are enough when the gateway serves public reads. */
export async function fetchObjectByRef(
  st: S3ObjectStore | undefined,
  ref: { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string },
  onFallback?: (path: string) => void
): Promise<Uint8Array> {
  const bucket = ref.bucket ?? st?.bucket
  if (ref.chirp !== undefined && bucket !== undefined) {
    if (st !== undefined) {
      try {
        return await fetchChirp(st, ref.chirp, bucket)
      } catch {
        onFallback?.('chirp-signed')
      }
    }
    if (ref.endpoint !== undefined) {
      try {
        return await fetchChirpUnsigned(ref.endpoint, bucket, ref.chirp)
      } catch {
        onFallback?.('chirp-unsigned')
      }
    }
  }
  if (ref.key !== undefined && ref.sha256 !== undefined) {
    if (st !== undefined) {
      try {
        return await st.getVerified({ bucket: ref.bucket, key: ref.key, sha256: ref.sha256, size: ref.size })
      } catch (e) {
        if (ref.endpoint === undefined || bucket === undefined) throw e
        onFallback?.('keyed-signed')
      }
    }
    if (ref.endpoint !== undefined && bucket !== undefined) {
      const bytes = await getObjectUnsigned(ref.endpoint, bucket, ref.key)
      if (toHex(sha256(bytes)) !== ref.sha256) throw new Error(`objectstore: sha256 mismatch for ${ref.key} (unsigned read)`)
      if (ref.size !== undefined && bytes.length !== ref.size) throw new Error(`objectstore: size mismatch for ${ref.key} (unsigned read)`)
      return bytes
    }
  }
  throw new Error('object store is not configured (NEKTAR_BSTORE_*) and the ref has no unsigned path')
}
