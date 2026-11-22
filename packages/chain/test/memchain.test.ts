// memchain — the in-memory tier's own guarantees, independent of the tier
// env: real PoW headers, SDK-checked scripts, node-like admission wording,
// proof delivery through ChainEvents, reorg, and the fault switches.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, PrivateKey } from '@bsv/sdk'
import { MemoryChain, MemoryTreasury, MemoryFunder, memoryTreasury } from '../src/memchain.js'
import { serializeHeader, headerPowValid, linksTo } from '../src/headers.js'
import type { TxStatusEvent } from '../src/interfaces.js'

const newChain = (opts?: ConstructorParameters<typeof MemoryChain>[0]) => {
  const key = PrivateKey.fromRandom()
  const chain = new MemoryChain({ treasuryKey: key, premine: 110, ...opts })
  return { chain, key }
}

const spend = async (chain: MemoryChain, key: PrivateKey, sats = 2_000, parent?: { tx: Transaction; vout: number }): Promise<Transaction> => {
  const src = parent ?? (await fundUtxo(chain, key))
  const t = new Transaction()
  t.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: sats })
  await t.fee()
  await t.sign()
  return t
}

const fundUtxo = async (chain: MemoryChain, key: PrivateKey, sats = 10_000): Promise<{ tx: Transaction; vout: number }> => {
  // a real funded outpoint: mature coinbase → split → mined. The treasury
  // must hold the chain's treasury key — its coinbases pay that address.
  const treasury = new MemoryTreasury(chain, chain.treasuryKey)
  const tx = await treasury.split(key.toPublicKey().toAddress([0x6f]), 1, sats)
  await chain.mine(1)
  return { tx, vout: 0 }
}

test('memchain: every header carries real work and links to its parent', async () => {
  const { chain } = newChain()
  const tip = await chain.currentHeight()
  assert.ok(tip >= 100)
  for (let h = 0; h <= tip; h++) {
    const hdr = await chain.headerAtHeight(h)
    assert.ok(hdr, `no header at ${h}`)
    assert.equal(serializeHeader(hdr)?.length, 80)
    assert.equal(headerPowValid(hdr), true, `header ${h} fails PoW`)
    if (h > 0) assert.ok(linksTo(hdr, (await chain.headerAtHeight(h - 1))!), `header ${h} does not link`)
    if (h === 0) assert.equal(hdr.prevHash, '0'.repeat(64))
  }
})

test('memchain: mined spend proves and SDK-verifies; coinbase leads the block', async () => {
  const { chain, key } = newChain()
  const tx = await spend(chain, key)
  assert.equal((await chain.broadcast(tx)).status, 'success')
  await chain.mine(1)
  const mined = chain.getTx(tx.id('hex'))!
  assert.ok(mined.merklePath)
  const h = mined.merklePath.blockHeight
  assert.equal(mined.merklePath.computeRoot(tx.id('hex')), (await chain.headerAtHeight(h))!.merkleRoot)
  assert.equal(await mined.verify(chain), true)
  // the coinbase's own proof sits at index 0 of the same block
  const cb = chain.blockList[h].txids[0]
  assert.equal((await chain.merklePathOf(cb))!.computeRoot(cb), (await chain.headerAtHeight(h))!.merkleRoot)
})

test('memchain: a wrong-key spend is refused and reported as rejected', async () => {
  const { chain, key } = newChain()
  const src = await fundUtxo(chain, key)
  const t = new Transaction()
  t.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(PrivateKey.fromRandom()) })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 2_000 })
  await t.fee()
  await t.sign()
  const events: TxStatusEvent[] = []
  chain.onStatus((ev) => events.push(ev))
  const r = await chain.broadcast(t)
  assert.equal(r.status, 'error')
  assert.match(r.description!, /TX_INVALID/)
  assert.equal(chain.isRejected(t.id('hex')), true)
  assert.ok(
    events.some((e) => e.txid === t.id('hex') && e.status === 'rejected'),
    'rejected event delivered'
  )
  chain.stopWatching()
})

test('memchain: double spends refuse; identical resubmits succeed', async () => {
  const { chain, key } = newChain()
  const src = await fundUtxo(chain, key)
  const mk = async (sats: number) => {
    const t = new Transaction()
    t.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
    t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: sats })
    await t.fee()
    await t.sign()
    return t
  }
  const first = await mk(2_000)
  assert.equal((await chain.broadcast(first)).status, 'success')
  assert.equal((await chain.broadcast(first)).status, 'success', 'identical resubmit')
  const r2 = await chain.broadcast(await mk(1_900))
  assert.equal(r2.status, 'error')
  assert.match(r2.description!, /UTXO_SPENT/)
  await chain.mine(1)
  const r3 = await chain.broadcast(await mk(1_800))
  assert.equal(r3.status, 'error')
  assert.match(r3.description!, /UTXO_SPENT/)
})

test('memchain: unknown parents are refused as transient', async () => {
  const { chain, key } = newChain()
  const ghost = new Transaction()
  ghost.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 5_000 })
  // the caller attaches the parent — but the chain never saw it, so its own
  // record has no such txid and admission refuses it
  const t = new Transaction()
  t.addInput({ sourceTransaction: ghost, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 2_000 })
  await t.fee()
  await t.sign()
  const r = await chain.broadcast(t)
  assert.equal(r.status, 'error')
  assert.match(r.description!, /TX_MISSING_PARENT/)
  assert.equal(chain.isRejected(t.id('hex')), false)
})

test('memchain: locktime-gated txs are non-final until the tip reaches them', async () => {
  const { chain, key } = newChain()
  const src = await fundUtxo(chain, key)
  const tip = await chain.currentHeight()
  const t = new Transaction()
  t.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(key), sequence: 0xfffffffe })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 2_000 })
  t.lockTime = tip + 5
  await t.fee()
  await t.sign()
  const r = await chain.broadcast(t)
  assert.equal(r.status, 'error')
  assert.match(r.description!, /non-final/)
  assert.equal(chain.isRejected(t.id('hex')), false)
  await chain.mine(5)
  assert.equal((await chain.broadcast(t)).status, 'success', 'final once the tip reaches lockTime')
})

test('memchain: coinbase outputs are unspendable before maturity', async () => {
  const { chain, key } = newChain()
  const tip = await chain.currentHeight()
  const cbid = chain.blockList[tip].txids[0]
  const cb = chain.getTx(cbid)!
  const t = new Transaction()
  t.addInput({ sourceTransaction: cb, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(chain.treasuryKey) })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 1_000 })
  await t.fee()
  await t.sign()
  const r = await chain.broadcast(t)
  assert.equal(r.status, 'error')
  assert.match(r.description!, /premature spend of coinbase/)
})

test('memchain: outputs may not exceed inputs', async () => {
  const { chain, key } = newChain()
  const src = await fundUtxo(chain, key)
  const t = new Transaction()
  t.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 9_999_999 })
  // sign() alone — fee() itself refuses an input-poor tx before the node can
  await t.sign()
  const r = await chain.broadcast(t)
  assert.equal(r.status, 'error')
  assert.match(r.description!, /outputs exceed inputs/)
})

test('memchain: proofs arrive through the status stream and waitProof', async () => {
  const { chain, key } = newChain()
  const events: TxStatusEvent[] = []
  chain.onStatus((ev) => events.push(ev))
  const tx = await spend(chain, key)
  await chain.broadcast(tx)
  await chain.mine(1)
  assert.ok(
    events.some((e) => e.txid === tx.id('hex') && e.status === 'mined' && e.merklePath !== undefined),
    'mined event with proof'
  )
  const proven = await chain.waitProof(tx.id('hex'), 1_000)
  assert.ok(proven?.merklePath)
  assert.ok(chain.getTx(tx.id('hex'))!.merklePath)
  chain.stopWatching()
})

test('memchain: reorg returns mined txs to the mempool and re-proves them', async () => {
  const { chain, key } = newChain()
  const fired: string[] = []
  chain.onReorg((removed, tip) => fired.push(`${removed.length}→${tip.height}`))
  const tx = await spend(chain, key)
  await chain.broadcast(tx)
  await chain.mine(1)
  const h1 = chain.getTx(tx.id('hex'))!.merklePath!.blockHeight
  const hash1 = (await chain.headerAtHeight(h1))!.hash
  chain.reorg(1)
  assert.deepEqual(fired, [`1→${h1 - 1}`])
  assert.equal(await chain.pendingBroadcasts(), 1)
  assert.equal(chain.getTx(tx.id('hex'))!.merklePath, undefined)
  assert.equal(await chain.currentHeight(), h1 - 1)
  await chain.mine(1)
  const proven = chain.getTx(tx.id('hex'))!.merklePath!
  assert.equal(proven.blockHeight, h1, 're-proved at the same height')
  assert.notEqual((await chain.headerAtHeight(h1))!.hash, hash1, 'under a different block hash')
  chain.stopWatching()
})

test('memchain: fault switches — reject, hold, observer', async () => {
  const { chain, key } = newChain()

  // reject: definitive refusal with the given description
  const bad = await spend(chain, key)
  chain.faults.reject = () => 'injected refusal'
  const rr = await chain.broadcast(bad)
  assert.equal(rr.status, 'error')
  assert.equal(rr.description, 'injected refusal')
  assert.equal(chain.isRejected(bad.id('hex')), true)
  chain.faults.reject = undefined

  // hold: admitted but never mined — nor its mempool child
  const held = await spend(chain, key)
  chain.faults.hold = (txid) => txid === held.id('hex')
  assert.equal((await chain.broadcast(held)).status, 'success')
  const child = await spend(chain, key, 1_500, { tx: held, vout: 0 })
  assert.equal((await chain.broadcast(child)).status, 'success')
  await chain.mine(3)
  assert.equal(await chain.merklePathOf(held.id('hex')), undefined)
  assert.equal(await chain.merklePathOf(child.id('hex')), undefined, 'descendant of a held tx stays unmined')
  assert.ok((await chain.pendingBroadcasts()) >= 2)
  chain.faults.hold = undefined
  await chain.mine(1)
  assert.ok(await chain.merklePathOf(held.id('hex')))
  assert.ok(await chain.merklePathOf(child.id('hex')))

  // observer fault: the chain holds the proof but nothing is delivered —
  // getTx stays proofless while the node-side lookup answers
  const tx = await spend(chain, key)
  await chain.broadcast(tx)
  chain.faults.observer = 'timeout'
  await chain.mine(1)
  assert.equal(chain.getTx(tx.id('hex'))!.merklePath, undefined, 'undelivered proof must not attach')
  assert.ok(await chain.merklePathOf(tx.id('hex')), 'the chain itself knows the proof')
  chain.faults.observer = undefined
  // the observe batch is proof-gated on a static tip — the waitFor/nudge path
  // (probeNow) is how a consumer re-asks without waiting for a block
  await chain.chainEvents.probeNow(tx.id('hex'))
  assert.ok(chain.getTx(tx.id('hex'))!.merklePath, 'delivery on the next observe')
  chain.stopWatching()
})

test('memchain: MemoryFunder funds zeroconf parents — proven on demand, like the stack tier', async () => {
  const treasury = memoryTreasury()
  const f = await MemoryFunder.fund(treasury, 3, 10_000)
  const parent = f.parents[0]
  // broadcast and spendable immediately, but unproven until a block takes it —
  // the StackFunder contract: tests mine only where a proof is asserted
  assert.equal(parent.merklePath, undefined)
  assert.equal(await treasury.chain.merklePathOf(parent.id('hex')), undefined)
  for (let i = 0; i < 3; i++) assert.equal(await treasury.chain.utxoStatus(parent.id('hex'), i), 'unspent')
  const u = await f.take(5_000)
  assert.equal(u.tx.id('hex'), parent.id('hex'))
  await treasury.chain.mine(1)
  const mp = await treasury.chain.merklePathOf(parent.id('hex'))
  assert.ok(mp, 'the same parent proves once mined')
  parent.merklePath = mp
  assert.equal(await parent.verify(treasury.chain), true)
  await assert.rejects(MemoryFunder.dry().take(1), /no utxo/)
  treasury.chain.stopWatching()
})

test('memchain: reject(txid) evicts the tx and its mempool descendants, freeing inputs', async () => {
  const { chain, key } = newChain()
  const src = await fundUtxo(chain, key)
  const parent = await spend(chain, key, 5_000, src)
  const child = await spend(chain, key, 4_000, { tx: parent, vout: 0 })
  assert.equal((await chain.broadcast(parent)).status, 'success')
  assert.equal((await chain.broadcast(child)).status, 'success')
  assert.equal(chain.reject(parent.id('hex')), true)
  assert.equal(chain.isRejected(parent.id('hex')), true)
  assert.equal(chain.isRejected(child.id('hex')), true, 'a mempool descendant is evicted with it')
  assert.equal(chain.getTx(child.id('hex')), undefined)
  // src's outpoint was freed — a fresh spend of it is admitted
  const again = await spend(chain, key, 4_500, src)
  assert.equal((await chain.broadcast(again)).status, 'success')
  assert.equal(chain.reject('ff'.repeat(32)), false, 'unknown txid is not evicted')
  chain.stopWatching()
})

test('memchain: concurrent MemoryFunder.fund calls serialize their splits', async () => {
  const treasury = new MemoryTreasury()
  const [fa, fb] = await Promise.all([MemoryFunder.fund(treasury, 2, 5_000), MemoryFunder.fund(treasury, 2, 5_000)])
  assert.notEqual(fa.parents[0].id('hex'), fb.parents[0].id('hex'), 'each funder got its own split tx')
  const ua = await fa.take(4_000)
  const t = new Transaction()
  t.addInput({ sourceTransaction: ua.tx, sourceOutputIndex: ua.vout, unlockingScriptTemplate: new P2PKH().unlock(fa.keyPair) })
  t.addOutput({ lockingScript: new P2PKH().lock(fa.address()), satoshis: 3_000 })
  await t.fee()
  await t.sign()
  assert.equal((await treasury.chain.broadcast(t)).status, 'success')
  treasury.chain.stopWatching()
})

test('memchain: constructing a MemoryChain takes under 500 ms', () => {
  const t0 = Date.now()
  new MemoryChain()
  assert.ok(Date.now() - t0 < 500, `constructor took ${Date.now() - t0}ms`)
})

test('memchain: mining past height 300 works; coinbase txids stay distinct', async () => {
  const { chain } = newChain()
  // pre-mined past 100; push past 300 so BIP34 height pushes take 3 hex digits
  await chain.mine(210)
  const tip = await chain.currentHeight()
  assert.ok(tip >= 300, `tip ${tip}`)
  const ids = chain.blockList.map((b) => b.txids[0])
  assert.equal(new Set(ids).size, ids.length, 'coinbase txids must be unique per block')
  const header = (await chain.headerAtHeight(tip))!
  assert.equal(headerPowValid(header), true)
  assert.equal(linksTo(header, (await chain.headerAtHeight(tip - 1))!), true)
})
