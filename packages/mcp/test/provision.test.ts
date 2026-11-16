import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { CustodyOps, type CustodyOpsDeps } from '../src/custody.js'

const key = PrivateKey.fromRandom()
const addr = key.toPublicKey().toAddress()
const parent = (sats: number): Transaction => {
  const p = new Transaction()
  p.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  p.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(addr) })
  return p
}

const laneLock = (): string => new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress()).toHex()

// fake custody deps; provisionPay may not exist yet — the cast keeps the
// file compiling ahead of the method
interface ProvisionPay {
  provisionPay?: (args: { outputs: Array<{ satoshis: number; scriptHex: string }>; address?: string; fundsSats?: number }) => Promise<{
    txid: string
    hex: string
    outputs: number
    fee: number
  }>
}
const mk = (): { ops: CustodyOps & ProvisionPay; aired: () => Transaction | undefined } => {
  let aired: Transaction | undefined
  const ops = new CustodyOps({
    funder: { take: async () => ({ tx: parent(1_000_000), vout: 0, satoshis: 1_000_000 }) },
    fundSigner: () => key,
    broadcast: async (tx: Transaction) => {
      aired = tx
      return { status: 'success' }
    },
    txs: () => {}
  } as CustodyOpsDeps) as CustodyOps & ProvisionPay
  return { ops, aired: () => aired }
}

test('provisionPay: plan bounds are enforced', async () => {
  const { ops } = mk()
  assert.ok(ops.provisionPay !== undefined, 'custody must expose provisionPay')
  const pay = ops.provisionPay.bind(ops)
  await assert.rejects(pay({ outputs: [] }), /at least one output/)
  await assert.rejects(pay({ outputs: Array.from({ length: 1001 }, () => ({ satoshis: 1, scriptHex: laneLock() })) }), /1000/)
  await assert.rejects(pay({ outputs: [{ satoshis: 0, scriptHex: laneLock() }] }), /1\.\.10000/)
  await assert.rejects(pay({ outputs: [{ satoshis: 10_001, scriptHex: laneLock() }] }), /1\.\.10000/)
  await assert.rejects(pay({ outputs: [{ satoshis: 1.5, scriptHex: laneLock() }] }), /integer/)
  await assert.rejects(pay({ outputs: [{ satoshis: 100, scriptHex: laneLock() }], fundsSats: 545 }), /546/)
})

test('provisionPay: one tx carries every lane output plus the P2PKH funds leg and change', async () => {
  const { ops, aired } = mk()
  const lockA = laneLock()
  const lockB = laneLock()
  const outputs = [
    { satoshis: 100, scriptHex: lockA },
    { satoshis: 100, scriptHex: lockA },
    { satoshis: 100, scriptHex: lockB }
  ]
  const r = await ops.provisionPay!({ outputs, address: addr, fundsSats: 10_000 })
  const tx = aired()!
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
