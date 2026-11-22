import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { MemoryFunds, listFundsStore } from '@nektar/chain'
import { CustodyOps } from '../src/custody.js'

const key = PrivateKey.fromRandom()
const addr = key.toPublicKey().toAddress()
const parent = (sats: number): Transaction => {
  const p = new Transaction()
  p.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  p.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(addr) })
  return p
}

const laneLock = (): string => new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress()).toHex()

const mk = (): { ops: CustodyOps; aired: () => Transaction | undefined } => {
  let aired: Transaction | undefined
  const store = listFundsStore()
  const coin = parent(1_000_000)
  void store.add(coin, 0)
  const ops = new CustodyOps({
    funds: new MemoryFunds(store, { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') }),
    fundSigner: () => key,
    broadcast: (tx: Transaction) => {
      aired = tx
      return Promise.resolve({ status: 'success' })
    },
    txs: () => {}
  })
  return { ops, aired: () => aired }
}

void test('provisionPay: plan bounds are enforced', async () => {
  const { ops } = mk()
  const pay = ops.provisionPay.bind(ops)
  await assert.rejects(pay({ outputs: [] }), /at least one output/)
  await assert.rejects(pay({ outputs: Array.from({ length: 1001 }, () => ({ satoshis: 1, scriptHex: laneLock() })) }), /1000/)
  await assert.rejects(pay({ outputs: [{ satoshis: 0, scriptHex: laneLock() }] }), /1\.\.10000/)
  await assert.rejects(pay({ outputs: [{ satoshis: 10_001, scriptHex: laneLock() }] }), /1\.\.10000/)
  await assert.rejects(pay({ outputs: [{ satoshis: 1.5, scriptHex: laneLock() }] }), /integer/)
  await assert.rejects(pay({ outputs: [{ satoshis: 100, scriptHex: laneLock() }], fundsSats: 545 }), /546/)
})

void test('provisionPay: one tx carries every lane output plus the P2PKH funds leg and change', async () => {
  const { ops, aired } = mk()
  const lockA = laneLock()
  const lockB = laneLock()
  const outputs = [
    { satoshis: 100, scriptHex: lockA },
    { satoshis: 100, scriptHex: lockA },
    { satoshis: 100, scriptHex: lockB }
  ]
  const r = await ops.provisionPay({ outputs, address: addr, fundsSats: 10_000 })
  const tx = aired()
  assert.ok(tx !== undefined, 'tx must reach broadcast')
  assert.equal(tx.id('hex'), r.txid)
  // 3 lane outputs + 1 P2PKH funds output + 1 change output
  assert.equal(tx.outputs.length, 5)
  assert.equal(r.outputs, 4, 'broadcast output count excludes change')
  const laneHexes = tx.outputs.slice(0, 3).map((o) => o.lockingScript.toHex())
  assert.deepEqual(laneHexes, [lockA, lockA, lockB])
  assert.ok(tx.outputs.slice(0, 3).every((o) => o.satoshis === 100))
  assert.equal(tx.outputs[3].satoshis, 10_000)
  assert.equal(tx.outputs[3].lockingScript.toHex(), new P2PKH().lock(addr).toHex())
  // change pays our own address; fee is real and positive
  assert.ok((tx.outputs[4].satoshis ?? 0) > 0)
  assert.ok(r.fee > 0 && r.fee < 50_000, `fee sane, got ${r.fee}`)
})
