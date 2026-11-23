import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { TxStore, fundsOf } from '@nektar/chain'
import { CustodyOps } from '../src/custody.js'
import { fundingProofAttacher } from '../src/funding.js'

const key = PrivateKey.fromRandom()
const funding = (): Transaction => {
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  tx.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  return tx
}
const pathFor = (tx: Transaction): MerklePath =>
  new MerklePath(9, [
    [
      { offset: 0, hash: '11'.repeat(32) },
      { offset: 1, hash: tx.id('hex'), txid: true }
    ]
  ])

void test('funding-proof: the served proof lands in TxStore where takeFunding reads it — watchTx never fires', async () => {
  const txStore = new TxStore()
  const tx = funding()
  const txid = tx.id('hex')
  txStore.put(tx) // the adoption seam puts bytes first — proof follows
  const mp = pathFor(tx)
  const watched: string[] = []
  const attach = fundingProofAttacher({
    txStore,
    proofLookup: (id) => Promise.resolve(id === txid ? mp : undefined),
    watchTx: (t) => void watched.push(t),
    log: () => {}
  })
  await attach(tx)
  assert.ok(txStore.get(txid)?.merklePath !== undefined, 'setProof stored the served path')
  assert.deepEqual(watched, [], 'a proven funding tx never hits the events floor')

  // the consumer contract: takeFunding's held-proof lookup now anchors the
  // tree minted on imported stock (pre-fix it returned a pathless tx)
  const ops = new CustodyOps({
    funds: fundsOf({ utxos: [{ tx, vout: 0, satoshis: 100_000 }], address: () => key.toAddress() }),
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    proofOf: (id) => txStore.get(id)?.merklePath
  })
  const out = await ops.takeFunding(1000)
  assert.ok(out.tx.merklePath !== undefined, 'takeFunding picks up the attached proof')
})

void test('funding-proof: no path served → watched once, and a watched txid is never asked again', async () => {
  const txStore = new TxStore()
  const tx = funding()
  txStore.put(tx)
  let lookups = 0
  const watched: string[] = []
  const attach = fundingProofAttacher({
    txStore,
    proofLookup: () => {
      lookups++
      return Promise.resolve(undefined)
    },
    watchTx: (t) => void watched.push(t),
    log: () => {}
  })
  await attach(tx)
  assert.equal(txStore.get(tx.id('hex'))?.merklePath, undefined)
  assert.deepEqual(watched, [tx.id('hex')])
  await attach(tx)
  assert.equal(lookups, 1, 'the events floor owns the txid now — no re-lookup')
  assert.equal(watched.length, 1, 'watchTx is a once-and-done handoff')
})

void test('funding-proof: a throwing lookup resolves to the watch — never blocks adoption', async () => {
  const txStore = new TxStore()
  const tx = funding()
  txStore.put(tx)
  const watched: string[] = []
  const attach = fundingProofAttacher({
    txStore,
    proofLookup: () => Promise.reject(new Error('index down')),
    watchTx: (t) => void watched.push(t),
    log: () => {}
  })
  await assert.doesNotReject(() => attach(tx))
  assert.deepEqual(watched, [tx.id('hex')])
})

void test('funding-proof: a tx already proven in the store is never re-fetched', async () => {
  const txStore = new TxStore()
  const tx = funding()
  txStore.put(tx)
  txStore.setProof(tx.id('hex'), pathFor(tx))
  let asked = false
  const attach = fundingProofAttacher({
    txStore,
    proofLookup: () => {
      asked = true
      return Promise.resolve(undefined)
    },
    watchTx: () => assert.fail('proven txs never hit the floor'),
    log: () => {}
  })
  await attach(tx)
  assert.equal(asked, false)
})

void test('funding-proof: concurrent calls for one txid share the single lookup', async () => {
  const txStore = new TxStore()
  const tx = funding()
  txStore.put(tx)
  let lookups = 0
  let gate!: () => void
  const hold = new Promise<void>((r) => {
    gate = r
  })
  const attach = fundingProofAttacher({
    txStore,
    proofLookup: async () => {
      lookups++
      await hold
      return pathFor(tx)
    },
    watchTx: () => {},
    log: () => {}
  })
  const p1 = attach(tx)
  const p2 = attach(tx)
  assert.equal(lookups, 1)
  gate()
  await Promise.all([p1, p2])
  assert.equal(lookups, 1, 'the second caller coalesced onto the in-flight read')
  assert.ok(txStore.get(tx.id('hex'))?.merklePath !== undefined)
})
