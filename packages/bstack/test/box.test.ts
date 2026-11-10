import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { MemoryTreeLedger } from '../src/tree.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { sendBox, readBox, boxFundingLock, DEFAULT_OFFICE } from '../src/box.js'
import { mintTree } from '../src/tree.js'
import { fund, prove } from './helpers.js'
import { minihost, FakeTracker } from '../src/testing.js'

const OFFICE = DEFAULT_OFFICE
const boxLedger = async (key: PrivateKey, n = 8) => {
  const l = new MemoryTreeLedger()
  const t = await mintTree({ key, funding: fund(key), n, satsPerOutput: 1000, lock: await boxFundingLock(key) })
  prove(t)
  await l.add(t, n, 'bbox')
  await l.markProven(t.id('hex'))
  return l
}

test('box: send -> recipient reads + decrypts; third key gets nothing; resend = one slot', async () => {
  const tr = new FakeTracker()
  const h1 = await minihost([], { tracker: tr, office: OFFICE })
  const h2 = await minihost([], { tracker: tr, office: OFFICE })
  try {
    const sender = PrivateKey.fromRandom(); const recip = PrivateKey.fromRandom(); const third = PrivateKey.fromRandom()
    const ledger = await boxLedger(sender)
    const journal = new MemoryCarrierJournal()
    const body = { market: { type: 'quote', wantId: 'w-1', priceSats: 42 } }
    const r1 = await sendBox({ key: sender, ledger, journal, hosts: [h1.url, h2.url], to: recip.toPublicKey().toString(), body })
    assert.ok(r1.took.length === 2)
    const read = await readBox({ key: recip, hosts: [h1.url, h2.url], tracker: tr })
    assert.equal(read.items.length, 1)
    assert.equal(read.items[0]!.from, sender.toPublicKey().toString())
    assert.deepEqual(read.items[0]!.body, body)
    const r3 = await readBox({ key: third, hosts: [h1.url], tracker: tr })
    assert.equal(r3.items.length, 0, 'third key cannot read')
    const slots = await ledger.remaining('bbox')
    const r2 = await sendBox({ key: sender, ledger, journal, hosts: [h1.url, h2.url], to: recip.toPublicKey().toString(), body })
    assert.equal(r2.txid, r1.txid, 'same carrier on retry')
    assert.equal(await ledger.remaining('bbox'), slots, 'no second slot')
  } finally { await h1.close(); await h2.close() }
})

test('box: corrupted answer dropped reader-side; envelope for someone else not returned', async () => {
  const tr = new FakeTracker()
  const h1 = await minihost([], { tracker: tr, office: OFFICE })
  const bad = await minihost([], { tracker: tr, office: OFFICE, corrupt: true })
  try {
    const sender = PrivateKey.fromRandom(); const recip = PrivateKey.fromRandom(); const other = PrivateKey.fromRandom()
    const ledger = await boxLedger(sender)
    const journal = new MemoryCarrierJournal()
    await sendBox({ key: sender, ledger, journal, hosts: [h1.url, bad.url], to: recip.toPublicKey().toString(), body: { market: { t: 1 } } })
    const read = await readBox({ key: recip, hosts: [h1.url], tracker: tr })
    assert.equal(read.items.length, 1)
    const readBad = await readBox({ key: recip, hosts: [bad.url], tracker: tr })
    assert.equal(readBad.items.length, 0, 'corrupted beef dropped reader-side')
    // wrong-addressed envelope: other holds it, not recip
    await sendBox({ key: sender, ledger, journal, hosts: [h1.url], to: other.toPublicKey().toString(), body: { market: { t: 2 } } })
    const read2 = await readBox({ key: recip, hosts: [h1.url], tracker: tr })
    assert.equal(read2.items.length, 1, 'envelope to someone else not returned for me')
  } finally { await h1.close(); await bad.close() }
})

test('ledger kind: market and bbox slots are separate; 4a schema migrates', async () => {
  const key = PrivateKey.fromRandom()
  const l = new MemoryTreeLedger()
  const t1 = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 100 })
  const t2 = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 100, lock: await boxFundingLock(key) })
  prove(t1); prove(t2)
  await l.add(t1, 4, 'market'); await l.markProven(t1.id('hex'))
  await l.add(t2, 4, 'bbox'); await l.markProven(t2.id('hex'))
  assert.equal(await l.remaining('market'), 4)
  assert.equal(await l.remaining('bbox'), 4)
  const slot = await l.take('bbox')
  assert.equal(slot!.tree.id('hex'), t2.id('hex'), 'bbox slot from bbox tree')
  assert.equal(await l.remaining('market'), 4)
  // pre-4a schema: tables without kind column -> ensure() adds it additively
  const knex = (await import('knex')).default
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  await kx.schema.createTable('bstack_trees', t => { t.string('txid').primary(); t.integer('n'); t.integer('proven'); t.text('beef') })
  await kx.schema.createTable('bstack_tree_outputs', t => { t.string('key').primary(); t.string('txid'); t.integer('vout') })
  await kx('bstack_trees').insert({ txid: 'old', n: 2, proven: 1, beef: JSON.stringify(t1.toBEEF()) })
  await kx('bstack_tree_outputs').insert({ key: 'old:0', txid: 'old', vout: 0 })
  const { KnexTreeLedger } = await import('../src/tree.js')
  const kl = new KnexTreeLedger(kx)
  assert.equal(await kl.remaining(), 1, 'old rows counted as market kind')
  await kl.add(t2, 4, 'bbox')
  assert.equal(await kl.remaining('bbox'), 4)
  assert.equal(await kl.remaining('market'), 1)
  await kx.destroy()
})
