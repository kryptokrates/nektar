// dispatchTool — the per-tool dispatch extracted from server.ts: the
// payment-gate preamble runs first, then the tool lands on one handler in
// the merged group maps. callTool (the public seam, re-exported by
// server.ts) lives here so group dispatchers recurse through
// DispatchCtx.call without a ../server.js import cycle (depcruise no-circular).
import { PAID_TOOLS, paymentsEnabled, paymentRequired, priceFor, settlePayment } from '../pay.js'
import type { NektarMemory } from '../memory.js'
import { PAID_SALE_TOOLS, paidSaleAuthorized } from './shared.js'
import type { DispatchCtx, ToolCtx, ToolHandler } from './shared.js'
import { handlers as payHandlers } from './pay.js'
import { handlers as saleHandlers } from './sale.js'
import { handlers as memoryHandlers } from './memory.js'
import { handlers as proofHandlers } from './proofs.js'
import { handlers as identityHandlers } from './identity.js'
import { handlers as bondHandlers } from './bond.js'
import { handlers as receiptHandlers } from './receipts.js'
import { handlers as fundingHandlers } from './funding.js'
import { handlers as opsHandlers } from './ops.js'

/** tools that change instance state — a successful call emits 'change' on
 *  mem.events so live surfaces (dashboard SSE) refresh instantly */
const MUTATING_TOOLS = new Set([
  'automation',
  'remember',
  'describeShard',
  'groundedAnswer',
  'commitReceipt',
  'importRecord',
  'importShard',
  'disclose',
  'rotate',
  'revoke',
  'payInvoice',
  'payQuote',
  'authorize',
  'challenge',
  'bondDeploy',
  'bondSeize',
  'bondCommit',
  'bondClose',
  'anchor',
  'automaton',
  'claim',
  'sale',
  'attestInference',
  'fundingImport',
  'sendSats',
  'fundingRefresh',
  'provisionPay'
])

export async function callTool(mem: NektarMemory, name: string, args: any, ctx: ToolCtx = {}): Promise<any> {
  const r = await dispatchTool(mem, name, args, ctx)
  if (MUTATING_TOOLS.has(name) || name.includes('#')) mem.events.emit('change', name)
  return r
}

const ALL: Record<string, ToolHandler> = {
  ...payHandlers,
  ...saleHandlers,
  ...memoryHandlers,
  ...proofHandlers,
  ...identityHandlers,
  ...bondHandlers,
  ...receiptHandlers,
  ...fundingHandlers,
  ...opsHandlers
}

// the dispatcher preamble (paid-sale gate + NEKTAR_PAYMENTS gate) is
// byte-identical to the old server.ts version — only the switch became a
// handler-map lookup, and the resolved handler receives the tool name so
// shared bodies (sale#escrow/sale#escrowBuild, sale#pay/sale#payBuild)
// can still branch on it
async function dispatchTool(mem: NektarMemory, name: string, args: any, ctx: ToolCtx = {}): Promise<any> {
  if (PAID_SALE_TOOLS.has(name) && !(await paidSaleAuthorized(mem.automation)))
    throw new Error(`${name}: paid disclosure is disabled — set NEKTAR_PAID_DISCLOSURE=1 or approve a policy with disclosure.paid`)
  // NEKTAR_PAYMENTS=1 (BRC-166-style): anchor/groundedAnswer/commitReceipt need a
  // settled payment tx before they run — atomic, one tx per call, publicly
  // auditable. No payment → return the invoice (the 402 challenge) as data.
  let paid: import('../pay.js').PaymentRecord | undefined
  // paid content paths: a manifest's offers price recall ('answer' → read
  // sats) and pack downloads ('pack' sats) for EXTERNAL callers. Priced
  // shards join the PAID_TOOLS gate with the manifest's amount.
  const pricedTool = async (): Promise<{ tool: string; amount: number } | undefined> => {
    if (!paymentsEnabled() || !ctx.external) return undefined
    if (name === 'recall' && args?.shard) {
      const p = await mem.contentPrice(String(args.shard), 'read').catch(() => 0)
      if (p > 0) return { tool: 'recall', amount: p }
    }
    if (name === 'exportShard' && args?.shard) {
      const p = await mem.contentPrice(String(args.shard), 'pack').catch(() => 0)
      if (p > 0) return { tool: 'exportShard', amount: p }
    }
    return undefined
  }
  const gated = paymentsEnabled() ? (PAID_TOOLS.has(name) ? { tool: name, amount: priceFor(name) } : await pricedTool()) : undefined
  if (gated) {
    if (typeof args?.payment?.transaction !== 'string') {
      const { required } = await paymentRequired(mem, gated.tool, gated.amount)
      return { ok: false, error: `payment required — ${gated.tool} costs ${gated.amount} sats`, ...required }
    }
    paid = await settlePayment(mem, gated.tool, args.payment)
  }
  const h = ALL[name]
  if (!h) throw new Error(`unknown tool ${name}`)
  const d: DispatchCtx = { ctx, paid, call: callTool }
  return h(mem, args, d, name)
}
