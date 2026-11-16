// MemoryChain — the in-memory tier must behave like the node: real admission
// rules (parents, spent set, finality, value, ALWAYS-on script validation),
// real headers whose PoW passes headerPowValid, real BUMPs that verify with
// the chain itself as tracker.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, P2PKH, Script, Transaction } from '@bsv/sdk'
import { headerPowValid, linksTo } from '../src/headers.js'
import { MemoryChain, MemoryFunder } from '../src/memchain.js'

const spendOne = async (funder: MemoryFunder, sats = 1_000): Promise<Transaction> => {
  const out = await funder.take(sats + 1_000)
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: sats })
  await tx.sign()
  return tx
}

test('broadcast then getTx has no path; after mine() the path verifies with the chain as tracker', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 3, 10_000)
  const tx = await spendOne(funder)
  assert.equal((await chain.broadcast(tx)).status, 'success')
  assert.equal(chain.getTx(tx.id('hex'))?.merklePath, undefined)
  await chain.mine()
  const mined = chain.getTx(tx.id('hex'))!
  assert.ok(mined.merklePath)
  assert.equal(await mined.merklePath.verify(tx.id('hex'), chain), true)
  const tip = await chain.tipHeight()
  const header = (await chain.headerAtHeight(tip))!
  assert.equal(headerPowValid(header), true)
  const prev = (await chain.headerAtHeight(tip - 1))!
  assert.equal(linksTo(header, prev), true)
  assert.equal(await chain.isValidRootForHeight(header.merkleRoot, tip), true)
})

test('double spend refused (definitive class); unknown parent refused (transient); bad signature refused', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 4, 10_000)
  const out = await funder.take(3_000)
  const mk = (extra: number): Transaction => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 1_000 + extra })
    return tx
  }
  const a = mk(0)
  await a.sign()
  assert.equal((await chain.broadcast(a)).status, 'success')
  const b = mk(1)
  await b.sign()
  const rb = await chain.broadcast(b)
  assert.equal(rb.status, 'error')
  assert.match(rb.description ?? '', /UTXO_SPENT|already.*spent/i)
  assert.equal(chain.isRejected(b.id('hex')), true) // definitive refusal is terminal

  // unknown parent — a tx the chain has never seen
  const orphan = new Transaction()
  orphan.addInput({ sourceTXID: 'bb'.repeat(32), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
  orphan.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 500 })
  const ro = await chain.broadcast(orphan)
  assert.equal(ro.status, 'error')
  assert.match(ro.description ?? '', /TX_MISSING_PARENT|missing.*(input|parent)/i)
  assert.equal(chain.isRejected(orphan.id('hex')), false) // transient — never terminal

  // bad signature — real parent, wrong key
  const wrongKey = PrivateKey.fromRandom()
  const out2 = await funder.take(2_000)
  const bad = new Transaction()
  bad.addInput({ sourceTransaction: out2.tx, sourceOutputIndex: out2.vout, unlockingScriptTemplate: new P2PKH().unlock(wrongKey) })
  bad.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 500 })
  await bad.sign()
  const rs = await chain.broadcast(bad)
  assert.equal(rs.status, 'error')
  assert.match(rs.description ?? '', /TX_INVALID|script/i)
})

test('non-final tx refused now and accepted after the locktime height passes', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 2, 10_000)
  const tip = await chain.tipHeight()
  const out = await funder.take(3_000)
  const tx = new Transaction()
  tx.addInput({
    sourceTransaction: out.tx,
    sourceOutputIndex: out.vout,
    unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair),
    sequence: 0xfffffffe
  })
  tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 1_000 })
  tx.lockTime = tip + 5
  await tx.sign()
  const r1 = await chain.broadcast(tx)
  assert.equal(r1.status, 'error')
  assert.match(r1.description ?? '', /non.?final/i)
  await chain.mine(5)
  assert.equal((await chain.broadcast(tx)).status, 'success')
})

test('onStatus has delivered mined+path by the time mine() resolves; waitProof resolves', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 2, 10_000)
  const events: Array<{ txid: string; status: string; hasPath: boolean }> = []
  chain.onStatus((ev) => events.push({ txid: ev.txid, status: ev.status, hasPath: ev.merklePath !== undefined }))
  const tx = await spendOne(funder)
  await chain.broadcast(tx)
  await chain.mine()
  const hit = events.find((e) => e.txid === tx.id('hex') && e.status === 'mined')
  assert.ok(hit, 'mined event must have been emitted before mine() resolved')
  assert.equal(hit.hasPath, true)
  const proven = await chain.waitProof(tx.id('hex'), 2_000)
  assert.ok(proven?.merklePath)
  chain.stopWatching()
})

test('reorg(1): tx back in mempool without a path, onReorg fired, re-mining gives a new valid path', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 2, 10_000)
  const reorgs: number[] = []
  chain.onReorg((removed) => reorgs.push(removed.length))
  const minedEvs: Array<{ txid: string; height: number }> = []
  chain.onStatus((ev) => {
    if (ev.status === 'mined') minedEvs.push({ txid: ev.txid, height: ev.blockHeight ?? -1 })
  })
  const tx = await spendOne(funder)
  await chain.broadcast(tx)
  await chain.mine()
  const h1 = await chain.tipHeight()
  assert.ok(chain.getTx(tx.id('hex'))!.merklePath)
  assert.equal(minedEvs.filter((e) => e.txid === tx.id('hex')).length, 1)
  chain.reorg(1)
  assert.deepEqual(reorgs, [1])
  assert.equal(await chain.tipHeight(), h1 - 1)
  assert.equal(chain.getTx(tx.id('hex'))!.merklePath, undefined)
  // both the funding tx and the spend were sealed in that block — both return
  assert.equal(await chain.pendingBroadcasts(), 2)
  await chain.mine()
  const again = chain.getTx(tx.id('hex'))!
  assert.ok(again.merklePath)
  assert.equal(await again.merklePath.verify(tx.id('hex'), chain), true)
  // reorg re-watched the tx: the re-mine delivered a SECOND mined event
  const after = minedEvs.filter((e) => e.txid === tx.id('hex'))
  assert.equal(after.length, 2)
  assert.equal(after[1]!.height, h1) // new tip, not the orphaned block
  chain.stopWatching()
})

test('faults.hold keeps a tx unmined across mine(); faults.observe=timeout makes observe throw', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 3, 10_000)
  const tx = await spendOne(funder)
  await chain.broadcast(tx)
  chain.faults.hold(tx.id('hex'))
  await chain.mine(2)
  assert.equal(chain.getTx(tx.id('hex'))!.merklePath, undefined)
  chain.faults.release(tx.id('hex'))
  await chain.mine()
  assert.ok(chain.getTx(tx.id('hex'))!.merklePath)

  chain.faults.observe = 'timeout'
  const tx2 = await spendOne(funder)
  await chain.broadcast(tx2)
  await chain.mine()
  // observe threw ("couldn't ask") — the stored tx still carries the path,
  // but no verified event could have been delivered
  assert.equal(await chain.chainEvents.waitFor(tx2.id('hex'), 300), undefined)
  chain.faults.observe = undefined
  const ev = await chain.chainEvents.probeNow(tx2.id('hex'))
  assert.equal(ev?.status, 'mined')
})

test('reject(txid) frees the inputs and reports rejected', async () => {
  const chain = new MemoryChain()
  const funder = await MemoryFunder.fund(chain, 2, 10_000)
  const out = await funder.take(3_000)
  const mk = (): Transaction => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: 1_000 })
    return tx
  }
  const a = mk()
  await a.sign()
  assert.equal((await chain.broadcast(a)).status, 'success')
  assert.equal(chain.reject(a.id('hex')), true)
  assert.equal(chain.isRejected(a.id('hex')), true)
  const b = mk()
  await b.sign()
  assert.equal((await chain.broadcast(b)).status, 'success') // inputs freed
})

test('two concurrent MemoryFunder.fund calls both succeed and their outputs are spendable zero-conf', async () => {
  const chain = new MemoryChain()
  const [fa, fb] = await Promise.all([MemoryFunder.fund(chain, 2, 5_000), MemoryFunder.fund(chain, 2, 5_000)])
  const spend = async (f: MemoryFunder): Promise<string> => {
    const out = await f.take(1_500)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(f.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(f.address()), satoshis: 1_000 })
    await tx.sign()
    const r = await chain.broadcast(tx)
    assert.equal(r.status, 'success', r.description)
    return tx.id('hex')
  }
  const [ta, tb] = await Promise.all([spend(fa), spend(fb)])
  assert.notEqual(ta, tb)
})

test('constructing a MemoryChain takes under 500 ms', () => {
  const t0 = Date.now()
  new MemoryChain()
  assert.ok(Date.now() - t0 < 500, `constructor took ${Date.now() - t0}ms`)
})

test('mining past height 300 works and every coinbase txid is distinct', async () => {
  const chain = new MemoryChain()
  // the constructor pre-mined 101; mine enough to pass height 300 — the
  // coinbase unlocking script must stay well-formed at 3-hex-digit heights
  await chain.mine(210)
  const tip = await chain.tipHeight()
  assert.ok(tip >= 300, `tip ${tip}`)
  const ids = chain.faucetUtxos.map((u) => u.tx.id('hex'))
  assert.equal(new Set(ids).size, ids.length, 'coinbase txids must be unique per height')
  const header = (await chain.headerAtHeight(tip))!
  assert.equal(headerPowValid(header), true)
  assert.equal(linksTo(header, (await chain.headerAtHeight(tip - 1))!), true)
})
