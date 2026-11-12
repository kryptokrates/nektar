import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { signWant } from '../src/artifacts.js'
import { buildCarrier, buildSweep } from '../src/carrier.js'
import { encodePayload, lanesFor } from '../src/market.js'
import { submitAll, readWants } from '../src/hosts.js'
import { provenTree } from './helpers.js'
import { FakeTracker, minihost } from '../src/testing.js'

const NET = 'regtest'

async function want(k: PrivateKey, domains = ['compute'], tree?: import('@bsv/sdk').Transaction, net = NET) {
  const sw = signWant(k, { domains, question: 'q', net, ts: Date.now(), exp: Date.now() + 60000 })
  const t = tree ?? (await provenTree(k))
  return { beef: (await buildCarrier({ key: k, payload: encodePayload({ v: 1, kind: 'want', body: sw }), slot: { tree: t, vout: 0 } })).beef, tree: t }
}

test('hosts: majority quorum still ok with one host down, missed names it', async () => {
  const tr = new FakeTracker()
  const h = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k)
  const r = await submitAll([h.url, 'http://127.0.0.1:1'], beef, lanesFor(NET, ['compute']), { timeoutMs: 2000 })
  assert.ok(r.ok)
  assert.deepEqual(r.took, [h.url])
  assert.equal(r.missed.length, 1)
  await h.close()
})

test('hosts: early resolve returns at quorum while a straggler still lands', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], tr)
  // a host that holds /submit open for 3s before answering — early resolve
  // must not wait for it, but the leg still completes and the object lands
  const { createServer } = await import('node:http')
  const slow = createServer((req, res) => {
    if (req.url === '/submit') {
      setTimeout(() => {
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end('{}')
      }, 3000)
      return
    }
    res.writeHead(404)
    res.end()
  })
  await new Promise<void>((r) => slow.listen(0, '127.0.0.1', r))
  const slowUrl = `http://127.0.0.1:${(slow.address() as { port: number }).port}`
  try {
    const k = PrivateKey.fromRandom()
    const { beef } = await want(k)
    const t0 = Date.now()
    const r = await submitAll([a.url, slowUrl], beef, lanesFor(NET, ['compute']), { retryWaits: [], early: true })
    const ms = Date.now() - t0
    assert.ok(r.ok)
    assert.ok(ms < 2500, `early resolve waited ${ms}ms for the straggler`)
    // the straggler leg completes in the background — no dangling work
    await new Promise((r2) => setTimeout(r2, 3300))
    // without early the same call is priced by the slow host
    const t1 = Date.now()
    const r2 = await submitAll([a.url, slowUrl], beef, lanesFor(NET, ['compute']), { retryWaits: [] })
    assert.ok(r2.ok)
    assert.ok(Date.now() - t1 >= 2900, 'non-early call waits for every host')
  } finally {
    slow.close()
    await a.close()
  }
})

test('hosts: readWants verifies and reports a host that withholds', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], tr)
  const b = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k)
  const sub = await submitAll([a.url], beef, lanesFor(NET, ['compute']))
  assert.ok(sub.ok)
  const r = await readWants([a.url, b.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
  assert.equal(r.items.length, 1)
  assert.deepEqual(r.perHost[a.url], [r.items[0]!.txid])
  assert.deepEqual(r.withheld, [{ host: b.url, txid: r.items[0]!.txid }])
  await a.close()
  await b.close()
})

test('hosts: garbage beef in an answer is dropped, not thrown', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], tr)
  const r = await readWants([a.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
  assert.equal(r.items.length, 0)
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k)
  beef[10] = beef[10]! ^ 0xff
  const sub = await submitAll([a.url], beef, lanesFor(NET, ['compute']))
  const r2 = await readWants([a.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
  assert.equal(r2.items.length, 0)
  await a.close()
})

test('hosts: retraction needs a MINED sweep � unmined or bad-proof spends do nothing', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const { beef, tree } = await want(k)
  assert.ok((await submitAll([a.url], beef, lanesFor(NET, ['compute']))).ok)
  const rd = () => readWants([a.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
  assert.equal((await rd()).items.length, 1)
  // unmined tx referencing the outpoint: no merklePath on the subject
  const sw1 = await buildSweep({ key: k, tree, vouts: [0], fees: { sats: 1, bytes: 1, floor: 1 } })
  await submitAll([a.url], sw1.toAtomicBEEF(), lanesFor(NET, ['compute']))
  assert.equal((await rd()).items.length, 1, 'unmined retraction ignored')
  // claimed-mined tx whose path the tracker rejects
  tr.strict = true
  const sw2 = await buildSweep({ key: k, tree, vouts: [0], fees: { sats: 1, bytes: 1, floor: 1 } })
  const { prove } = await import('./helpers.js')
  prove(sw2) // path exists but its root is not registered with the strict tracker
  await submitAll([a.url], sw2.toAtomicBEEF(), lanesFor(NET, ['compute']))
  tr.strict = false
  assert.equal((await rd()).items.length, 1, 'bad-proof retraction ignored')
  // genuine mined sweep: subject proven, wants's funding outpoint spent
  const sw3 = await buildSweep({ key: k, tree, vouts: [0], fees: { sats: 1, bytes: 1, floor: 1 } })
  prove(sw3)
  await submitAll([a.url], sw3.toAtomicBEEF(), lanesFor(NET, ['compute']))
  assert.equal((await rd()).items.length, 0, 'mined sweep frees the want')
  await a.close()
})

test('hosts: host that 200s with empty admittance is took, not admitted', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k)
  const good = await submitAll([a.url], beef, lanesFor(NET, ['compute']))
  assert.ok(good.admitted.includes(a.url))
  const dup = await submitAll([a.url], beef, lanesFor(NET, ['compute']))
  // same carrier again: tm claims 'dup' and still admits [0] � use a refused beef instead
  const bad = await submitAll([a.url], beef.slice().fill(0, 0, 4), lanesFor(NET, ['compute']))
  assert.ok(bad.took.includes(a.url))
  assert.ok(!bad.admitted.includes(a.url))
  assert.ok(dup.ok)
  await a.close()
})

test('hosts: evil host answering an off-query want is dropped by the reader', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], { tracker: tr, evil: true })
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k, ['storage'])
  assert.ok((await submitAll([a.url], beef, lanesFor(NET, ['storage']))).ok)
  const r = await readWants([a.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
  assert.equal(r.items.length, 0)
  await a.close()
})

test('hosts: unreachable host goes to unreachable, not withheld', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k)
  assert.ok((await submitAll([a.url], beef, lanesFor(NET, ['compute']))).ok)
  const r = await readWants([a.url, 'http://127.0.0.1:1'], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
  assert.equal(r.items.length, 1)
  assert.deepEqual(r.unreachable, ['http://127.0.0.1:1'])
  assert.equal(r.withheld.length, 0)
  await a.close()
})

test('hosts: multi-net � a want on net A is not returned for net B', async () => {
  const tr = new FakeTracker()
  const a = await minihost([NET, 'other'], tr)
  const k = PrivateKey.fromRandom()
  const { beef } = await want(k, ['compute'], undefined, NET)
  assert.ok((await submitAll([a.url], beef, lanesFor(NET, ['compute']))).ok)
  assert.equal((await readWants([a.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })).items.length, 1)
  const r = await readWants([a.url], { net: 'other', domain: 'compute' }, { tracker: tr, net: 'other' })
  assert.equal(r.items.length, 0)
  await a.close()
})
