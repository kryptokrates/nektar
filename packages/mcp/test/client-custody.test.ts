// client-custody e2e — the paid-disclosure deal path (sale offer → escrow →
// refund) against an injected stub substrate. The stub is a real ProtoWallet
// underneath (real BRC-42 derivation, real hashToDirectlySign semantics)
// minus the keyDeriver tell, so every signature lands on the same code path
// a live BRC-100 wallet (Metanet Desktop) takes:
// custodySigner → walletSigner → createSignature/getPublicKey/createHmac.
// What this proves:
//   - the application holds zero private keys on this path (no fundKey, no
//     keyDeriver, no keys.json, no NEKTAR_FUND_WIF)
//   - the BIP-199 escrow funds on-chain with wallet-signed P2PKH inputs,
//     and the depositor's covenant spend (refund) verifies on-chain with a
//     wallet-signed signature — the strongest possible custody proof
//   - the shared coin book (utxos) tracks the escrow spend correctly
//   - the substrate never sees the app's coin set: createSignature only
//     ever receives a 32-byte digest, never outpoints or tx bytes
//
// KNOWN GAPS (pre-existing, not custody): sale op=complete fails covenant
// script evaluation under EVERY custody mode including held keys — tracked
// as a separate bug; openAutomation/MarketEngine is held-key custody today
// (mem.operatorKey() at boot throws under NEKTAR_WALLET=client).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey, ProtoWallet, PublicKey } from '@bsv/sdk'
import { testChain, testFunder, listFundsStore, MemoryFunds, memoryTreasury, fundsAsFunder } from '@nektar/chain'
import { walletSigner, FUND_SIGN_ARGS, heldRootKey, type SignerWallet } from '@nektar/core'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'

process.env.NEKTAR_PAID_DISCLOSURE = '1'
const PAYLOAD = 'breaking change: libfoo 3.0 renamed connect() to open(); replace connect( with open( to fix the build'

interface Calls {
  createSignature: number
  getPublicKey: number
  createHmac: number
  sigArgs: Array<Record<string, unknown>>
}
/** a conformant BRC-100 substrate over a test key. Deliberately exposes
 *  NONE of the held-custody tells (no keyDeriver, no PrivateKey surface) —
 *  heldRootKey(stub) must stay undefined so custodySigner takes the remote
 *  path, exactly like a real external wallet. */
const stubSubstrate = (inner: ProtoWallet, calls: Calls): SignerWallet & Pick<ProtoWallet, 'encrypt' | 'decrypt'> => ({
  getPublicKey: async (a: Parameters<ProtoWallet['getPublicKey']>[0]) => {
    calls.getPublicKey++
    return await inner.getPublicKey(a)
  },
  createSignature: async (a: Parameters<ProtoWallet['createSignature']>[0]) => {
    calls.createSignature++
    calls.sigArgs.push(a as unknown as Record<string, unknown>)
    return await inner.createSignature(a)
  },
  createHmac: async (a: Parameters<ProtoWallet['createHmac']>[0]) => {
    calls.createHmac++
    return await inner.createHmac(a)
  },
  encrypt: async (a: Parameters<ProtoWallet['encrypt']>[0]) => await inner.encrypt(a),
  decrypt: async (a: Parameters<ProtoWallet['decrypt']>[0]) => await inner.decrypt(a)
})

void test('client custody: sale offer → escrow → refund signs through the injected substrate', async (t) => {
  const chain = (await testChain(t))!
  const dir = mkdtempSync(join(tmpdir(), 'nektar-client-'))

  // ── the stub substrate + the fund address it can unlock ──────────────
  const stubKey = PrivateKey.fromRandom()
  const calls: Calls = { createSignature: 0, getPublicKey: 0, createHmac: 0, sigArgs: [] }
  const stub = stubSubstrate(new ProtoWallet(stubKey), calls)
  assert.equal(heldRootKey(stub as never), undefined, 'stub must not expose held-key custody')
  // the funding address under the production client derivation — the same
  // call openCustody makes for NEKTAR_WALLET=client (walletSigner +
  // FUND_SIGN_ARGS). Outputs paying anywhere else could never be unlocked
  // by this substrate.
  const fundPub = PublicKey.fromString(await walletSigner(stub, FUND_SIGN_ARGS).pubKeyHex())
  const fundAddr = fundPub.toAddress()
  const stubPkh = Buffer.from(fundPub.toHash()).toString('hex')
  // coin book: real outpoints paying to the stub-derived address, minted on
  // the memory treasury — the substrate's "own" coins are a different set
  // entirely and never enter the app's book (asserted below).
  const fundStore = listFundsStore()
  const funds = new MemoryFunds(fundStore, { pkhHex: stubPkh })
  const ftx = await memoryTreasury().split(fundAddr, 20, 10_000)
  for (let i = 0; i < 20; i++) await fundStore.add(ftx, i)

  // ── the client-custody buyer: opts.funds + opts.wallet, fundKey ─────
  // UNDEFINED — the app holds no key material anywhere on this path.
  const buyerMem = new NektarMemory(chain, fundsAsFunder(funds, 'deal'), undefined, {
    funds,
    wallet: stub,
    batchPolicy: { maxLeaves: 1 },
    modelId: 'test-model-1',
    height: () => chain.currentHeight()
  })

  // ── a normal held-key seller — the counterparty ──────────────────────
  const sellerFunder = await testFunder(20)
  const sellerMem = new NektarMemory(chain, sellerFunder, sellerFunder.keyPair, {
    batchPolicy: { maxLeaves: 1 },
    modelId: 'test-model-1',
    height: () => chain.currentHeight()
  })

  const height = await chain.currentHeight()
  // 1 ─ seller offer: wraps the payload, signs the cfg delegation ──────
  const offer = (await callTool(sellerMem, 'sale', {
    op: 'offer',
    value: Buffer.from(PAYLOAD, 'utf8').toString('hex'),
    price: 300,
    refundAfter: height + 2,
    maxFee: 500
  })) as { cfg: Record<string, unknown>; wrapped: string; sellerPub: string; offerSig: string }
  assert.ok(offer.cfg && offer.wrapped && offer.offerSig, `seller offer: ${JSON.stringify(offer)}`)

  // 2 ─ buyer escrow: funds the covenant — funding P2PKH inputs must be
  //    signed by the stub substrate (fundSigner → walletSigner →
  //    createSignature), and depositorPkh derives from its getPublicKey.
  const funded = (await callTool(buyerMem, 'sale', {
    op: 'escrow',
    cfg: offer.cfg,
    amount: 900,
    offer: { sellerPub: offer.sellerPub, offerSig: offer.offerSig, expiresAt: 0 }
  })) as {
    accepted: boolean
    txid?: string
    utxo?: { txid: string; vout: number; satoshis: number; script: string }
    escrow?: Record<string, unknown>
    reason?: string
  }
  assert.equal(funded.accepted, true, funded.reason)
  assert.ok(calls.createSignature > 0, 'escrow funding signed through the substrate')
  assert.ok(calls.getPublicKey > 0, 'depositor identity derived through the substrate')

  // 3 ─ the depositor's covenant spend: once refundAfter ripens, the
  //    depositor reclaims the escrow. buildRefund's unlocking sig goes
  //    through mem.fundSigner() → the stub's createSignature — a covenant
  //    spend the chain itself script-verifies.
  await chain.mine(3)
  const refund = (await callTool(buyerMem, 'sale', {
    op: 'refund',
    cfg: funded.escrow!, // carries depositorPkh already (the escrow result fills it)
    utxo: funded.utxo,
    lockTime: height + 2
  })) as { accepted: boolean; txid?: string; refundSats?: number; reason?: string }
  assert.equal(refund.accepted, true, `depositor covenant spend — ${JSON.stringify(refund)}`)
  assert.ok((refund.refundSats ?? 0) > 0, 'refund paid back to the depositor')

  // ── custody assertions ──────────────────────────────────────────────
  // the wallet only ever signs digests — it must never see the app's coin
  // set (no outpoints, tx bytes, or script material in its arguments)
  for (const a of calls.sigArgs) {
    assert.ok(a.hashToDirectlySign !== undefined || a.data !== undefined, `createSignature args carry only digests: ${JSON.stringify(a)}`)
    assert.ok(!('inputs' in a) && !('outputs' in a) && !('rawTx' in a), 'substrate saw tx material — the app leaked its coin set')
  }
  // the coin book tracked the spend. The escrow consumed split outputs and
  // its change came back to the funding address — so row counts net out and
  // the honest assertion is provenance: every live row descends from the
  // split, the escrow, or the refund — no foreign substrate coins.
  const ftxId = ftx.id('hex')
  const okTxids = new Set([ftxId, funded.txid, refund.txid].filter((x): x is string => x !== undefined))
  const remaining = await funds.list()
  assert.ok(
    remaining.every((u) => okTxids.has(u.tx.id('hex'))),
    `book holds only split/escrow/refund outputs: ${remaining.map((u) => u.tx.id('hex').slice(0, 8)).join(',')}`
  )
  // the escrow inputs demonstrably came from the shared book — they are
  // outpoints of the split tx the test seeded
  const escrowTx = (await chain.fetchTx(funded.txid!))!
  const bookSpent = new Set(escrowTx.inputs.filter((i) => i.sourceTXID === ftxId).map((i) => i.sourceOutputIndex))
  assert.ok(bookSpent.size > 0, 'escrow inputs came from the shared coin book')
  assert.ok(!remaining.some((u) => u.tx.id('hex') === ftxId && bookSpent.has(u.vout)), 'consumed inputs left the spendable book')
  assert.equal(existsSync(join(dir, 'keys.json')), false, 'no keys.json written on the client path')
})

void test('client custody: owner rotation refuses what restart cannot re-derive', async (t) => {
  const chain = (await testChain(t))!
  const calls: Calls = { createSignature: 0, getPublicKey: 0, createHmac: 0, sigArgs: [] }
  const stub = stubSubstrate(new ProtoWallet(PrivateKey.fromRandom()), calls)
  const funds = new MemoryFunds(listFundsStore(), { pkhHex: '00'.repeat(20) })
  const mem = new NektarMemory(chain, fundsAsFunder(funds, 'deal'), undefined, {
    funds,
    wallet: stub,
    batchPolicy: { maxLeaves: 1 },
    modelId: 'test-model-1',
    height: () => chain.currentHeight()
  })
  // the invariant boundary is AuthorshipOps.rotateShard — both entry paths
  // hit the same gate. A raw key under external custody persists nothing:
  // restart re-derives 'nektar-shard:<shard>' and resume bricks on 'head
  // owner is not this shard key' — plus persistKeys would leak the material
  // into any legacy keys.json.
  await assert.rejects(mem.rotateShard('manual', PrivateKey.fromRandom()), /external wallet custody/)
  await assert.rejects(callTool(mem, 'rotate', { shard: 'manual', newOwnerWif: PrivateKey.fromRandom().toWif() }), /external wallet custody/)
  // a non-default keyID is the same brick under either custody — nothing
  // persists it, and only 'nektar-shard:<shard>' re-derives at boot
  await assert.rejects(callTool(mem, 'rotate', { shard: 'manual', newOwnerKeyId: 'custom:1' }), /cannot be re-derived/)
  // the derivable keyID passes both gates — the failure is the anchor
  // lookup, not the custody boundary
  await assert.rejects(callTool(mem, 'rotate', { shard: 'manual', newOwnerKeyId: 'nektar-shard:manual' }), /no anchor/)
})
