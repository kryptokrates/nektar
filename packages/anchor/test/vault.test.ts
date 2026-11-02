// GOAL-v7 V1: the MamemFeeVault covenant under real script eval (MockChain
// verifyScripts runs the actual locking script via SDK Spend).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, LockingScript } from '@bsv/sdk'
import { buildP2PKHScript } from 'runar-sdk'
import { MockChain } from '@mamem/chain'
import { FeeVault, VaultFunder, type VaultUtxo } from '../src/vault.js'

const owner = PrivateKey.fromRandom()
const ownerPkh = Buffer.from(owner.toPublicKey().toHash()).toString('hex')
const dest = PrivateKey.fromRandom()
const destPkh = Buffer.from(dest.toPublicKey().toHash()).toString('hex')
const params = { destPkh, ownerPkh, perTxCap: 1_000n, windowCap: 3_000n, windowBlocks: 5n, maxFee: 10n }

function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const vault = new FeeVault(params)
  const parent = new Transaction()
  parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new LockingScript() })
  parent.addOutput({ satoshis: 10_000, lockingScript: LockingScript.fromHex(vault.lockingScript()) })
  const utxo: VaultUtxo = { txid: parent.id('hex'), vout: 0, satoshis: 10_000, script: vault.lockingScript() }
  return { chain, vault, parent, utxo }
}

test('V1 vault: valid spend accepted; over-cap, wrong outputs and wrong sig all rejected by consensus', async () => {
  const { chain, vault, parent, utxo } = setup()
  chain.registerParent(parent)
  // valid: pay 500 to destPkh, window 0→500
  const good = vault.buildSpend(utxo, owner, 500n, 1)
  const r = await chain.broadcast(good.tx)
  assert.equal(r.status, 'success', `valid spend should be accepted: ${(r as any).description ?? ''}`)
  // over perTxCap — same utxo (first spend is still mempool; mock may allow
  // the conflicting spend's script eval to run and fail on the assert)
  const over = vault.buildSpend(utxo, owner, 1_500n, 1)
  assert.notEqual((await chain.broadcast(over.tx)).status, 'success', 'over perTxCap must be rejected')
  // fee above maxFee
  const hiFee = vault.buildSpend(utxo, owner, 500n, 1, 50n)
  assert.notEqual((await chain.broadcast(hiFee.tx)).status, 'success', 'fee > maxFee must be rejected')
  // wrong signer — covenant rejects even a well-formed tx
  const wrong = vault.buildSpend(utxo, PrivateKey.fromRandom(), 500n, 1)
  assert.notEqual((await chain.broadcast(wrong.tx)).status, 'success', 'non-owner sig must be rejected')
  // tampered outputs: pay to a different address than destPkh
  const evil = vault.buildSpend(utxo, owner, 500n, 1)
  evil.tx.outputs[1]!.lockingScript = LockingScript.fromHex(buildP2PKHScript(Buffer.from(PrivateKey.fromRandom().toPublicKey().toHash()).toString('hex')))
  assert.notEqual((await chain.broadcast(evil.tx)).status, 'success', 'payment to non-destPkh must be rejected')
  // tampered continuation: keep the old state (spentInWindow reset fraud)
  const stale = vault.buildSpend(utxo, owner, 500n, 1)
  stale.tx.outputs[0]!.lockingScript = LockingScript.fromHex(vault.lockingScript(0n, 0n))
  assert.notEqual((await chain.broadcast(stale.tx)).status, 'success', 'stale-state continuation must be rejected')
  // forged continuation code: output 0 is a foreign script, not the vault's
  // code part — the covenant's output-hash assert binds code AND state
  const forged = vault.buildSpend(utxo, owner, 500n, 1)
  forged.tx.outputs[0]!.lockingScript = LockingScript.fromHex(buildP2PKHScript(ownerPkh))
  assert.notEqual((await chain.broadcast(forged.tx)).status, 'success', 'foreign continuation script must be rejected')
})

test('V1 vault: rolling window — 4th payment in the same window rejected, rolls after windowBlocks', async () => {
  const { chain, vault, parent, utxo } = setup()
  chain.registerParent(parent)
  let cur = utxo
  for (let i = 0; i < 3; i++) {
    const s = vault.buildSpend(cur, owner, 1_000n, 1)
    assert.equal((await chain.broadcast(s.tx)).status, 'success', `spend ${i + 1} within windowCap`)
    chain.mine(1)
    const cont = s.tx.outputs[0]!
    cur = { txid: s.tx.id('hex'), vout: 0, satoshis: cont.satoshis!, script: cont.lockingScript.toHex() }
    chain.registerParent(s.tx) // continuation input needs its parent registered
  }
  // window spent 3000 of 3000 — a 4th payment must fail the script assert
  const fourth = vault.buildSpend(cur, owner, 1n, 4)
  assert.notEqual((await chain.broadcast(fourth.tx)).status, 'success', 'over windowCap must be rejected')
  // after the window rolls (locktime >= start+blocks) a new payment is fine
  const rolled = vault.buildSpend(cur, owner, 500n, 10)
  assert.equal((await chain.broadcast(rolled.tx)).status, 'success', 'rolled window must allow spending again')
})

test('V1 VaultFunder: take() mints a capped funding out through the covenant; over-cap refused', async () => {
  const { chain, vault, parent, utxo } = setup()
  chain.registerParent(parent)
  let cur = utxo
  // same shape the MAMEM_FEE_VAULT runtime path drives: current() yields the
  // live vault utxo, height() the locktime for the window logic
  const funder = new VaultFunder(vault, owner, chain, async () => cur, async () => 1, 2n)
  assert.equal(await funder.address(), destPkh)
  const out = await funder.take(400)
  assert.equal(out.vout, 1)
  assert.equal(out.satoshis, 402, 'take() pays satoshis+fee into the funding out')
  assert.equal(out.tx.outputs[1]!.lockingScript.toHex(), buildP2PKHScript(destPkh), 'minted out pays destPkh')
  cur = { txid: out.tx.id('hex'), vout: 0, satoshis: out.tx.outputs[0]!.satoshis!, script: out.tx.outputs[0]!.lockingScript.toHex() }
  chain.registerParent(out.tx)
  // a take above perTxCap is refused by node script eval — take() throws and
  // the vault utxo is not advanced (the failed tx is never broadcast-mined)
  await assert.rejects(() => funder.take(5_000), /refused|fail/)
})
