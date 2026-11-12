// objectstore.test.ts — the SigV4 S3 client against an in-process fake S3
// that re-derives the signature with the known secret (the same recomputation
// the bstore gateway performs), plus getVerified's digest/size enforcement.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { createHash, createHmac } from 'node:crypto'
import { S3ObjectStore, objectStoreFromEnv, OBJECT_MAX_BYTES } from '../src/objectstore.js'

const SECRET = 'quickstart-secret-not-secret'
const ACCESS = 'QUICKSTARTACCESSKEY'
const REGION = 'us-east-1'

const sha256Hex = (b: Uint8Array): string => createHash('sha256').update(b).digest('hex')
const hmac = (key: Buffer | string, data: string): Buffer => createHmac('sha256', key).update(data).digest()

/** recompute the SigV4 signature an honest client should have produced for
 *  this request — the gateway's own check. Returns the expected Signature. */
function expectedSig(method: string, path: string, headers: Record<string, string>, secret = SECRET): string {
  const auth = headers.authorization!
  const signed = auth.match(/SignedHeaders=([^,]+)/)![1]!.split(';')
  const canonicalHeaders = signed.map((h) => `${h}:${headers[h]!.replace(/\s+/g, ' ').trim()}\n`).join('')
  const payloadHash = headers['x-amz-content-sha256']!
  const canonical = [method, path, '', canonicalHeaders, signed.join(';'), payloadHash].join('\n')
  const amzDate = headers['x-amz-date']!
  const scope = `${amzDate.slice(0, 8)}/${REGION}/s3/aws4_request`
  const sts = `AWS4-HMAC-SHA256\n${amzDate}\n${scope}\n${sha256Hex(new TextEncoder().encode(canonical))}`
  const k = hmac(hmac(hmac(hmac(`AWS4${secret}`, amzDate.slice(0, 8)), REGION), 's3'), 'aws4_request')
  return createHmac('sha256', k).update(sts).digest('hex')
}

/** minimal path-style S3: PUT/GET/HEAD on /{bucket}/{key}, SigV4 enforced. */
async function fakeS3(secret = SECRET): Promise<{ server: Server; url: string; objects: Map<string, { bytes: Uint8Array; meta: Record<string, string> }> }> {
  const objects = new Map<string, { bytes: Uint8Array; meta: Record<string, string> }>()
  const server = createServer((req, res) => {
    const headers = Object.fromEntries(Object.entries(req.headers).map(([k, v]) => [k, Array.isArray(v) ? v.join(',') : String(v)]))
    const auth = headers.authorization ?? ''
    const m = auth.match(/Signature=([0-9a-f]+)/)
    if (m === null || m[1] !== expectedSig(req.method!, req.url!, headers, secret)) {
      res.writeHead(403, { 'content-type': 'application/xml' }).end(`<Error><Code>SignatureDoesNotMatch</Code><Message>bad sig</Message></Error>`)
      return
    }
    const key = req.url!.split('/').slice(2).join('/')
    if (req.method === 'PUT') {
      const chunks: Buffer[] = []
      req.on('data', (c) => chunks.push(c))
      req.on('end', () => {
        const meta: Record<string, string> = {}
        for (const [h, v] of Object.entries(headers)) if (h.startsWith('x-amz-meta-')) meta[h] = v
        objects.set(key, { bytes: new Uint8Array(Buffer.concat(chunks)), meta })
        res.writeHead(200, { etag: '"deadbeef"' }).end()
      })
      return
    }
    const o = objects.get(key)
    if (o === undefined) {
      res.writeHead(404, { 'content-type': 'application/xml' }).end(`<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>`)
      return
    }
    if (req.method === 'HEAD') {
      res
        .writeHead(200, {
          'content-length': o.bytes.length,
          ...(o.meta['x-amz-meta-sha256'] !== undefined ? { 'x-amz-meta-sha256': o.meta['x-amz-meta-sha256'] } : {})
        })
        .end()
      return
    }
    if (req.method === 'GET') {
      res.writeHead(200, { 'content-length': o.bytes.length }).end(Buffer.from(o.bytes))
      return
    }
    res.writeHead(405).end()
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  const { port } = server.address() as { port: number }
  return { server, url: `http://127.0.0.1:${port}`, objects }
}

const store = (url: string, secret = SECRET) =>
  new S3ObjectStore({ endpoint: url, bucket: 'packs-zzktyfefka', accessKey: ACCESS, secretKey: secret, region: REGION })

test('objectstore: put/get/head round trip on a signature-checking fake S3', async (t) => {
  const { server, url, objects } = await fakeS3()
  try {
    const s = store(url)
    const bytes = new TextEncoder().encode('{"v":1,"evidence":true}')
    const ref = await s.put('evidence/mock/op-1.json', bytes, 'application/json')
    assert.equal(ref.key, 'evidence/mock/op-1.json')
    assert.equal(ref.size, bytes.length)
    assert.equal(ref.sha256, sha256Hex(bytes))
    const head = await s.head(ref.key)
    assert.equal(head?.size, bytes.length)
    assert.equal(head?.sha256, ref.sha256) // x-amz-meta-sha256 carried through
    assert.deepEqual([...(await s.get(ref.key))], [...bytes])
    assert.equal(objects.has('evidence/mock/op-1.json'), true)
  } finally {
    server.close()
  }
})

test('objectstore: wrong secret → 403 surfaces as an error', async (t) => {
  const { server, url } = await fakeS3()
  try {
    await assert.rejects(() => store(url, 'wrong-secret').put('k.json', new Uint8Array([1])), /403|SignatureDoesNotMatch/)
  } finally {
    server.close()
  }
})

test('objectstore: getVerified returns verified bytes', async (t) => {
  const { server, url } = await fakeS3()
  try {
    const s = store(url)
    const bytes = new TextEncoder().encode('pack bytes')
    const ref = await s.put('packs/mock/shard/abc.json', bytes)
    assert.deepEqual([...(await s.getVerified(ref))], [...bytes])
    assert.deepEqual([...(await s.getVerified({ key: ref.key, sha256: ref.sha256 }))], [...bytes])
    await assert.rejects(() => s.getVerified({ key: ref.key, sha256: ref.sha256, size: bytes.length + 1 }), /expected/)
  } finally {
    server.close()
  }
})

test('objectstore: getVerified rejects tampered content', async (t) => {
  const { server, url, objects } = await fakeS3()
  try {
    const s = store(url)
    const ref = await s.put('x.json', new TextEncoder().encode('honest bytes'))
    objects.get('x.json')!.bytes = new TextEncoder().encode('forged-bytes') // same length — reaches the digest check
    await assert.rejects(() => s.getVerified(ref), /sha256 mismatch/)
  } finally {
    server.close()
  }
})

test('objectstore: getVerified rejects oversized content and oversized refs', async (t) => {
  const { server, url, objects } = await fakeS3()
  try {
    const s = store(url)
    // a ref that declares a size over the bound is refused before any GET
    await assert.rejects(() => s.getVerified({ key: 'big', sha256: 'ab'.repeat(32), size: OBJECT_MAX_BYTES + 1 }), /bound/)
    // and a stored object over the bound is refused after fetch
    objects.set('big', { bytes: new Uint8Array(OBJECT_MAX_BYTES + 1), meta: {} })
    await assert.rejects(() => s.getVerified({ key: 'big', sha256: 'ab'.repeat(32) }), /bound/)
  } finally {
    server.close()
  }
})

test('objectstore: env config — off without URL/bucket, secret via file or env', async t => {
  assert.equal(objectStoreFromEnv({} as NodeJS.ProcessEnv), undefined)
  assert.equal(
    objectStoreFromEnv({
      NEKTAR_BSTORE_S3_URL: 'http://x',
      NEKTAR_BSTORE_BUCKET: 'b',
      NEKTAR_BSTORE_ACCESS_KEY: 'a',
      NEKTAR_BSTORE_SECRET_KEY: 's'
    } as unknown as NodeJS.ProcessEnv)?.bucket,
    'b'
  )
  assert.throws(() => objectStoreFromEnv({ NEKTAR_BSTORE_S3_URL: 'http://x', NEKTAR_BSTORE_BUCKET: 'b' } as unknown as NodeJS.ProcessEnv), /key/)
})
