import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { claimsRecord, decodeValue, encode as cborEncode, CborMap, type Output } from '@lightwebinc/bcommon'
import { signWant, signOffer } from '../src/artifacts.js'
import { buildCarrier } from '../src/carrier.js'
import { decodePayload, encodePayload, lanesFor, laneOf } from '../src/market-wire.js'
import { createNektarModule } from '../src/module.js'
import { provenTree } from './helpers.js'
import { FakeTracker } from '../src/testing.js'

const NET = 'regtest'
const mod = (t: FakeTracker) => createNektarModule({ log: () => {}, chainTracker: t, engine: { submit: async () => ({}) } }, [NET])

async function wantCarrier(signer: PrivateKey, minter: PrivateKey, domains = ['compute'], net = NET, tree?: import('@bsv/sdk').Transaction) {
  const sw = await signWant(signer, { domains, question: 'q', net, ts: Date.now(), exp: Date.now() + 60000 })
  const beef = (
    await buildCarrier({ key: minter, payload: encodePayload({ v: 1, kind: 'want', body: sw }), slot: { tree: tree ?? (await provenTree(minter)), vout: 0 } })
  ).beef
  return beef
}

test('market: same signer+minter admits on the right lane', async () => {
  const k = PrivateKey.fromRandom()
  const t = new FakeTracker()
  const m = mod(t)
  const lane = laneOf(NET, 'compute')
  const r = await m.topics[lane].identifyAdmissibleOutputs(await wantCarrier(k, k), [])
  assert.deepEqual(r.outputsToAdmit, [0])
})

test('market: signed by A, minted by B -> refused', async () => {
  const a = PrivateKey.fromRandom()
  const b = PrivateKey.fromRandom()
  const m = mod(new FakeTracker())
  const r = await m.topics[laneOf(NET, 'compute')].identifyAdmissibleOutputs(await wantCarrier(a, b), [])
  assert.deepEqual(r.outputsToAdmit, [])
})

test('market: domain hashing elsewhere is refused by this lane; >3 domains refused', async () => {
  const k = PrivateKey.fromRandom()
  const m = mod(new FakeTracker())
  const right = lanesFor(NET, ['compute', 'storage'])
  const wrong = 'tm_nektar_regtest_' + '0123456789abcdef'.split('').find((n) => !right.includes(`tm_nektar_regtest_${n}`))!
  const beef = await wantCarrier(k, k, ['compute', 'storage'])
  assert.deepEqual((await m.topics[wrong].identifyAdmissibleOutputs(beef, [])).outputsToAdmit, [])
  assert.throws(() => lanesFor(NET, ['a', 'b', 'c', 'd']), /1\.\.3|domains/)
})

test('market: wrong net refused', async () => {
  const k = PrivateKey.fromRandom()
  const m = mod(new FakeTracker())
  const r = await m.topics[laneOf(NET, 'compute')].identifyAdmissibleOutputs(await wantCarrier(k, k, ['compute'], 'other'), [])
  assert.deepEqual(r.outputsToAdmit, [])
})

test('market: second different carrier on same funding outpoint refused; same carrier ok', async () => {
  const k = PrivateKey.fromRandom()
  const m = mod(new FakeTracker())
  const lane = laneOf(NET, 'compute')
  const tm = m.topics[lane]
  const tree = await provenTree(k)
  const b1 = await wantCarrier(k, k, ['compute'], NET, tree)
  const b2 = await wantCarrier(k, k, ['compute'], NET, tree)
  assert.deepEqual((await tm.identifyAdmissibleOutputs(b1, [])).outputsToAdmit, [0])
  assert.deepEqual((await tm.identifyAdmissibleOutputs(b1, [])).outputsToAdmit, [0], 'same carrier = dup')
  assert.deepEqual((await tm.identifyAdmissibleOutputs(b2, [])).outputsToAdmit, [], 'conflict refused')
})

async function wantCarrierExp(signer: PrivateKey, minter: PrivateKey, exp: number | undefined, tree?: import('@bsv/sdk').Transaction) {
  const sw = await signWant(signer, { domains: ['compute'], question: 'q', net: NET, ts: Date.now(), ...(exp !== undefined ? { exp } : {}) })
  const beef = (
    await buildCarrier({ key: minter, payload: encodePayload({ v: 1, kind: 'want', body: sw }), slot: { tree: tree ?? (await provenTree(minter)), vout: 0 } })
  ).beef
  return { beef, sw }
}

test('market: expiry required — missing, past or >7d-ahead exp refused; +1h admitted', async () => {
  let clock = Date.now()
  const m = createNektarModule({ log: () => {}, chainTracker: new FakeTracker(), engine: { submit: async () => ({}) } }, [NET], { now: () => clock })
  const lane = laneOf(NET, 'compute')
  const tm = m.topics[lane]
  const k = PrivateKey.fromRandom()
  assert.deepEqual((await tm.identifyAdmissibleOutputs((await wantCarrierExp(k, k, undefined)).beef, [])).outputsToAdmit, [], 'no exp refused')
  // signWant refuses a past exp at sign time — sign ahead, then move the module clock past it
  const past = await wantCarrierExp(k, k, Date.now() + 60_000)
  clock = Date.now() + 120_000
  assert.deepEqual((await tm.identifyAdmissibleOutputs(past.beef, [])).outputsToAdmit, [], 'past exp refused')
  clock = Date.now()
  assert.deepEqual((await tm.identifyAdmissibleOutputs((await wantCarrierExp(k, k, clock + 8 * 86_400_000)).beef, [])).outputsToAdmit, [], '>7d ahead refused')
  assert.deepEqual((await tm.identifyAdmissibleOutputs((await wantCarrierExp(k, k, clock + 3_600_000)).beef, [])).outputsToAdmit, [0], '+1h admitted')
})

test('market: expired artifacts leave the index — lookup returns nothing and the outpoint frees', async () => {
  let clock = Date.now()
  const t = new FakeTracker()
  const m = createNektarModule({ log: () => {}, chainTracker: t, engine: { submit: async () => ({}) } }, [NET], { now: () => clock })
  const lane = laneOf(NET, 'compute')
  const tm = m.topics[lane]
  const k = PrivateKey.fromRandom()
  const tree = await provenTree(k)
  const { beef } = await wantCarrierExp(k, k, clock + 3_600_000, tree)
  assert.deepEqual((await tm.identifyAdmissibleOutputs(beef, [])).outputsToAdmit, [0])
  await m.lookups['ls_nektar'].outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: beef, outputIndex: 0, topic: lane })
  const wants = (await m.lookups['ls_nektar'].lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })) as Array<{ txid: string }>
  assert.equal(wants.length, 1, 'admitted want listed')
  clock += 3_600_001 // now past exp
  const gone = (await m.lookups['ls_nektar'].lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })) as Array<{ txid: string }>
  assert.equal(gone.length, 0, 'expired want no longer returned')
  // the freed outpoint admits a different carrier — proof the entry was dropped
  const second = await wantCarrierExp(k, k, clock + 3_600_000, tree)
  assert.deepEqual((await tm.identifyAdmissibleOutputs(second.beef, [])).outputsToAdmit, [0], 'expired entry released its outpoint')
})

// ── restore — boot rehydration of LaneIndex from persisted engine rows ──
const engineRow = (beef: number[], topic: string, spent = false): Output => {
  const tx = Transaction.fromAtomicBEEF(beef)
  return {
    txid: tx.id('hex'),
    outputIndex: 0,
    outputScript: tx.outputs[0].lockingScript.toBinary(),
    satoshis: tx.outputs[0].satoshis ?? 1,
    topic,
    spent,
    outputsConsumed: tx.inputs.map((i) => ({ txid: i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? '', outputIndex: i.sourceOutputIndex })),
    consumedBy: [],
    beef
  }
}

test('restore: rehydrated rows serve lookups and hold their outpoint claim', async () => {
  const k = PrivateKey.fromRandom()
  const t = new FakeTracker()
  const m = mod(t)
  const lane = laneOf(NET, 'compute')
  const tree = await provenTree(k)
  const beef = await wantCarrier(k, k, ['compute'], NET, tree)
  const svc = m.lookups['ls_nektar']
  // no admission ever ran — the index rebuild comes only from engine rows
  const n = await svc.restore!([engineRow(beef, lane)], { findOutput: async () => null })
  assert.equal(n, 1)
  const wants = (await svc.lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })) as Array<{ txid: string }>
  assert.equal(wants.length, 1, 'restored want is served')
  // the claim survives: a *different* carrier on the same funding outpoint
  // is refused exactly as if the original admission had happened live
  const other = await wantCarrier(k, k, ['compute'], NET, tree)
  assert.deepEqual((await m.topics[lane].identifyAdmissibleOutputs(other, [])).outputsToAdmit, [])
  // and the restored claim dedupes the same carrier
  assert.deepEqual((await m.topics[lane].identifyAdmissibleOutputs(beef, [])).outputsToAdmit, [0])
  // a second restore is a no-op, not a duplicate
  assert.equal(await svc.restore!([engineRow(beef, lane)], { findOutput: async () => null }), 1)
  assert.equal((await svc.lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })).length, 1)
})

test('restore: skips spent rows, dead payloads, foreign topics and expired artifacts', async () => {
  let clock = Date.now()
  const k = PrivateKey.fromRandom()
  const m = createNektarModule({ log: () => {}, chainTracker: new FakeTracker(), engine: { submit: async () => ({}) } }, [NET], { now: () => clock })
  const lane = laneOf(NET, 'compute')
  const svc = m.lookups['ls_nektar']
  const beef = await wantCarrierExp(k, k, clock + 60_000)
  const deadBeef = engineRow(beef.beef, lane)
  deadBeef.beef = [9, 9, 9]
  const rows: Output[] = [
    { ...engineRow(beef.beef, lane), spent: true },
    { ...engineRow(beef.beef, 'tm_unrelated'), beef: undefined },
    deadBeef,
    { ...engineRow(beef.beef, lane), outputScript: [0, 81, 0], beef: undefined }
  ]
  assert.equal(await svc.restore!(rows, { findOutput: async () => null }), 0)
  assert.equal((await svc.lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })).length, 0)
  // restored while valid, then the artifact's own exp passes -> dropped
  assert.equal(await svc.restore!([engineRow(beef.beef, lane)], { findOutput: async () => null }), 1)
  clock += 120_000
  assert.equal((await svc.lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })).length, 0, 'expired restored entry evicts on read')
})

test('restore: script-only row rebuilds from the locking script with a fresh proof deadline', async () => {
  const k = PrivateKey.fromRandom()
  const t = new FakeTracker()
  const m = mod(t)
  const lane = laneOf(NET, 'compute')
  const tree = await provenTree(k)
  const beef = await wantCarrier(k, k, ['compute'], NET, tree)
  const row = engineRow(beef, lane)
  row.beef = undefined
  const svc = m.lookups['ls_nektar']
  assert.equal(await svc.restore!([row], { findOutput: async () => null }), 1)
  assert.equal((await svc.lookup({ service: 'ls_nektar', query: { wants: { net: NET, domain: 'compute' } } })).length, 1)
  assert.deepEqual((await m.topics[lane].identifyAdmissibleOutputs(await wantCarrier(k, k, ['compute'], NET, tree), [])).outputsToAdmit, [])
})

// ── wire format — v2 canonical record, dual decode ─────────────────────
const NKT1 = new TextEncoder().encode('nkt1')
const aWant = () => {
  const k = PrivateKey.fromRandom()
  return signWant(k, { domains: ['compute'], question: 'q', net: NET, ts: Date.now(), exp: Date.now() + 60000 })
}

test('market wire: v2 emits a claimsRecord nkt1 record {magic,kind,body}', async () => {
  const sw = await aWant()
  const b = encodePayload({ v: 1, kind: 'want', body: sw })
  assert.ok(claimsRecord(b, NKT1), 'payload claims the nkt1 record')
  const m = decodeValue(b)
  assert.ok(m instanceof CborMap)
  assert.equal(m.get(1n), 1n, 'kind 1 = want')
  const bodyText = m.get(2n)
  assert.equal(typeof bodyText, 'string')
  assert.deepEqual(JSON.parse(bodyText as string), sw, 'body is the untouched signed artifact')
})

test('market wire: v2 and v1 encode the same artifact and both decode', async () => {
  const sw = await aWant()
  const p = { v: 1 as const, kind: 'want' as const, body: sw }
  const v2 = decodePayload(encodePayload(p))
  const v1 = decodePayload(encodePayload(p, { legacy: true }))
  assert.deepEqual(v2, p)
  assert.deepEqual(v1, p)
})

test('market wire: offer kind round-trips as kind 2', async () => {
  const k = PrivateKey.fromRandom()
  const so = await signOffer(k, { shard: 's', seq: 0, priceSats: 10, domains: ['compute'], net: NET, ts: Date.now() })
  const b = encodePayload({ v: 1, kind: 'offer', body: so })
  assert.equal((decodeValue(b) as CborMap).get(1n), 2n)
  assert.deepEqual(decodePayload(b), { v: 1, kind: 'offer', body: so })
})

test('market wire: refuse wrong magic, truncated, non-canonical, missing keys', async () => {
  const sw = await aWant()
  const b = encodePayload({ v: 1, kind: 'want', body: sw })
  assert.throws(() => decodePayload(b.subarray(0, b.length - 5)), /market payload|bad market payload/)
  const wrongMagic = new Uint8Array(b)
  wrongMagic[wrongMagic.indexOf(0x6e)] = 0x78 // 'n' -> 'x' inside 'nkt1'
  assert.throws(() => decodePayload(wrongMagic), /magic|bad market payload/)
  assert.throws(() => decodePayload(new TextEncoder().encode('{"v":2,"kind":"want","body":{}}')), /bad market payload/)
  assert.throws(() => decodePayload(new TextEncoder().encode('{"v":1,"kind":"want"}')), /bad market payload/)
})

test('market wire: extra keys above 2 are preserved-but-ignored on decode', async () => {
  const sw = await aWant()
  const b = encodePayload({ v: 1, kind: 'want', body: sw })
  const m = decodeValue(b) as CborMap
  m.entries.push({ key: 9n, val: 'future-field' })
  const d = decodePayload(cborEncode(m))
  assert.equal(d.kind, 'want')
})
