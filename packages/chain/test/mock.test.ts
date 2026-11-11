import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MockChain, MockFunder } from '../src/mock.js'
import { Transaction, PrivateKey, P2PKH, Script } from '@bsv/sdk'

const U = Script.fromHex('51')
test('mock chain: mine assigns BUMP consistent with header root', async () => {
  const chain = new MockChain()
  const f = new MockFunder(); f.topup(3)
  const u = await f.take(1)
  const t1 = new Transaction()
  t1.addInput({ sourceTransaction: u.tx, sourceOutputIndex: 0, unlockingScript: U })
  const u2 = await f.take(1)
  const t2 = new Transaction()
  t2.addInput({ sourceTransaction: u2.tx, sourceOutputIndex: 0, unlockingScript: U })
  await chain.broadcast(t1); await chain.broadcast(t2)
  const [h] = chain.mine(1)
  assert.equal(t1.merklePath!.computeRoot(t1.id('hex')), h!.merkleRoot)
  assert.equal(t2.merklePath!.computeRoot(t2.id('hex')), h!.merkleRoot)
  assert.equal(await chain.isValidRootForHeight(h!.merkleRoot, h!.height), true)
  assert.equal(await t1.verify(chain), true)
})

test('mock chain: ancestor depth limit enforced', async () => {
  const chain = new MockChain(2)
  const f = new MockFunder(); f.topup(1)
  const u = await f.take(1)
  const mk = (parent?: { tx: Transaction, vout: number }) => {
    const t = new Transaction()
    t.addInput({ sourceTransaction: (parent ?? u).tx, sourceOutputIndex: (parent ?? u).vout, unlockingScript: U })
    if (parent) t.addOutput({ lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress()), satoshis: 1 })
    return t
  }
  const a = mk()
  assert.equal((await chain.broadcast(a)).status, 'success')
  const b = mk({ tx: a, vout: 0 })
  assert.equal((await chain.broadcast(b)).status, 'success')
  const c = mk({ tx: b, vout: 0 })
  const r = await chain.broadcast(c)
  assert.equal(r.status, 'error') // depth 3 > limit 2
})

test('mock chain: reorg drops mined txs', async () => {
  const chain = new MockChain()
  const f = new MockFunder(); f.topup(1)
  const u = await f.take(1)
  const t = new Transaction()
  t.addInput({ sourceTransaction: u.tx, sourceOutputIndex: 0, unlockingScript: U })
  let events: string[] = []
  chain.onStatus(e => events.push(e.status))
  await chain.broadcast(t)
  chain.mine(1)
  assert.deepEqual(events, ['unconfirmed', 'mined'])
  chain.reorg(1)
  assert.deepEqual(events, ['unconfirmed', 'mined', 'unconfirmed'])
  assert.equal(await chain.isValidRootForHeight('anything', 1), false)
})

test('mock chain: UTXO_SPENT rejects double-spend, reorg releases it', async () => {
  const chain = new MockChain()
  const f = new MockFunder(); f.topup(2)
  const u = await f.take(1)
  // a double-spend is a DIFFERENT txid claiming the same outpoint — identical
  // bytes resubmitted are an idempotent retry (broadcaster 202), not a conflict
  const mk = (sats?: number) => {
    const t = new Transaction()
    t.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScript: U })
    if (sats !== undefined) t.addOutput({ lockingScript: new P2PKH().lock(f.address()), satoshis: sats })
    return t
  }
  const r1 = await chain.broadcast(mk())
  assert.equal(r1.status, 'success')
  const r2 = await chain.broadcast(mk(500))
  assert.equal(r2.status, 'error')
  assert.match(r2.description ?? '', /UTXO_SPENT/)
  chain.mine(1)
  const r3 = await chain.broadcast(mk(600))
  assert.equal(r3.status, 'error', 'mined spend still rejected')
  chain.reorg(1)
  const r4 = await chain.broadcast(mk(700))
  assert.equal(r4.status, 'success', 'reorg releases the outpoint')
})

test('MockFunder: markSpent blocks a utxo, addOwned returns an output to the pool', async () => {
  const f = new MockFunder()
  f.topup(3, 10_000)
  const a = await f.take(1_000)
  const b = await f.take(1_000)
  void a; void b
  const leftover = (f as unknown as { utxos: Array<{ tx: Transaction, vout: number }> }).utxos[0]!
  f.markSpent(leftover.tx.id('hex'), leftover.vout)
  await assert.rejects(f.take(1), /no utxo/)
  const t = new Transaction()
  t.addOutput({ lockingScript: new P2PKH().lock(f.address()), satoshis: 5_000 })
  f.addOwned(t, 0)
  const d = await f.take(4_000)
  assert.equal(d.tx.id('hex'), t.id('hex'))
})
