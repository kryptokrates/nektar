import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { signWant, verifyWant, signOffer, verifyOffer, wantTaskDigest, type SignedOffer } from '../src/artifacts.js'

const key = PrivateKey.fromRandom()
const other = PrivateKey.fromRandom()
const want = (o: Record<string, unknown> = {}, k = key) => signWant(k, { domains: ['soccer'], ts: Date.now(), ...o })
const offer = (o: Record<string, unknown> = {}, k = key) => signOffer(k, { shard: 's', seq: 0, priceSats: 100, domains: ['soccer'], ts: Date.now(), ...o })
const pub = key.toPublicKey().toString()
const quote = (price = 100) => ({
  cfg: { sellerPkh: 'a'.repeat(40), price, hWrapped: 'b'.repeat(64), refundAfter: 1_000_000, maxFee: 10 },
  sellerPub: pub,
  offerSig: '30' + '44'.repeat(35)
})

// pinned artifacts produced by the retired @nektar/overlay presence.ts —
// byte-compatible digests + signatures are the migration contract
const FIXTURE = {
  want: {
    domains: ['libfoo'],
    question: 'libfoo 3.0 migration?',
    maxSats: 450,
    ts: 1700000000000,
    exp: 1800000000000,
    // net is signature-covered — the overlay fixture was signed with 'mock';
    // do NOT sweep this to 'regtest' with the migration rename
    net: 'mock',
    wantId: 'fixture-want-1',
    streamSeq: 3,
    prevHash: 'ab'.repeat(32),
    tip: 'cd'.repeat(32),
    tipHeight: 77,
    revision: 1,
    status: 'open' as const,
    taskHash: 'bd8ea22fb176e7a2bdc6ba0ae71dcd4d61372c900bd252544d01b1209ffc6d10',
    pub: '02fe8d1eb1bcb3432b1db5833ff5f2226d9cb5e65cee430558c18ed3a3c86ce1af'
  },
  wantSig: '30450221008db5f4f336d9bcdead28144d5ec5a54412bbc42c4c99e4b75ac90073bb43c90c022031a7defdea770dfec23d5fc270d60a4b1e6d962d5fbdfdb334c70888d65683d9',
  offer: {
    shard: 'guide',
    seq: 5,
    priceSats: 200,
    domains: ['libfoo'],
    ts: 1700000000001,
    exp: 1800000000001,
    net: 'mock',
    wantId: 'fixture-want-1',
    buyerPub: '02fe8d1eb1bcb3432b1db5833ff5f2226d9cb5e65cee430558c18ed3a3c86ce1af',
    offerId: 'fixture-offer-1',
    streamSeq: 7,
    prevHash: 'ef'.repeat(32),
    offer: {
      cfg: { sellerPkh: 'a'.repeat(40), price: 200, hWrapped: 'bb'.repeat(32), refundAfter: 9000, maxFee: 10 },
      sellerPub: '03d528ecd9b696b54c907a9ed045447a79bb408ec39b68df504bb51f459bc3ffc9',
      expiresAt: 9001,
      offerSig: 'cc'.repeat(70)
    },
    revision: 1,
    pub: '03d528ecd9b696b54c907a9ed045447a79bb408ec39b68df504bb51f459bc3ffc9'
  },
  offerSig: '30450221008e399de7410470c1d6b7e4040bcc60eddd6c954567ddbd153b6608814283e87c022010fc069bccce42a8d78924b53c30820f24fad0e3dcd81aebba66df242ec4b366'
}

test('fixture: artifacts signed by @nektar/overlay still verify byte-for-byte', () => {
  assert.equal(verifyWant(FIXTURE.want, FIXTURE.wantSig, { allowExpired: true }), true)
  assert.equal(verifyOffer(FIXTURE.offer, FIXTURE.offerSig, { allowExpired: true }), true)
  // the same fields re-serialize to the same canonical taskHash
  assert.equal(wantTaskDigest({ domains: ['libfoo'], question: 'libfoo 3.0 migration?', maxSats: 450 }), FIXTURE.want.taskHash)
})

test('signature covers the whole canonical artifact — any mutation fails', () => {
  const w = want({ question: 'who won', maxSats: 500 })
  assert.ok(verifyWant(w.want, w.sig))
  for (const mutate of [
    () => ({ ...w.want, question: 'other' }),
    () => ({ ...w.want, maxSats: 1 }),
    () => ({ ...w.want, taskHash: 'e'.repeat(64) }),
    () => ({ ...w.want, wantId: 'other-id' }),
    () => ({ ...w.want, revision: 5, previousHash: 'f'.repeat(64) }),
    () => ({ ...w.want, net: 'main' }),
    () => ({ ...w.want, domains: ['rugby'] })
  ])
    assert.equal(verifyWant(mutate(), w.sig), false)
  // malformed values verify false without throwing
  for (const bad of [
    null,
    undefined,
    {},
    { ...w.want, wantId: '   ' },
    { ...w.want, revision: 0 },
    { ...w.want, taskHash: 'zz' },
    { ...w.want, status: 'weird' }
  ])
    assert.equal(verifyWant(bad as never, w.sig), false)
  // signing invalid values throws instead of producing an unverifiable artifact
  assert.throws(() => want({ wantId: 'has space' }))
  assert.throws(() => want({ taskHash: 'nothex' }))
  assert.throws(() => want({ revision: 0 }))
  // revision>1 must name its predecessor
  assert.throws(() => want({ revision: 2 }), /previousHash/)
  // taskHash default commits to the request terms
  assert.equal(w.want.taskHash, wantTaskDigest({ domains: ['soccer'], question: 'who won', maxSats: 500 }))
})

test('expired wants verify only with allowExpired (evidence path)', async () => {
  const w = want({ exp: Date.now() + 30 })
  await new Promise((r) => setTimeout(r, 60)) // let the expiry lapse
  assert.equal(verifyWant(w.want, w.sig), false)
  assert.equal(verifyWant(w.want, w.sig, { allowExpired: true }), true)
})

test('offer: want binding, quote without wrapped, revision + id rules', () => {
  const w = want()
  const o = offer({ wantId: w.want.wantId, taskHash: w.want.taskHash, wantRevision: 1, buyerPub: w.want.pub, replyTo: w.want.pub })
  assert.ok(verifyOffer(o.offer, o.sig))
  assert.equal(o.offer.offerId.length > 0 && o.offer.revision, 1)
  // mutation of nested quote / want binding / price breaks the signature
  for (const mutate of [
    () => ({ ...o.offer, priceSats: 5 }),
    () => ({ ...o.offer, wantId: 'other' }),
    () => ({ ...o.offer, taskHash: 'c'.repeat(64) }),
    () => ({ ...o.offer, wantRevision: 2 }),
    () => ({ ...o.offer, offer: quote(50) }),
    () => ({ ...o.offer, net: 'main' }),
    () => ({ ...o.offer, buyerPub: other.toPublicKey().toString() })
  ])
    assert.equal(verifyOffer(mutate(), o.sig), false)
  // a quote carrying the reveal credential can never be signed or verified
  const leaky = { ...quote(), wrapped: 'aa'.repeat(32) }
  assert.throws(() => offer({ offer: leaky }), /quote|wrapped/i)
  assert.equal(verifyOffer({ ...o.offer, offer: leaky }, o.sig), false)
  // revision chain enforced
  assert.throws(() => offer({ offerId: o.offer.offerId, revision: 2 }), /previousHash/)
  const o2 = offer({ offerId: o.offer.offerId, revision: 2, previousHash: 'f'.repeat(64) })
  assert.ok(verifyOffer(o2.offer, o2.sig))
})

test('signWant/signOffer stamp monotonic stream fields per producer', () => {
  const k = PrivateKey.fromRandom()
  const w1 = signWant(k, { domains: ['soccer'], ts: Date.now() })
  const w2 = signWant(k, { domains: ['soccer'], ts: Date.now() })
  assert.equal(w2.want.streamSeq, (w1.want.streamSeq ?? 0) + 1)
  assert.notEqual(w2.want.prevHash, '0'.repeat(64))
  const o = signOffer(k, { shard: 's', seq: 0, priceSats: 1, domains: ['soccer'], ts: Date.now() })
  assert.equal(o.offer.streamSeq, 1, 'offer stream is its own sequence')
})
