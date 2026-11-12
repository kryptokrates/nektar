// Real-node conformance — the semantic guarantees the chain backend must hold,
// verified against the actual regtest node: mined BUMPs match the header's
// merkle root, chained unconfirmed spends are admitted, and a double-spend is
// refused. Requires the Teranode stack (skipUnlessStack).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, PrivateKey } from '@bsv/sdk'
import { stackWorld, type StackFunder } from '../src/testkit.js'

const spend = async (f: StackFunder, sats: number, parent?: { tx: Transaction; vout: number }): Promise<Transaction> => {
  const src = parent ?? (await f.take(sats))
  const t = new Transaction()
  t.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(f.keyPair) })
  t.addOutput({ lockingScript: new P2PKH().lock(f.keyPair.toPublicKey().toAddress([0x6f])), satoshis: sats })
  await t.fee()
  await t.sign()
  return t
}

test('chain: mined BUMP root matches the block header root and verifies', async (t) => {
  const w = await stackWorld(t, 3, 4_000)
  if (w === undefined) return
  const { chain, funder } = w
  const t1 = await spend(funder, 2_000)
  const t2 = await spend(funder, 2_000)
  assert.equal((await chain.broadcast(t1)).status, 'success')
  assert.equal((await chain.broadcast(t2)).status, 'success')
  await chain.mine(1)
  for (const tx of [t1, t2]) {
    const mined = await chain.getTx(tx.id('hex'))
    assert.ok(mined?.merklePath, 'node has no proof for a mined tx')
    const hdr = await chain.headerAtHeight(mined.merklePath.blockHeight)
    assert.ok(hdr)
    assert.equal(mined.merklePath.computeRoot(tx.id('hex')), hdr.merkleRoot)
    assert.equal(await mined.verify(chain), true, 'SDK SPV verify failed on a real node proof')
    assert.equal(await chain.isValidRootForHeight(hdr.merkleRoot, hdr.height), true)
  }
})

test('chain: chained unconfirmed spends admit, then prove together', async (t) => {
  const w = await stackWorld(t, 3, 4_000)
  if (w === undefined) return
  const { chain, funder } = w
  const a = await spend(funder, 2_000)
  const b = new Transaction()
  b.addInput({ sourceTransaction: a, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  b.addOutput({ lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress([0x6f])), satoshis: 1_500 })
  await b.fee()
  await b.sign()
  const c = new Transaction()
  c.addInput({ sourceTransaction: b, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  c.addOutput({ lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress([0x6f])), satoshis: 1_000 })
  await c.fee()
  await c.sign()
  // zero-conf chain: the node admits children of mempool parents
  assert.equal((await chain.broadcast(a)).status, 'success', `a: ${(await chain.broadcast(a)).description ?? ''}`)
  assert.equal((await chain.broadcast(b)).status, 'success')
  assert.equal((await chain.broadcast(c)).status, 'success')
  // children of mempool parents can land a block later — mine until all prove
  for (let i = 0; i < 6; i++) {
    await chain.mine(1)
    const proofs = await Promise.all([a, b, c].map((tx) => chain.getTx(tx.id('hex'))))
    if (proofs.every((p) => p?.merklePath !== undefined)) break
  }
  for (const tx of [a, b, c]) assert.ok((await chain.getTx(tx.id('hex')))?.merklePath, `no proof for ${tx.id('hex').slice(0, 8)}`)
})

test('chain: a second spend of the same outpoint is refused', async (t) => {
  const w = await stackWorld(t, 2, 4_000)
  if (w === undefined) return
  const { chain, funder } = w
  const u = await funder.take(3_000)
  const mk = async (sats: number): Promise<Transaction> => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: sats })
    await tx.fee()
    await tx.sign()
    return tx
  }
  const r1 = await chain.broadcast(await mk(2_000))
  assert.equal(r1.status, 'success')
  // a real conflict needs different bytes — identical resubmits are a 202 retry
  const r2 = await chain.broadcast(await mk(1_900))
  assert.equal(r2.status, 'error', 'double-spend admitted')
  await chain.mine(1)
  const r3 = await chain.broadcast(await mk(1_800))
  assert.equal(r3.status, 'error', 'mined spend was re-admitted')
})

test('StackFunder: take, markSpent, addOwned manage the pool like the faucet did', async (t) => {
  const w = await stackWorld(t, 3, 10_000)
  if (w === undefined) return
  const { funder } = w
  await funder.take(1_000)
  await funder.take(1_000)
  const leftover = (funder as unknown as { utxos: Array<{ tx: Transaction; vout: number }> }).utxos[0]!
  funder.markSpent(leftover.tx.id('hex'), leftover.vout)
  await assert.rejects(funder.take(1), /no utxo/)
  const tx = new Transaction()
  tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 5_000 })
  funder.addOwned(tx, 0)
  const d = await funder.take(4_000)
  assert.equal(d.tx.id('hex'), tx.id('hex'))
})
