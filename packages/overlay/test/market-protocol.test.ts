import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import knexPkg from 'knex'
import { createLsPresence, signWant, verifyWant, signOffer, verifyOffer, wantArtifactHash, offerArtifactHash, wantTaskDigest, wantOutputScript, offerOutputScript, parseWantOutput } from '../src/presence.js'

const key = PrivateKey.fromRandom()
const other = PrivateKey.fromRandom()
const want = (o: Record<string, unknown> = {}, k = key) => signWant(k, { domains: ['soccer'], ts: Date.now(), ...o } as never)
const offer = (o: Record<string, unknown> = {}, k = key) => signOffer(k, { shard: 's', seq: 0, priceSats: 100, domains: ['soccer'], ts: Date.now(), ...o } as never)
const pub = key.toPublicKey().toString()
const quote = (price = 100) => ({
  cfg: { sellerPkh: 'a'.repeat(40), price, hWrapped: 'b'.repeat(64), refundAfter: 1_000_000, maxFee: 10 },
  sellerPub: pub, offerSig: '30' + '44'.repeat(35)
})

test('two independent wants from one publisher coexist; wantFor addresses each by id', async () => {
  const ls = createLsPresence()
  const w1 = want({ question: 'results' })
  const w2 = want({ question: 'fixtures' })
  assert.notEqual(w1.want.wantId, w2.want.wantId)
  assert.equal(await ls.announceWant(w1), true)
  assert.equal(await ls.announceWant(w2), true)
  assert.equal(ls.wantsFor(pub).length, 2)
  assert.equal(ls.wantFor(pub, w1.want.wantId)?.want.question, 'results')
  assert.equal(ls.wantFor(pub, w2.want.wantId)?.want.question, 'fixtures')
  // no-id lookup stays backwards compatible: latest open want for the pub
  assert.equal(ls.wantFor(pub)?.want.wantId, w2.want.ts >= w1.want.ts ? w2.want.wantId : w1.want.wantId)
  // a second publisher's want doesn't collide
  const w3 = want({}, other)
  assert.equal(await ls.announceWant(w3), true)
  assert.equal(ls.wantsFor().length, 3)
})

test('id-stable retry: identical re-announce is a no-op; revision chain enforced', async () => {
  const ls = createLsPresence()
  const w1 = want({ question: 'q' })
  assert.equal(await ls.announceWant(w1), true)
  assert.equal(await ls.announceWant(w1), true)
  assert.equal((await ls.feed({ kind: 'want' })).length, 1, 'duplicate produced no second event')
  // rev 2 must name its predecessor
  assert.throws(() => want({ wantId: w1.want.wantId, revision: 2, ts: Date.now() + 1 }), /previousHash/)
  const w2 = want({ wantId: w1.want.wantId, revision: 2, previousHash: wantArtifactHash(w1), question: 'q2', ts: Date.now() + 1 })
  assert.equal(await ls.announceWant(w2), true)
  assert.equal(ls.wantFor(pub, w1.want.wantId)?.want.revision, 2)
  // stale rev ignored; conflicting same rev rejected; wrong predecessor rejected
  assert.equal(await ls.announceWant(w1), true)
  assert.equal(ls.wantFor(pub, w1.want.wantId)?.want.revision, 2)
  const conflict = want({ wantId: w1.want.wantId, revision: 2, previousHash: wantArtifactHash(w1), question: 'evil', ts: Date.now() + 2 })
  assert.equal(await ls.announceWant(conflict), false)
  const wrongPrev = want({ wantId: w1.want.wantId, revision: 3, previousHash: 'f'.repeat(64), ts: Date.now() + 3 })
  assert.equal(await ls.announceWant(wrongPrev), false)
  // rev>1 for an unknown wantId is a missing predecessor, never an insert
  const orphan = want({ revision: 2, previousHash: wantArtifactHash(w1), ts: Date.now() + 4 })
  assert.equal(await ls.announceWant(orphan), false)
})

test('cancel tombstones the wantId but is never returned as open', async () => {
  const ls = createLsPresence()
  const w1 = want()
  await ls.announceWant(w1)
  const cancel = want({ wantId: w1.want.wantId, revision: 2, previousHash: wantArtifactHash(w1), status: 'canceled', ts: Date.now() + 1 })
  assert.equal(await ls.announceWant(cancel), true)
  assert.equal(ls.wantFor(pub, w1.want.wantId), undefined)
  assert.equal(ls.wantsFor(pub).length, 0)
  // a canceled chain still enforces revisions — replay of rev1 stays ignored
  assert.equal(await ls.announceWant(w1), true)
})

test('signature covers the whole canonical artifact — any mutation fails', async () => {
  const ls = createLsPresence()
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
  ]) assert.equal(verifyWant(mutate(), w.sig), false)
  // malformed values verify false without throwing
  for (const bad of [null, undefined, {}, { ...w.want, wantId: '   ' }, { ...w.want, revision: 0 }, { ...w.want, taskHash: 'zz' }, { ...w.want, status: 'weird' }])
    assert.equal(verifyWant(bad as never, w.sig), false)
  // signing invalid values throws instead of producing an unverifiable artifact
  assert.throws(() => want({ wantId: 'has space' }))
  assert.throws(() => want({ taskHash: 'nothex' }))
  assert.throws(() => want({ revision: 0 }))
  // taskHash default commits to the request terms
  assert.equal(w.want.taskHash, wantTaskDigest({ domains: ['soccer'], question: 'who won', maxSats: 500 }))
  assert.equal(await ls.announceWant(w), true)
})

test('offer: want binding, quote without wrapped, revision + id rules', async () => {
  const ls = createLsPresence()
  const w = want()
  await ls.announceWant(w)
  const o = offer({ wantId: w.want.wantId, taskHash: w.want.taskHash, wantRevision: 1, buyerPub: w.want.pub, replyTo: w.want.pub })
  assert.ok(verifyOffer(o.offer, o.sig))
  assert.equal(await ls.announceOffer(o), true)
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
  ]) assert.equal(verifyOffer(mutate(), o.sig), false)
  // a quote carrying the reveal credential can never be signed or verified
  const leaky = { ...quote(), wrapped: 'aa'.repeat(32) }
  assert.throws(() => offer({ offer: leaky }), /quote|wrapped/i)
  assert.equal(verifyOffer({ ...o.offer, offer: leaky }, o.sig), false)
  // valid public quote signs and verifies; revision chain works
  const o2 = offer({ offerId: o.offer.offerId, revision: 2, previousHash: 'f'.repeat(64), offer: quote(100) })
  const o3 = offer({ offerId: o.offer.offerId, revision: 2, previousHash: offerArtifactHash(o), offer: quote(100) })
  assert.equal(await ls.announceOffer(o2), false)
  assert.equal(await ls.announceOffer(o3), true)
  assert.equal(ls.offersFor('soccer').length, 1)
})

test('wrong-network want/offer rejected identically off-chain and on-chain', async () => {
  const ls = createLsPresence(undefined, { net: 'ttn', strictNet: true })
  const bad = want({ net: 'main' })
  assert.equal(await ls.announceWant(bad), false)
  await ls.outputAdmittedByTopic({ mode: 'locking-script', txid: '0'.repeat(64), outputIndex: 0, lockingScript: wantOutputScript(bad), satoshis: 0, topic: 'tm_presence', beef: [] } as never)
  assert.equal(ls.wantsFor().length, 0)
  const badOffer = offer({ net: 'main' })
  assert.equal(await ls.announceOffer(badOffer), false)
  await ls.outputAdmittedByTopic({ mode: 'locking-script', txid: '0'.repeat(64), outputIndex: 0, lockingScript: offerOutputScript(badOffer), satoshis: 0, topic: 'tm_presence', beef: [] } as never)
  assert.equal(ls.offersFor().length, 0)
  // unbound artifacts are also refused in strict mode, both paths
  const unbound = want()
  assert.equal(await ls.announceWant(unbound), false)
  await ls.outputAdmittedByTopic({ mode: 'locking-script', txid: '0'.repeat(64), outputIndex: 0, lockingScript: wantOutputScript(unbound), satoshis: 0, topic: 'tm_presence', beef: [] } as never)
  assert.equal(ls.wantsFor().length, 0)
  const good = want({ net: 'ttn' })
  assert.equal(await ls.announceWant(good), true)
})

test('on-chain output round-trips every signed field', () => {
  const w = want({ question: 'q', maxSats: 9, net: 'ttn' })
  const parsed = parseWantOutput(wantOutputScript(w))!
  assert.deepEqual(parsed.want, w.want)
  assert.ok(verifyWant(parsed.want, parsed.sig))
})

test('SQLite restore: only new-codec-verifying artifacts come back', async () => {
  const kx = knexPkg({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const l1 = createLsPresence(kx)
  await l1.init()
  const w = want()
  const o = offer()
  await l1.announceWant(w)
  await l1.announceOffer(o)
  const l2 = createLsPresence(kx)
  await l2.init()
  assert.equal(l2.wantFor(pub, w.want.wantId)?.want.wantId, w.want.wantId)
  assert.equal(l2.offersFor('soccer').length, 1)
  // a row tampered in the table never resurfaces as a verified artifact
  await kx('nektar_ls_market_wants').update({ want: JSON.stringify({ ...w.want, maxSats: 999999 }) })
  const l3 = createLsPresence(kx)
  await l3.init()
  assert.equal(l3.wantsFor().length, 0)
  await kx.destroy()
})
