import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, P2PKH, Transaction, MerklePath, UnlockingScript } from '@bsv/sdk'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knex from 'knex'
import { TxStore } from '@nektar/chain'
import { SqlFunds } from '../src/sqlfunds.js'

// SqlFunds — the sqlite coin book. Memory tier: real knex/sqlite3, no chain.
// `take`/`spent`/`died`/`adopt`/`revive`/tombstone semantics are ported from
// toolbox-funder.test.ts; concurrency, crash-reopen and missing-bytes cases
// are the migration gates the basket backend never had to answer.

const KEY = PrivateKey.fromRandom()
const PKH = KEY.toPublicKey().toHash()
const PKH_HEX = Buffer.from(PKH).toString('hex')
const OWN_LOCK = new P2PKH().lock(KEY.toPublicKey().toHash())

const fundingTx = (sats: number, lock = OWN_LOCK): Transaction => {
  const tx = new Transaction()
  tx.addOutput({ lockingScript: lock, satoshis: sats })
  return tx
}
const spendTx = (parent: Transaction, vout: number, ours: number, theirs: number): Transaction => {
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: parent, sourceOutputIndex: vout, unlockingScript: new UnlockingScript() })
  tx.addOutput({ lockingScript: OWN_LOCK, satoshis: ours })
  tx.addOutput({ lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()), satoshis: theirs })
  return tx
}

const mkDb = (file = ':memory:') => knex({ client: 'sqlite3', connection: { filename: file }, useNullAsDefault: true })
const mkFunds = async (db: knex.Knex, txs = new TxStore(), deps: Record<string, unknown> = {}) =>
  await SqlFunds.open(db, txs, { pkhHex: PKH_HEX, log: () => {}, ...deps })
const close = async (db: knex.Knex) => await db.destroy()

test('SqlFunds: adopt adds live rows; refresh rebuilds the mirror', async () => {
  const db = mkDb()
  try {
    const f = await mkFunds(db)
    const a = fundingTx(1000)
    assert.equal(await f.adopt(a, 0), true)
    assert.equal(f.utxos.length, 1)
    assert.equal(f.utxos[0].tx.id('hex'), a.id('hex'))
    assert.equal(await f.has(a.id('hex'), 0), true)
    await f.refresh()
    assert.equal(f.utxos.length, 1)
    // duplicates are no-ops
    assert.equal(await f.adopt(a, 0), false)
    assert.equal(f.utxos.length, 1)
  } finally {
    await close(db)
  }
})

test('SqlFunds: take honors lease, hintSkip, sats bound and smallest-sufficient', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(700)
    const b = fundingTx(900)
    const c = fundingTx(300)
    const txs = new TxStore()
    const f = await mkFunds(db, txs)
    for (const t of [a, b, c]) await f.adopt(t, 0)
    f.leased = async (txid) => txid === a.id('hex')
    const pick = await f.take('deal', 500)
    assert.equal(pick.tx.id('hex'), b.id('hex')) // c too small, a leased → b wins
    const g = await mkFunds(db, txs) // a "restart" shares the persisted tx store
    await g.refresh()
    g.hintSkip = async (tx) => tx.id('hex') === b.id('hex')
    const pick2 = await g.take('deal', 500)
    assert.equal(pick2.tx.id('hex'), a.id('hex')) // b hinted out → a (b reserved by f)
  } finally {
    await close(db)
  }
})

test('SqlFunds: drop tombstones; adopt respects it; revive lifts it', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(800)
    const f = await mkFunds(db)
    await f.adopt(a, 0)
    await f.burn(a.id('hex'), 0)
    assert.equal(f.utxos.length, 0)
    // an index re-announce cannot resurrect a tombstone through adopt —
    // that refusal is the externally visible tombstone check
    assert.equal(await f.adopt(a, 0), false)
    assert.equal(f.utxos.length, 0)
    // revive is the deliberate, caller-verified re-own
    await f.revive(a, 0)
    assert.equal(f.utxos.length, 1)
    assert.equal(await f.has(a.id('hex'), 0), true)
  } finally {
    await close(db)
  }
})

test('SqlFunds: spent() drops inputs and adopts our change; died() frees inputs and drops our outputs', async () => {
  const db = mkDb()
  try {
    const parent = fundingTx(1000)
    const spend = spendTx(parent, 0, 550, 300)
    const f = await mkFunds(db)
    await f.adopt(parent, 0)
    await f.spent(spend)
    assert.equal(await f.has(parent.id('hex'), 0), false)
    assert.equal(f.utxos.length, 1)
    assert.equal(f.utxos[0].satoshis, 550) // our change output adopted
    assert.equal(f.utxos[0].tx.id('hex'), spend.id('hex'))
    // the spend dies on-chain — inputs revive, the adopted change drops
    await f.died(spend)
    assert.equal(await f.has(spend.id('hex'), 0), false)
    assert.equal(await f.has(parent.id('hex'), 0), true)
    assert.equal(f.utxos.length, 1)
    assert.equal(f.utxos[0].tx.id('hex'), parent.id('hex'))
  } finally {
    await close(db)
  }
})

test('SqlFunds: concurrent take() on one coin — exactly one taker wins', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(1000)
    const f = await mkFunds(db)
    await f.adopt(a, 0)
    const r = await Promise.allSettled([f.take('deal', 500), f.take('deal', 500)])
    const won = r.filter((x) => x.status === 'fulfilled')
    const lost = r.filter((x) => x.status === 'rejected')
    assert.equal(won.length, 1)
    assert.equal((won[0] as PromiseFulfilledResult<unknown>).value !== undefined, true)
    assert.equal(lost.length, 1)
    assert.match(String((lost[0] as PromiseRejectedResult).reason), /no spendable output/)
  } finally {
    await close(db)
  }
})

test('SqlFunds: take reserves until spent or released', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(1000)
    const b = fundingTx(2000)
    const f = await mkFunds(db)
    await f.adopt(a, 0)
    await f.adopt(b, 0)
    const u1 = await f.take('deal', 500)
    assert.equal(u1.tx.id('hex'), a.id('hex'))
    const u2 = await f.take('deal', 500)
    assert.equal(u2.tx.id('hex'), b.id('hex'))
    await assert.rejects(() => f.take('deal', 500), /no spendable output/)
    f.release(a.id('hex'), 0)
    const u3 = await f.take('deal', 500)
    assert.equal(u3.tx.id('hex'), a.id('hex'))
  } finally {
    await close(db)
  }
})

test('SqlFunds: crash between take and spent — reopening gives an identical live set', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-sqlfunds-'))
  const file = join(dir, 'records.sqlite')
  try {
    const a = fundingTx(1000)
    const parent = fundingTx(700)
    const spend = spendTx(parent, 0, 400, 200)
    const txs = new TxStore()
    const db1 = mkDb(file)
    const f1 = await mkFunds(db1, txs)
    await f1.adopt(a, 0)
    await f1.adopt(parent, 0)
    await f1.take('deal', 500) // reserves a in-process — the crash loses the reservation
    await f1.spent(spend) // durable: parent dropped, change adopted
    await close(db1) // "crash": in-flight reservations die with the process
    const db2 = mkDb(file)
    const f2 = await mkFunds(db2, txs)
    const ids = f2.utxos.map((u) => `${u.tx.id('hex')}:${u.vout}`).sort()
    // post-crash truth = the durable rows: a live (never spent), parent dropped,
    // change adopted. The lost reservation releases a — correct: it never
    // reached the chain.
    assert.deepEqual(ids.sort(), [`${a.id('hex')}:0`, `${spend.id('hex')}:0`].sort())
    await close(db2)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('SqlFunds: tombstone sweep — liveSet positively names a mark to lift', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(800)
    const b = fundingTx(900)
    const txs = new TxStore()
    const f = await mkFunds(db, txs)
    await f.adopt(a, 0)
    await f.adopt(b, 0)
    // both orphaned marks — no consuming draft heals them
    await f.burn(a.id('hex'), 0)
    await f.burn(b.id('hex'), 0)
    assert.equal(f.utxos.length, 0)
    // the index names a only — the starve sweep lifts just that mark
    f.liveSet = async () => new Set([`${a.id('hex')}:0`])
    const pick = await f.take('deal', 800)
    assert.equal(pick.tx.id('hex'), a.id('hex'))
    // b stays tombstoned — a name it doesn't carry cannot lift it
    assert.equal(await f.adopt(b, 0), false)
    assert.equal(await f.has(b.id('hex'), 0), false)
  } finally {
    await close(db)
  }
})

test('SqlFunds: a live row whose tx is missing still counts in balance()', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(1000)
    const txs = new TxStore()
    const f = await mkFunds(db, txs)
    await f.adopt(a, 0)
    // simulate lost bytes: a second TxStore that never saw the tx
    const blind = new TxStore()
    const g = await mkFunds(db, blind)
    assert.equal(g.utxos.length, 0) // unserializable stock is skipped in the mirror
    assert.equal(await g.balance(), 1000) // but the book still owns the coin
  } finally {
    await close(db)
  }
})

test('SqlFunds: a proven tx keeps its merkle path through the book', async () => {
  const db = mkDb()
  try {
    const a = fundingTx(1000)
    a.merklePath = new MerklePath(7, [
      [
        { offset: 0, hash: '00'.repeat(32) },
        { offset: 1, hash: a.id('hex'), txid: true }
      ]
    ])
    const f = await mkFunds(db)
    await f.adopt(a, 0)
    assert.equal(f.utxos[0].tx.merklePath?.blockHeight, 7)
    // the shared store carries the proof — a reopened book still sees it
    const txs2 = new TxStore()
    txs2.put(a)
    const h = await mkFunds(db, txs2)
    assert.equal(h.utxos[0].tx.merklePath?.blockHeight, 7)
  } finally {
    await close(db)
  }
})
