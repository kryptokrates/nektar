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
export const chirpKey = (hashHex: string): string => `chirp/${hashHex}`

/** every object the tree for `bytes` produces, keyed for the store:
 *  `chirp/<hash>` → node/blob bytes (root first), plus the chirp:// URL. */
export function chirpObjects(bytes: Uint8Array): { url: string; rootHash: string; objects: Map<string, Uint8Array> } {
  const built = buildChirp(bytes)
  const objects = new Map<string, Uint8Array>()
  objects.set(chirpKey(toHex(Uint8Array.from(built.rootHash))), built.root)
  for (const b of built.branches) objects.set(chirpKey(toHex(Uint8Array.from(sha256(b)))), b)
  for (const b of built.blobs ?? []) objects.set(chirpKey(toHex(Uint8Array.from(sha256(b)))), b)
  return { url: chirpURL(built.rootHash), rootHash: toHex(Uint8Array.from(built.rootHash)), objects }
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
export async function resolveChirp(url: string, fetch: (hash: Uint8Array) => Promise<Uint8Array> | Uint8Array): Promise<Uint8Array> {
  const rootHash = parseChirpURL(url)
  const got = new Map<string, Uint8Array>()
  const closure = await verifyClosure(rootHash, async (h) => {
    const b = await fetch(h)
    got.set(toHex(Uint8Array.from(h)), b)
    return b
  })
  return concat(closure.blobs.map((c) => got.get(toHex(Uint8Array.from(c.hash)))!))
}

/** chirp:// resolution through a signed store client. */
export async function fetchChirp(store: S3ObjectStore, url: string, bucket?: string): Promise<Uint8Array> {
  return await resolveChirp(url, signedFetch(store, bucket))
}

/** chirp:// resolution through unsigned GETs — the importer needs no
 *  credentials when the gateway serves the bucket publicly. */
export async function fetchChirpUnsigned(endpoint: string, bucket: string, url: string): Promise<Uint8Array> {
  return await resolveChirp(url, unsignedFetch(endpoint, bucket))
}
