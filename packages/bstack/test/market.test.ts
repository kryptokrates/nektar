import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { signWant } from '@nektar/overlay/presence'
import { buildCarrier, checkCarrier } from '../src/carrier.js'
import { encodePayload, lanesFor, laneOf } from '../src/market.js'
import { createNektarModule } from '../src/module.js'
import { provenTree } from './helpers.js'
import { FakeTracker } from './minihost.js'

const NET = 'regtest'
const mod = (t: FakeTracker) => createNektarModule({ log: () => {}, chainTracker: t as never, engine: { submit: async () => ({}) } }, [NET])

async function wantCarrier (signer: PrivateKey, minter: PrivateKey, domains = ['compute'], net = NET, tree?: import('@bsv/sdk').Transaction) {
  const sw = signWant(signer, { domains, question: 'q', net, ts: Date.now() })
  const beef = (await buildCarrier({ key: minter, payload: encodePayload({ v: 1, kind: 'want', body: sw }), slot: { tree: tree ?? await provenTree(minter), vout: 0 } })).beef
  return beef
}

test('market: same signer+minter admits on the right lane', async () => {
  const k = PrivateKey.fromRandom(); const t = new FakeTracker(); const m = mod(t)
  const lane = laneOf(NET, 'compute')
  const r = await m.topics[lane]!.identifyAdmissibleOutputs(await wantCarrier(k, k), [])
  assert.deepEqual(r.outputsToAdmit, [0])
})

test('market: signed by A, minted by B -> refused', async () => {
  const a = PrivateKey.fromRandom(); const b = PrivateKey.fromRandom(); const m = mod(new FakeTracker())
  const r = await m.topics[laneOf(NET, 'compute')]!.identifyAdmissibleOutputs(await wantCarrier(a, b), [])
  assert.deepEqual(r.outputsToAdmit, [])
})

test('market: domain hashing elsewhere is refused by this lane; >3 domains refused', async () => {
  const k = PrivateKey.fromRandom(); const m = mod(new FakeTracker())
  const right = lanesFor(NET, ['compute', 'storage'])
  const wrong = 'tm_nektar_regtest_' + '0123456789abcdef'.split('').find(n => !right.includes(`tm_nektar_regtest_${n}`))!
  const beef = await wantCarrier(k, k, ['compute', 'storage'])
  assert.deepEqual((await m.topics[wrong]!.identifyAdmissibleOutputs(beef, [])).outputsToAdmit, [])
  assert.throws(() => lanesFor(NET, ['a', 'b', 'c', 'd']), /1\.\.3|domains/)
})

test('market: wrong net refused', async () => {
  const k = PrivateKey.fromRandom(); const m = mod(new FakeTracker())
  const r = await m.topics[laneOf(NET, 'compute')]!.identifyAdmissibleOutputs(await wantCarrier(k, k, ['compute'], 'other'), [])
  assert.deepEqual(r.outputsToAdmit, [])
})

test('market: second different carrier on same funding outpoint refused; same carrier ok', async () => {
  const k = PrivateKey.fromRandom(); const m = mod(new FakeTracker())
  const lane = laneOf(NET, 'compute'); const tm = m.topics[lane]!
  const tree = await provenTree(k)
  const b1 = await wantCarrier(k, k, ['compute'], NET, tree)
  const b2 = await wantCarrier(k, k, ['compute'], NET, tree)
  assert.deepEqual((await tm.identifyAdmissibleOutputs(b1, [])).outputsToAdmit, [0])
  assert.deepEqual((await tm.identifyAdmissibleOutputs(b1, [])).outputsToAdmit, [0], 'same carrier = dup')
  assert.deepEqual((await tm.identifyAdmissibleOutputs(b2, [])).outputsToAdmit, [], 'conflict refused')
})
