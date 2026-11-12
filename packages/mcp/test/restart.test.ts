// Restart safety: a new NektarMemory on the same persisted state continues
// the SAME shard (next anchor spends the persisted head — the script-
// evaluating, double-spend-checking node rejects any fork), old records
// stay provable, and record metadata (level gate) survives in sqlite.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knexFactory from 'knex'
import { PrivateKey, type Transaction } from '@bsv/sdk'
import { stackWorld } from '@nektar/chain'
import { KnexShardStore } from '@nektar/log'
import { NektarMemory, FileStateStore } from '../src/index.js'

test('restart: resume from persisted head, no fork, proofs + level gate survive', async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-restart-'))
  const w = await stackWorld(t, 20)
  if (w === undefined) return
  const { chain, funder } = w
  const knex = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, 'records.sqlite') }, useNullAsDefault: true })
  try {
    const store = new KnexShardStore(knex)
    await store.init()
    const shardKeys = new Map([['s', PrivateKey.fromRandom()]])
    const signingKey = PrivateKey.fromRandom()
    const opts = () => ({ batchPolicy: { maxLeaves: 2 }, shardKeys, signingKey, stateStore: new FileStateStore(join(dir, 'state.json')) })
    const a = new NektarMemory(chain, funder, funder.keyPair, opts(), store)
    await a.remember('s', 'public fact')
    await a.remember('s', 'secret fact', {}, 2)
    await chain.mine(1)
    a.confirmAnchor('s', a.tailLink('s')!.txid)
    await a.remember('s', 'c')
    await a.remember('s', 'd') // second anchor, unconfirmed at "crash"
    const headBefore = a.anchorHead('s')!
    // restart
    const b = new NektarMemory(chain, funder, funder.keyPair, opts(), store)
    assert.equal(b.anchorHead('s')!.tx.id('hex'), headBefore.tx.id('hex'), 'resumed at the persisted head')
    assert.equal(b.anchorLinks('s').length, 2)
    assert.equal(b.checkpoints('s').length, 2)
    await b.remember('s', 'e')
    const r = await b.remember('s', 'f')
    assert.equal(r.anchored?.n, 2, 'next anchor is n=2 — it spent the persisted head (a fork would be rejected by script/double-spend checks)')
    await chain.mine(1)
    // push confirmer: 'mined' status events deliver BUMPs for every txid this
    // instance tracks — including the anchor restored from BEEF — so nothing
    // stays unproven waiting on a poll
    assert.equal(b.unprovenAnchors().length, 0, 'mined events attached proofs to both anchors, restored one included')
    const mp = (await chain.merklePathOf(b.anchorLinks('s')[0].txid))!
    assert.throws(() => b.attachProof('s', b.tailLink('s')!.txid, mp), /does not contain this txid/)
    for (const seq of [0, 1, 2, 3, 4, 5]) assert.equal((await b.prove('s', seq)).result.ok, true, `seq ${seq} provable after restart`)
    const sec = (await b.recall('s', { seq: 1 })).items[0]
    assert.equal(sec.text, undefined, 'level-2 record still gated after restart (level persisted in sqlite)')
    assert.match(sec.reason!, /zkaccess required/)
  } finally {
    await knex.destroy()
  }
})

test('recovery: a failed anchor broadcast re-batches and anchors (at-least-once)', async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-recover-'))
  const w = await stackWorld(t, 20)
  if (w === undefined) return
  const { chain, funder } = w
  const knex = knexFactory({ client: 'sqlite3', connection: { filename: join(dir, 'records.sqlite') }, useNullAsDefault: true })
  try {
    const store = new KnexShardStore(knex)
    await store.init()
    const shardKeys = new Map([['s', PrivateKey.fromRandom()]])
    const signingKey = PrivateKey.fromRandom()
    const opts = () => ({ batchPolicy: { maxLeaves: 2 }, shardKeys, signingKey, stateStore: new FileStateStore(join(dir, 'state.json')) })
    const mem = new NektarMemory(chain, funder, funder.keyPair, opts(), store)
    // inject an outage at broadcast: closeBatch marks the records batched,
    // the anchor never lands — the orphan case resetUnanchored repairs
    const orig = chain.broadcast.bind(chain)
    let fail = true
    chain.broadcast = async (tx: Transaction) => (fail ? { status: 'error', description: 'injected outage' } : orig(tx))
    await mem.remember('s', 'a')
    await assert.rejects(() => mem.remember('s', 'b'), /injected outage|broadcast failed/, 'the failing anchor surfaces')
    fail = false
    const a = await mem.anchorShard('s')
    assert.ok(a, 'recovery anchor lands')
    const recs = await store.all('s')
    assert.equal(
      recs.every((r) => r.state === 'anchored' && r.anchorTxid === a.txid),
      true,
      'orphaned records re-batched and anchored under the recovery anchor'
    )
    assert.equal(mem.checkpoints('s').length, 1, 'one checkpoint — no stray duplicate batch')
    await chain.mine(1)
    mem.confirmAnchor('s', a.txid)
    for (const seq of [0, 1]) assert.equal((await mem.prove('s', seq)).result.ok, true, `seq ${seq} provable`)
  } finally {
    await knex.destroy()
  }
})
