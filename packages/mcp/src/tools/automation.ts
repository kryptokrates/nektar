// automation.ts — the market-engine lane: want/sell entry points, deals
// views, the fund surface, and the raw 'automation' op dispatcher.
import type { NektarMemory } from '../memory.js'
import type { ToolHandler } from './ctx.js'

/** the single non-revoked policy carrying `role` for this agent+network —
 *  exactly-one is an operator decision: none means "approve one first",
 *  several means "revoke down to one" — the agent never picks silently */
async function singlePolicy(au: NonNullable<NektarMemory['automation']>, role: 'buyer' | 'seller', tool: string): Promise<string> {
  const found: string[] = []
  for (const r of await au.store.policies()) {
    if (r.revokedAt !== undefined) continue
    const p = await au.store.activePolicy(r.policyHash)
    if (p && p.roles.includes(role) && p.agentPub === au.engine.agentPub && p.network === au.engine.network) found.push(r.policyHash)
  }
  if (found.length !== 1) throw new Error(`${tool}: operator must approve exactly one ${role} policy (pnpm automation policy approve …)`)
  return found[0]
}

export const AUTOMATION_HANDLERS: Record<string, ToolHandler> = {
  want: async (d) => {
    const { mem, args } = d
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    const ph = await singlePolicy(au, 'buyer', 'want')
    const r = await au.engine.publishWant(
      {
        domains: (args?.domains ?? []).map(String),
        ...(args?.question ? { question: String(args.question).slice(0, 560) } : {}),
        maxSats: Number(args?.maxSats)
      },
      ph
    )
    void au.cycle()
    return { wantId: r.wantId, operationId: r.operationId }
  },
  sell: async (d) => {
    const { mem, args } = d
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('sell: shard required')
    if (!Array.isArray(args?.domains)) throw new Error('sell: domains[] required')
    const priceSats = Number(args?.priceSats)
    if (!Number.isSafeInteger(priceSats) || priceSats <= 0) throw new Error('sell: priceSats must be a positive integer')
    await mem.describeShard(args.shard, { domains: args.domains, desc: args.desc, offers: [{ kind: 'record', priceSats }] })
    const ph = await singlePolicy(au, 'seller', 'sell')
    await au.engine.enableSeller(ph)
    void au.cycle()
    return { shard: args.shard, domains: args.domains, priceSats, policyHash: ph }
  },
  automationDoctor: async (d) => {
    const { mem } = d
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    return await au.preflight()
  },
  deals: async (d) => {
    const { mem, args } = d
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    const entry = (o: import('@nektar/automation').Operation): Record<string, unknown> => {
      const d = o.data
      const selKey = typeof (d.selected as { key?: string } | undefined)?.key === 'string' ? (d.selected as { key: string }).key : undefined
      return {
        operationId: o.operationId,
        role: o.role,
        kind: o.kind,
        state: o.state,
        wantId: (d.want as { want?: { wantId?: string } } | undefined)?.want?.wantId,
        priceSats: (d.amounts as { purchaseSats?: number } | undefined)?.purchaseSats ?? d.priceSats,
        counterparty: o.role === 'seller' ? (d.want as { want?: { pub?: string } } | undefined)?.want?.pub : selKey?.split(':')[0],
        escrowTxid: d.escrowTxid,
        disclosureTxid: (d.result as { disclosureTxid?: string } | undefined)?.disclosureTxid,
        refundTxid: d.refundTxid,
        ...(d.evidenceRef !== undefined ? { evidence: { ref: d.evidenceRef, stored: true } } : {}),
        updatedAt: o.updatedAt
      }
    }
    const detail = (o: import('@nektar/automation').Operation, events: unknown): Record<string, unknown> => {
      const e: Record<string, unknown> = { ...entry(o), data: o.data, events, ...(o.lastError ? { lastError: o.lastError } : {}) }
      if (o.role === 'buyer' && (o.state === 'disclosure_verified' || o.state === 'useful')) {
        const r = o.data.result as { text?: string; taint?: string } | undefined
        // D4 — the taint tag rides the consumer surface: purchased content
        // reaches the caller TAGGED, never bare
        if (r?.text !== undefined) e.result = { text: r.text, taint: r.taint ?? 'untrusted-purchased-content' }
      }
      return e
    }
    if (args?.operationId) {
      const st = await au.engine.status(String(args.operationId))
      if (!st) throw new Error(`deals: unknown operationId ${args.operationId}`)
      const e = detail(st.operation, st.events)
      const j = await au.dealJournal?.(st.operation)
      if (j !== undefined) e.journal = j
      return e
    }
    const ops = (await au.store.operations())
      .filter((o) => o.kind === 'purchase' || o.kind === 'sale')
      .sort((a, b) => b.updatedAt - a.updatedAt)
      .slice(0, 50)
      .map(entry)
    return { deals: ops }
  },
  fund: async (d) => {
    const { mem, ctx } = d
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    const w = ctx.ops?.wallet ? ((await ctx.ops.wallet()) as Record<string, unknown>) : ((await mem.fundingStatus()) as unknown as Record<string, unknown>)
    const slots = await au.marketSlots()
    const laneStock = await au.laneStock()
    const live = (await au.store.policies()).filter((x) => x.revokedAt === undefined)
    return {
      address: w.address,
      network: au.engine.network,
      spendableSats: Number(w.balanceSats ?? 0),
      utxos: Number(w.utxos ?? 0),
      marketSlots: slots,
      laneStock,
      ...(live.length === 1 ? { budget: await au.store.budgetStatus(live[0].policyHash) } : {})
    }
  },
  automation: async (d) => {
    const { mem, args } = d
    const au = mem.automation
    if (!au) throw new Error('automation: not enabled — start the instance with NEKTAR_AUTOMATION=1')
    const op = String(args?.op ?? '')
    if (op === 'publishWant') {
      const r = await au.engine.publishWant(
        {
          domains: (args?.domains ?? []).map(String),
          ...(args?.question ? { question: String(args.question).slice(0, 560) } : {}),
          maxSats: Number(args?.maxSats),
          ...(args?.wantId ? { wantId: String(args.wantId) } : {})
        },
        String(args?.policyHash ?? '')
      )
      void au.cycle()
      return { ok: true, ...r }
    }
    if (op === 'enableSeller') {
      await au.engine.enableSeller(String(args?.policyHash ?? ''))
      void au.cycle()
      return { ok: true }
    }
    if (op === 'status') {
      const st = await au.engine.status(String(args?.operationId ?? ''))
      if (!st) throw new Error('automation: unknown operationId')
      return st
    }
    if (op === 'operations')
      return {
        operations: (await au.store.operations()).map((o) => ({
          operationId: o.operationId,
          role: o.role,
          kind: o.kind,
          state: o.state,
          updatedAt: o.updatedAt,
          ...(o.lastError ? { lastError: o.lastError } : {})
        }))
      }
    if (op === 'approvals') return { approvals: await au.store.approvals('pending') }
    if (op === 'budget') return await au.store.budgetStatus(String(args?.policyHash ?? ''))
    if (op === 'policies') return { policies: await au.store.policies() }
    throw new Error('automation: op required — publishWant|enableSeller|status|operations|approvals|budget|policies')
  }
}
