// GOAL-v7 V2: the NektarCheckpointBond covenant under real node admission —
// buildCommit/buildSeize/buildClose broadcast through the regtest node, so
// every rejection below is the node's own script evaluation.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript, P2PKH } from '@bsv/sdk'
import { stackWorld, type StackChain, type StackFunder } from '@nektar/chain'
import { CheckpointBond, type BondUtxo } from '../src/bond.js'
import { buildP2PKHScript } from '@nektar/anchor'

const operator = PrivateKey.fromRandom()
const operatorPkh = Buffer.from(operator.toPublicKey().toHash()).toString('hex')
const cfg = { operatorPkh, bondReleaseHeight: 100n, maxFee: 10n, epochLen: 10n }
const R1 = 'aa'.repeat(32)
const R2 = 'bb'.repeat(32)

type World = { chain: StackChain; funder: StackFunder; bond: CheckpointBond }
async function setup(t: { skip: (m?: string) => void }): Promise<World | undefined> {
  const w = await stackWorld(t, 8, 20_000)
  if (w === undefined) return undefined
  return { chain: w.chain, funder: w.funder, bond: new CheckpointBond(cfg) }
}

/** a real bond utxo — funding tx admitted by the node */
async function bondOut(w: World, script: string, satoshis = 10_000): Promise<BondUtxo> {
  const out = await w.funder.take(satoshis + 2_000)
  const fund = new Transaction()
  fund.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(w.funder.keyPair) })
  fund.addOutput({ satoshis, lockingScript: LockingScript.fromHex(script) })
  fund.addOutput({ lockingScript: new P2PKH().lock(w.funder.address()), change: true })
  await fund.fee()
  await fund.sign()
  assert.equal((await w.chain.broadcast(fund)).status, 'success', 'bond funding must be admitted')
  return { txid: fund.id('hex'), vout: 0, satoshis, script }
}

test('V2 bond: commit chain continues n→n+1 with lastRoot; wrong signer rejected', async (t) => {
  const w = await setup(t)
  if (w === undefined) return
  const { chain, bond } = w
  const utxo = await bondOut(w, bond.lockingScript())
  // genesis deadline = releaseHeight (100) — a commit at height 50 renews to 60
  const c1 = await bond.buildCommit(utxo, operator, R1, 50, chain)
  assert.deepEqual(c1.state, { n: 1n, lastRoot: R1, deadline: 60n, seized: 0n })
  // continuation carries the state on-chain — only n+1 can ever continue
  const cont = c1.tx.outputs[0]
  const u2: BondUtxo = { txid: c1.tx.id('hex'), vout: 0, satoshis: cont.satoshis!, script: cont.lockingScript.toHex() }
  await chain.mine(1)
  const c2 = await bond.buildCommit(u2, operator, R2, 55, chain)
  assert.deepEqual(c2.state, { n: 2n, lastRoot: R2, deadline: 65n, seized: 0n })
  // non-operator commit — consensus rejects
  await assert.rejects(() => bond.buildCommit(u2, PrivateKey.fromRandom(), R1, 55, chain), /refused|SCRIPT|error/i)
  // LATE commit: lockTime >= deadline (60) — the covenant refuses, bond now seizable
  await assert.rejects(() => bond.buildCommit(u2, operator, R2, 60, chain), /refused|SCRIPT|error/i)
  // fee above maxFee — the leftover bound must reject it
  await assert.rejects(() => bond.buildCommit(u2, operator, R2, 55, chain, 500n), /refused|SCRIPT|error/i)
})

test('V2 bond: close gated by release height', async (t) => {
  const w = await setup(t)
  if (w === undefined) return
  const { chain, bond } = w
  const utxo = await bondOut(w, bond.lockingScript())
  // too early — locktime below release height must be refused by consensus
  await assert.rejects(() => bond.buildClose(utxo, operator, 50, chain), /refused|SCRIPT|error/i)
  // at release height — bond pays out to operator
  const close = await bond.buildClose(utxo, operator, 100, chain)
  assert.ok(close.tx.id('hex').length === 64, 'close at release accepted')
})

test('V2 seize: deadline lapse → permissionless slashing (§12 SLA enforcement)', async (t) => {
  const w = await setup(t)
  if (w === undefined) return
  const { chain, bond } = w
  const utxo = await bondOut(w, bond.lockingScript())
  // commit at 50 → deadline 60
  const c1 = await bond.buildCommit(utxo, operator, R1, 50, chain)
  const u2: BondUtxo = { txid: c1.tx.id('hex'), vout: 0, satoshis: c1.tx.outputs[0].satoshis!, script: c1.tx.outputs[0].lockingScript.toHex() }

  // too early: lockTime 59 < deadline 60 — refused even by a stranger
  await assert.rejects(() => bond.buildSeize(u2, buildP2PKHScript(operatorPkh), 59, chain), /refused|SCRIPT|error/i)

  // deadline reached: a FOREIGN key (no operator sig anywhere) seizes the bond
  const seizerPkh = Buffer.from(PrivateKey.fromRandom().toPublicKey().toHash()).toString('hex')
  const se = await bond.buildSeize(u2, buildP2PKHScript(seizerPkh), 60, chain)
  assert.equal(se.tx.outputs[0].lockingScript.toHex(), buildP2PKHScript(seizerPkh), 'payout to the seizer, not the operator')
  assert.ok(se.tx.outputs[0].satoshis! > u2.satoshis - 20, 'seizer takes the bond minus bounded fee')
  const husk = bond.stateOf(se.tx.outputs[1].lockingScript.toHex())
  assert.equal(husk.seized, 1n, 'husk continuation marked seized')
  // the husk cannot be committed further — a 1-sat input can't even fund a
  // commit, and the covenant asserts seized==0 regardless
  await assert.rejects(
    () => bond.buildCommit({ txid: se.tx.id('hex'), vout: 1, satoshis: 1, script: se.tx.outputs[1].lockingScript.toHex() }, operator, R2, 61, chain),
    /refused|SCRIPT|satoshis|error/i
  )
})

test('V2 seize boundary: exactly at deadline seizable, commit exactly at deadline refused', async (t) => {
  const w = await setup(t)
  if (w === undefined) return
  const { chain, bond } = w
  const utxo = await bondOut(w, bond.lockingScript())
  const c1 = await bond.buildCommit(utxo, operator, R1, 90, chain) // deadline = 100 = releaseHeight
  const u2: BondUtxo = { txid: c1.tx.id('hex'), vout: 0, satoshis: c1.tx.outputs[0].satoshis!, script: c1.tx.outputs[0].lockingScript.toHex() }
  // commit AT the deadline boundary: lt < deadline fails (100 < 100 false)
  await assert.rejects(() => bond.buildCommit(u2, operator, R2, 100, chain), /refused|SCRIPT|error/i)
  // seize AT the same boundary: lt >= deadline holds
  const se = await bond.buildSeize(u2, buildP2PKHScript(operatorPkh), 100, chain)
  assert.ok(se.tx.id('hex').length === 64, 'seize exactly at deadline must succeed')
})
