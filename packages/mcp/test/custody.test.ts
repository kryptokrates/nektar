import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { MemoryFunds, listFundsStore, type FundingOut, type FundsDeps } from '@nektar/chain'
import { CustodyOps, type CustodyOpsDeps } from '../src/custody.js'

const key = PrivateKey.fromRandom()
const PKH = Buffer.from(key.toPublicKey().toHash()).toString('hex')
const parent = (): Transaction => {
  const p = new Transaction()
  p.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  p.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  return p
}
const pathFor = (tx: Transaction): MerklePath =>
  new MerklePath(9, [
    [
      { offset: 0, hash: '11'.repeat(32) },
      { offset: 1, hash: tx.id('hex'), txid: true }
    ]
  ])

const funds = (candidates: FundingOut[], deps: Partial<FundsDeps> = {}): MemoryFunds => {
  const store = listFundsStore()
  for (const u of candidates) store.add(u.tx, u.vout)
  return new MemoryFunds(store, { pkhHex: PKH, dealFloor: 0, ...deps })
}

const ops = (u: FundingOut, deps: Partial<FundsDeps> = {}, extra: Partial<CustodyOpsDeps> = {}): CustodyOps =>
  new CustodyOps({
    funds: funds([u], deps),
    fundSigner: () => key,
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    ...extra
  })

test('custody: takeFunding attaches the held proof to a pathless funder tx', async () => {
  const tx = parent()
  const mp = pathFor(tx)
  const proofOf = async (t: Transaction) => {
    if (t.id('hex') === tx.id('hex') && t.merklePath === undefined) t.merklePath = MerklePath.fromBinary(mp.toBinary())
  }
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, { proofOf }).takeFunding('deal', 1000)
  // a copy, not the store's own object — consumers may extract() on it
  assert.deepEqual(out.tx.merklePath?.toBinary(), mp.toBinary())
})

test('custody: takeFunding leaves the tx pathless when no proof is held', async () => {
  const tx = parent()
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }).takeFunding('deal', 1000)
  assert.equal(out.tx.merklePath, undefined)
})

test('custody: takeFunding never queries proofOf for a proven candidate', async () => {
  const tx = parent()
  const own = pathFor(tx)
  tx.merklePath = own
  let calls = 0
  const proofOf = async () => {
    calls++
  }
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, { proofOf }).takeFunding('deal', 1000)
  assert.equal(out.tx.merklePath, own)
  assert.equal(calls, 0, 'a proof-carrying parent needs no attach')
})

// G1: a candidate whose parent the broadcaster rejected is passed over for
// this call only — a status hint is never written to custody state
test('custody: takeFunding skips a dead-parent candidate without touching custody state', async () => {
  const bad = parent()
  const good = parent()
  // parent() is deterministic — an extra dust output makes the txids differ
  good.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  const statuses: string[] = []
  const store = listFundsStore()
  for (const u of [
    { tx: bad, vout: 0, satoshis: 100_000 },
    { tx: good, vout: 0, satoshis: 100_000 }
  ])
    store.add(u.tx, u.vout)
  const f = new MemoryFunds(store, {
    pkhHex: PKH,
    dealFloor: 0,
    deadParent: async (tx) => {
      statuses.push(tx.id('hex'))
      return tx.id('hex') === bad.id('hex')
    }
  })
  const ops = new CustodyOps({ funds: f, fundSigner: () => key, broadcast: async () => ({ status: 'success' }), txs: () => {} })
  const u = await ops.takeFunding('deal', 1000)
  assert.equal(u.tx.id('hex'), good.id('hex'))
  assert.deepEqual(statuses, [bad.id('hex'), good.id('hex')])
  assert.deepEqual([...store.tombstones], [], 'a skipped candidate is not tombstoned')
})

test('custody: a proven candidate is never status-queried', async () => {
  const p = parent()
  p.merklePath = pathFor(p)
  const statuses: string[] = []
  const store = listFundsStore()
  store.add(p, 0)
  const f = new MemoryFunds(store, {
    pkhHex: PKH,
    dealFloor: 0,
    deadParent: async (tx) => {
      statuses.push(tx.id('hex'))
      return true
    }
  })
  const ops = new CustodyOps({ funds: f, fundSigner: () => key, broadcast: async () => ({ status: 'success' }), txs: () => {} })
  const u = await ops.takeFunding('deal', 1000)
  assert.equal(u.tx, p)
  assert.deepEqual(statuses, [], 'a proof-carrying parent needs no status hint')
})

test('custody: provisionPay throws when the broadcaster rejects right after submit — the take is released', async () => {
  const f = funds([{ tx: parent(), vout: 0, satoshis: 100_000 }])
  const ops = new CustodyOps({
    funds: f,
    fundSigner: () => key,
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    txStatus: async () => 'REJECTED'
  })
  await assert.rejects(
    ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 }),
    /rejected after submit/
  )
  // the reservation was released — the same out is takeable again
  const u = await f.take('deal', 1000)
  assert.equal(u.satoshis, 100_000)
})

test('custody: a seen status returns after one read — bounded confirmation, not a wait loop', async () => {
  let reads = 0
  const f = funds([{ tx: parent(), vout: 0, satoshis: 100_000 }])
  const ops = new CustodyOps({
    funds: f,
    fundSigner: () => key,
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    txStatus: async () => {
      reads++
      return 'SEEN_ON_NETWORK'
    }
  })
  await ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 })
  assert.equal(reads, 1, 'first live read settles the check')
})
