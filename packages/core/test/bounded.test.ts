import { test } from 'node:test'
import assert from 'node:assert/strict'
import { BoundedMap, BoundedSet } from '../src/index.js'

test('BoundedMap: oldest entry evicts past max; get refreshes recency', () => {
  const m = new BoundedMap<string, number>(3)
  m.set('a', 1).set('b', 2).set('c', 3)
  m.set('d', 4)
  assert.equal(m.has('a'), false, 'the oldest insert evicts')
  assert.equal(m.size, 3)
  m.get('b') // touch — 'c' becomes the oldest
  m.set('e', 5)
  assert.equal(m.has('c'), false, 'a refreshed key survives the next insert')
  assert.equal(m.get('b'), 2)
  // re-set of an existing key refreshes it too
  m.set('b', 20)
  m.set('f', 6)
  assert.equal(m.get('b'), 20)
  assert.equal(m.has('d'), false)
  m.delete('b')
  assert.equal(m.has('b'), false)
  m.clear()
  assert.equal(m.size, 0)
})

test('BoundedSet: membership-only variant evicts the oldest member past max', () => {
  const s = new BoundedSet<string>(2)
  s.add('x').add('y').add('z')
  assert.equal(s.has('x'), false)
  assert.equal(s.size, 2)
  s.delete('y')
  assert.equal(s.has('y'), false)
  s.add('y')
  assert.equal(s.has('y'), true)
  s.clear()
  assert.equal(s.size, 0)
})
