import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { CustodyOps, type CustodyOpsDeps } from '../src/custody.js'

const key = PrivateKey.fromRandom()
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

// proofOf is an optional dep — the cast keeps this file compiling while the
// field is added; a pathless funder tx should pick up the held proof
const ops = (u: { tx: Transaction; vout: number; satoshis: number }, proofOf?: (txid: string) => MerklePath | undefined): CustodyOps =>
  new CustodyOps({
    funder: { take: async () => u },
    fundSigner: () => key,
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    proofOf
  } as CustodyOpsDeps)

test('custody: takeFunding attaches the held proof to a pathless funder tx', async () => {
  const tx = parent()
  const mp = pathFor(tx)
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, (txid) => (txid === tx.id('hex') ? mp : undefined)).takeFunding(1000)
  // a copy, not the store's own object — consumers may extract() on it
  assert.deepEqual(out.tx.merklePath, mp)
})

test('custody: takeFunding leaves the tx pathless when no proof is held', async () => {
  const tx = parent()
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, () => undefined).takeFunding(1000)
  assert.equal(out.tx.merklePath, undefined)
})

test('custody: takeFunding never overwrites a proof the funder already carried', async () => {
  const tx = parent()
  const own = pathFor(tx)
  tx.merklePath = own
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, () => pathFor(tx)).takeFunding(1000)
  assert.equal(out.tx.merklePath, own)
})
