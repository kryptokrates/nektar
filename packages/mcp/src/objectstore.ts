// objectstore.ts — dependency-free S3 (SigV4) client for bstore's s3d gateway.
//
// Boundary: bstore is the storage market — objects carry bounties, providers
// prove possession, the operator's gateway settles. Nektar does NONE of that:
// it is an S3 client that writes evidence bundles / public shard packs to a
// gateway the deployment operator runs, and verifies every byte it reads
// against a digest it already holds (getVerified). Provider possession proofs,
// bounty races and gateway availability are the operator's business; a wrong
// or tampered object is simply refused on the digest.
//
// Protocol notes (bstore docs/s3.md): path-style URLs, AWS SigV4, region
// us-east-1; signed headers must include `host` and every `x-amz-*` header the
// request carries; ETag is NOT an MD5 — integrity is our own SHA-256. The
// gateway's bounty is configured server-side (the bounty object in the
// gateway config); there is no per-request bounty header to set.

import { createHash, createHmac } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'

export interface ObjectRef {
  bucket: string
  key: string
  sha256: string
  size: number
  /** CHIRP/BRC-167 content address (`chirp://<id>`) of the object — the same
   *  bytes are also PUT as `chirp/<hash>` objects so any holder of the URL
   *  can resolve them from any store that carries them (see chirp.ts). */
  chirp?: string
  /** the gateway base URL this ref was written through — lets an importer
   *  attempt an unsigned read before it needs credentials of its own. */
  endpoint?: string
}
export interface StoredHead {
  size: number
  sha256?: string
}

/** hard read bound — evidence packs are small; anything bigger is refused
 *  before it can exhaust memory (bstore's own ceiling is 5 GiB) */
export const OBJECT_MAX_BYTES = 8 * 1024 * 1024

const EMPTY_SHA256 = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'

const sha256Hex = (b: Uint8Array): string => createHash('sha256').update(b).digest('hex')
const hmac = (key: Buffer | string, data: string): Buffer => createHmac('sha256', key).update(data).digest()

// RFC 3986 unreserved characters stay; everything else is %HH (uppercase)
const uriEncode = (s: string): string => {
  let out = ''
  for (const b of new TextEncoder().encode(s)) {
    const c = String.fromCharCode(b)
    out += /^[A-Za-z0-9\-._~]$/.test(c) ? c : `%${b.toString(16).toUpperCase().padStart(2, '0')}`
  }
  return out
}
// S3 keys may contain '/': encode each segment, keep the slashes
const encodeKey = (key: string): string => key.split('/').map(uriEncode).join('/')

export interface ObjectStoreConfig {
  endpoint: string // e.g. http://127.0.0.1:9000
  bucket: string
  accessKey: string
  secretKey: string
  region?: string
}

export class S3ObjectStore {
  readonly bucket: string
  readonly endpoint: string
  private readonly accessKey: string
  private readonly secretKey: string
  private readonly region: string

  constructor(cfg: ObjectStoreConfig) {
    this.endpoint = cfg.endpoint.replace(/\/+$/, '')
    this.bucket = cfg.bucket
    this.accessKey = cfg.accessKey
    this.secretKey = cfg.secretKey
    this.region = cfg.region ?? 'us-east-1'
  }

  /** PUT an object; stores our sha256 as x-amz-meta-sha256 so HEAD can confirm
   *  an earlier write survived (the gateway's ETag is not an MD5). */
  async put(key: string, bytes: Uint8Array, contentType = 'application/octet-stream', bucket = this.bucket): Promise<ObjectRef> {
    const sha256 = sha256Hex(bytes)
    const r = await this.request('PUT', bucket, key, bytes, {
      'content-type': contentType,
      'x-amz-meta-sha256': sha256
    })
    if (!r.ok) throw await this.s3Error(r)
    return { bucket, key, sha256, size: bytes.length }
  }

  async get(key: string, bucket = this.bucket): Promise<Uint8Array> {
    const r = await this.request('GET', bucket, key)
    if (!r.ok) throw await this.s3Error(r)
    return new Uint8Array(await r.arrayBuffer())
  }

  async head(key: string, bucket = this.bucket): Promise<StoredHead | undefined> {
    const r = await this.request('HEAD', bucket, key)
    if (r.status === 404) return undefined
    if (!r.ok) throw await this.s3Error(r)
    const size = Number(r.headers.get('content-length') ?? -1)
    const meta = r.headers.get('x-amz-meta-sha256') ?? undefined
    return { size, ...(meta !== undefined ? { sha256: meta } : {}) }
  }

  /** Verified read — every read of stored content goes through here. Fetches,
   *  enforces the size bound and an optional expected size, then compares the
   *  SHA-256 against the reference. Any mismatch throws. */
  async getVerified(ref: { bucket?: string; key: string; sha256: string; size?: number }): Promise<Uint8Array> {
    if (ref.size !== undefined && ref.size > OBJECT_MAX_BYTES) throw new Error(`objectstore: ref size ${ref.size} exceeds ${OBJECT_MAX_BYTES} byte bound`)
    const bytes = await this.get(ref.key, ref.bucket ?? this.bucket)
    if (bytes.length > OBJECT_MAX_BYTES) throw new Error(`objectstore: object ${ref.key} is ${bytes.length} bytes, over the ${OBJECT_MAX_BYTES} byte bound`)
    if (ref.size !== undefined && bytes.length !== ref.size) throw new Error(`objectstore: object ${ref.key} is ${bytes.length} bytes, expected ${ref.size}`)
    const digest = sha256Hex(bytes)
    if (digest !== ref.sha256) throw new Error(`objectstore: sha256 mismatch for ${ref.key} — got ${digest}, expected ${ref.sha256}`)
    return bytes
  }

  private async request(method: string, bucket: string, key: string, body?: Uint8Array, extra: Record<string, string> = {}): Promise<Response> {
    const path = `/${uriEncode(bucket)}/${encodeKey(key)}`
    const url = `${this.endpoint}${path}`
    const now = new Date()
    const amzDate = now.toISOString().replace(/[:-]|\.\d+/g, '')
    const date = amzDate.slice(0, 8)
    const host = new URL(url).host
    const payloadHash = body !== undefined ? sha256Hex(body) : EMPTY_SHA256
    const headers: Record<string, string> = {
      host,
      'x-amz-content-sha256': payloadHash,
      'x-amz-date': amzDate,
      ...extra
    }
    const signed = Object.keys(headers).sort()
    const canonicalHeaders = signed.map((h) => `${h}:${headers[h]!.replace(/\s+/g, ' ').trim()}\n`).join('')
    const canonical = [method, path, '', canonicalHeaders, signed.join(';'), payloadHash].join('\n')
    const scope = `${date}/${this.region}/s3/aws4_request`
    const stringToSign = `AWS4-HMAC-SHA256\n${amzDate}\n${scope}\n${sha256Hex(new TextEncoder().encode(canonical))}`
    const k = hmac(hmac(hmac(hmac(`AWS4${this.secretKey}`, date), this.region), 's3'), 'aws4_request')
    const signature = createHmac('sha256', k).update(stringToSign).digest('hex')
    const authorization = `AWS4-HMAC-SHA256 Credential=${this.accessKey}/${scope}, SignedHeaders=${signed.join(';')}, Signature=${signature}`
    return await fetch(url, {
      method,
      headers: { ...headers, authorization },
      ...(body !== undefined ? { body: Buffer.from(body) } : {})
    })
  }

  private async s3Error(r: Response): Promise<Error> {
    const text = await r.text().catch(() => '')
    const code = text.match(/<Code>([^<]*)<\/Code>/)?.[1] ?? ''
    const message = text.match(/<Message>([^<]*)<\/Message>/)?.[1] ?? text.slice(0, 200)
    return new Error(`s3 ${r.status}${code !== '' ? ` ${code}` : ''}: ${message}`)
  }
}

/** Plain unsigned GET of one S3 object — no SigV4, no credentials. Works
 *  against a gateway whose bucket allows public reads; any refusal (403/404)
 *  throws so the caller can fall back to a signed read or another source.
 *  Content integrity is never delegated to this fetch: callers verify the
 *  returned bytes against a digest (getVerified-style) or a CHIRP closure. */
export async function getObjectUnsigned(endpoint: string, bucket: string, key: string): Promise<Uint8Array> {
  const url = `${endpoint.replace(/\/+$/, '')}/${uriEncode(bucket)}/${encodeKey(key)}`
  const r = await fetch(url)
  if (!r.ok) throw new Error(`s3 unsigned GET ${bucket}/${key} → ${r.status}`)
  const bytes = new Uint8Array(await r.arrayBuffer())
  if (bytes.length > OBJECT_MAX_BYTES) throw new Error(`objectstore: object ${key} is ${bytes.length} bytes, over the ${OBJECT_MAX_BYTES} byte bound`)
  return bytes
}

/** Env configuration — feature is OFF when NEKTAR_BSTORE_S3_URL or
 *  NEKTAR_BSTORE_BUCKET are absent, and nothing else changes.
 *
 *  NEKTAR_BSTORE_S3_URL          gateway base URL (operator-run bstore s3d)
 *  NEKTAR_BSTORE_BUCKET          S3 bucket name (bstore id with '-' for '_')
 *  NEKTAR_BSTORE_ACCESS_KEY      SigV4 access key
 *  NEKTAR_BSTORE_SECRET_KEY      SigV4 secret — or NEKTAR_BSTORE_SECRET_KEY_FILE
 *  (the put bounty is the gateway's own `bounty` config — no client setting) */
export function objectStoreFromEnv(env = process.env): S3ObjectStore | undefined {
  const endpoint = env.NEKTAR_BSTORE_S3_URL
  const bucket = env.NEKTAR_BSTORE_BUCKET
  const accessKey = env.NEKTAR_BSTORE_ACCESS_KEY
  const keyFile = env.NEKTAR_BSTORE_SECRET_KEY_FILE
  const secretKey = env.NEKTAR_BSTORE_SECRET_KEY ?? (keyFile !== undefined && existsSync(keyFile) ? readFileSync(keyFile, 'utf8').trim() : undefined)
  if (endpoint === undefined || bucket === undefined) return undefined
  if (accessKey === undefined || secretKey === undefined) throw new Error('objectstore: NEKTAR_BSTORE_S3_URL/BUCKET set but access or secret key is missing')
  return new S3ObjectStore({ endpoint, bucket, accessKey, secretKey })
}
