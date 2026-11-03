// BRC-166-style payments: paid tools 402 → signed tx settles atomically →
// the receipt commits the settlement txid; one tx can pay for one call only.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, Script, LockingScript } from '@bsv/sdk'
import { MockChain, MockFunder } from '@mamem/chain'
import { utf8 } from '@mamem/core'
import { MamemMemory, type MamemMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { verifyReceipt } from '../src/receipt.js'
import { _testReset } from '../src/pay.js'
import { openRuntime } from '../src/runtime.js'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

function setup (opts: MamemMemoryOptions = {}) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(50)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, ...opts })
  const settle = (shard: string) => { chain.mine(1); const l = mem.tailLink(shard); if (l) mem.confirmAnchor(shard, l.txid) }
  const payer = new MockFunder(); payer.topup(4)
  for (const t of payer.parents) chain.registerParent(t)
  return { chain, funder, mem, settle, payer }
}

/** sign a real payment tx satisfying the invoice: pays payTo ≥ amount + carries the OP_RETURN tag */
async function payTx (payer: MockFunder, inv: { amount: number, lockingScriptHex: string, opReturn: string }, opts: { sats?: number, skipTag?: boolean } = {}): Promise<string> {
  const tx = new Transaction()
  const fund = await payer.take(opts.sats ?? inv.amount + 2000)
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(payer.keyPair) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(inv.lockingScriptHex), satoshis: opts.sats ?? inv.amount })
  if (!opts.skipTag) tx.addOutput({ lockingScript: new Script().writeOpCode(0x00).writeOpCode(0x6a).writeBin([...utf8(inv.opReturn)]), satoshis: 0 })
  tx.addOutput({ lockingScript: new P2PKH().lock(payer.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee(); await tx.sign()
  return tx.toHex()
}

const OP_KEY = PrivateKey.fromRandom()

test('payments: openRuntime refuses to start without a valid operator wallet', async (t) => {
  _testReset()
  process.env.MAMEM_PAYMENTS = '1'
  delete process.env.MAMEM_OPERATOR_WIF
  t.after(() => { delete process.env.MAMEM_PAYMENTS; delete process.env.MAMEM_OPERATOR_WIF; _testReset() })

  const dir = () => mkdtempSync(join(tmpdir(), 'mamem-pay-init-'))
  await assert.rejects(() => openRuntime({ network: 'mock', dataDir: dir() }), /requires MAMEM_OPERATOR_WIF/, 'missing wallet → no startup')
  process.env.MAMEM_OPERATOR_WIF = 'definitely-not-a-wif'
  await assert.rejects(() => openRuntime({ network: 'mock', dataDir: dir() }), /not a valid WIF/, 'garbage WIF → no startup')
  process.env.MAMEM_OPERATOR_WIF = OP_KEY.toWif([0xef])
  const rt = await openRuntime({ network: 'mock', dataDir: dir() })
  t.after(() => rt.close())
  assert.equal(rt.network, 'mock', 'valid operator wallet → startup proceeds')
})

test('payments: gated tool 402s → signed tx settles → receipt commits the payment txid', async (t) => {
  _testReset()
  process.env.MAMEM_PAYMENTS = '1'
  process.env.MAMEM_OPERATOR_WIF = OP_KEY.toWif([0xef])
  t.after(() => { delete process.env.MAMEM_PAYMENTS; delete process.env.MAMEM_OPERATOR_WIF; _testReset() })

  const { mem, settle, payer } = setup()
  await mem.remember('s', 'the fee is 250 sats')
  await mem.anchorShard('s'); settle('s')

  // 1) unpaid call → 402 challenge with an invoice (x402-shaped)
  const denied = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'the fee is 250 sats', cites: [{ shard: 's', seq: 0 }] }] })
  assert.equal(denied.ok, false)
  const acc = denied.accepts[0]
  assert.equal(acc.scheme, 'exact')
  assert.equal(acc.extra.opReturn, `mamempay:${acc.extra.invoiceId}`)
  assert.match(acc.extra.lockingScriptHex, /^76a914[0-9a-f]{40}88ac$/, 'P2PKH to the operator address')

  // 2) pay it — build + sign the settlement tx, retry with payment
  const hex = await payTx(payer, { amount: Number(acc.amount), lockingScriptHex: acc.extra.lockingScriptHex, opReturn: acc.extra.opReturn })
  const r = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'the fee is 250 sats', cites: [{ shard: 's', seq: 0 }] }], payment: { invoiceId: acc.extra.invoiceId, transaction: hex } })
  assert.equal(r.committed, true)
  assert.equal(r.receipt.payment.amountSats, 250)
  assert.equal(r.receipt.payment.payTo, OP_KEY.toPublicKey().toAddress([0x6f]))
  const payTxid = Transaction.fromHex(hex).id('hex')
  assert.equal(r.receipt.payment.txid, payTxid, 'receipt commits the settlement txid')

  // 3) independent verify → 'paid' check green, settlement auditable
  const v = await verifyReceipt(mem, { token: r.receiptToken })
  assert.equal(v.ok, true)
  assert.match(v.checks.find(c => c.name === 'paid')!.detail, /250 sats settled/)

  // 4) replay: a payment tx is bound to the call it was tagged for — reusing
  // it against a NEW invoice fails the tag check, and against the ORIGINAL
  // (now consumed) invoice fails lookup
  await mem.remember('s', 'x'); await mem.anchorShard('s'); settle('s')
  const denied2 = await callTool(mem, 'groundedAnswer', { claims: [{ text: 'x', cites: [{ shard: 's', seq: 1 }] }] })
  const acc2 = denied2.accepts[0]
  await assert.rejects(
    () => callTool(mem, 'groundedAnswer', { claims: [{ text: 'x', cites: [{ shard: 's', seq: 1 }] }], payment: { invoiceId: acc2.extra.invoiceId, transaction: hex } }),
    /missing the invoice tag/, 'old tx tags a different invoice — not payable here')
  await assert.rejects(
    () => callTool(mem, 'groundedAnswer', { claims: [{ text: 'x', cites: [{ shard: 's', seq: 1 }] }], payment: { invoiceId: acc.extra.invoiceId, transaction: hex } }),
    /unknown or wrong-tool/, 'consumed invoices are gone — one tx, one call')
})

test('payments: underpayment, missing tag and unknown invoice are refused', async (t) => {
  _testReset()
  process.env.MAMEM_PAYMENTS = '1'
  process.env.MAMEM_OPERATOR_WIF = OP_KEY.toWif([0xef])
  t.after(() => { delete process.env.MAMEM_PAYMENTS; delete process.env.MAMEM_OPERATOR_WIF; _testReset() })

  const { mem, payer } = setup()
  const q = await callTool(mem, 'payQuote', { tool: 'anchor' })
  assert.equal(q.invoice.amount, 100)

  // underpaid
  const low = await payTx(payer, q.invoice, { sats: q.invoice.amount - 1 })
  await assert.rejects(() => callTool(mem, 'anchor', { shard: 's', payment: { invoiceId: q.invoice.invoiceId, transaction: low } }), /does not pay/)

  // untagged
  const notag = await payTx(payer, q.invoice, { skipTag: true })
  await assert.rejects(() => callTool(mem, 'anchor', { shard: 's', payment: { invoiceId: q.invoice.invoiceId, transaction: notag } }), /missing the invoice tag/)

  // wrong invoice id on a valid tx
  const good = await payTx(payer, q.invoice)
  await assert.rejects(() => callTool(mem, 'anchor', { shard: 's', payment: { invoiceId: 'deadbeef', transaction: good } }), /unknown or wrong-tool/)

  // payments off → tools stay free, receipts verify with paid=info
  delete process.env.MAMEM_PAYMENTS
  const free = await callTool(mem, 'payQuote', { tool: 'anchor' }).then(() => 'unreachable', (e: Error) => e.message)
  assert.match(free, /payments are disabled/)
})
