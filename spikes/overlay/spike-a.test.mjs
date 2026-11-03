// SPIKE A (offline): prove tm_nektar enforces the successor rule through the
// real @bsv/overlay Engine — no chain needed. Parents are synthetic mined txs
// carrying a MerklePath accepted by a mock ChainTracker.
import test from 'node:test'
import assert from 'node:assert/strict'
import knex from 'knex'
import { Engine } from '@bsv/overlay'
import { KnexStorage } from '@bsv/overlay'
import allMigrations from '@bsv/overlay/storage/knex/all-migrations.ts'
import { Transaction, PrivateKey, ProtoWallet, PushDrop, MerklePath, Script } from '@bsv/sdk'
import {
  anchorFields, parseAnchorFields, anchorLockingScript, anchorUnlockTemplate,
  chainRp, dsha256, u64le, ZERO32, p2pkhLock, p2pkhUnlock
} from '../lib/anchor.mjs'
import { createTmNektar } from './tm_nektar.mjs'
import { createLsNektar } from './ls_nektar.mjs'

const HEIGHT = 100
const fundKey = PrivateKey.fromRandom()
const shardKey = PrivateKey.fromRandom()
const SHARD = 'shard-alpha'

// ---- mock chain ----
const validRoots = new Set() // `${height}:${merkleRootHex}`
const chainTracker = {
  currentHeight: async () => 1000,
  isValidRootForHeight: async (root, height) => validRoots.has(`${height}:${root}`)
}
const broadcaster = { broadcast: async (tx) => ({ status: 'success', txid: tx.id('hex') }) }

// Build a synthetic mined parent tx with N P2PKH outputs to fundKey.
async function minedParent (nOutputs) {
  const tx = new Transaction()
  tx.addInput({
    sourceTXID: '00'.repeat(32),
    sourceOutputIndex: 0,
    unlockingScript: Script.fromHex('51'), // OP_TRUE-looking, contents irrelevant offline
  })
  for (let i = 0; i < nOutputs; i++) {
    tx.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 10_000 })
  }
  const txid = tx.id('hex')
  tx.merklePath = new MerklePath(HEIGHT, [[{ offset: 0, hash: txid, txid: true }]])
  validRoots.add(`${HEIGHT}:${tx.merklePath.computeRoot(txid)}`)
  return tx
}

// Build+sign an anchor tx. funding: {tx, vout}. prevHead: {tx, vout, fields} | null.
async function anchorTx ({ shard = SHARD, n, size, R, Rp, funding, prevHead = null, spendWrongHead = null }) {
  const wallet = new ProtoWallet(shardKey)
  const tx = new Transaction()
  if (prevHead) {
    const h = spendWrongHead ?? prevHead
    tx.addInput({
      sourceTransaction: h.tx,
      sourceOutputIndex: h.vout,
      unlockingScriptTemplate: anchorUnlockTemplate(wallet, h.shardKey ?? shard,
        h.tx.outputs[h.vout].satoshis, h.tx.outputs[h.vout].lockingScript)
    })
  }
  tx.addInput({
    sourceTransaction: funding.tx,
    sourceOutputIndex: funding.vout,
    unlockingScriptTemplate: p2pkhUnlock(fundKey)
  })
  const fields = anchorFields({ shard, n, size, R, Rp })
  tx.addOutput({
    lockingScript: await anchorLockingScript(wallet, shard, fields),
    satoshis: 1
  })
  tx.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 9_000, change: true })
  await tx.fee()
  await tx.sign()
  return { tx, fields }
}

const R = (i) => dsha256(Buffer.from(`batch-${i}`))

async function makeEngine () {
  const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  for (const m of allMigrations) await m.up(kx)
  const storage = new KnexStorage(kx)
  const ls = createLsNektar()
  const tm = createTmNektar(ls)
  const engine = new Engine(
    { tm_nektar: tm },
    { ls_nektar: ls },
    storage,
    chainTracker,
    undefined, undefined, undefined,
    broadcaster,
    undefined, undefined,
    false, '', false, undefined, undefined, false
  )
  return { engine, ls, kx }
}

async function submit (engine, tx) {
  const steak = await engine.submit({ beef: tx.toBEEF(), topics: ['tm_nektar'] }, undefined, 'current-tx')
  return steak
}
const admitted = (steak) =>
  (steak.tm_nektar?.outputsToAdmit?.length ?? 0) > 0

test('SPIKE A part 1: admit genesis+successor, lookups work', async (t) => {
  const { engine, ls, kx } = await makeEngine()
  t.after(async () => { await kx.destroy() })
  const parent = await minedParent(10)
  const fund = (vout) => ({ tx: parent, vout })

  // 1) valid genesis admitted
  const g = await anchorTx({ n: 0, size: 10, R: R(0), Rp: chainRp(ZERO32, R(0), 10), funding: fund(0) })
  const steak0 = await submit(engine, g.tx)
  assert.equal(admitted(steak0), true, 'genesis should be admitted')
  let head = ls.getHead(SHARD)
  assert.equal(head.txid, g.tx.id('hex'))
  assert.equal(head.n, 0)

  // 2) valid successor admitted, becomes head
  const a1 = await anchorTx({
    n: 1, size: 20, R: R(1), Rp: chainRp(parseAnchorFields(g.tx.outputs[0].lockingScript).Rp, R(1), 20),
    funding: fund(1), prevHead: { tx: g.tx, vout: 0 }
  })
  const steak1 = await submit(engine, a1.tx)
  assert.equal(admitted(steak1), true, 'successor should be admitted')
  head = ls.getHead(SHARD)
  assert.equal(head.txid, a1.tx.id('hex'))
  assert.equal(head.n, 1)

  // 3) lookup returns current head + anchor-by-n
  const viaLookup = await engine.lookup({
    service: 'ls_nektar',
    query: { shard: SHARD }
  })
  assert.equal(viaLookup.type, 'output-list')
  assert.equal(viaLookup.outputs.length, 1)
  const headTx = Transaction.fromBEEF(viaLookup.outputs[0].beef)
  assert.equal(headTx.id('hex'), a1.tx.id('hex'), 'lookup must return current head')
  const headCtx = JSON.parse(Buffer.from(viaLookup.outputs[0].context).toString('utf8'))
  assert.equal(headCtx.n, 1)

  const byN = await engine.lookup({
    service: 'ls_nektar',
    query: { shard: SHARD, n: 0 }
  })
  assert.equal(byN.outputs.length, 1)
  assert.equal(Transaction.fromBEEF(byN.outputs[0].beef).id('hex'), g.tx.id('hex'),
    'anchor n=0 still retrievable (retained history)')
})

test('SPIKE A part 2: rejection cases leave head unchanged', async (t) => {
  const { engine, ls, kx } = await makeEngine()
  t.after(async () => { await kx.destroy() })
  const parent = await minedParent(10)
  const fund = (vout) => ({ tx: parent, vout })

  const g = await anchorTx({ n: 0, size: 10, R: R(0), Rp: chainRp(ZERO32, R(0), 10), funding: fund(0) })
  await submit(engine, g.tx)
  const a1 = await anchorTx({
    n: 1, size: 20, R: R(1), Rp: chainRp(parseAnchorFields(g.tx.outputs[0].lockingScript).Rp, R(1), 20),
    funding: fund(1), prevHead: { tx: g.tx, vout: 0 }
  })
  await submit(engine, a1.tx)
  assert.equal(ls.getHead(SHARD).txid, a1.tx.id('hex'))

  // 1) wrong n (skips to 3) rejected
  const prevRp = parseAnchorFields(a1.tx.outputs[0].lockingScript).Rp
  const badN = await anchorTx({
    n: 3, size: 30, R: R(2), Rp: chainRp(prevRp, R(2), 30),
    funding: fund(2), prevHead: { tx: a1.tx, vout: 0 }
  })
  const steakBad = await submit(engine, badN.tx)
  assert.equal(admitted(steakBad), false, 'wrong n must be rejected')
  assert.equal(ls.getHead(SHARD).txid, a1.tx.id('hex'), 'head unchanged')

  // 2) wrong R' rejected
  const badRp = await anchorTx({
    n: 2, size: 30, R: R(2), Rp: dsha256('bogus'),
    funding: fund(3), prevHead: { tx: a1.tx, vout: 0 }
  })
  const steakBadRp = await submit(engine, badRp.tx)
  assert.equal(admitted(steakBadRp), false, 'wrong Rprime must be rejected')
  assert.equal(ls.getHead(SHARD).txid, a1.tx.id('hex'))

  // 3) tx not spending the head rejected (n=2 but no head spend input)
  const noSpend = await anchorTx({
    n: 2, size: 30, R: R(2), Rp: chainRp(prevRp, R(2), 30),
    funding: fund(4), prevHead: null
  })
  const steakNoSpend = await submit(engine, noSpend.tx)
  assert.equal(admitted(steakNoSpend), false, 'non-head-spend must be rejected')
  assert.equal(ls.getHead(SHARD).txid, a1.tx.id('hex'))

  // 4) second genesis (n=0 not spending head) rejected as fork
  const fork0 = await anchorTx({
    n: 0, size: 5, R: R(99), Rp: chainRp(ZERO32, R(99), 5), funding: fund(5), prevHead: null
  })
  const steakFork = await submit(engine, fork0.tx)
  assert.equal(admitted(steakFork), false, 'second genesis must be rejected')
})
