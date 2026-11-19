// funds.ts selection semantics — the shared coin-book rules every store
// backend inherits from FundsBase.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, P2PKH, PrivateKey, Transaction } from '@bsv/sdk'
import { MemoryFunds, listFundsStore } from '../src/funds.js'

test('funds: take prefers a proven parent over a cheaper unproven one', async () => {
  const key = PrivateKey.fromRandom()
  const addr = key.toAddress()
  const store = listFundsStore()
  const funds = new MemoryFunds(store, { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') })
  const out = (sats: number): Transaction => {
    const tx = new Transaction()
    tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
    return tx
  }
  // a BUMP on the subject tx = 'proven' for selection — an input built on it
  // terminates its BEEF ancestry at the anchor instead of dragging
  // unconfirmed parents into every spend
  const unprovenSmall = out(4_000)
  const provenMid = out(6_000)
  const provenLarge = out(8_000)
  for (const tx of [provenMid, provenLarge]) tx.merklePath = new MerklePath(1, [[{ offset: 0, hash: tx.id('hex'), txid: true }]])
  for (const tx of [unprovenSmall, provenMid, provenLarge]) store.add(tx, 0)

  const first = await funds.take('deal', 2_000)
  assert.equal(first.tx.id('hex'), provenMid.id('hex'), 'proven tier first, smallest-cover inside it')
  const second = await funds.take('deal', 2_000)
  assert.equal(second.tx.id('hex'), provenLarge.id('hex'))
  const third = await funds.take('deal', 2_000)
  assert.equal(third.tx.id('hex'), unprovenSmall.id('hex'), 'unproven parents are the fallback, not the first pick')
})

test('funds: among unproven candidates the shallowest ancestry wins — fanned-out change beats a deeper chain', async () => {
  const key = PrivateKey.fromRandom()
  const addr = key.toAddress()
  const store = listFundsStore()
  const funds = new MemoryFunds(store, { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') })
  const lock = () => new P2PKH().lock(addr)
  // root is mined; depthOne spends it (unproven, parent proven = one hop);
  // depthTwo spends depthOne's change (two hops). Smallest-cover alone
  // would hand depthTwo out first — the chain it drags is the cost
  const root = new Transaction()
  root.addOutput({ lockingScript: lock(), satoshis: 50_000 })
  root.merklePath = new MerklePath(1, [[{ offset: 0, hash: root.id('hex'), txid: true }]])
  const depthOne = new Transaction()
  depthOne.addInput({ sourceTransaction: root, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  depthOne.addOutput({ lockingScript: lock(), satoshis: 30_000 })
  await depthOne.sign()
  const depthTwo = new Transaction()
  depthTwo.addInput({ sourceTransaction: depthOne, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  depthTwo.addOutput({ lockingScript: lock(), satoshis: 10_000 })
  await depthTwo.sign()
  store.add(depthOne, 0)
  store.add(depthTwo, 0)

  const first = await funds.take('deal', 2_000)
  assert.equal(first.tx.id('hex'), depthOne.id('hex'), 'depth-1 ancestry beats smaller depth-2 change')
  const second = await funds.take('deal', 2_000)
  assert.equal(second.tx.id('hex'), depthTwo.id('hex'), 'deeper stock is still reachable once the shallow outs are taken')
})
