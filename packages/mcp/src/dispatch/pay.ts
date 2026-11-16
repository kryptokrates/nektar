// pay tools — extracted verbatim from the server.ts dispatchTool switch
import { PAID_TOOLS, paymentsEnabled, paymentRequired } from '../pay.js'
import type { ToolHandler } from './shared.js'

export const handlers: Record<string, ToolHandler> = {
  payQuote: async (mem, args) => {
    if (!PAID_TOOLS.has(String(args?.tool))) throw new Error(`payQuote: unknown or unpaid tool "${args?.tool}"`)
    if (!paymentsEnabled()) throw new Error('payments are disabled — set NEKTAR_PAYMENTS=1 (and NEKTAR_OPERATOR_WIF)')
    const { invoice, required } = await paymentRequired(mem, String(args.tool))
    return {
      invoice: {
        invoiceId: invoice.invoiceId,
        tool: invoice.tool,
        amount: invoice.amount,
        payTo: invoice.payTo,
        lockingScriptHex: invoice.lockingScriptHex,
        opReturn: invoice.opReturn,
        expires: new Date(invoice.expires).toISOString(),
        expiresMs: invoice.expires,
        pub: invoice.pub,
        quoteSig: invoice.quoteSig
      },
      paymentRequired: required
    }
  },
  payInvoice: async (mem, args) => {
    // F3 — a grantId scopes this payment to the cumulative ledger; the
    // lifetime cap enforces, not just this call's amount
    const gid = typeof args?.grantId === 'string' ? args.grantId : undefined
    if (gid && mem.budget) {
      const r = mem.budget.reserve(gid, `payInvoice:${args.invoiceId}`, Number(args.amount))
      if (r === 'exists') throw new Error(`payInvoice: action already recorded for grant ${gid} � idempotent replay refused`)
      if (r !== 'ok') throw new Error(`payInvoice: grant ${gid} budget ${r}`)
      try {
        const out = await mem.payInvoice({
          invoiceId: String(args.invoiceId),
          amount: Number(args.amount),
          lockingScriptHex: String(args.lockingScriptHex),
          opReturn: String(args.opReturn)
        })
        mem.budget.commit(gid, `payInvoice:${args.invoiceId}`)
        return out
      } catch (e) {
        mem.budget.release(gid, `payInvoice:${args.invoiceId}`)
        throw e
      }
    }
    return await mem.payInvoice({
      invoiceId: String(args.invoiceId),
      amount: Number(args.amount),
      lockingScriptHex: String(args.lockingScriptHex),
      opReturn: String(args.opReturn)
    })
  },
  sendSats: async (mem, args) => {
    return await mem.sendSats({ amount: Number(args.amount), address: args.address, scriptHex: args.scriptHex, opReturn: args.opReturn })
  }
}
