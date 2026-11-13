// Golden conformance vectors for the market wire (docs/MARKET-WIRE.md) —
// a foreign implementation that reproduces these bytes speaks the protocol:
// lane naming, marketJson canonicalization, artifact sigs, the nkt1 record,
// and the covenant-identity pin.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { PrivateKey } from '@bsv/sdk'
import { signWant, signOffer, verifyWant, verifyOffer, encodePayload, decodePayload, laneOf, laneNibble } from '@nektar/bstack'
import { saleQuoteWire, publicSaleQuote } from '@nektar/identity'
import { COVENANT_ID } from '@nektar/anchor'

const hex = (s: string): Uint8Array => new Uint8Array(Buffer.from(s, 'hex'))
const mv = JSON.parse(readFileSync(new URL('../../../bstack/test/vectors/market-v1.json', import.meta.url), 'utf8'))

test('market wire — lanes, canonical artifacts, nkt1 payloads are byte-stable', async () => {
  const key = PrivateKey.fromHex(mv.privateKeyHex)
  // lane naming is part of the contract — a foreign impl must land on the
  // same topic for the same {net, domain}
  assert.equal(laneNibble('libfoo'), mv.nibble_libfoo)
  assert.equal(laneOf('regtest', 'libfoo'), mv.laneOf_regtest_libfoo)
  // artifact signing is deterministic — same key + fields → same bytes
  const sw = await signWant(key, { domains: ['libfoo'], question: 'q?', maxSats: 500, wantId: 'want-vec-1', ts: 1800000000000, exp: 1800100000000 })
  assert.deepEqual(sw, mv.want)
  assert.ok(verifyWant(sw.want, sw.sig, { allowExpired: true, now: 1800000000000 }))
  const so = await signOffer(key, {
    offerId: 'offer-vec-1',
    shard: 'guide',
    seq: 1,
    shardId: 'dd'.repeat(32),
    domains: ['libfoo'],
    leaf: 'ef'.repeat(32),
    hdp: '12'.repeat(32),
    priceSats: 200,
    offer: mv.saleQuote,
    wantId: 'want-vec-1',
    taskHash: 'ff'.repeat(32),
    wantRevision: 1,
    buyerPub: key.toPublicKey().toString(),
    ts: 1800000000000,
    net: 'regtest'
  })
  assert.deepEqual(so, mv.offer)
  assert.ok(verifyOffer(so.offer, so.sig, { now: 1800000000000 }))
  // the nkt1 record bytes are canonical — a decoder must lift the same body
  const wp = encodePayload({ v: 1, kind: 'want', body: sw })
  const op = encodePayload({ v: 1, kind: 'offer', body: so })
  assert.equal(Buffer.from(wp).toString('hex'), mv.wantPayloadHex)
  assert.equal(Buffer.from(op).toString('hex'), mv.offerPayloadHex)
  assert.deepEqual(decodePayload(hex(mv.wantPayloadHex)).body, sw)
  assert.deepEqual(decodePayload(hex(mv.offerPayloadHex)).body, so)
  // the covenant pin — the script hash foreign impls check quotes against
  assert.equal(mv.covenant.artifactId, COVENANT_ID.artifactId)
  assert.equal(mv.covenant.scriptHash, COVENANT_ID.scriptHash)
  // the wire quote keeps the artifact through the public projection
  const sq = saleQuoteWire(publicSaleQuote({ ...mv.saleQuote, cfg: { ...mv.saleQuote.cfg }, artifact: mv.covenant } as never))
  assert.deepEqual(sq.artifact, mv.covenant)
  // direct: the rail advertisement rides inside the signed offer — it is not
  // a post-hoc flag, so tampering breaks the market signature like any field
  const dq = { ...mv.saleQuote, cfg: { ...mv.saleQuote.cfg }, artifact: mv.covenant, direct: true }
  assert.equal(saleQuoteWire(publicSaleQuote(dq as never)).direct, true)
  const dso = await signOffer(PrivateKey.fromHex(mv.privateKeyHex), { ...mv.offer.offer, offer: dq })
  assert.equal((dso.offer.offer as { direct?: boolean }).direct, true)
  assert.ok(verifyOffer(dso.offer, dso.sig, { now: 1800000000000 }))
})

test('market wire — a malformed artifact field fails quote shape validation', async () => {
  const key = PrivateKey.fromHex(mv.privateKeyHex)
  const bad = { ...mv.saleQuote, artifact: { artifactId: 'DisclosureSale', scriptHash: 'zz'.repeat(32) } }
  await assert.rejects(() => signOffer(key, { ...mv.offer.offer, offer: bad }), /bad artifact\.scriptHash/)
  // a lookalike covenant (well-formed but wrong scriptHash) passes shape —
  // the buyer-side offerReject (market.ts) is what refuses it before funding
  const lookalike = { ...mv.saleQuote, artifact: { artifactId: 'DisclosureSale', scriptHash: '00'.repeat(32) } }
  const so = await signOffer(key, { ...mv.offer.offer, offer: lookalike })
  assert.notEqual((so.offer.offer as { artifact: { scriptHash: string } }).artifact.scriptHash, COVENANT_ID.scriptHash)
})
