// Client side of the paid-call flow: build the signed settlement tx for an
// invoice (from payQuote / a 402 paymentRequired). Needs only the payer's WIF
// key and its spendable utxos — no wallet protocol. Emits BEEF hex so the
// funding parents travel inline (mock verifyScripts + Teranode EF both want
// them attached).
import { Transaction, PrivateKey, P2PKH, Script, LockingScript } from '@bsv/sdk'

export interface PaymentInvoice {
  amount: number | string
  lockingScriptHex: string
  opReturn: string
  invoiceId?: string
}

/** normalize either the tool-level invoice or accepts[].extra of a 402 payload */
export const invoiceOf = (
  paymentRequired: { accepts?: Array<{ amount?: string; extra?: Record<string, unknown> }> } | { invoice?: Record<string, unknown> }
): PaymentInvoice => {
  const acc = (paymentRequired as any).accepts?.[0]
  if (acc)
    return {
      amount: Number(acc.amount),
      lockingScriptHex: String(acc.extra?.lockingScriptHex ?? ''),
      opReturn: String(acc.extra?.opReturn ?? ''),
      invoiceId: String(acc.extra?.invoiceId ?? '')
    }
  const inv = (paymentRequired as any).invoice
  return {
    amount: Number(inv?.amount),
    lockingScriptHex: String(inv?.lockingScriptHex ?? ''),
    opReturn: String(inv?.opReturn ?? ''),
    invoiceId: String(inv?.invoiceId ?? '')
  }
}

/** build + sign the payment: pays lockingScriptHex ≥ amount, carries the
 *  OP_RETURN invoice tag, change back to the payer. utxos = the payer's own
 *  unspent outputs ({tx, vout, satoshis} — sourceTransaction attached). */
export async function buildPaymentTx(
  inv: PaymentInvoice,
  payer: PrivateKey,
  utxos: Array<{ tx: Transaction; vout: number; satoshis: number }>,
  opts: { testnet?: boolean } = {}
): Promise<{ hex: string; txid: string }> {
  const amount = Number(inv.amount)
  const addr = payer.toPublicKey().toAddress(opts.testnet === false ? undefined : [0x6f])
  const tx = new Transaction()
  let inSats = 0
  for (const u of utxos) {
    tx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(payer) })
    inSats += u.satoshis
    if (inSats >= amount + 2000) break
  }
  if (inSats < amount + 200) throw new Error(`payer utxos too small: ${inSats} < ${amount}+fee`)
  tx.addOutput({ lockingScript: LockingScript.fromHex(inv.lockingScriptHex), satoshis: amount })
  tx.addOutput({
    lockingScript: new Script()
      .writeOpCode(0x00)
      .writeOpCode(0x6a)
      .writeBin([...Buffer.from(inv.opReturn, 'utf8')]),
    satoshis: 0
  })
  tx.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
  await tx.fee()
  await tx.sign()
  return { hex: Buffer.from(tx.toBEEF()).toString('hex'), txid: tx.id('hex') }
}
