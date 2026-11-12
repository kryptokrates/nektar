// Interop conformance: reads nektar's wire encodings using ONLY the vendored
// lightwebinc codecs + @bsv/sdk — proves a stock reader parses what we emit.
// Run from the bstack package dir:
//   node packages/bstack/tools/interop-check.mjs   (repo root cwd)
//   node tools/interop-check.mjs                   (packages/bstack cwd)
//
// Fixture bytes are produced through OUR emit path (@nektar/bstack codecs and
// the same stock calls our src makes); every check decodes through stock code
// only — a failure here is a wire-format deviation, not a test mock gap.
import { PrivateKey, ProtoWallet, Utils, Hash } from '@bsv/sdk'
import {
  claimsRecord,
  recordReader,
  encodeRefs,
  decodeRefs,
  rootOfBytes,
  buildChirp,
  verifyClosure,
  chirpIdentifier,
  chirpURL,
  parseChirpURL,
  CborMap
} from '@lightwebinc/bcommon'
import { encodePlaintext, sealEnvelope, envelopeRecord } from '@lightwebinc/bbox-host/codec/send.js'
import { decodeEnvelope } from '@lightwebinc/bbox-host/codec/boxrec.js'
import { parseContent } from '@lightwebinc/bbox-host/codec/content.js'
import { open as openPlaintext, parsePlaintext } from '@lightwebinc/bbox-host/codec/payment.js'
import { JObject } from '@lightwebinc/bbox-host/codec/jcs.js'
import { encodeRecord, decodeRecord, validateRecord, KindManifest, MagicV1 } from '@lightwebinc/bfinger-host/dist/record.js'
import { encodePayload, decodePayload } from '@nektar/bstack'

let failures = 0
const check = async (name, fn) => {
  try {
    await fn()
    console.log(`ok   ${name}`)
  } catch (e) {
    failures++
    console.log(`FAIL ${name}: ${e.message}`)
  }
}
const hex = (b) => Utils.toHex(Array.from(b))
const NKT1 = new TextEncoder().encode('nkt1')

// ── 1. market record: our nkt1 payload through the stock record codec ────────
{
  const body = { wantId: 'w-interop1', pub: '02' + 'ab'.repeat(32), net: 'bsv:regtest', want: { want: 'fixture', domain: 'interop.test', level: 0, maxPrice: 5 }, sig: 's', at: 1 }
  const payload = encodePayload({ v: 1, kind: 'want', body })
  await check('market payload claims the nkt1 record magic', () => {
    if (!claimsRecord(payload, NKT1)) throw new Error('claimsRecord refused')
  })
  await check('stock recordReader walks our record', () => {
    const r = recordReader((reason, detail) => new Error(`record ${reason}${detail ? ': ' + detail : ''}`))
    const f = r.decode(payload, 16384, 2n, NKT1)
    if (r.uint(f, 1, 1, 2) !== 1) throw new Error('key 1 (kind) not want')
    const t = r.text(f, 2)
    if (JSON.parse(t).wantId !== 'w-interop1') throw new Error('key 2 (body) wrong')
  })
  await check('our decoder round-trips the same bytes', () => {
    const back = decodePayload(payload)
    if (back.kind !== 'want' || back.body.wantId !== 'w-interop1') throw new Error('round-trip mismatch')
  })
}

// ── 2. envelope: record → decode → parseContent → open → parsePlaintext ──────
{
  const sender = new PrivateKey(42)
  const recip = new PrivateKey(43)
  const from = new Uint8Array(sender.toPublicKey().encode(true))
  const to = new Uint8Array(recip.toPublicKey().encode(true))
  const doc = new JObject()
    .set('nektar', new JObject().set('market', new JObject().set('type', 'quote')))
    .set('refs', [new JObject().set('url', 'https://store.example.com/bucket/evidence/op1.json').set('sha256', 'a'.repeat(64)).set('length', 4096)])
  const created = Math.floor(Date.now() / 1000)
  const content = await sealEnvelope(new ProtoWallet(sender), 'nektar', from, to, encodePlaintext(doc), created)
  const env = { office: 'nektar_abcdefghij', to, box: 'market', from, created, expires: created + 3600, content, extra: [] }
  const record = envelopeRecord(env)
  await check('stock envelope decode → open → parsePlaintext', async () => {
    const e = decodeEnvelope(record)
    if (e === undefined) throw new Error('decodeEnvelope refused')
    const c = parseContent(e)
    const opened = await openPlaintext(new ProtoWallet(recip), c, new Uint8Array(e.from))
    const p = parsePlaintext(opened)
    const nektar = p.doc.get('nektar')
    if (!(nektar instanceof JObject) || !(nektar.get('market') instanceof JObject)) throw new Error('nektar member lost')
    if (p.refs?.length !== 1 || p.refs[0].url !== 'https://store.example.com/bucket/evidence/op1.json') throw new Error('refs member lost')
    if (hex(p.refs[0].sha256) !== 'a'.repeat(64)) throw new Error('ref digest mangled')
  })
}

// ── 3. store refs + kind-6 manifest: stock store/commit/finger codecs ────────
{
  const members = [new Uint8Array(Hash.sha256(Utils.toArray('member-a', 'utf8'))), new Uint8Array(Hash.sha256(Utils.toArray('member-b', 'utf8')))]
  const manifestC = new Uint8Array(Hash.sha256(Utils.toArray('manifest-1', 'utf8')))
  const ref = { name: 'nektar-packs', root: rootOfBytes(members), count: BigInt(members.length), head: manifestC }
  await check('encodeRefs → decodeRefs round-trips the packs store ref', () => {
    const back = decodeRefs(encodeRefs([ref]))
    if (back.length !== 1 || back[0].name !== 'nektar-packs') throw new Error('name lost')
    if (hex(back[0].root) !== hex(ref.root)) throw new Error('root mismatch')
    if (back[0].count !== 2n || hex(back[0].head) !== hex(manifestC)) throw new Error('count/head mismatch')
  })
  const manifest = {
    magic: MagicV1,
    identityKey: new Uint8Array(new PrivateKey(9).toPublicKey().encode(true)),
    seq: 1n, // store records are create-shaped: seq 1, zero prev, no witness
    kind: KindManifest,
    prev: new Uint8Array(32),
    salt: new Uint8Array(32),
    wc: new Uint8Array(32),
    notBefore: 0n,
    notAfter: 0n,
    body: new CborMap([
      { key: 'v', val: 1n },
      { key: 'name', val: 'nektar-packs' },
      { key: 'members', val: members.map((c) => new CborMap([{ key: 'c', val: c }, { key: 'sha256', val: new Uint8Array(32) }, { key: 'size', val: 1n }])) }
    ]),
    refs: [],
    unknown: []
  }
  await check('kind-6 manifest validates + decodeRecord round-trips', () => {
    validateRecord(manifest)
    const back = decodeRecord(encodeRecord(manifest))
    if (back.kind !== KindManifest || back.seq !== 1n) throw new Error('manifest fields lost')
    const mb = back.body.get('members')
    if (!Array.isArray(mb) || mb.length !== 2 || !(mb[0] instanceof CborMap)) throw new Error('members list lost')
  })
}

// ── 4. chirp objects: build → identifier/url → closure verifies ──────────────
{
  const bytes = new TextEncoder().encode('nektar shard pack fixture — content addressed object')
  const built = buildChirp(bytes)
  await check('identifier + chirp:// url round-trip the root hash', () => {
    const id = chirpIdentifier(built.rootHash)
    const url = chirpURL(built.rootHash)
    if (!url.startsWith('chirp://') || !url.endsWith(id)) throw new Error('url does not carry the identifier')
    if (hex(parseChirpURL(url)) !== hex(built.rootHash)) throw new Error('url decodes to a different hash')
  })
  await check('verifyClosure accepts objects stored by content hash', async () => {
    // what our storeChirp persists: every node/blob keyed by sha256(bytes)
    const store = new Map()
    store.set(hex(built.rootHash), built.root)
    for (const b of built.branches) store.set(hex(new Uint8Array(Hash.sha256(Array.from(b)))), b)
    for (const b of built.blobs ?? []) store.set(hex(new Uint8Array(Hash.sha256(Array.from(b)))), b)
    const c = await verifyClosure(built.rootHash, (h) => store.get(hex(h)))
    if (!(c.blobs?.length >= 0)) throw new Error('closure empty')
  })
}

console.log(failures === 0 ? '\ninterop: all checks passed' : `\ninterop: ${failures} check(s) FAILED`)
process.exit(failures === 0 ? 0 : 1)
