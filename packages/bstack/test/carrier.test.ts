import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { buildCarrier, checkCarrier, buildSweep } from '../src/carrier.js'
import { FUNDING_TAG } from '../src/tree.js'
import { provenTree } from './helpers.js'
import { FakeTracker } from './minihost.js'

const pay = new TextEncoder().encode('hello carrier')

test('carrier: build + check round-trips payload and funding outpoint', async () => {
  const key = PrivateKey.fromRandom()
  const tree = await provenTree(key)
  const { beef, carrier } = await buildCarrier({ key, payload: pay, slot: { tree, vout: 0 } })
  const c = await checkCarrier(beef, { tracker: new FakeTracker() })
  assert.ok(c.ok, JSON.stringify(c))
  assert.deepEqual([...c.payload], [...pay])
  assert.equal(c.fundingOutpoint, `${tree.id('hex')}:0`)
  assert.equal(c.carrier.id('hex'), carrier.id('hex'))
})

test('carrier: flipped payload byte fails', async () => {
  const key = PrivateKey.fromRandom()
  const tree = await provenTree(key)
  const { beef } = await buildCarrier({ key, payload: pay, slot: { tree, vout: 0 } })
  beef[beef.length - 5] = beef[beef.length - 5]! ^ 0xff
  assert.equal((await checkCarrier(beef, { tracker: new FakeTracker() })).ok, false)
})

test('carrier: tree proof failing the tracker fails', async () => {
  const key = PrivateKey.fromRandom()
  const tree = await provenTree(key)
  const { beef } = await buildCarrier({ key, payload: pay, slot: { tree, vout: 0 } })
  const bad = new FakeTracker(); bad.ok = false
  const c = await checkCarrier(beef, { tracker: bad })
  assert.equal(c.ok, false); assert.equal(c.reason, 'spv')
})

test('carrier: unproven tree cannot build (beef would not be atomic-verifiable)', async () => {
  const key = PrivateKey.fromRandom()
  const t = await provenTree(key)
  t.merklePath = undefined
  await assert.rejects(buildCarrier({ key, payload: pay, slot: { tree: t, vout: 0 } }), /not proven/)
})

test('sweep: builds a self-paying sweep of tree outputs', async () => {
  const key = PrivateKey.fromRandom()
  const tree = await provenTree(key)
  const s = await buildSweep({ key, tree, vouts: [1, 2], fees: { sats: 1, bytes: 1, floor: 1 } })
  assert.equal(s.inputs.length, 2)
  assert.ok(s.outputs[0]!.satoshis! > 0)
})
