// BRC-166-style P2PKH payment settlement for paid tools (x402 `exact` scheme
// profile): the caller signs a whole raw tx paying the operator address AND
// carrying an OP_RETURN invoice tag, hands it over, we verify + broadcast.
// No wallet ecosystem — anything that can sign a BSV tx can pay (a WIF is
// enough). Settlement is publicly auditable from the tx alone: (pays payTo ≥
// amount) ∧ (carries `mamempay:<invoiceId>`) — the receipt commits the txid.
import { Transaction, PrivateKey, P2PKH } from '@bsv/sdk'
import { randomBytes } from 'node:crypto'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { utf8 } from '@mamem/core'
import type { MamemMemory } from './memory.js'

export const paymentsEnabled = (): boolean => /^(1|true|yes)$/i.test(process.env.MAMEM_PAYMENTS ?? '')

/** tools that cost sats when MAMEM_PAYMENTS=1 */
export const PAID_TOOLS = new Set(['anchor', 'groundedAnswer', 'commitReceipt'])

const networkTag = (): string => {
  const n = process.env.MAMEM_NETWORK ?? 'mock'
  return n === 'main' ? 'bsv:mainnet' : n === 'ttn' ? 'bsv:teratestnet' : n === 'regtest' ? 'bsv:regtest' : 'bsv:mock'
}

let opAddr: string | undefined
const operatorAddress = (): string => {
  if (opAddr === undefined) {
    const wif = process.env.MAMEM_OPERATOR_WIF
    if (!wif) throw new Error('MAMEM_PAYMENTS=1 needs MAMEM_OPERATOR_WIF — the WIF whose address payments settle to (never broadcast, address only is served)')
    opAddr = PrivateKey.fromWif(wif).toPublicKey().toAddress(process.env.MAMEM_NETWORK === 'main' ? [0x00] : [0x6f])
  }
  return opAddr
}

/** per-tool price: MAMEM_PRICE_<TOOL>_SATS, falling back to MAMEM_PRICE_SATS, then defaults */
export const priceFor = (tool: string): number =>
  Number(process.env[`MAMEM_PRICE_${tool.toUpperCase()}_SATS`] ?? process.env.MAMEM_PRICE_SATS ?? (tool === 'anchor' ? 100 : 250))

interface Invoice {
  invoiceId: string
  tool: string
  amount: number
  payTo: string
  lockingScriptHex: string
  /** ascii the payment tx must carry in an OP_RETURN output — binds the tx to THIS call */
  opReturn: string
  expires: number
}

const invoices = new Map<string, Invoice>()
const INVOICE_TTL_MS = 10 * 60_000

/** consumed payment txids — replay protection, persisted in the data dir */
const payFile = (): string => join(process.env.MAMEM_DATA_DIR ?? join(process.cwd(), '.mamem'), 'payments.json')
const consumed = new Set<string>()
let consumedLoaded = false
const loadConsumed = (): void => {
  if (consumedLoaded) return
  consumedLoaded = true
  try { if (existsSync(payFile())) for (const t of JSON.parse(readFileSync(payFile(), 'utf8')).consumed ?? []) consumed.add(String(t)) } catch { /* fresh or unreadable — start empty */ }
}
const persistConsumed = (): void => {
  try { writeFileSync(payFile(), JSON.stringify({ consumed: [...consumed] })) } catch { /* data dir may be absent on read-only tests */ }
}

/** issue an invoice for a paid tool call */
export const issueInvoice = (tool: string): Invoice => {
  const payTo = operatorAddress()
  const inv: Invoice = {
    invoiceId: randomBytes(8).toString('hex'),
    tool,
    amount: priceFor(tool),
    payTo,
    lockingScriptHex: new P2PKH().lock(payTo).toHex(),
    opReturn: '',
    expires: Date.now() + INVOICE_TTL_MS
  }
  inv.opReturn = `mamempay:${inv.invoiceId}`
  invoices.set(inv.invoiceId, inv)
  return inv
}

/** the 402 payload — x402 v2 PaymentRequired shape carrying the invoice in accepts[].extra */
export const paymentRequired = (tool: string): { invoice: Invoice, required: Record<string, unknown> } => {
  const invoice = issueInvoice(tool)
  const required = {
    x402Version: 2,
    error: `payment required — ${tool} costs ${invoice.amount} sats`,
    resource: { url: `tool://${tool}`, description: `mamem ${tool}` },
    accepts: [{
      scheme: 'exact',
      network: networkTag(),
      amount: String(invoice.amount),
      payTo: invoice.payTo,
      extra: {
        invoiceId: invoice.invoiceId,
        lockingScriptHex: invoice.lockingScriptHex,
        opReturn: invoice.opReturn,
        expires: new Date(invoice.expires).toISOString(),
        howToPay: 'build + sign a tx with (1) an output paying lockingScriptHex (or payTo) ≥ amount sats and (2) an OP_FALSE OP_RETURN output carrying the ascii `opReturn`, then retry the SAME call with payment:{invoiceId, transaction:<raw tx hex>}'
      }
    }]
  }
  return { invoice, required }
}

export interface PaymentRecord { txid: string, amountSats: number, payTo: string, invoiceId: string }

/** verify + broadcast the caller's payment tx; returns the settlement record a receipt commits */
export const settlePayment = async (mem: MamemMemory, tool: string, payment: { invoiceId?: string, transaction?: string }): Promise<PaymentRecord> => {
  const inv = invoices.get(String(payment?.invoiceId ?? ''))
  if (!inv || inv.tool !== tool) throw new Error(`unknown or wrong-tool invoiceId "${payment?.invoiceId}" — call payQuote for a fresh invoice`)
  if (Date.now() > inv.expires) { invoices.delete(inv.invoiceId); throw new Error('invoice expired — call payQuote for a fresh one') }
  loadConsumed()
  let tx: Transaction
  try { tx = Transaction.fromHex(String(payment.transaction ?? '')) } catch (e) { throw new Error(`payment.transaction is not a parseable raw tx (${(e as Error).message.slice(0, 80)})`) }
  const tagHex = Buffer.from(utf8(inv.opReturn)).toString('hex')
  const paid = tx.outputs.some(o => o.lockingScript.toHex() === inv.lockingScriptHex && (o.satoshis ?? 0) >= inv.amount)
  const tagged = tx.outputs.some(o => { const h = o.lockingScript.toHex(); return h.startsWith('006a') && h.includes(tagHex) })
  if (!paid) throw new Error(`payment tx does not pay ${inv.payTo} ≥ ${inv.amount} sats`)
  if (!tagged) throw new Error(`payment tx is missing the invoice tag output — needs OP_FALSE OP_RETURN "${inv.opReturn}"`)
  const txid = tx.id('hex')
  if (consumed.has(txid)) throw new Error(`payment txid ${txid.slice(0, 16)}… already consumed — one tx pays for one call`)
  const r = await mem.broadcastTx(tx)
  if (r.status !== 'success') throw new Error(`payment broadcast failed: ${r.description}`)
  consumed.add(txid); persistConsumed(); invoices.delete(inv.invoiceId)
  return { txid, amountSats: inv.amount, payTo: inv.payTo, invoiceId: inv.invoiceId }
}

export const _testReset = (): void => { invoices.clear(); consumed.clear(); consumedLoaded = false; opAddr = undefined }
