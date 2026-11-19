// shared types + file-local helpers for the dispatch/ group modules — moved
// verbatim out of server.ts (the old monolithic dispatchTool switch).
import { nektarEnv } from '@nektar/core'
import type { NektarMemory } from '../memory.js'
import type { PaymentRecord } from '../pay.js'

/** ready-to-paste share surface for a receipt — the model drops `markdown`
 *  straight into PRs/docs; every pasted badge is a verify-page entry point */
export const shareBlock = (token: string) => {
  const verifyUrl = `${verifyBase()}/v1/verify?r=${token}`
  const badgeUrl = `${verifyBase()}/v1/badge?r=${token}`
  return { token, verifyUrl, badgeUrl, markdown: `[![verified answer](${badgeUrl})](${verifyUrl})` }
}

/** public base of /v1/verify for footers/QRs — NEKTAR_VERIFY_BASE for hosted
 *  deployments (https://host or ngrok), else the local HTTP port. */
export const verifyBase = (): string => nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://localhost:${nektarEnv('NEKTAR_HTTP_PORT') ?? 8080}`

// consolidated state machines: op → internal case label (never listed in
// TOOLS — reachable only through the 'sale'/'claim' dispatchers, which keep
// the paid-disclosure gate on BOTH the tool and the resolved op)
export const PAID_SALE_TOOLS = new Set([
  'sale',
  'sale#offer',
  'sale#offerMemory',
  'sale#escrow',
  'sale#escrowBuild',
  'sale#complete',
  'sale#refund',
  'sale#claim',
  'sale#pay',
  'sale#payBuild'
])
export const paidSaleEnabled = () => /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAID_DISCLOSURE') ?? '')

/** the paid-sale authority: an explicit NEKTAR_PAID_DISCLOSURE decides
 *  (truthy on, any other set value a hard off that outranks policy); unset,
 *  an approved policy carrying `disclosure.paid` is the grant — `policy
 *  approve` arms paid deals with no env and no restart */
export async function paidSaleAuthorized(au: NektarMemory['automation'] | undefined): Promise<boolean> {
  const env = nektarEnv('NEKTAR_PAID_DISCLOSURE')
  if (env !== undefined && env !== '') return paidSaleEnabled()
  if (au === undefined) return false
  for (const r of await au.store.policies().catch(() => [])) {
    if (r.revokedAt !== undefined) continue
    const p = await au.store.activePolicy(r.policyHash).catch(() => undefined)
    if (p !== undefined && p.disclosure.paid === true && p.agentPub === au.engine.agentPub && p.network === au.engine.network) return true
  }
  return false
}

/** live-network runtime surface handed in by the MCP/HTTP transport layer —
 *  absent under stdio-less callers (tests, hooks), in which case the funding
 *  tools fall back to mem's own reserve view */
export interface ToolCtx {
  /** true for unauthenticated external callers (HTTP without bearer); owner
   *  channels (stdio/MCP, bearer-authed) leave it unset — priced-content
   *  gates charge externals only */
  external?: boolean
  ops?: {
    wallet?: () => Promise<unknown>
    costs?: () => unknown
    importFunding?: (txid: string) => Promise<unknown>
    refreshFunding?: () => Promise<unknown>
  }
}

/** per-call bag handed to every group handler: the transport ctx, the payment
 *  record settled by the preamble gate, and the callTool recursion seam (the
 *  'sale'/'claim' group dispatchers recurse through it so the resolved op
 *  re-runs the payment gate and emits 'change'; dispatch/ cannot import
 *  ../server.js — depcruise no-circular — so index.ts injects it here) */
export interface DispatchCtx {
  ctx: ToolCtx
  paid?: PaymentRecord
  call: (mem: NektarMemory, name: string, args: any, ctx?: ToolCtx) => Promise<any>
}

export type ToolHandler = (mem: NektarMemory, args: any, d: DispatchCtx, name: string) => Promise<any>

/** the single non-revoked policy carrying `role` for this agent+network —
 *  exactly-one is an operator decision: none means "approve one first",
 *  several means "revoke down to one" — the agent never picks silently */
export async function singlePolicy(au: NonNullable<NektarMemory['automation']>, role: 'buyer' | 'seller', tool: string): Promise<string> {
  const found: string[] = []
  for (const r of await au.store.policies()) {
    if (r.revokedAt !== undefined) continue
    const p = await au.store.activePolicy(r.policyHash)
    if (p && p.roles.includes(role) && p.agentPub === au.engine.agentPub && p.network === au.engine.network) found.push(r.policyHash)
  }
  if (found.length !== 1) throw new Error(`${tool}: operator must approve exactly one ${role} policy (pnpm automation policy approve …)`)
  return found[0]
}
