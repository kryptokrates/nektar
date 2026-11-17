// push-verify — POST /v1/lane feeds pushed carrier bytes through the exact
// verification the host-read path uses: openEnvelope (BRC-78 sealed boxes)
// and checkCarrier + decodePayload/verifyBody for artifacts (the latter two
// are pinned in carrier.test.ts / market.test.ts). These tests pin the
// envelope surface adversarially, plus per-item isolation: one malformed
// record in a lookup answer must not poison the whole batch (the
// `boxrec: range: content` incident — VerifiedCache.key ran before the
// per-item catch and a non-array beef aborted Promise.all).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { MemoryTreeLedger } from '../src/tree.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { sendBox, readBox, openEnvelope, boxFundingLock, DEFAULT_OFFICE } from '../src/box.js'
import { mintTree } from '../src/tree.js'
import { walletOrProto } from '../src/carrier.js'
import { readWants } from '../src/hosts.js'
import { publishArtifact } from '../src/publish.js'
import { signWant } from '../src/artifacts.js'
import { fund, prove } from './helpers.js'
import { minihost, FakeTracker } from '../src/testing.js'

const OFFICE = DEFAULT_OFFICE
const NET = 'regtest'

const boxLedger = async (key: PrivateKey, n = 8) => {
  const l = new MemoryTreeLedger()
  const t = await mintTree({ key, funding: fund(key), n, satsPerOutput: 1000, lock: await boxFundingLock(key) })
  prove(t)
  await l.add(t, n, 'bbox')
  await l.markProven(t.id('hex'), t.merklePath!)
  return l
}

const marketLedger = async (key: PrivateKey) => {
  const l = new MemoryTreeLedger()
  const t = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000 })
  prove(t)
  await l.add(t, 4)
  await l.markProven(t.id('hex'), t.merklePath!)
  return l
}

test('push verify surface: sealed envelope opens for the recipient only; wrong key, tampered beef and garbage all reject cleanly', async () => {
  const tr = new FakeTracker()
  const h = await minihost([], { tracker: tr, office: OFFICE })
  try {
    const sender = PrivateKey.fromRandom()
    const recip = PrivateKey.fromRandom()
    const other = PrivateKey.fromRandom()
    const ledger = await boxLedger(sender)
    const journal = new MemoryCarrierJournal()
    const body = { market: { type: 'accept', wantId: 'w-1' } }
    const r = await sendBox({ key: sender, ledger, journal, hosts: [h.url], to: recip.toPublicKey().toString(), body })
    const row = await journal.get(r.hash)
    const beef = row?.beef
    assert.ok(Array.isArray(beef), 'journal carries the carrier beef')
    const me = recip.toPublicKey().toString()
    const ok = await openEnvelope(beef!, walletOrProto(recip), OFFICE, me, tr)
    assert.ok(ok !== undefined && ok !== null, 'recipient opens the pushed envelope')
    assert.deepEqual(ok.body, body)
    // sealed for someone else → the wrong key/pub pair fails §4.7 checks
    assert.equal(await openEnvelope(beef!, walletOrProto(other), OFFICE, other.toPublicKey().toString(), tr), undefined)
    assert.equal(await openEnvelope(beef!, walletOrProto(other), OFFICE, me, tr), undefined)
    // tampered carrier → admit/content verification fails
    const flipped = [...beef!]
    flipped[flipped.length - 8] ^= 0xff
    assert.equal(await openEnvelope(flipped, walletOrProto(recip), OFFICE, me, tr), undefined)
    // garbage bytes — the ingest route must not throw
    assert.equal(await openEnvelope([0xde, 0xad, 0xbe, 0xef], walletOrProto(recip), OFFICE, me, tr), undefined)
  } finally {
    await h.close()
  }
})

test('poisoned box listing: malformed records beside a valid envelope are skipped, not fatal', async () => {
  const tr = new FakeTracker()
  const h = await minihost([], { tracker: tr, office: OFFICE, poisonOutputs: true })
  try {
    const sender = PrivateKey.fromRandom()
    const recip = PrivateKey.fromRandom()
    const ledger = await boxLedger(sender)
    const journal = new MemoryCarrierJournal()
    const body = { market: { type: 'delivered', wantId: 'w-9' } }
    await sendBox({ key: sender, ledger, journal, hosts: [h.url], to: recip.toPublicKey().toString(), body })
    const read = await readBox({ key: recip, hosts: [h.url], tracker: tr })
    assert.equal(read.items.length, 1, 'the valid envelope survives the poison rows')
    assert.deepEqual(read.items[0].body, body)
  } finally {
    await h.close()
  }
})

test('poisoned wants listing: malformed records beside a valid want are skipped, not fatal', async () => {
  const tr = new FakeTracker()
  const h = await minihost([NET], { tracker: tr, poisonOutputs: true })
  try {
    const k = PrivateKey.fromRandom()
    const ledger = await marketLedger(k)
    const journal = new MemoryCarrierJournal()
    const body = await signWant(k, { domains: ['compute'], question: 'q', net: NET, ts: Date.now(), exp: Date.now() + 60000 })
    await publishArtifact({ key: k, ledger, journal, hosts: [h.url], net: NET, payload: { v: 1, kind: 'want', body } })
    const read = await readWants([h.url], { net: NET, domain: 'compute' }, { tracker: tr, net: NET })
    assert.equal(read.items.length, 1, 'the valid want survives the poison rows')
  } finally {
    await h.close()
  }
})
