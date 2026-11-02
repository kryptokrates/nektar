// GOAL-v7 V2: the MamemCheckpointBond covenant under real script eval
// (MockChain verifyScripts runs the actual locking script via SDK Spend).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript } from '@bsv/sdk'
import { MockChain } from '@mamem/chain'
import { CheckpointBond, type BondUtxo } from '../src/bond.js'

const operator = PrivateKey.fromRandom()
const operatorPkh = Buffer.from(operator.toPublicKey().toHash()).toString('hex')
const cfg = { operatorPkh, bondReleaseHeight: 100n, maxFee: 10n }
const R1 = 'aa'.repeat(32)
const R2 = 'bb'.repeat(32)

function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const bond = new CheckpointBond(cfg)
  const parent = new Transaction()
  parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new LockingScript() })
  parent.addOutput({ satoshis: 10_000, lockingScript: LockingScript.fromHex(bond.lockingScript()) })
  const utxo: BondUtxo = { txid: parent.id('hex'), vout: 0, satoshis: 10_000, script: bond.lockingScript() }
  return { chain, bond, parent, utxo }
}

test('V2 bond: commit chain continues n→n+1 with lastRoot; wrong signer rejected', async () => {
  const { chain, bond, parent, utxo } = setup()
  chain.registerParent(parent)
  const c1 = bond.buildCommit(utxo, operator, R1)
  const r1 = await chain.broadcast(c1.tx)
  assert.equal(r1.status, 'success', `commit 1 accepted: ${(r1 as any).description ?? ''}`)
  assert.deepEqual(c1.state, { n: 1n, lastRoot: R1 })
  // continuation carries the state on-chain — only n+1 can ever continue
  const cont = c1.tx.outputs[0]!
  const u2: BondUtxo = { txid: c1.tx.id('hex'), vout: 0, satoshis: cont.satoshis!, script: cont.lockingScript.toHex() }
  chain.registerParent(c1.tx)
  chain.mine(1)
  const c2 = bond.buildCommit(u2, operator, R2)
  const r2 = await chain.broadcast(c2.tx)
  assert.equal(r2.status, 'success', `commit 2 accepted: ${(r2 as any).description ?? ''}`)
  assert.deepEqual(c2.state, { n: 2n, lastRoot: R2 })
  // non-operator commit — consensus rejects
  const bad = bond.buildCommit(u2, PrivateKey.fromRandom(), R1)
  assert.notEqual((await chain.broadcast(bad.tx)).status, 'success', 'non-operator commit must be rejected')
  // fee above maxFee — the leftover bound must reject it
  const hiFee = bond.buildCommit(u2, operator, R2, 500n)
  assert.notEqual((await chain.broadcast(hiFee.tx)).status, 'success', 'fee > maxFee must be rejected')
  // forged continuation: output 0 keeps the sats but swaps in a foreign
  // script — the out-hash assert binds code+state, so consensus rejects
  const { buildP2PKHScript } = await import('runar-sdk')
  const forged = bond.buildCommit(u2, operator, R2)
  forged.tx.outputs[0]!.lockingScript = LockingScript.fromHex(buildP2PKHScript(operatorPkh))
  assert.notEqual((await chain.broadcast(forged.tx)).status, 'success', 'foreign continuation script must be rejected')
})

test('V2 bond: close gated by release height', async () => {
  const { chain, bond, parent, utxo } = setup()
  chain.registerParent(parent)
  // too early — locktime below release height must be refused by consensus
  const early = bond.buildClose(utxo, operator, 50)
  assert.notEqual((await chain.broadcast(early.tx)).status, 'success', 'close before release height must be rejected')
  // at release height — bond pays out to operator
  const close = bond.buildClose(utxo, operator, 100)
  const r = await chain.broadcast(close.tx)
  assert.equal(r.status, 'success', `close at release height accepted: ${(r as any).description ?? ''}`)
})
