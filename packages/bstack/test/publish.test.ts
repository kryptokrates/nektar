import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { signWant } from '../src/artifacts.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { MemoryTreeLedger } from '../src/tree.js'
import { publishArtifact, republishProved, artifactHash } from '../src/publish.js'
import { checkCarrier } from '../src/carrier.js'
import { ensureTrees } from '../src/trees.js'
import { provenTree, anchoredTree, fund, prove } from './helpers.js'
import { FakeTracker, minihost } from '../src/testing.js'

const NET = 'regtest'
const wantPayload = async (k: PrivateKey) => ({
  v: 1 as const,
  kind: 'want' as const,
  body: await signWant(k, { domains: ['compute'], question: 'q', net: NET, ts: Date.now(), exp: Date.now() + 60000 })
})

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
  await ledger.add(tree, 2)
  await ledger.markProven(tree.id('hex'))
  const j = new MemoryCarrierJournal()
  const p = { key: k, ledger, journal: j, hosts: [h.url], net: NET }
  const payload = await wantPayload(k)
  const r1 = await publishArtifact({ ...p, payload })
  const r2 = await publishArtifact({ ...p, payload })
  assert.equal(r1.txid, r2.txid)
  assert.equal(await ledger.remaining(), 1, 'same payload -> one slot')
  await h.close()
})

test('publish: no slot throws', async () => {
  const h = await minihost([NET], new FakeTracker())
  const k = PrivateKey.fromRandom()
  await assert.rejects(
    wantPayload(k).then((payload) =>
      publishArtifact({ key: k, ledger: new MemoryTreeLedger(), journal: new MemoryCarrierJournal(), hosts: [h.url], net: NET, payload })
    ),
    /no funding slot/
  )
  await h.close()
})

test('ensureTrees: mints once, no double-mint while unproven, proven then low-water mints again', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const broadcasted: import('@bsv/sdk').Transaction[] = []
  const bc = async (tx: import('@bsv/sdk').Transaction) => {
    broadcasted.push(tx)
    return { status: 'success' as const, txid: tx.id('hex') }
  }
  const fetchTx = async (txid: string) => broadcasted.find((t) => t.id('hex') === txid)
  const p = { key: k, ledger, lowWater: 4, size: 4, satsPerOutput: 100, takeFunding: async (s: number) => fund(k, 1_000_000), broadcast: bc, fetchTx }
  const r1 = await ensureTrees(p)
  assert.equal(broadcasted.length, 1)
  assert.ok(r1.minted)
  await ensureTrees(p)
  assert.equal(broadcasted.length, 1, 'no second mint while unproven')
  prove(broadcasted[0])
  const r3 = await ensureTrees(p)
  assert.deepEqual(r3.proven, [broadcasted[0].id('hex')])
  assert.equal(await ledger.remaining(), 4)
  for (let i = 0; i < 2; i++) await ledger.take()
  const r4 = await ensureTrees(p)
  assert.ok(r4.minted, 'below low-water with all proven -> mints another')
  assert.equal(broadcasted.length, 2)
  const r5 = await ensureTrees(p)
  assert.equal(r5.minted, undefined, 'second tree still unproven -> no third mint')
  assert.equal(broadcasted.length, 2)
})

test('ensureTrees: stuck mint — keep while healing, drop on definitive verdict, gate re-opens', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const broadcasted: Transaction[] = []
  let status = 'unknown'
  let verdict: 'keep' | 'drop' = 'keep'
  let stuckCalls = 0
  const rejected: string[] = []
  const bc = async (tx: Transaction) => {
    broadcasted.push(tx)
    return { status: 'success' as const, txid: tx.id('hex') }
  }
  const fetchTx = async (txid: string) => broadcasted.find((t) => t.id('hex') === txid)
  const p = {
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    takeFunding: async () => fund(k, 1_000_000),
    broadcast: bc,
    fetchTx,
    txStatus: async () => status,
    onReject: (t: Transaction) => {
      rejected.push(t.id('hex'))
    },
    onStuck: async () => {
      stuckCalls++
      return verdict
    }
  }
  await ensureTrees(p)
  assert.equal(broadcasted.length, 1)
  // observer can't see the mint: onStuck fires, 'keep' leaves the row
  await ensureTrees(p)
  assert.equal(stuckCalls, 1)
  assert.equal((await ledger.unproven()).length, 1)
  // 'seen' is a live answer — healing never engages while it propagates
  status = 'seen'
  await ensureTrees(p)
  assert.equal(stuckCalls, 1)
  // 'unknown' + definitive drop: row reaped, ghost cleanup ran, gate re-opens
  status = 'unknown'
  verdict = 'drop'
  const r = await ensureTrees(p)
  assert.deepEqual(rejected, [broadcasted[0].id('hex')])
  assert.ok(r.minted, 'mint gate re-opens the same pass')
  assert.equal((await ledger.unproven()).length, 1, 'the replacement mint is now the unproven row')
  // a chain rejection never reaches onStuck — dropped outright, re-minted
  status = 'rejected'
  await ensureTrees(p)
  assert.equal(stuckCalls, 2)
  assert.deepEqual(rejected, [broadcasted[0].id('hex'), broadcasted[1].id('hex')])
  assert.equal((await ledger.unproven()).length, 1)
})

test('journal: fundedBy returns rows for a funding txid; put upserts beef', async () => {
  const j = new MemoryCarrierJournal()
  await j.put('a', { txid: 'c1', beef: [1], topics: ['t'], fundingTxid: 'tree1' })
  await j.put('b', { txid: 'c2', beef: [2], topics: ['t'], fundingTxid: 'tree1' })
  await j.put('c', { txid: 'c3', beef: [3], topics: ['t'], fundingTxid: 'tree2' })
  await j.put('d', { txid: 'c4', beef: [4], topics: ['t'] })
  assert.deepEqual((await j.fundedBy!('tree1')).map((r) => r.hash).sort(), ['a', 'b'])
  assert.deepEqual((await j.fundedBy!('nope')).length, 0)
  await j.put('a', { txid: 'c1', beef: [9], topics: ['t'], fundingTxid: 'tree1' })
  assert.deepEqual((await j.get('a'))!.beef, [9])
})

test('republishProved: journal beef gains the funding parent BUMP under the same txid', async () => {
  const tr = new FakeTracker()
  const h = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  // unproven mint on anchored ancestry — the zero-conf slot the market lane serves
  const tree = await anchoredTree(k, 2)
  await ledger.add(tree, 2)
  const j = new MemoryCarrierJournal()
  const payload = await wantPayload(k)
  const r1 = await publishArtifact({ key: k, ledger, journal: j, hosts: [h.url], net: NET, payload })
  const hash = artifactHash(payload)
  const held = (await j.get(hash))!
  const t1 = Transaction.fromAtomicBEEF(held.beef)
  assert.equal(t1.inputs[0].sourceTransaction!.merklePath, undefined, 'funding parent unproven at publish')
  // the tree's own BUMP lands → republish enriches the carrier, same txid
  prove(tree)
  const rp = await republishProved({ journal: j, hosts: [h.url], treeTxid: tree.id('hex'), merklePath: tree.merklePath! })
  assert.equal(rp.republished, 1)
  const held2 = (await j.get(hash))!
  const t2 = Transaction.fromAtomicBEEF(held2.beef)
  assert.equal(t2.id('hex'), r1.txid)
  assert.notEqual(t2.inputs[0].sourceTransaction!.merklePath, undefined, 'parent now carries the BUMP')
  // the host's stored copy is the enriched beef — admitted free of the
  // unproven-funding eviction deadline
  const hostBeef = h.raw().find((b) => Transaction.fromAtomicBEEF(b).id('hex') === r1.txid)!
  const c = await checkCarrier(hostBeef, { tracker: tr })
  assert.ok(c.ok && c.unprovenFunding === false)
  // a second pass finds the enriched journal row and skips it
  const rp2 = await republishProved({ journal: j, hosts: [h.url], treeTxid: tree.id('hex'), merklePath: tree.merklePath! })
  assert.equal(rp2.republished, 0)
  await h.close()
})
