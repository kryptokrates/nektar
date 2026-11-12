// GOAL-v7 V1: the NektarFeeVault covenant under real node admission — every
// spend is broadcast to the regtest node, which runs the locking script at
// the UTXO layer. Each negative case gets its own funded vault outpoint: once
// a spend lands the outpoint is gone and a sibling dies on double-spend
// grounds before the covenant is ever evaluated.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript, P2PKH } from '@bsv/sdk'
import { buildP2PKHScript } from 'runar-sdk'
import { stackWorld, type StackChain, type StackFunder } from '@nektar/chain'
import { FeeVault, VaultFunder, type VaultUtxo } from '../src/vault.js'

const owner = PrivateKey.fromRandom()
const ownerPkh = Buffer.from(owner.toPublicKey().toHash()).toString('hex')
const dest = PrivateKey.fromRandom()
const destPkh = Buffer.from(dest.toPublicKey().toHash()).toString('hex')
const params = { destPkh, ownerPkh, perTxCap: 1_000n, windowCap: 3_000n, windowBlocks: 5n, maxFee: 10n }

type World = { chain: StackChain, funder: StackFunder, vault: FeeVault }
async function setup (t: { skip: (m?: string) => void }): Promise<World | undefined> {
  const w = await stackWorld(t, 16, 20_000)
  if (w === undefined) return undefined
  return { chain: w.chain, funder: w.funder, vault: new FeeVault(params) }
}

/** a real covenant-funded utxo: the node saw and admitted the funding tx */
async function vaultOut (w: World, script: string, satoshis = 10_000): Promise<VaultUtxo> {
  const out = await w.funder.take(satoshis + 2_000)
  const fund = new Transaction()
  fund.addInput({ sourceTransaction: out.tx, sourceOutputIndex: out.vout, unlockingScriptTemplate: new P2PKH().unlock(w.funder.keyPair) })
  fund.addOutput({ satoshis, lockingScript: LockingScript.fromHex(script) })
  fund.addOutput({ lockingScript: new P2PKH().lock(w.funder.address()), change: true })
  await fund.fee(); await fund.sign()
  assert.equal((await w.chain.broadcast(fund)).status, 'success', 'vault funding must be admitted')
  return { txid: fund.id('hex'), vout: 0, satoshis, script }
}

test('V1 vault: valid spend accepted; over-cap, wrong outputs and wrong sig all rejected by consensus', async t => {
  const w = await setup(t); if (w === undefined) return
  const { chain, vault } = w
  const good = vault.buildSpend(await vaultOut(w, vault.lockingScript()), owner, 500n, 1)
  const r = await chain.broadcast(good.tx)
  assert.equal(r.status, 'success', `valid spend should be accepted: ${(r as any).description ?? ''}`)
  // over perTxCap
  const over = vault.buildSpend(await vaultOut(w, vault.lockingScript()), owner, 1_500n, 1)
  assert.notEqual((await chain.broadcast(over.tx)).status, 'success', 'over perTxCap must be rejected')
  // fee above maxFee
  const hiFee = vault.buildSpend(await vaultOut(w, vault.lockingScript()), owner, 500n, 1, 50n)
  assert.notEqual((await chain.broadcast(hiFee.tx)).status, 'success', 'fee > maxFee must be rejected')
  // wrong signer — covenant rejects even a well-formed tx
  const wrong = vault.buildSpend(await vaultOut(w, vault.lockingScript()), PrivateKey.fromRandom(), 500n, 1)
  assert.notEqual((await chain.broadcast(wrong.tx)).status, 'success', 'non-owner sig must be rejected')
  // tampered outputs: pay to a different address than destPkh
  const evil = vault.buildSpend(await vaultOut(w, vault.lockingScript()), owner, 500n, 1)
  evil.tx.outputs[1]!.lockingScript = LockingScript.fromHex(buildP2PKHScript(Buffer.from(PrivateKey.fromRandom().toPublicKey().toHash()).toString('hex')))
  assert.notEqual((await chain.broadcast(evil.tx)).status, 'success', 'payment to non-destPkh must be rejected')
  // tampered continuation: keep the old state (spentInWindow reset fraud)
  const stale = vault.buildSpend(await vaultOut(w, vault.lockingScript()), owner, 500n, 1)
  stale.tx.outputs[0]!.lockingScript = LockingScript.fromHex(vault.lockingScript(0n, 0n))
  assert.notEqual((await chain.broadcast(stale.tx)).status, 'success', 'stale-state continuation must be rejected')
  // forged continuation code: output 0 is a foreign script, not the vault's
  // code part — the covenant's output-hash assert binds code AND state
  const forged = vault.buildSpend(await vaultOut(w, vault.lockingScript()), owner, 500n, 1)
  forged.tx.outputs[0]!.lockingScript = LockingScript.fromHex(buildP2PKHScript(ownerPkh))
  assert.notEqual((await chain.broadcast(forged.tx)).status, 'success', 'foreign continuation script must be rejected')
})

test('V1 vault: rolling window — 4th payment in the same window rejected, rolls after windowBlocks', async t => {
  const w = await setup(t); if (w === undefined) return
  const { chain, vault } = w
  let cur = await vaultOut(w, vault.lockingScript())
  for (let i = 0; i < 3; i++) {
    const s = vault.buildSpend(cur, owner, 1_000n, 1)
    assert.equal((await chain.broadcast(s.tx)).status, 'success', `spend ${i + 1} within windowCap`)
    await chain.mine(1)
    const cont = s.tx.outputs[0]!
    cur = { txid: s.tx.id('hex'), vout: 0, satoshis: cont.satoshis!, script: cont.lockingScript.toHex() }
  }
  // window spent 3000 of 3000 — a 4th payment must fail the script assert
  const fourth = vault.buildSpend(cur, owner, 1n, 4)
  assert.notEqual((await chain.broadcast(fourth.tx)).status, 'success', 'over windowCap must be rejected')
  // after the window rolls (locktime >= start+blocks) a new payment is fine
  const rolled = vault.buildSpend(cur, owner, 500n, 10)
  assert.equal((await chain.broadcast(rolled.tx)).status, 'success', 'rolled window must allow spending again')
})

test('V1 VaultFunder: take() mints a capped funding out through the covenant; over-cap refused', async t => {
  const w = await setup(t); if (w === undefined) return
  const { chain, vault } = w
  let cur = await vaultOut(w, vault.lockingScript())
  // same shape the NEKTAR_FEE_VAULT runtime path drives: current() yields the
  // live vault utxo, height() the locktime for the window logic
  const funder = new VaultFunder(
    vault,
    owner,
    chain,
    async () => cur,
    async () => 1,
    2n
  )
  assert.equal(await funder.address(), destPkh)
  const out = await funder.take(400)
  assert.equal(out.vout, 1)
  assert.equal(out.satoshis, 402, 'take() pays satoshis+fee into the funding out')
  assert.equal(out.tx.outputs[1]!.lockingScript.toHex(), buildP2PKHScript(destPkh), 'minted out pays destPkh')
  cur = { txid: out.tx.id('hex'), vout: 0, satoshis: out.tx.outputs[0]!.satoshis!, script: out.tx.outputs[0]!.lockingScript.toHex() }
  // a take above perTxCap is refused by node script eval — take() throws and
  // the vault utxo is not advanced (the failed tx is never broadcast-mined)
  await assert.rejects(() => funder.take(5_000), /refused|fail|error/i)
})
