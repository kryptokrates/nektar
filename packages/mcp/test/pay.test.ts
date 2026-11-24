// BRC-166-style payments: paid tools 402 → signed tx settles atomically →
// the receipt commits the settlement txid; one tx can pay for one call only.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, Script, LockingScript } from '@bsv/sdk'
import { testWorld, stackChain, skipUnlessStack, StackFunder, stackTreasury, testFunder, stackOnly, buildBump, type TestFunder } from '@nektar/chain'
import { utf8 } from '@nektar/core'
import { NektarMemory, type NektarMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { verifyReceipt } from '../src/receipt.js'
import { _testReset, verifyQuoteSig, settlePayment, paymentRequired } from '../src/pay.js'
import { HeaderUnavailableError, isHeaderUnavailable } from '@nektar/chain'
import { openRuntime } from '../src/runtime.js'
import { existsSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

async function setup(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}) {
  const w = await testWorld(t, 50)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, ...opts })
  const settle = async (shard: string) => {
    await chain.mine(1)
    const l = mem.tailLink(shard)
    if (l) mem.confirmAnchor(shard, l.txid)
  }
  // real funding: the payer signs real settlement txs that actually broadcast —
  // a dry funder throws at the first take() (mid-migration leftover)
  const payer = await testFunder(6)

  return { chain, funder, mem, settle, payer }
}

/** sign a real payment tx satisfying the invoice: pays payTo ≥ amount + carries the OP_RETURN tag */
async function payTx(
  payer: TestFunder,
  inv: { amount: number; lockingScriptHex: string; opReturn: string },
  opts: { sats?: number; skipTag?: boolean } = {}
): Promise<string> {
  const tx = new Transaction()
  const fund = await payer.take(opts.sats ?? inv.amount + 2000)
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(payer.keyPair) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(inv.lockingScriptHex), satoshis: opts.sats ?? inv.amount })
  if (!opts.skipTag)
    tx.addOutput({
      lockingScript: new Script()
        .writeOpCode(0x00)
        .writeOpCode(0x6a)
        .writeBin([...utf8(inv.opReturn)]),
      satoshis: 0
    })
  tx.addOutput({ lockingScript: new P2PKH().lock(payer.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee()
  await tx.sign()
  return tx.toHex()
}

const OP_KEY = PrivateKey.fromRandom()

void test('payments: openRuntime refuses to start without a valid operator wallet', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  delete process.env.NEKTAR_OPERATOR_WIF
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    _testReset()
  })

  const dir = () => mkdtempSync(join(tmpdir(), 'nektar-pay-init-'))
  await assert.rejects(() => openRuntime({ network: 'regtest', dataDir: dir() }), /requires NEKTAR_OPERATOR_WIF/, 'missing wallet → no startup')
  process.env.NEKTAR_OPERATOR_WIF = 'definitely-not-a-wif'
  await assert.rejects(() => openRuntime({ network: 'regtest', dataDir: dir() }), /not a valid WIF/, 'garbage WIF → no startup')
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  if (stackOnly(t, 'openRuntime boots a real regtest reserve from treasury.json')) return
  const rc = await skipUnlessStack(t)
  if (rc === undefined) return
  // the runtime bootstraps its fee reserve from a treasury tail in its own
  // dataDir — seed one with a real split tx to a fresh fund key (a fresh dir
  // has no coinbases and the shared treasury's tail belongs to that reserve)
  const chain = await stackChain(t)
  if (chain === undefined) return
  const treasury = await stackTreasury()
  const fundKey = PrivateKey.fromRandom()
  const dd = dir()
  {
    // one big spendable input for the runtime's bootstrap split (it needs a
    // ≥1e6-sat parent output); StackFunder.fund handles lease+refill itself
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
  t.after(() => rt.close())
  assert.equal(rt.network, 'regtest', 'valid operator wallet → startup proceeds')
})

void test('payments: gated tool 402s → signed tx settles → receipt commits the payment txid', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    _testReset()
  })

  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, settle, payer } = _w
  await mem.remember('s', 'the fee is 250 sats')
  await mem.anchorShard('s')
  await settle('s')

  // 1) unpaid call → 402 challenge with an invoice (x402-shaped)
  const denied = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'the fee is 250 sats', cites: [{ shard: 's', seq: 0 }] }] })
  assert.equal(denied.ok, false)
  const acc = denied.accepts[0]
  assert.equal(acc.scheme, 'exact')
  assert.equal(acc.extra.opReturn, `nektarpay:${acc.extra.invoiceId}`)
  assert.match(acc.extra.lockingScriptHex, /^76a914[0-9a-f]{40}88ac$/, 'P2PKH to the operator address')

  // 2) pay it — build + sign the settlement tx, retry with payment
  const hex = await payTx(payer, { amount: Number(acc.amount), lockingScriptHex: acc.extra.lockingScriptHex, opReturn: acc.extra.opReturn })
  const r = await callTool(mem, 'groundedAnswer', {
    claims: [{ text: 'the fee is 250 sats', cites: [{ shard: 's', seq: 0 }] }],
    payment: { invoiceId: acc.extra.invoiceId, transaction: hex }
  })
  assert.equal(r.committed, true)
  assert.equal(r.receipt.payment.amountSats, 250)
  assert.equal(r.receipt.payment.payTo, OP_KEY.toPublicKey().toAddress([0x6f]))
  const payTxid = Transaction.fromHex(hex).id('hex')
  assert.equal(r.receipt.payment.txid, payTxid, 'receipt commits the settlement txid')

  // 3) independent verify → 'paid' check green, settlement auditable
  const v = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v.ok, true)
  assert.match(v.checks.find((c) => c.name === 'paid')!.detail, /250 sats settled/)

  // 4) replay: a payment tx is bound to the call it was tagged for — reusing
  // it against a NEW invoice fails the tag check, and against the ORIGINAL
  // (now consumed) invoice fails lookup
  await mem.remember('s', 'x')
  await mem.anchorShard('s')
  await settle('s')
  const denied2 = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'x', cites: [{ shard: 's', seq: 1 }] }] })
  const acc2 = denied2.accepts[0]
  await assert.rejects(
    () =>
      callTool(mem, 'groundedAnswer', {
        claims: [{ text: 'x', cites: [{ shard: 's', seq: 1 }] }],
        payment: { invoiceId: acc2.extra.invoiceId, transaction: hex }
      }),
    /missing the invoice tag/,
    'old tx tags a different invoice — not payable here'
  )
  await assert.rejects(
    () =>
      callTool(mem, 'groundedAnswer', {
        claims: [{ text: 'x', cites: [{ shard: 's', seq: 1 }] }],
        payment: { invoiceId: acc.extra.invoiceId, transaction: hex }
      }),
    /unknown or wrong-tool/,
    'consumed invoices are gone — one tx, one call'
  )
})

void test('payments: underpayment, missing tag and unknown invoice are refused', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    _testReset()
  })

  const _w = await setup(t)
  if (_w === undefined) return
  const { mem, payer } = _w
  const q = await callTool(mem, 'payQuote', { tool: 'anchor' })
  assert.equal(q.invoice.amount, 100)

  // underpaid
  const low = await payTx(payer, q.invoice, { sats: q.invoice.amount - 1 })
  await assert.rejects(() => callTool(mem, 'anchor', { shard: 's', payment: { invoiceId: q.invoice.invoiceId, transaction: low } }), /does not pay/)

  // untagged
  const notag = await payTx(payer, q.invoice, { skipTag: true })
  await assert.rejects(
    () => callTool(mem, 'anchor', { shard: 's', payment: { invoiceId: q.invoice.invoiceId, transaction: notag } }),
    /missing the invoice tag/
  )

  // wrong invoice id on a valid tx
  const good = await payTx(payer, q.invoice)
  await assert.rejects(() => callTool(mem, 'anchor', { shard: 's', payment: { invoiceId: 'deadbeef', transaction: good } }), /unknown or wrong-tool/)

  // payments off → tools stay free, receipts verify with paid=info
  delete process.env.NEKTAR_PAYMENTS
  const free = await callTool(mem, 'payQuote', { tool: 'anchor' }).then(
    () => 'unreachable',
    (e: Error) => e.message
  )
  assert.match(free, /payments are disabled/)
})

void test('payments: invoices carry a verifiable signed quote; tampering detected', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    _testReset()
  })

  const _w = await setup(t)
  if (_w === undefined) return
  const { mem } = _w
  const q = await callTool(mem, 'payQuote', { tool: 'groundedAnswer' })
  const inv = q.invoice
  assert.equal(inv.pub, OP_KEY.toPublicKey().toString(), 'quote is attributable to the operator key')
  assert.equal(verifyQuoteSig(inv), true, 'signature verifies over the canonical quote')
  // doctored fields fail verification — a cheaper-signed quote is detectable
  assert.equal(verifyQuoteSig({ ...inv, amount: inv.amount - 1 }), false, 'doctored amount rejected')
  assert.equal(verifyQuoteSig({ ...inv, payTo: 'mgarbage' }), false, 'doctored payTo rejected')
  // the same quote rides inside the 402 accepts[].extra too
  const denied = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'x', cites: [{ shard: 's', seq: 0 }] }] })
  const extra = denied.accepts[0].extra
  assert.equal(verifyQuoteSig({ ...extra, amount: Number(denied.accepts[0].amount), expires: extra.expiresMs }), true, '402-carried quote verifies')
})

// SPV-02: a header outage inside ancestry verification is "no verdict" — the
// payer sees the retryable tool error (the thrown HeaderUnavailableError
// reaches the callTool isError surface with its 'not a verdict, retry'
// message), the invoice and the payment are never consumed, and the SAME
// payment settles once headers answer.
void test('payments: a header outage during ancestry verification is a retry, not a consumption', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  const dir = mkdtempSync(join(tmpdir(), 'nektar-pay-'))
  process.env.NEKTAR_DATA_DIR = dir
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    delete process.env.NEKTAR_DATA_DIR
    _testReset()
  })

  const w = await testWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const payer = await testFunder(6)
  await chain.mine(1) // land the payer's split tx so its parent carries a real BUMP
  const { required } = await paymentRequired(mem, 'anchor', 100)
  const accept = (required as { accepts: Array<{ amount: string; extra: { invoiceId: string; lockingScriptHex: string; opReturn: string } }> }).accepts[0]
  const inv = accept.extra
  const fund = await payer.take(Number(accept.amount) + 2000)
  const mp = await chain.merklePathOf(fund.tx.id('hex'))
  assert.ok(mp !== undefined, 'the mined funding parent must carry a proof — verify() is what the outage hits')
  fund.tx.merklePath = mp
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(payer.keyPair) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(inv.lockingScriptHex), satoshis: Number(accept.amount) })
  tx.addOutput({
    lockingScript: new Script()
      .writeOpCode(0x00)
      .writeOpCode(0x6a)
      .writeBin([...utf8(inv.opReturn)]),
    satoshis: 0
  })
  tx.addOutput({ lockingScript: new P2PKH().lock(payer.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee()
  await tx.sign()
  // atomic BEEF keeps the carried parent's BUMP attached through the parse —
  // the plain-beef path wires parents from raw bytes and drops it
  const beefHex = Buffer.from(tx.toAtomicBEEF()).toString('hex')
  // the outage: every root check throws — ancestry verification can never
  // reach a verdict, so nothing negative may be recorded
  const realTracker = mem.sdkTracker.bind(mem)
  const down = {
    isValidRootForHeight: (_r: string, h: number) => Promise.reject(new HeaderUnavailableError(h, 'uncorroborated')),
    currentHeight: () => Promise.resolve(0)
  }
  mem.sdkTracker = () => down as ReturnType<NektarMemory['sdkTracker']>
  await assert.rejects(
    () => settlePayment(mem, 'anchor', { invoiceId: inv.invoiceId, transaction: beefHex }),
    (e: unknown) => isHeaderUnavailable(e) && e.why === 'uncorroborated',
    'the outage surfaces as a retryable error — never a consumed or refused payment'
  )
  // heal — the SAME invoice and the SAME payment settle: nothing was spent,
  // refused, or consumed on the failed pass
  mem.sdkTracker = realTracker
  const res = await settlePayment(mem, 'anchor', { invoiceId: inv.invoiceId, transaction: beefHex })
  assert.equal(res.txid, tx.id('hex'), 'the payment settles once headers answer again')
})

void test('payments: plain BEEF (0100beef) preserves parent BUMP, forged BUMP refused, payments.json renamed to .migrated', async (t) => {
  _testReset()
  process.env.NEKTAR_PAYMENTS = '1'
  process.env.NEKTAR_OPERATOR_WIF = OP_KEY.toWif([0xef])
  const dir = mkdtempSync(join(tmpdir(), 'nektar-pay-beef-'))
  process.env.NEKTAR_DATA_DIR = dir
  const legacyFile = join(dir, 'payments.json')
  writeFileSync(legacyFile, JSON.stringify({ consumed: ['aa'.repeat(32)] }))
  t.after(() => {
    delete process.env.NEKTAR_PAYMENTS
    delete process.env.NEKTAR_OPERATOR_WIF
    delete process.env.NEKTAR_DATA_DIR
    _testReset()
  })

  const w = await testWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const payer = await testFunder(6)
  await chain.mine(1)
  const { required } = await paymentRequired(mem, 'anchor', 100)
  const accept = (required as { accepts: Array<{ amount: string; extra: { invoiceId: string; lockingScriptHex: string; opReturn: string } }> }).accepts[0]
  const inv = accept.extra
  const fund = await payer.take(Number(accept.amount) + 2000)
  const realMp = (await chain.merklePathOf(fund.tx.id('hex')))!

  // 1) forged BUMP on parent in plain BEEF -> rejected (strippedBumps > 0)
  const badParent = Transaction.fromHex(fund.tx.toHex())
  badParent.merklePath = buildBump([badParent.id('hex'), 'bb'.repeat(32)], 0, 777_777)
  const badTx = new Transaction()
  badTx.addInput({ sourceTransaction: badParent, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(payer.keyPair) })
  badTx.addOutput({ lockingScript: LockingScript.fromHex(inv.lockingScriptHex), satoshis: Number(accept.amount) })
  badTx.addOutput({
    lockingScript: new Script()
      .writeOpCode(0x00)
      .writeOpCode(0x6a)
      .writeBin([...utf8(inv.opReturn)]),
    satoshis: 0
  })
  badTx.addOutput({ lockingScript: new P2PKH().lock(payer.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await badTx.fee()
  await badTx.sign()
  await assert.rejects(
    () => settlePayment(mem, 'anchor', { invoiceId: inv.invoiceId, transaction: Buffer.from(badTx.toBEEF()).toString('hex') }),
    /ancestry fails verification/
  )
  // legacy payments.json was migrated and renamed on first getReceiver() call
  assert.equal(existsSync(legacyFile), false, 'payments.json renamed aside after migration')
  assert.equal(existsSync(`${legacyFile}.migrated`), true, 'payments.json.migrated marker written')

  // 2) valid plain BEEF (0100beef) with real parent BUMP settles cleanly
  fund.tx.merklePath = realMp
  const goodTx = new Transaction()
  goodTx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(payer.keyPair) })
  goodTx.addOutput({ lockingScript: LockingScript.fromHex(inv.lockingScriptHex), satoshis: Number(accept.amount) })
  goodTx.addOutput({
    lockingScript: new Script()
      .writeOpCode(0x00)
      .writeOpCode(0x6a)
      .writeBin([...utf8(inv.opReturn)]),
    satoshis: 0
  })
  goodTx.addOutput({ lockingScript: new P2PKH().lock(payer.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await goodTx.fee()
  await goodTx.sign()
  const res = await settlePayment(mem, 'anchor', { invoiceId: inv.invoiceId, transaction: Buffer.from(goodTx.toBEEF()).toString('hex') })
  assert.equal(res.txid, goodTx.id('hex'), 'plain BEEF settles through TxStore.read')
})
