import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, existsSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import knex from 'knex'
import { PrivateKey, P2PKH, Transaction, ProtoWallet, UnlockingScript } from '@bsv/sdk'
import * as tb from '@nektar/automation'
import { TxStore, type ChainPort } from '@nektar/chain'
import { sweepBasket } from '../src/funding.js'

// migration gates: the basket sweep lands real wallet.sqlite rows in the
// `utxos` table; ProtoWallet is a drop-in signer; client custody keeps the
// external substrate blind to coins it never internalized. Memory tier —
// stub services, a fabricated chain port, no node.

const KEY = PrivateKey.fromRandom()
const LOCK = new P2PKH().lock(KEY.toPublicKey().toHash())

const stubServices = {
  getHeight: async () => 900000,
  getHeaderForHeight: async () => [0],
  getRawTx: async () => ({}),
  getMerklePath: async () => ({}),
  postBeef: async (beef: unknown, txids: string[]) => [
    { name: 'stub', status: 'success', txidResults: txids.map((txid) => ({ txid, status: 'success', alreadyKnown: false })) }
  ],
  getUtxoStatus: async () => ({}),
  getStatusForTxids: async () => ({}),
  getScriptHashHistory: async () => ({ outputs: [] }),
  getBsvExchangeRate: async () => 50,
  getFiatExchangeRate: async () => 1,
  nLockTimeIsFinal: async () => true,
  getBeefForTxid: async () => {
    throw new Error('no beef')
  },
  isValidRootForHeight: async () => true,
  getChainTracker: async () => ({ currentHeight: async () => 900000, isValidRootForHeight: async () => true }) as never,
  chain: 'test',
  hashOutputScript: (script: string) => script,
  isUtxo: async () => true,
  hashToHeader: async () => ({}) as never,
  getServicesCallHistory: () => ({}) as never
} as unknown as Parameters<typeof tb.openToolboxWallet>[0]['services']

const root = new Transaction()
root.addOutput({ lockingScript: LOCK, satoshis: 1_000_000 })
const fund = (sats: number): Transaction => {
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: root, sourceOutputIndex: 0, sequence: 0xffffffff, unlockingScript: new UnlockingScript() })
  tx.addOutput({ lockingScript: LOCK, satoshis: sats })
  return tx
}

test('basket sweep: real wallet.sqlite basket rows land in the utxos table; wallet renamed aside', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-sweep-'))
  try {
    // write the basket the old code path wrote: two basket-inserted coins
    const tbh = await tb.openToolboxWallet({ fundKey: KEY, dataDir: dir, chain: 'test', services: stubServices })
    const a = fund(1_500)
    const b = fund(2_500)
    for (const t of [a, b]) {
      await tbh.wallet.internalizeAction({
        description: 'old-path funding coin',
        tx: t.toAtomicBEEF(),
        outputs: [{ outputIndex: 0, protocol: 'basket insertion', insertionRemittance: { basket: tb.NEKTAR_BASKET } }]
      })
    }
    await tbh.close()
    assert.equal(existsSync(join(dir, 'wallet.sqlite')), true)

    const db = knex({ client: 'sqlite3', connection: { filename: join(dir, 'records.sqlite') }, useNullAsDefault: true })
    try {
      const txs = new TxStore()
      const funds = await tb.SqlFunds.open(db, txs, { pkhHex: Buffer.from(KEY.toPublicKey().toHash()).toString('hex'), log: () => {} })
      // storage-level sweep: no wallet, no services — getTx/txLookup absent
      // means the stored bytes resolve + a broadcast recovery airs them
      const port = { tracker: {}, broadcast: async () => ({ status: 'success' }) } as unknown as ChainPort
      await sweepBasket({ tb, dataDir: dir, chain: 'test', port, funds, log: () => {} })
      const ids = funds.utxos.map((u) => u.tx.id('hex')).sort()
      assert.deepEqual(ids, [a.id('hex'), b.id('hex')].sort())
      assert.equal(await funds.balance(), 4_000)
      // the original file is kept, renamed aside — the sweep is revertible
      assert.equal(existsSync(join(dir, 'wallet.sqlite.migrated')), true)
      // and a second run is a no-op, not a double-import
      await sweepBasket({ tb, dataDir: dir, chain: 'test', port, funds, log: () => {} })
      assert.equal(funds.utxos.length, 2)
    } finally {
      await db.destroy()
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('basket sweep: storage-level rows catch what listOutputs hides — stalled parents broadcast-recover, dead parents skip', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-sweep-hidden-'))
  try {
    // two basket coins, then tamper parent statuses into the states the doc
    // says listOutputs hides: `unprocessed` (stalled delayed broadcast) and
    // `failed` (dead draft)
    const tbh = await tb.openToolboxWallet({ fundKey: KEY, dataDir: dir, chain: 'test', services: stubServices })
    const stalled = fund(1_500)
    const dead = fund(2_500)
    for (const t of [stalled, dead]) {
      await tbh.wallet.internalizeAction({
        description: 'old-path funding coin',
        tx: t.toAtomicBEEF(),
        outputs: [{ outputIndex: 0, protocol: 'basket insertion', insertionRemittance: { basket: tb.NEKTAR_BASKET } }]
      })
    }
    // fixture sanity: unproven parents are listable before the tamper
    assert.equal((await tbh.wallet.listOutputs({ basket: tb.NEKTAR_BASKET })).totalOutputs, 2)
    await tbh.close()
    const wdb = knex({ client: 'sqlite3', connection: { filename: join(dir, 'wallet.sqlite') }, useNullAsDefault: true })
    await wdb('transactions')
      .where({ txid: stalled.id('hex') })
      .update({ status: 'unprocessed' })
    await wdb('transactions')
      .where({ txid: dead.id('hex') })
      .update({ status: 'failed' })
    await wdb.destroy()
    // the documented visibility rule: wallet-layer listOutputs now hides BOTH
    const tbh2 = await tb.openToolboxWallet({ fundKey: KEY, dataDir: dir, chain: 'test', services: stubServices })
    assert.equal((await tbh2.wallet.listOutputs({ basket: tb.NEKTAR_BASKET })).totalOutputs, 0)
    await tbh2.close()
    // storage-level enumeration still sees them, with parent status + bytes
    const store = await tb.openToolboxStore({ dataDir: dir, chain: 'test' })
    const rows = await tb.basketRows(store.activeStorage, tb.NEKTAR_BASKET)
    const rowCheck = (() => {
      if (rows.length !== 2) return `expected 2 rows, got ${rows.length}`
      if (rows.find((r) => r.txid === stalled.id('hex'))?.parentStatus !== 'unprocessed') return 'stalled parent not unprocessed'
      if (rows.find((r) => r.txid === dead.id('hex'))?.parentStatus !== 'failed') return 'dead parent not failed'
      return undefined
    })()
    await store.close()
    assert.equal(rowCheck, undefined)
    // sweep: the stalled parent airs through the port's broadcast, then
    // adopts; the dead parent is skipped without blocking the rename
    const broadcast: string[] = []
    const port = {
      tracker: {},
      broadcast: async (tx: Transaction) => {
        broadcast.push(tx.id('hex'))
        return { status: 'success' as const }
      }
    } as unknown as ChainPort
    const db = knex({ client: 'sqlite3', connection: { filename: join(dir, 'records.sqlite') }, useNullAsDefault: true })
    try {
      const funds = await tb.SqlFunds.open(db, new TxStore(), { pkhHex: Buffer.from(KEY.toPublicKey().toHash()).toString('hex'), log: () => {} })
      await sweepBasket({ tb, dataDir: dir, chain: 'test', port, funds, log: () => {} })
      assert.deepEqual(
        funds.utxos.map((u) => u.tx.id('hex')),
        [stalled.id('hex')]
      )
      assert.deepEqual(broadcast, [stalled.id('hex')])
      assert.equal(existsSync(join(dir, 'wallet.sqlite.migrated')), true)
    } finally {
      await db.destroy()
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('signer parity: ProtoWallet and toolbox wallet interoperate on every method our surface calls', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-parity-'))
  try {
    const tbh = await tb.openToolboxWallet({ fundKey: KEY, dataDir: dir, chain: 'test', services: stubServices })
    const pw = new ProtoWallet(KEY)
    const args = { protocolID: [2, 'nektar fragment'] as [2, string], keyID: '42', counterparty: 'self' }
    const eq = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)
    assert.equal(eq(await tbh.wallet.getPublicKey(args), await pw.getPublicKey(args)), true)
    assert.equal(eq(await tbh.wallet.getPublicKey({ identityKey: true }), await pw.getPublicKey({ identityKey: true })), true)
    assert.equal(eq(await tbh.wallet.createHmac({ ...args, data: [1, 2, 3] }), await pw.createHmac({ ...args, data: [1, 2, 3] })), true)
    // encrypt is ephemeral-keyed — interop is cross-decrypt, not byte equality
    const ctA = (await tbh.wallet.encrypt({ ...args, plaintext: [9, 8, 7] })).ciphertext
    const ctB = (await pw.encrypt({ ...args, plaintext: [9, 8, 7] })).ciphertext
    assert.deepEqual((await pw.decrypt({ ...args, ciphertext: ctA })).plaintext, [9, 8, 7])
    assert.deepEqual((await tbh.wallet.decrypt({ ...args, ciphertext: ctB })).plaintext, [9, 8, 7])
    await tbh.close()
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('client custody: the external substrate never sees our coins — it cannot double-spend them', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-client-'))
  try {
    // stand-in external wallet: an empty toolbox instance on another key/data dir
    const extDir = mkdtempSync(join(tmpdir(), 'nektar-ext-'))
    const ext = await tb.openToolboxWallet({ fundKey: PrivateKey.fromRandom(), dataDir: extDir, chain: 'test', services: stubServices })
    const db = knex({ client: 'sqlite3', connection: { filename: join(dir, 'records.sqlite') }, useNullAsDefault: true })
    try {
      const funds = await tb.SqlFunds.open(db, new TxStore(), { pkhHex: Buffer.from(KEY.toPublicKey().toHash()).toString('hex'), log: () => {} })
      const a = fund(3_000)
      assert.equal(await funds.adopt(a, 0), true)
      // the coin lives only in our table — the external wallet's view is empty
      const view = await ext.wallet.listOutputs({ basket: 'default', limit: 10 })
      assert.equal(view.totalOutputs, 0)
      // reconcile the other way: our book is chain-verified by hooks, not by
      // the substrate — spendCheck/liveSet are the only arbiters
      assert.equal(await funds.balance(), 3_000)
      await ext.close()
      rmSync(extDir, { recursive: true, force: true })
    } finally {
      await db.destroy()
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
