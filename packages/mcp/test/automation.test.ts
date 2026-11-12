// Automation friction regressions — the fixes from the mainnet session study
// (docs/AUTOMATION.md): smallest-sufficient reserve picks, spend-check pruning,
// one-call broadcast sends, and bond covenant spend serialization.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ReserveFunder, stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { NektarMemory } from '../src/memory.js'
import { PrivateKey, Transaction, P2PKH, Script } from '@bsv/sdk'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const mkParent = (key: PrivateKey, satoshis: number, tag: number): Transaction => {
  const t = new Transaction()
  t.addInput({
    sourceTXID: Buffer.from(`parent-${tag}`.padEnd(32, 'x')).toString('hex').slice(0, 64),
    sourceOutputIndex: 0,
    unlockingScript: Script.fromHex('51')
  })
  t.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis })
  return t
}

test('takeReserve: smallest-sufficient pick — dust never selected for a big spend', async t => {
  const key = PrivateKey.fromRandom()
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-reserve-')), 'reserve.json')
  const r = new ReserveFunder(key, file)
  r.addOwned(mkParent(key, 500, 1), 0)
  r.addOwned(mkParent(key, 50_000, 2), 0)
  r.addOwned(mkParent(key, 5_000, 3), 0)
  const u = await r.takeReserve(undefined, 4_000)
  assert.equal(u?.satoshis, 5_000, 'picked the smallest entry that covers the spend, not the first/dust entry')
  const small = await r.takeReserve(undefined, 400)
  assert.equal(small?.satoshis, 500)
})

test('takeReserve: bare call never hands out sub-dust change entries', async t => {
  const key = PrivateKey.fromRandom()
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-reserve-')), 'reserve.json')
  const r = new ReserveFunder(key, file)
  r.addOwned(mkParent(key, 1, 1), 0) // 1-sat change output — real but unspendable alone
  r.addOwned(mkParent(key, 5_000, 2), 0)
  const u = await r.takeReserve()
  assert.equal(u?.satoshis, 5_000, 'bare takeReserve skips entries below the 546 dust floor')
  const explicit = await r.takeReserve(undefined, 1)
  assert.equal(explicit?.satoshis, 1, 'an explicit minimum below the floor still works')
})

test('takeReserve: null when every entry is insufficient — no negative-fee tx', async t => {
  const key = PrivateKey.fromRandom()
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-reserve-')), 'reserve.json')
  const r = new ReserveFunder(key, file)
  r.addOwned(mkParent(key, 500, 1), 0)
  r.addOwned(mkParent(key, 900, 2), 0)
  assert.equal(await r.takeReserve(undefined, 10_000), null)
})

test('takeReserve: spendCheck prunes chain-spent candidates and tries the next', async t => {
  const key = PrivateKey.fromRandom()
  const file = join(mkdtempSync(join(tmpdir(), 'nektar-reserve-')), 'reserve.json')
  const r = new ReserveFunder(key, file)
  const dead = mkParent(key, 1_000, 1)
  const live = mkParent(key, 2_000, 2)
  r.addOwned(dead, 0)
  r.addOwned(live, 0)
  const deadOp = `${dead.id('hex')}:0`
  const u = await r.takeReserve(async (t, v) => `${t}:${v}` === deadOp, 500)
  assert.equal(u?.tx.id('hex'), live.id('hex'), 'spent candidate pruned, next-smallest tried')
  assert.ok(r.isSpent(dead.id('hex'), 0), 'pruned candidate marked spent')
})

test('sendSats: one call broadcasts a P2PKH spend to the payee + tracks change', async t => {
  const w = await stackWorld(t, 3, 50_000)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, {})
  const payee = PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])
  const r = await mem.sendSats({ amount: 5_000, address: payee })
  const tx = (chain as any).known.get(r.txid) as Transaction
  assert.ok(tx, 'tx aired on the chain, not just returned as BEEF')
  assert.equal(tx.outputs[0]!.satoshis, 5_000)
  assert.equal(tx.outputs[0]!.lockingScript.toHex(), new P2PKH().lock(payee).toHex())
})

test('sendSats: scriptHex target + opReturn tag', async t => {
  const w = await stackWorld(t, 1, 50_000)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, {})
  const payeeScript = new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toAddress([0x6f])).toHex()
  const r = await mem.sendSats({ amount: 1_000, scriptHex: payeeScript, opReturn: 'nektar-test' })
  const tx = (chain as any).known.get(r.txid) as Transaction
  assert.ok(tx.outputs.some((o) => o.satoshis === 1_000 && o.lockingScript.toHex() === payeeScript))
  assert.ok(
    tx.outputs.some((o) => o.lockingScript.toHex().startsWith('006a')),
    'OP_RETURN output present'
  )
})

test('bond ops serialize: a second covenant spend while one is in flight throws', async t => {
  const w = await stackWorld(t, 5, 100_000)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { bond: { sats: 5_000, epochBlocks: 3, horizonBlocks: 4 } })
  await mem.bondDeploy(5_000)
  const root = 'bb'.repeat(32)
  const p1 = mem.bondCommit(root)
  // the parallel call must hit the in-flight guard deterministically — the
  // guard is set synchronously before the first await
  await assert.rejects(mem.bondCommit(root), /bond op in flight/)
  await p1
})
