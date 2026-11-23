import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, UnlockingScript, type MerklePath } from '@bsv/sdk'
import { zeroconfVerdict, unconfirmedAncestors } from '../src/market/zeroconf.js'

// zeroconf — the seller's delivery policy for an unproven payment, and the
// bounded ancestry walk that feeds it. Pure functions: txs are built in
// memory, the resolver is a map lookup.

const key = PrivateKey.fromRandom()
const lock = () => ({ satoshis: 1000, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })

const OPTS = { maxZeroconfSats: 1000, maxUnconfirmedAncestors: 2 }

/** a tx spending `parents` (or a bare coinbase-like input when none given);
 *  `proven` carries a marker merklePath — the policy only asks `!== undefined`
 *  (a parsed MerklePath needs a real block; the object stands in for it). */
// every input gets a distinct dummy unlocking script so two structurally
// identical spends still mint distinct txids (id() serializes the script)
let seq = 0
const spend = () => ({ sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51' + (seq++).toString(16).padStart(2, '0')) })
const tx = (parents: Transaction[] = [], proven = false): Transaction => {
  const t = new Transaction()
  for (const p of parents) t.addInput({ sourceTransaction: p, ...spend() })
  if (parents.length === 0) t.addInput({ sourceTXID: '00'.repeat(32), ...spend() })
  t.addOutput(lock())
  if (proven) t.merklePath = { blockHeight: 1 } as MerklePath
  return t
}

void test('zeroconfVerdict: proven wins regardless of price or ancestry', () => {
  assert.equal(zeroconfVerdict({ proven: true, priceSats: 1_000_000, unconfirmedAncestors: 99, opts: OPTS }), 'proven')
  assert.equal(zeroconfVerdict({ proven: true, priceSats: 5, unconfirmedAncestors: 0, opts: { ...OPTS, maxZeroconfSats: 0 } }), 'proven')
})

void test('zeroconfVerdict: at the cap is zeroconf; one sat over awaits proof', () => {
  assert.equal(zeroconfVerdict({ proven: false, priceSats: 1000, unconfirmedAncestors: 0, opts: OPTS }), 'zeroconf')
  assert.equal(zeroconfVerdict({ proven: false, priceSats: 1001, unconfirmedAncestors: 0, opts: OPTS }), 'await-proof')
})

void test('zeroconfVerdict: ancestors at the bound pass, over it waits', () => {
  assert.equal(zeroconfVerdict({ proven: false, priceSats: 500, unconfirmedAncestors: 2, opts: OPTS }), 'zeroconf')
  assert.equal(zeroconfVerdict({ proven: false, priceSats: 500, unconfirmedAncestors: 3, opts: OPTS }), 'await-proof')
})

void test('zeroconfVerdict: cap 0 means every unproven payment waits', () => {
  assert.equal(
    zeroconfVerdict({ proven: false, priceSats: 1, unconfirmedAncestors: 0, opts: { maxZeroconfSats: 0, maxUnconfirmedAncestors: 64 } }),
    'await-proof'
  )
})

void test('unconfirmedAncestors: a proven parent counts nothing behind it', async () => {
  const p = tx([], true)
  const t = tx([p])
  assert.equal(await unconfirmedAncestors(t, () => Promise.resolve(undefined), 8), 0)
})

void test('unconfirmedAncestors: a chain of three unproven parents counts three', async () => {
  const p3 = tx([], false)
  const p2 = tx([p3])
  const p1 = tx([p2])
  const t = tx([p1])
  assert.equal(await unconfirmedAncestors(t, () => Promise.resolve(undefined), 8), 3)
})

void test('unconfirmedAncestors: an unresolvable parent counts as one and ends the branch', async () => {
  const deep = tx([], false) // behind the missing parent — must never be reached
  const missing = new Transaction()
  missing.addInput({ sourceTransaction: deep, ...spend() })
  missing.addOutput(lock())
  const t = new Transaction()
  t.addInput({ sourceTXID: missing.id('hex'), ...spend() })
  t.addOutput(lock())
  // the resolver knows nothing — the missing parent counts once, its own
  // ancestry is not walked
  assert.equal(await unconfirmedAncestors(t, () => Promise.resolve(undefined), 8), 1)
})

void test('unconfirmedAncestors: a diamond ancestry counts the shared ancestor once', async () => {
  const shared = tx([], false)
  const p1 = tx([shared])
  const p2 = tx([shared])
  const t = tx([p1, p2])
  // p1, p2, shared = 3 distinct — the diamond root is visited once
  assert.equal(await unconfirmedAncestors(t, () => Promise.resolve(undefined), 8), 3)
})

void test('unconfirmedAncestors: the walk stops at cap + 1', async () => {
  const p3 = tx([], false)
  const p2 = tx([p3])
  const p1 = tx([p2])
  const t = tx([p1])
  assert.equal(await unconfirmedAncestors(t, () => Promise.resolve(undefined), 2), 3, 'cap 2 → returns 3 without walking further')
  // and the resolver was never asked — carried ancestry only
  let asked = 0
  await unconfirmedAncestors(t, () => Promise.resolve(void asked++), 2)
  assert.equal(asked, 0)
})
