// pay.ts — money movement + funding surface: invoice payment, direct sends,
// reserve status, provisioning, and the live-network import/refresh ops.
import { PAID_TOOLS, paymentsEnabled, paymentRequired } from '../pay.js'
import type { ToolHandler } from './ctx.js'

export const PAY_HANDLERS: Record<string, ToolHandler> = {
  payQuote: async (d) => {
    const { mem, args } = d
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
  payInvoice: async (d) => {
    const { mem, args } = d
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
  sendSats: async (d) => {
    const { mem, args } = d
    return await mem.sendSats({ amount: Number(args.amount), address: args.address, scriptHex: args.scriptHex, opReturn: args.opReturn })
  },
  fundingStatus: async (d) => {
    const { mem, ctx } = d
    // prefer the runtime's live wallet view (spend caps, runway, correct
    // net prefix); fall back to the reserve mem tracks itself
    if (ctx.ops?.wallet) {
      const w = (await ctx.ops.wallet()) as Record<string, unknown>
      return { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
    }
    return await mem.fundingStatus()
  },
  provisionPlan: async (d) => {
    const { mem } = d
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    const plan = await au.provisionPlan()
    const w = await mem.fundingStatus()
    return { ...plan, address: w.address }
  },
  provisionPay: async (d) => {
    const { mem, args } = d
    const plan = args?.plan as { outputs?: Array<{ lockingScriptHex?: string; satoshis?: number; count?: number }> } | undefined
    const outputs: Array<{ satoshis: number; scriptHex: string }> = []
    for (const e of plan?.outputs ?? []) {
      if (typeof e.lockingScriptHex !== 'string' || typeof e.satoshis !== 'number' || !Number.isInteger(e.count) || e.count! < 1)
        throw new Error('provisionPay: bad plan entry (lockingScriptHex, satoshis, count required)')
      for (let i = 0; i < e.count!; i++) outputs.push({ satoshis: e.satoshis, scriptHex: e.lockingScriptHex })
    }
    if (outputs.length === 0) throw new Error('provisionPay: empty plan')
    if (typeof args?.address !== 'string') throw new Error('provisionPay: address required')
    const r = await mem.provisionPay({ outputs, address: args.address, fundsSats: Number(args.fundsSats) })
    return { txid: r.txid, hex: mem.txWire(r.txid) ?? r.hex, outputs: r.outputs, fee: r.fee }
  },
  fundingImport: async (d) => {
    const { mem, args, ctx } = d
    if (!ctx.ops?.importFunding) throw new Error('fundingImport: live network only (regtest funds via the test harness)')
    if (typeof args?.txid !== 'string' || args.txid.length < 8) throw new Error('fundingImport: txid required')
    const res = (await ctx.ops.importFunding(args.txid)) as Record<string, unknown>
    // a provisioning tx carries lane-locked outputs — adopt them as slot stock
    if (mem.automation !== undefined) {
      const adopted: Record<string, number> = {}
      for (const txid of (res as { txids?: string[] }).txids ?? []) {
        const tx = await mem.fetchTx(txid)
        if (tx === undefined) continue
        for (const [k, n] of Object.entries(await mem.automation.adoptProvision(tx))) adopted[k] = (adopted[k] ?? 0) + n
      }
      if (Object.keys(adopted).length > 0) return { ...res, adopted }
    }
    return res
  },
  fundingRefresh: async (d) => {
    const { ctx } = d
    if (!ctx.ops?.refreshFunding) throw new Error('fundingRefresh: live network only')
    return await ctx.ops.refreshFunding()
  }
}
