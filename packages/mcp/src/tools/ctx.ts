// ctx.ts — the dispatch context every tool-group handler receives. server.ts's
// dispatchTool owns the payment gate, resolves the handler by tool name, and
// passes ONE of these through; group files never see the switch plumbing.
import { nektarEnv } from '@nektar/core'
import type { NektarMemory } from '../memory.js'

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

interface Dispatch {
  mem: NektarMemory
  name: string
  args: any
  ctx: ToolCtx
  /** the settled payment record when NEKTAR_PAYMENTS gated this call —
   *  receipt-producing tools embed it for auditability */
  paid: import('../pay.js').PaymentRecord | undefined
  /** re-entry into the full dispatcher — grouped ops ('sale', 'claim')
   *  forward to their internal '<group>#<op>' handlers through it */
  callTool: (mem: NektarMemory, name: string, args: any, ctx: ToolCtx) => Promise<any>
}
export type ToolHandler = (d: Dispatch) => Promise<any>

/** public base of /v1/verify for footers/QRs — NEKTAR_VERIFY_BASE for hosted
 *  deployments (https://host or ngrok), else the local HTTP port. */
export const verifyBase = (): string => nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://localhost:${nektarEnv('NEKTAR_HTTP_PORT') ?? 8080}`

/** ready-to-paste share surface for a receipt — the model drops `markdown`
 *  straight into PRs/docs; every pasted badge is a verify-page entry point */
export const shareBlock = (token: string) => {
  const verifyUrl = `${verifyBase()}/v1/verify?r=${token}`
  const badgeUrl = `${verifyBase()}/v1/badge?r=${token}`
  return { token, verifyUrl, badgeUrl, markdown: `[![verified answer](${badgeUrl})](${verifyUrl})` }
}
