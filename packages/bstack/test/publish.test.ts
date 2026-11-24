import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, ProtoWallet, Transaction, UnlockingScript } from '@bsv/sdk'
import knex from 'knex'
import { signWant } from '../src/artifacts.js'
import { fundingLock } from '@lightwebinc/bcommon'
import { DERIVATION, FUNDING_TAG, ORIGINATOR, KnexTreeLedger } from '../src/tree.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { MemoryTreeLedger } from '../src/tree.js'
import { publishArtifact, republishProved, republishOne, resubmitCarrier, artifactHash } from '../src/publish.js'
import { checkCarrier } from '../src/carrier.js'
import { ensureTrees } from '../src/trees.js'
import { FundsStarvedError } from '@nektar/chain'
import { provenTree, anchoredTree, fund, prove } from './helpers.js'
import { FakeTracker, minihost, faulthost } from '../src/testing.js'

const NET = 'regtest'
const wantPayload = async (k: PrivateKey) => ({
  v: 1 as const,
  kind: 'want' as const,
  body: await signWant(k, { domains: ['compute'], question: 'q', net: NET, ts: Date.now(), exp: Date.now() + 60000 })
})

/** legacy status word → TxFacts: keeps the old fixtures readable — 'rejected'
 *  is an outside-word hint (verdict stays pending); 'dead' models a
 *  first-party verdict (abandon horizon / ledger terminal row) */
const factsFor = (status: string): import('@nektar/chain').TxFacts =>
  status === 'dead'
    ? { verdict: 'dead', source: 'first-party' }
    : status === 'abandoned'
      ? { verdict: 'dead', source: 'abandon' }
      : { verdict: 'pending', hint: { status: status === 'rejected' ? 'REJECTED' : status === 'seen' ? 'SEEN_ON_NETWORK' : '', answered: true } }

test('journal: round-trip', async () => {
  const j = new MemoryCarrierJournal()
  assert.equal(await j.get('x'), undefined)
  void j.put('x', { txid: 't', beef: [1, 2], topics: ['a'] })
  assert.deepEqual(await j.get('x'), { txid: 't', beef: [1, 2], topics: ['a'] })
})

test('publish: same payload twice burns one slot, same txid, both submits ok', async () => {
  const tr = new FakeTracker()
  const h = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const tree = await provenTree(k)
  await ledger.add(tree, 2)
  await ledger.markProven(tree.id('hex'), tree.merklePath!)
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
  const bc = (tx: import('@bsv/sdk').Transaction) => {
    broadcasted.push(tx)
    return Promise.resolve({ status: 'success' as const, txid: tx.id('hex') })
  }
  const fetchTx = (txid: string) => Promise.resolve(broadcasted.find((t) => t.id('hex') === txid))
  const p = {
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    satsPerOutput: 100,
    takeFunding: (_s: number) => Promise.resolve(fund(k, 1_000_000)),
    broadcast: bc,
    fetchTx
  }
  const r1 = await ensureTrees(p)
  assert.equal(broadcasted.length, 1)
  assert.equal(r1.minted.length, 1)
  await ensureTrees(p)
  assert.equal(broadcasted.length, 1, 'no second mint while unproven')
  prove(broadcasted[0])
  const r3 = await ensureTrees(p)
  assert.deepEqual(r3.proven, [broadcasted[0].id('hex')])
  assert.equal(await ledger.remaining(), 4)
  for (let i = 0; i < 2; i++) await ledger.take()
  const r4 = await ensureTrees(p)
  assert.equal(r4.minted.length, 1, 'below low-water with all proven -> mints another')
  assert.equal(broadcasted.length, 2)
  const r5 = await ensureTrees(p)
  assert.equal(r5.minted.length, 0, 'second tree still unproven -> no third mint')
  assert.equal(broadcasted.length, 2)
})

// a burst that drains a tree needs refill depth in the same pass, not one
// mint per upkeep tick — proven-only lanes (bbox/log/finger codecs demand
// the tree's own BUMP) have no anchored stock to dip into, so the fill loop
// mints back-to-back until lowWater or the maxUnproven in-flight bound
test('ensureTrees: fills to the in-flight bound in one pass, then parks until proven', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const broadcasted: Transaction[] = []
  const bc = (tx: Transaction) => {
    broadcasted.push(tx)
    return Promise.resolve({ status: 'success' as const, txid: tx.id('hex') })
  }
  const fetchTx = (txid: string) => Promise.resolve(broadcasted.find((t) => t.id('hex') === txid))
  // distinct funding per mint — a deterministic fake parent would mint the
  // same txid twice and the ledger's idempotent add would collapse them
  let mints = 0
  const p = {
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    satsPerOutput: 100,
    maxUnproven: 2,
    takeFunding: (_s: number) => Promise.resolve(fund(k, 1_000_000 + 1000 * mints++)),
    broadcast: bc,
    fetchTx
  }
  const r1 = await ensureTrees(p)
  assert.equal(r1.minted.length, 2, 'empty stock + headroom -> back-to-back mints to maxUnproven, not one per pass')
  assert.equal(broadcasted.length, 2)
  const r2 = await ensureTrees(p)
  assert.equal(r2.minted.length, 0, 'in-flight bound reached -> parks')
  for (const tx of broadcasted) prove(tx)
  const r3 = await ensureTrees(p)
  assert.equal(r3.proven.length, 2)
  assert.equal(await ledger.remaining(), 8)
  assert.equal(r3.minted.length, 0, 'stock above low-water -> no mint')
})

test('ensureTrees: reserveSats sizes the take — a small treasury mints below the default floor', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const takes: number[] = []
  const mk = (reserveSats?: number) => ({
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    satsPerOutput: 1,
    ...(reserveSats !== undefined ? { reserveSats } : {}),
    takeFunding: (s: number) => {
      takes.push(s)
      return Promise.resolve(fund(k, 1_000_000))
    },
    broadcast: (tx: Transaction) => Promise.resolve({ status: 'success' as const, txid: tx.id('hex') }),
    fetchTx: () => Promise.resolve(undefined)
  })
  await ensureTrees(mk())
  assert.deepEqual(takes, [2004], 'default: outputs + the 2000-sat change reserve')
  takes.length = 0
  // the first tree is unproven — a second ledger for the small-treasury mint
  const ledger2 = new MemoryTreeLedger()
  await ensureTrees({ ...mk(200), ledger: ledger2 })
  assert.deepEqual(takes, [204], 'reserveSats lowers the take floor — a 2000-sat utxo covers it with change to spare')
})

test('ensureTrees: a starved take names the knob in its error', async () => {
  const k = PrivateKey.fromRandom()
  const p = {
    key: k,
    ledger: new MemoryTreeLedger(),
    lowWater: 4,
    size: 4,
    reserveSats: 200,
    takeFunding: () => Promise.reject(new FundsStarvedError('funds: no spendable output ≥ 204 sats')),
    broadcast: () => Promise.reject(new Error('unreachable')),
    fetchTx: () => Promise.resolve(undefined)
  }
  await assert.rejects(() => ensureTrees(p), /NEKTAR_TREE_RESERVE_SATS/)
})

test('ensureTrees: stuck mint — keep while healing, drop on definitive verdict, gate re-opens', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const broadcasted: Transaction[] = []
  let status = 'unknown'
  let verdict: 'keep' | 'drop' = 'keep'
  let stuckCalls = 0
  const rejected: string[] = []
  const bc = (tx: Transaction) => {
    broadcasted.push(tx)
    return Promise.resolve({ status: 'success' as const, txid: tx.id('hex') })
  }
  const fetchTx = (txid: string) => Promise.resolve(broadcasted.find((t) => t.id('hex') === txid))
  const p = {
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
    broadcast: bc,
    fetchTx,
    facts: () => Promise.resolve(factsFor(status)),
    onReject: (t: Transaction) => {
      rejected.push(t.id('hex'))
    },
    onStuck: () => {
      stuckCalls++
      return Promise.resolve(verdict)
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
  assert.equal(r.minted.length, 1, 'mint gate re-opens the same pass')
  assert.equal((await ledger.unproven()).length, 1, 'the replacement mint is now the unproven row')
  // a REJECTED status word routes through onStuck — a pushed hint escalates
  // to the heal path, never to an outright drop; the healer's own 'drop'
  // verdict still reaps the row
  status = 'rejected'
  await ensureTrees(p)
  assert.equal(stuckCalls, 3)
  assert.deepEqual(rejected, [broadcasted[0].id('hex'), broadcasted[1].id('hex')])
  assert.equal((await ledger.unproven()).length, 1)
})

test('ensureTrees: onStuck receives the durable row age — a backdated phantom is heal-eligible across restarts', async () => {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  try {
    const k = PrivateKey.fromRandom()
    const ledger = new KnexTreeLedger(kx)
    const broadcasted: Transaction[] = []
    const bc = (tx: Transaction) => {
      broadcasted.push(tx)
      return Promise.resolve({ status: 'success' as const, txid: tx.id('hex') })
    }
    const fetchTx = (txid: string) => Promise.resolve(broadcasted.find((t) => t.id('hex') === txid))
    const p = {
      key: k,
      ledger,
      lowWater: 4,
      size: 4,
      takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
      broadcast: bc,
      fetchTx
    }
    await ensureTrees(p)
    const phantom = broadcasted[0].id('hex')
    // a never-propagated mint: age the row in the ledger itself — heal policy
    // must read THIS, not an in-memory first-seen that resets on boot
    const backdated = Date.now() - 11 * 60_000
    await kx('bstack_trees').where({ txid: phantom }).update({ added_at: backdated })
    let seen: number | undefined
    await ensureTrees({
      ...p,
      facts: () => Promise.resolve(factsFor('unknown')),
      onStuck: (_tx, unprovenSince) => {
        seen = unprovenSince
        return Promise.resolve('keep')
      }
    })
    assert.equal(seen, backdated, 'onStuck must see the persisted row age, not process uptime')
    // a legacy row (added_at NULL) reports undefined — the caller falls back
    // to its own first-sighting window instead of treating it as brand-new
    await kx('bstack_trees').where({ txid: phantom }).update({ added_at: null })
    seen = 1
    await ensureTrees({
      ...p,
      facts: () => Promise.resolve(factsFor('unknown')),
      onStuck: (_tx, unprovenSince) => {
        seen = unprovenSince
        return Promise.resolve('keep')
      }
    })
    assert.equal(seen, undefined)
  } finally {
    await kx.destroy()
  }
})

test('ensureTrees: a refused mint fires onRefused so the take heals, then fails the pass', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const refused: Array<{ txid: string; description: string | undefined }> = []
  // the 'parent rejected (ancestor X)' refusal shape the broadcaster hands
  // back — the mint never books a row, so without onRefused its take
  // reservation would leak for the life of the process
  const bc = () =>
    Promise.resolve({
      status: 'error' as const,
      description: `parent rejected (ancestor ${'ab'.repeat(32)}): retryable — resubmit after the ancestor is accepted`
    })
  const p = {
    key: k,
    ledger,
    lowWater: 2,
    size: 4,
    takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
    broadcast: bc,
    fetchTx: () => Promise.resolve(undefined),
    onRefused: (t: Transaction, r: { status: string; description?: string }) => {
      refused.push({ txid: t.id('hex'), description: r.description })
    }
  }
  await assert.rejects(() => ensureTrees(p), /broadcast failed/, 'the pass still fails — the next tick mints on surviving stock')
  assert.equal(refused.length, 1, 'the heal seam fired with the refused mint and its result')
  assert.match(refused[0].description ?? '', /parent rejected/)
  assert.equal((await ledger.unproven()).length, 0, 'a provably-dead refusal drops the pre-booked row back out')
})

test('ensureTrees: persists the tree BEFORE broadcast so a transport timeout does not orphan 64 slots', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  let seenAtBroadcast = 0
  const p = {
    key: k,
    ledger,
    lowWater: 2,
    size: 4,
    takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
    broadcast: async () => {
      seenAtBroadcast = (await ledger.unproven()).length
      return { status: 'error' as const, description: 'transport: ETIMEDOUT' }
    },
    fetchTx: () => Promise.resolve(undefined)
  }
  await assert.rejects(() => ensureTrees(p), /broadcast failed/)
  assert.equal(seenAtBroadcast, 1, 'tree row was persisted before broadcast ran')
  assert.equal((await ledger.unproven()).length, 1, 'non-definitive transport failure retains the row for txProof/stuckMint reconcile')
})

test('journal: fundedBy returns rows for a funding txid; put upserts beef', async () => {
  const j = new MemoryCarrierJournal()
  await j.put('a', { txid: 'c1', beef: [1], topics: ['t'], fundingTxid: 'tree1' })
  await j.put('b', { txid: 'c2', beef: [2], topics: ['t'], fundingTxid: 'tree1' })
  await j.put('c', { txid: 'c3', beef: [3], topics: ['t'], fundingTxid: 'tree2' })
  await j.put('d', { txid: 'c4', beef: [4], topics: ['t'] })
  assert.deepEqual((await j.fundedBy('tree1')).map((r) => r.hash).sort(), ['a', 'b'])
  assert.deepEqual((await j.fundedBy('nope')).length, 0)
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

test('resubmitCarrier: dead host throws, live host holds it, missing row resolves', async () => {
  const tr = new FakeTracker()
  const h1 = await minihost([NET], tr)
  const h2 = await minihost([NET], tr)
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const tree = await provenTree(k)
  await ledger.add(tree, 2)
  await ledger.markProven(tree.id('hex'), tree.merklePath!)
  const j = new MemoryCarrierJournal()
  const p = { key: k, ledger, journal: j, hosts: [h1.url], net: NET }
  const payload = await wantPayload(k)
  const r1 = await publishArtifact({ ...p, payload })
  const hash = artifactHash(payload)
  try {
    // a missing row is a no-op — nothing left to repair
    await resubmitCarrier({ journal: j, host: h2.url, key: 'never-journaled' })
    // a dead host throws so its repair row retries
    await assert.rejects(resubmitCarrier({ journal: j, host: 'http://127.0.0.1:1', key: hash }), /resubmit http:\/\/127\.0\.0\.1:1:/)
    // a live host takes the journaled carrier — its repair leg lands
    await resubmitCarrier({ journal: j, host: h2.url, key: hash })
    assert.ok(
      h2.raw().some((b) => Transaction.fromAtomicBEEF(b).id('hex') === r1.txid),
      'the repaired host now holds the carrier it missed'
    )
  } finally {
    await h1.close()
    await h2.close()
  }
})

test('republishOne: quorum fail leaves the row unenriched; retry carries the parent BUMP', async () => {
  const tr = new FakeTracker()
  const h = await minihost([NET], tr)
  const f = await faulthost(h.url)
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const tree = await anchoredTree(k, 2)
  await ledger.add(tree, 2)
  const j = new MemoryCarrierJournal()
  const payload = await wantPayload(k)
  await publishArtifact({ key: k, ledger, journal: j, hosts: [h.url], net: NET, payload })
  const hash = artifactHash(payload)
  prove(tree)
  try {
    // the only host is down — quorum fails, the journal row is untouched
    f.fault.down = true
    const r1 = await republishOne({ journal: j, hosts: [f.url], hash, treeTxid: tree.id('hex'), merklePath: tree.merklePath! })
    assert.equal(r1.done, false)
    const held1 = (await j.get(hash))!
    assert.equal(Transaction.fromAtomicBEEF(held1.beef).inputs[0].sourceTransaction!.merklePath, undefined, 'unenriched — retries next pass')
    // host back — the row enriches and re-serves
    f.fault.down = false
    const r2 = await republishOne({ journal: j, hosts: [f.url], hash, treeTxid: tree.id('hex'), merklePath: tree.merklePath! })
    assert.equal(r2.done, true)
    assert.equal(r2.submitted, true)
    const held2 = (await j.get(hash))!
    assert.notEqual(Transaction.fromAtomicBEEF(held2.beef).inputs[0].sourceTransaction!.merklePath, undefined, 'stored beef now carries the parent path')
    // already-enriched rows are done without another submit
    const r3 = await republishOne({ journal: j, hosts: [f.url], hash, treeTxid: tree.id('hex'), merklePath: tree.merklePath! })
    assert.equal(r3.done, true)
    assert.notEqual(r3.submitted, true)
  } finally {
    await h.close()
    await f.close()
  }
})

// spv/opaque-wait: a stocked-out lane names its state, not just 'no slot'
test('publish: an empty ledger reports why the lane is stocked out', async () => {
  const k = PrivateKey.fromRandom()
  const payload = await wantPayload(k)
  await assert.rejects(
    publishArtifact({ key: k, ledger: new MemoryTreeLedger(), journal: new MemoryCarrierJournal(), hosts: ['http://127.0.0.1:1'], net: NET, payload }),
    /funding slot \(market: 0 proven, 0 anchored, 0 tree\(s\) awaiting a block\)/
  )
})

// a provision tree in the ledger must not block the mint gate, and it still
// gets proven when fetchTx delivers its path
test('ensureTrees: unproven provision tree does not gate the mint, and gets proven', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const marketLock = await fundingLock(new ProtoWallet(k), ORIGINATOR, DERIVATION, FUNDING_TAG)
  const prov = new Transaction()
  prov.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  prov.addOutput({ satoshis: 100, lockingScript: marketLock })
  const led = ledger as unknown as { adopt?: (t: Transaction, s: { vout: number; kind: string }[]) => Promise<number> }
  assert.ok(led.adopt !== undefined, 'ledger must implement adopt')
  assert.equal(await led.adopt(prov, [{ vout: 0, kind: 'market' }]), 1)
  const broadcasted: Transaction[] = []
  const p = {
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    satsPerOutput: 100,
    takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
    broadcast: (t: Transaction) => {
      broadcasted.push(t)
      return Promise.resolve({ status: 'success' as const })
    },
    fetchTx: (id: string) => Promise.resolve(id === prov.id('hex') ? prov : undefined)
  }
  const r1 = await ensureTrees(p)
  assert.ok(r1.minted.length > 0, 'a pending provision tree must not block the market refill')
  assert.equal(broadcasted.length, 1)
  // once the provision tx is proven, the proving loop marks it even though
  // its tree kind is 'provision', not 'market'
  prove(prov)
  const r2 = await ensureTrees(p)
  assert.ok(r2.proven.includes(prov.id('hex')), 'provision tree must be marked proven by the upkeep loop')
  assert.deepEqual((await ledger.unproven()).map((u) => u.txid).sort(), [...r1.minted].sort(), 'only the freshly minted tree remains unproven')
})

// G3: a pushed REJECTED word can never reap a row — a provision tree held on
// the hint waits for the first-party verdict (the 'dead' sibling test below
// covers that reap)
test('ensureTrees: a rejected provision tree waits for the verdict — a hint never reaps', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const marketLock = await fundingLock(new ProtoWallet(k), ORIGINATOR, DERIVATION, FUNDING_TAG)
  const prov = new Transaction()
  prov.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  prov.addOutput({ satoshis: 100, lockingScript: marketLock })
  const led = ledger as unknown as { adopt?: (t: Transaction, s: { vout: number; kind: string }[]) => Promise<number> }
  assert.equal(await led.adopt!(prov, [{ vout: 0, kind: 'market' }]), 1)
  let rejected = 0
  await ensureTrees({
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    satsPerOutput: 100,
    takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
    broadcast: () => Promise.resolve({ status: 'success' as const }),
    fetchTx: () => Promise.resolve(undefined),
    facts: (id: string) => Promise.resolve(factsFor(id === prov.id('hex') ? 'rejected' : 'seen')),
    onReject: () => {
      rejected++
    }
  })
  assert.equal(rejected, 0, 'no tombstone either way — the inputs are foreign')
  assert.equal(await led.adopt!(prov, [{ vout: 0, kind: 'market' }]), 0, 'the row stays registered on a hint — re-adopt dedupes to nothing')
})

// F5 regression (memory tier): the tree upkeep used to ask a status word that
// could never carry 'rejected', so a refused provision mint gated the lane
// forever. Under txFacts a first-party dead verdict — abandon horizon or a
// ledger terminal row — must reap the row exactly like a rejection hint.
test('ensureTrees: a provision tree dead on a first-party verdict is reaped', async () => {
  const k = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const marketLock = await fundingLock(new ProtoWallet(k), ORIGINATOR, DERIVATION, FUNDING_TAG)
  const prov = new Transaction()
  prov.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  prov.addOutput({ satoshis: 100, lockingScript: marketLock })
  const led = ledger as unknown as { adopt?: (t: Transaction, s: { vout: number; kind: string }[]) => Promise<number> }
  assert.equal(await led.adopt!(prov, [{ vout: 0, kind: 'market' }]), 1)
  let rejected = 0
  await ensureTrees({
    key: k,
    ledger,
    lowWater: 4,
    size: 4,
    satsPerOutput: 100,
    takeFunding: () => Promise.resolve(fund(k, 1_000_000)),
    broadcast: () => Promise.resolve({ status: 'success' as const }),
    fetchTx: () => Promise.resolve(undefined),
    facts: () => Promise.resolve({ verdict: 'dead', source: 'abandon' }),
    onReject: () => {
      rejected++
    }
  })
  assert.equal(rejected, 0, 'provision rows are reaped without tombstoning foreign inputs')
  assert.ok(
    (await ledger.unproven()).every((u) => u.txid !== prov.id('hex')),
    'the dead provision row was reaped, not left gating the lane'
  )
  assert.equal(await led.adopt!(prov, [{ vout: 0, kind: 'market' }]), 1, 're-adopt registers the row again')
})
