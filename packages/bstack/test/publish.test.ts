import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { signWant } from '../src/artifacts.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { MemoryTreeLedger } from '../src/tree.js'
import { publishArtifact } from '../src/publish.js'
import { ensureTrees } from '../src/trees.js'
import { encodePayload } from '../src/market.js'
import { provenTree, fund, prove } from './helpers.js'
import { FakeTracker, minihost } from '../src/testing.js'

const NET = 'regtest'
const wantPayload = (k: PrivateKey) => ({ v: 1 as const, kind: 'want' as const, body: signWant(k, { domains: ['compute'], question: 'q', net: NET, ts: Date.now(), exp: Date.now() + 60000 }) })

test('journal: round-trip', async () => {
  const j = new MemoryCarrierJournal()
  assert.equal(await j.get('x'), undefined)
  await j.put('x', { txid: 't', beef: [1, 2], topics: ['a'] })
  assert.deepEqual(await j.get('x'), { txid: 't', beef: [1, 2], topics: ['a'] })
})

test('publish: same payload twice burns one slot, same txid, both submits ok', async () => {
  const tr = new FakeTracker()
  const h = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const tree = await provenTree(k)
  await ledger.add(tree, 2); await ledger.markProven(tree.id('hex'))
  const j = new MemoryCarrierJournal()
  const p = { key: k, ledger, journal: j, hosts: [h.url], net: NET }
  const payload = wantPayload(k)
  const r1 = await publishArtifact({ ...p, payload })
  const r2 = await publishArtifact({ ...p, payload })
  assert.equal(r1.txid, r2.txid)
  assert.equal(await ledger.remaining(), 1, 'same payload -> one slot')
  await h.close()
})

test('publish: no slot throws', async () => {
  const h = await minihost([NET], new FakeTracker())
  const k = PrivateKey.fromRandom()
  await assert.rejects(publishArtifact({ key: k, ledger: new MemoryTreeLedger(), journal: new MemoryCarrierJournal(), hosts: [h.url], net: NET, payload: wantPayload(k) }), /no funding slot/)
  await h.close()
})

test('ensureTrees: mints once, no double-mint while unproven, proven then low-water mints again', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const broadcasted: import('@bsv/sdk').Transaction[] = []
  const bc = async (tx: import('@bsv/sdk').Transaction) => { broadcasted.push(tx); return { status: 'success' as const, txid: tx.id('hex') } }
  const fetchTx = async (txid: string) => broadcasted.find(t => t.id('hex') === txid)
  const p = { key: k, ledger, lowWater: 4, size: 4, satsPerOutput: 100, takeFunding: async (s: number) => fund(k, 1_000_000), broadcast: bc, fetchTx }
  const r1 = await ensureTrees(p)
  assert.equal(broadcasted.length, 1); assert.ok(r1.minted)
  const r2 = await ensureTrees(p)
  assert.equal(broadcasted.length, 1, 'no second mint while unproven')
  prove(broadcasted[0]!)
  const r3 = await ensureTrees(p)
  assert.deepEqual(r3.proven, [broadcasted[0]!.id('hex')])
  assert.equal(await ledger.remaining(), 4)
  for (let i = 0; i < 2; i++) await ledger.take()
  const r4 = await ensureTrees(p)
  assert.ok(r4.minted, 'below low-water with all proven -> mints another')
  assert.equal(broadcasted.length, 2)
  const r5 = await ensureTrees(p)
  assert.equal(r5.minted, undefined, 'second tree still unproven -> no third mint')
  assert.equal(broadcasted.length, 2)
})
