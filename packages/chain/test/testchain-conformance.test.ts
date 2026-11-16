// conformance — the same short script must pass on whichever tier is active:
// broadcast ok, double spend refused, proof after mine verifies with the
// chain as tracker, utxoStatus flips to spent, pendingBroadcasts drains.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { P2PKH, Transaction } from '@bsv/sdk'
import { testWorld } from '../src/testworld.js'

test('testchain conformance: broadcast, double-spend refusal, proof, utxoStatus, drain', async (t) => {
  const w = await testWorld(t, 4, 10_000)
  if (w === undefined) return
  const { chain, funder } = w

  const out = await funder.take(4_000)
  const mk = (extra: number): Transaction => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 1_000 + extra })
    return tx
  }
  const tx = mk(0)
  await tx.sign()
  const r = await chain.broadcast(tx)
  assert.equal(r.status, 'success', r.description)
  assert.ok((await chain.pendingBroadcasts()) >= 1)
  assert.equal(await chain.utxoStatus(out.tx.id('hex'), out.vout), 'spent')

  const dbl = mk(1)
  await dbl.sign()
  const rd = await chain.broadcast(dbl)
  assert.equal(rd.status, 'error')
  assert.equal(chain.isRejected(dbl.id('hex')), true)

  await chain.mine()
  const mined = chain.getTx(tx.id('hex')) ?? (await chain.fetchTx(tx.id('hex')))
  assert.ok(mined?.merklePath, 'mined tx must carry a proof')
  assert.equal(await mined.merklePath!.verify(tx.id('hex'), chain), true)
  assert.equal(await chain.pendingBroadcasts(), 0)
  const tip = await chain.tipHeight()
  assert.ok((await chain.currentHeight()) >= tip)
})
