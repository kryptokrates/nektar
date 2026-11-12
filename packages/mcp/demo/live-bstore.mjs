// live demo against the bstore quickstart sandbox's s3d gateway
// (bstore/deploy/quickstart): put an evidence-shaped JSON object through the
// SigV4 client, HEAD it, read it back through getVerified (digest-checked),
// then prove a tampered ref is refused. Nektar verifies content SHA-256 —
// provider possession proofs and bounty settlement stay with the gateway.
//
//   node packages/mcp/demo/live-bstore.mjs
// env overrides: NEKTAR_BSTORE_S3_URL / _BUCKET / _ACCESS_KEY / _SECRET_KEY
import { createHash } from 'node:crypto'
import { S3ObjectStore } from '../dist/src/objectstore.js'

const store = new S3ObjectStore({
  endpoint: process.env.NEKTAR_BSTORE_S3_URL ?? 'http://127.0.0.1:9000',
  bucket: process.env.NEKTAR_BSTORE_BUCKET ?? 'evidence-quickstart',
  accessKey: process.env.NEKTAR_BSTORE_ACCESS_KEY ?? 'QUICKSTARTACCESSKEY',
  secretKey: process.env.NEKTAR_BSTORE_SECRET_KEY ?? 'quickstart-secret-not-secret',
  region: 'us-east-1'
})
const log = []
const ok = (k, v) => log.push(`${v ? 'PASS' : 'FAIL'} ${k}`)
const info = (s) => log.push(`INFO ${s}`)
const sha = (b) => createHash('sha256').update(b).digest('hex')

// an object shaped like a redacted trade-evidence bundle (no secrets — the
// question and disclosure tx are digests, never plaintext)
const bundle = {
  v: 1,
  redacted: true,
  network: 'regtest',
  want: { want: { wantId: 'demo', questionSha256: 'ab'.repeat(32) }, sig: '00' },
  offers: [],
  selectedOfferKey: 'demo:0',
  escrow: { txid: '11'.repeat(32), txHex: 'deadbeef', vout: 0, depositorPkh: '22'.repeat(20) },
  disclosure: { txid: '33'.repeat(32), txHexSha256: '44'.repeat(32) },
  buyerEvents: [],
  claims: { confirmation: 'broadcast-accepted-not-proven', semantic: 'not_asserted' }
}
const bytes = new TextEncoder().encode(JSON.stringify(bundle))
const key = `evidence/regtest/demo-${Date.now()}.json`

const t0 = Date.now()
const ref = await store.put(key, bytes, 'application/json')
ok(`put evidence-shaped object (${bytes.length} B → ${ref.bucket}/${ref.key})`, ref.sha256 === sha(bytes) && ref.size === bytes.length)

// the gateway may take a beat before the object serves — measure put→readable
let head
for (let i = 0; i < 120 && head === undefined; i++) {
  head = await store.head(key).catch(() => undefined)
  if (head === undefined) await new Promise((r) => setTimeout(r, 1000))
}
const readableMs = Date.now() - t0
ok(`head readable ${(readableMs / 1000).toFixed(1)}s after put (size ${head?.size})`, head !== undefined && head.size === bytes.length)
ok(`head returns our sha256 meta`, head?.sha256 === ref.sha256)

const back = await store.getVerified(ref)
ok('getVerified returns byte-identical content', back.length === bytes.length && Buffer.compare(Buffer.from(back), Buffer.from(bytes)) === 0)

// tamper: a wrong digest or wrong size must be refused before use
await store
  .getVerified({ ...ref, sha256: '00'.repeat(32) })
  .then(() => ok('getVerified refuses a forged digest', false))
  .catch(() => ok('getVerified refuses a forged digest', true))
await store
  .getVerified({ ...ref, size: bytes.length + 1 })
  .then(() => ok('getVerified refuses a wrong size', false))
  .catch(() => ok('getVerified refuses a wrong size', true))
await store
  .getVerified({ key: 'never/put.json', sha256: '00'.repeat(32) })
  .then(() => ok('getVerified surfaces a missing object', false))
  .catch(() => ok('getVerified surfaces a missing object', true))

for (const l of log) console.log(l)
const fails = log.filter((l) => l.startsWith('FAIL'))
console.log(
  fails.length === 0
    ? `ALL PASS (${log.filter((l) => l.startsWith('PASS')).length}/${log.filter((l) => l.startsWith('PASS')).length})`
    : `${fails.length} FAILURES`
)
process.exit(fails.length === 0 ? 0 : 1)
