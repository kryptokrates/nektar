import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey, ProtoWallet, MerklePath } from '@bsv/sdk'
import { fundingLock } from '@lightwebinc/bcommon'
import { stackWorld, TeranodeRPCTracker, merklePathFor } from '@nektar/chain'
import { DERIVATION, FUNDING_TAG, ORIGINATOR } from '@nektar/bstack'
import { openToolboxWallet, RegtestWalletServices, fundToolbox, ToolboxFunder, NEKTAR_BASKET, type ToolboxHandle } from '../src/automation/toolbox.js'

// The wallet-toolbox lane on the local Teranode stack: construction, its own
// createAction funding path (broadcast through RegtestWalletServices), output
// listing via ToolboxFunder, and SigningWallet conformance for carrier locks.
test('toolbox wallet: fund, list, sign against Teranode', async (t) => {
  try {
    const world = await stackWorld(t, 4, 5_000)
    if (world === undefined) return
    const { funder, treasury } = world
    const cfg = treasury.cfg
    const dir = mkdtempSync(join(tmpdir(), 'nektar-tb-'))
    const tbs: ToolboxHandle[] = []
    t.after(async () => {
      for (const tb of tbs) {
        await tb.wallet.destroy?.().catch(() => undefined)
        await tb.activeStorage.destroy?.().catch(() => undefined)
      }
      rmSync(dir, { recursive: true, force: true })
    })

    const tracker = new TeranodeRPCTracker(cfg)
    const tb = await openToolboxWallet({
      fundKey: PrivateKey.fromRandom(),
      dataDir: dir,
      chain: 'regtest',
      services: new RegtestWalletServices(cfg, tracker)
    })
    tbs.push(tb)
    const id = await tb.wallet.getPublicKey({ identityKey: true }, 'nektar')
    assert.equal(typeof id.publicKey, 'string')
    assert.ok(tb.funder instanceof ToolboxFunder)

    // 1. ingest two P2PKH outpoints through the wallet's own createAction —
    //    broadcast goes through RegtestWalletServices.postBeef → sendrawtransaction
    const outs: Array<{ txid: string; vout: number; tx: import('@bsv/sdk').Transaction }> = []
    for (let i = 0; i < 2; i++) {
      const u = await funder.take(1000)
      outs.push({ txid: u.tx.id('hex'), vout: u.vout, tx: u.tx })
    }
    // createAction validates inputBEEF under trustSelf:'known' — the source txs
    // must carry merkle paths, so prove them before ingestion
    await world.chain.mine(1)
    for (const o of outs) {
      const r = await merklePathFor(cfg, o.txid, tracker, 50)
      assert.ok(r !== null, `no proof for ${o.txid}`)
      o.tx.merklePath = new MerklePath(r.blockHeight, r.path)
    }
    const funded = await fundToolbox(tb, outs, funder.keyPair)
    assert.deepEqual(
      funded.map((r) => r.success),
      [true, true],
      JSON.stringify(funded)
    )

    // 2. outputs are listed in the funding basket (custody stays explicit);
    //    ToolboxFunder materializes a real source tx
    const lo = await tb.wallet.listOutputs({ basket: NEKTAR_BASKET, include: 'entire transactions' }, 'nektar')
    assert.ok(lo.outputs.length >= 1, `wallet should hold spendable outputs in ${NEKTAR_BASKET}`)
    const taken = await tb.funder.take(500)
    assert.equal(typeof taken.tx.id('hex'), 'string')
    assert.ok(taken.satoshis >= 500)

    // 3. SigningWallet conformance: the funding lock derives from the wallet —
    //    a different key gives a different lock; the wallet's matches its own id
    const walletLock = (await fundingLock(tb.wallet, ORIGINATOR, DERIVATION, FUNDING_TAG)).toHex()
    const otherLock = (await fundingLock(new ProtoWallet(PrivateKey.fromRandom()), ORIGINATOR, DERIVATION, FUNDING_TAG)).toHex()
    assert.notEqual(walletLock, otherLock)

    // 4. services round-trips: the funding tx is seen by the node the wallet uses
    const st = await tb.services.getStatusForTxids([outs[0].txid])
    assert.ok(st.results[0].status === 'mined' || st.results[0].status === 'known', `unexpected: ${JSON.stringify(st.results[0])}`)
    const raw = await tb.services.getRawTx(outs[0].txid)
    assert.ok(raw.rawTx !== undefined && raw.rawTx.length > 0, 'getRawTx should return the tx bytes')
  } catch (e) {
    process.stderr.write(`\n[toolbox-test] FAIL: ${e instanceof Error ? e.stack : String(e)}\n`)
    throw e
  }
})
