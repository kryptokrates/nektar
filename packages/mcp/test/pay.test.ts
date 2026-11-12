// BRC-166-style payments: paid tools 402 → signed tx settles atomically →
// the receipt commits the settlement txid; one tx can pay for one call only.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, Script, LockingScript } from '@bsv/sdk'
import { stackWorld, stackChain, skipUnlessStack, StackFunder } from '@nektar/chain'
import { utf8 } from '@nektar/core'
import { NektarMemory, type NektarMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { verifyReceipt } from '../src/receipt.js'
import { _testReset, verifyQuoteSig } from '../src/pay.js'
import { openRuntime } from '../src/runtime.js'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

async function setup(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}) {
  const w = await stackWorld(t, 50)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, ...opts })
  const settle = async (shard: string) => {
    await chain.mine(1)
    const l = mem.tailLink(shard)
    if (l) mem.confirmAnchor(shard, l.txid)
  }
  const payer = StackFunder.dry()
  
  return { chain, funder, mem, settle, payer }
}

/** sign a real payment tx satisfying the invoice: pays payTo ≥ amount + carries the OP_RETURN tag */
async function payTx(
  payer: StackFunder,
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

test('payments: openRuntime refuses to start without a valid operator wallet', async (t) => {
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
  const rc = await skipUnlessStack(t)
  if (rc === undefined) return
  const rt = await openRuntime({ network: 'regtest', dataDir: dir(), regtest: { ...rc, fundWif: process.env.NEKTAR_REGTEST_WIF! } })
  t.after(() => rt.close())
  assert.equal(rt.network, 'regtest', 'valid operator wallet → startup proceeds')
})

test('payments: gated tool 402s → signed tx settles → receipt commits the payment txid', async (t) => {
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
  settle('s')

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
  settle('s')
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

test('payments: underpayment, missing tag and unknown invoice are refused', async (t) => {
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

test('payments: invoices carry a verifiable signed quote; tampering detected', async (t) => {
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
