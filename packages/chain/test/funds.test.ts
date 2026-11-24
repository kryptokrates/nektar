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
  for (const tx of [unprovenSmall, provenMid, provenLarge]) void store.add(tx, 0)

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
  void store.add(depthOne, 0)
  void store.add(depthTwo, 0)

  const first = await funds.take('deal', 2_000)
  assert.equal(first.tx.id('hex'), depthOne.id('hex'), 'depth-1 ancestry beats smaller depth-2 change')
  const second = await funds.take('deal', 2_000)
  assert.equal(second.tx.id('hex'), depthTwo.id('hex'), 'deeper stock is still reachable once the shallow outs are taken')
})

test('funds: died() never resurrects an input whose parent is refusal-class dead', async () => {
  const key = PrivateKey.fromRandom()
  const addr = key.toAddress()
  const deps = { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') }
  const lock = () => new P2PKH().lock(addr)
  // parent pays us (out at vout 0); draft consumes it then dies — the
  // consume's tombstone is the state spent() leaves behind
  const parent = new Transaction()
  parent.addOutput({ lockingScript: lock(), satoshis: 50_000 })
  const draft = new Transaction()
  draft.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  draft.addOutput({ lockingScript: lock(), satoshis: 30_000 })
  await draft.sign()
  // refusal-class parent (the 'parent rejected (ancestor X)' shape): the
  // out looks unspent but no child built on it can ever air — reviving it
  // just re-poisons the next take, the starvation loop this test guards
  const deadStore = listFundsStore()
  const dead = new MemoryFunds(deadStore, deps)
  dead.deadParent = (tx) => Promise.resolve(tx.id('hex') === parent.id('hex'))
  void deadStore.add(parent, 0)
  void deadStore.drop(parent.id('hex'), 0)
  await dead.died(draft)
  assert.equal((await dead.list()).length, 0, 'a sealed-dead parent keeps its outs tombstoned')
  assert.equal(deadStore.tombstones.has(`${parent.id('hex')}:0`), true)
  // control: with no dead verdict the same tombstone revives as before
  const liveStore = listFundsStore()
  const live = new MemoryFunds(liveStore, deps)
  void liveStore.add(parent, 0)
  void liveStore.drop(parent.id('hex'), 0)
  await live.died(draft)
  assert.equal((await live.list()).length, 1, 'a live parent still heals the dead draft')
})

test('funds: a deadParents oracle answers dead — a later deadParent write cannot remove it', async () => {
  const key = PrivateKey.fromRandom()
  const addr = key.toAddress()
  const deps = { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') }
  const lock = () => new P2PKH().lock(addr)
  const parent = new Transaction()
  parent.addOutput({ lockingScript: lock(), satoshis: 50_000 })
  const draft = new Transaction()
  draft.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  draft.addOutput({ lockingScript: lock(), satoshis: 30_000 })
  await draft.sign()
  const deadStore = listFundsStore()
  const dead = new MemoryFunds(deadStore, deps)
  // the append seam: a registered mark source answers before deadParent —
  // and a later deadParent assignment (the clobber the seam exists for)
  // cannot remove it
  dead.deadParents.push((tx) => Promise.resolve(tx.id('hex') === parent.id('hex')))
  dead.deadParent = () => Promise.resolve(false)
  void deadStore.add(parent, 0)
  void deadStore.drop(parent.id('hex'), 0)
  await dead.died(draft)
  assert.equal((await dead.list()).length, 0, 'the registered mark still vets the revive')
  assert.equal(deadStore.tombstones.has(`${parent.id('hex')}:0`), true)
})

test('funds: starve sweep throttles repeated liveSet calls and resets on adopt', async () => {
  const key = PrivateKey.fromRandom()
  const addr = key.toAddress()
  const deps = { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') }
  const lock = () => new P2PKH().lock(addr)
  const store = listFundsStore()
  const funds = new MemoryFunds(store, deps)
  const tomb = new Transaction()
  tomb.addOutput({ lockingScript: lock(), satoshis: 50_000 })
  void store.add(tomb, 0)
  void store.drop(tomb.id('hex'), 0)

  let liveSetCalls = 0
  funds.liveSet = () => {
    liveSetCalls++
    return Promise.resolve(new Set<string>())
  }
  await assert.rejects(() => funds.take('upkeep', 10_000))
  assert.equal(liveSetCalls, 1, 'first starve triggers one liveSet scan')
  await assert.rejects(() => funds.take('upkeep', 10_000))
  assert.equal(liveSetCalls, 1, 'repeat starve inside cooldown skips liveSet scan')

  const small = new Transaction()
  small.addOutput({ lockingScript: lock(), satoshis: 1_000 })
  await funds.adopt(small, 0)
  await assert.rejects(() => funds.take('upkeep', 10_000))
  assert.equal(liveSetCalls, 2, 'adopt() resets the starve-sweep cooldown')
})

test('funds: takeMany consolidates fragmented outputs, skips dust, and releases cleanly', async () => {
  const key = PrivateKey.fromRandom()
  const addr = key.toAddress()
  const deps = { pkhHex: Buffer.from(key.toPublicKey().toHash()).toString('hex') }
  const lock = () => new P2PKH().lock(addr)
  const store = listFundsStore()
  const funds = new MemoryFunds(store, deps)
  // 1 dust output (300 sats) + 4 distinct small outputs (4001..4004 sats)
  for (const sats of [300, 4_001, 4_002, 4_003, 4_004]) {
    const t = new Transaction()
    t.addOutput({ lockingScript: lock(), satoshis: sats })
    await funds.adopt(t, 0)
  }
  const picked = await funds.takeMany('upkeep', 10_000, { minOutputSats: 600, maxInputs: 4, selfPay: true })
  assert.equal(picked.length, 3, 'picks 3 small outputs (>= 12000 sats), skipping 300-sat dust')
  assert.ok(picked.every((u) => u.satoshis >= 4_001))
  for (const u of picked) funds.release(u.tx.id('hex'), u.vout)
  assert.equal((await funds.outs()).length, 5, 'releasing restores all picked outpoints')
})
