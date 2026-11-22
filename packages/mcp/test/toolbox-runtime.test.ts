// toolbox custody through the REAL production path: openRuntime boots with
// NEKTAR_WALLET=toolbox → SqlFunds owns the funding-UTXO lifecycle in
// records.sqlite (treasury.json seeds once, then selection/spent/change live
// in the utxos table) → mem.remember → mem.anchorShard broadcasts through
// Funds.take, not the file-backed ReserveFunder.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH } from '@bsv/sdk'
import { stackChain, skipUnlessStack, stackTreasury, StackFunder, stackOnly } from '@nektar/chain'
import { openRuntime } from '../src/runtime.js'
import { DatabaseSync } from 'node:sqlite'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const OP_KEY = PrivateKey.fromRandom()

void test('toolbox custody: openRuntime defaults to the wallet — anchor spends through it', async (t) => {
  // NEKTAR_WALLET unset — toolbox is the custody default (the production path)
  delete process.env.NEKTAR_WALLET
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  t.after(() => {
    delete process.env.NEKTAR_OPERATOR_WIF
  })
  if (stackOnly(t, 'openRuntime boots a real regtest reserve from treasury.json')) return
  const rc = await skipUnlessStack(t)
  if (rc === undefined) return
  const chain = await stackChain(t)
  if (chain === undefined) return
  const treasury = await stackTreasury()
  const fundKey = PrivateKey.fromRandom()
  const dd = mkdtempSync(join(tmpdir(), 'nektar-tb-rt-'))
  {
    // same seed shape pay.test uses: the runtime's bootstrap needs one
    // ≥1e6-sat treasury tail in its own dataDir — the reserve utxos it splits
    // are what toolbox internalizes at boot
    const ff = await StackFunder.fund(treasury, 2, 1_100_000)
    const u = await ff.take(1_100_000)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(ff.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(fundKey.toPublicKey().toAddress([0x6f])), satoshis: 1_000_000 })
    tx.addOutput({ lockingScript: new P2PKH().lock(ff.keyPair.toPublicKey().toAddress([0x6f])), change: true })
    await tx.fee()
    await tx.sign()
    const b = await chain.broadcast(tx)
    assert.equal(b.status, 'success', `seed split must broadcast: ${JSON.stringify(b)}`)
    writeFileSync(join(dd, 'treasury.json'), JSON.stringify({ treasury: { txid: tx.id('hex'), vout: 0, txHex: tx.toHex() } }))
  }
  const rt = await openRuntime({ network: 'regtest', dataDir: dd, regtest: { ...rc, fundWif: fundKey.toWif([0xef]) } })
  t.after(async () => await rt.close())

  // the toolbox custody book materialized — SqlFunds owns the utxos table in
  // records.sqlite (wallet.sqlite is the legacy book — only swept when it
  // pre-exists, never created on this path)
  const book = new DatabaseSync(join(dd, 'records.sqlite'), { readOnly: true })
  const utxoRows = (book.prepare('select count(*) as c from utxos').get() as { c: number }).c
  book.close()
  assert.ok(utxoRows > 0, `toolbox mode must internalize reserve utxos into records.sqlite — saw ${utxoRows}`)

  // the money path: remember → close batch → anchor broadcast — the anchor's
  // funding input is taken through WalletFunds.take (the wallet's own UTXO
  // selection), not the legacy file reserve
  const r = await rt.mem.remember('s', 'toolbox-funded record')
  assert.equal(r.seq, 0, 'remember commits seq 0')
  const a = await rt.mem.anchorShard('s')
  assert.ok(a !== null, 'anchorShard must close the batch and broadcast')
  assert.match(a.txid, /^[0-9a-f]{64}$/, 'anchor broadcast a real txid')
  assert.ok(rt.mem.anchorTx(a.txid) !== undefined, 'anchor tx is remembered')
})
