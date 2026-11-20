// operator/automation surface tools — extracted verbatim from the server.ts dispatchTool switch
import { paidSaleAuthorized, singlePolicy } from './shared.js'
import type { ToolHandler } from './shared.js'

export const handlers: Record<string, ToolHandler> = {
  // NOTE: the body below declares its own `d` (the doctor result) — the
  // dispatch ctx param is `dd` here so the verbatim body still compiles
  doctor: async (mem, args, dd) => {
    const d = await mem.doctor()
    if (dd.ctx.ops?.wallet) {
      const w = (await dd.ctx.ops.wallet()) as Record<string, unknown>
      d.funding = { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
    }
    // B3 — bootstrap state: 'sealed' = the agent publishes its bfinger
    // record and is discoverable; 'unsealed' = it still needs a configured
    // host set for counterparties to reach it
    const out: Record<string, unknown> = { ...d, bootstrap: mem.automation?.bootstrap?.() ?? 'unsealed' }
    // bumper layer — each degraded check names the unblock command, not just
    // the state; the caller should never need docs to take the next step
    const hints: string[] = []
    if (out.bootstrap === 'unsealed') hints.push("unsealed — set NEKTAR_MARKET_HOSTS to this instance's public host pair so counterparties can reach it")
    const funding = out.funding as Record<string, unknown> | undefined
    if (funding?.funded === false)
      hints.push(
        `unfunded — send ${out.network === 'main' ? '' : 'testnet '}sats to ${String(funding.address)}, then POST /v1/tools/fundingImport {"txid":"<txid>"} (or boot with NEKTAR_WAIT_FUNDING=1)`
      )
    if (typeof out.unprovenAnchors === 'number' && out.unprovenAnchors > 0)
      hints.push(`${out.unprovenAnchors} anchor(s) proving — safe to use meanwhile; nothing to do`)
    if (hints.length > 0) out.hints = hints
    if (args?.fix) return { ...out, repair: await mem.doctorRepair() }
    return out
  },
  want: async (mem, args) => {
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
  sell: async (mem, args) => {
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    // selling without paid disclosure can never settle — the buyer-side
    // sale# rail is gated off, so arming a seller would publish offers
    // that no counterparty may pay
    if (!(await paidSaleAuthorized(au)))
      throw new Error('sell: paid disclosure is disabled — set NEKTAR_PAID_DISCLOSURE=1 or approve a policy with disclosure.paid')
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
  automationDoctor: async (mem) => {
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    return await au.preflight()
  },
  deals: async (mem, args) => {
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    // first-party chain truth: the wallet ledger's per-txid states are the
    // record the labels can't lag — 'submitted' broadcast-accepted,
    // 'observed'/'confirmed' network-seen/proven, 'rejected'/'conflicted'
    // dead. One scan joins into every entry
    const ledger = new Map(
      (await au.store.walletTxs(['prepared', 'exported', 'submission_unknown', 'submitted', 'observed', 'confirmed', 'rejected', 'conflicted', 'expired'])).map(
        (w) => [w.txid, w.state]
      )
    )
    const chainOf = (txid: unknown): string | undefined => (typeof txid === 'string' ? ledger.get(txid) : undefined)
    const entry = (o: import('@nektar/automation').Operation): Record<string, unknown> => {
      const d = o.data
      const selKey = typeof (d.selected as { key?: string } | undefined)?.key === 'string' ? (d.selected as { key: string }).key : undefined
      const disclosureTxid = (d.result as { disclosureTxid?: string } | undefined)?.disclosureTxid
      const chain: Record<string, string> = {}
      for (const [k, txid] of [
        ['escrow', d.escrowTxid],
        ['disclosure', disclosureTxid ?? d.completeTxid],
        ['refund', d.refundTxid]
      ] as const) {
        const s = chainOf(txid)
        if (s !== undefined) chain[k] = s
      }
      return {
        operationId: o.operationId,
        role: o.role,
        kind: o.kind,
        state: o.state,
        wantId: (d.want as { want?: { wantId?: string } } | undefined)?.want?.wantId,
        priceSats: (d.amounts as { purchaseSats?: number } | undefined)?.purchaseSats ?? d.priceSats,
        counterparty: o.role === 'seller' ? (d.want as { want?: { pub?: string } } | undefined)?.want?.pub : selKey?.split(':')[0],
        escrowTxid: d.escrowTxid,
        disclosureTxid,
        completeTxid: d.completeTxid,
        sellerSats: d.sellerSats,
        refundTxid: d.refundTxid,
        ...(Object.keys(chain).length > 0 ? { chain } : {}),
        ...(d.evidenceRef !== undefined ? { evidence: { ref: d.evidenceRef, stored: true } } : {}),
        ...(o.lastError ? { reason: o.lastError } : {}),
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
      const waiting = await au.store.waitingOn(st.operation.operationId)
      const pending = waiting.filter((w) => w.status === 'pending')
      if (pending.length)
        e.waitingOn = pending.map((w) => ({
          action: w.kind,
          ...(w.deferClass ? { class: w.deferClass } : {}),
          ...(w.lastError ? { reason: w.lastError } : {}),
          nextAt: w.nextAt
        }))
      else if (e.reason === undefined) {
        const dead = waiting.find((w) => w.status === 'dead' && w.lastError !== undefined)
        if (dead) e.reason = dead.lastError
      }
      return e
    }
    const ops = (await au.store.operations())
      .filter((o) => o.kind === 'purchase' || o.kind === 'sale')
      .sort((a, b) => b.updatedAt - a.updatedAt)
      .slice(0, 50)
      .map(entry)
    return { deals: ops }
  },
  automation: async (mem, args) => {
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
      if (!(await paidSaleAuthorized(au)))
        throw new Error('automation enableSeller: paid disclosure is disabled — set NEKTAR_PAID_DISCLOSURE=1 or approve a policy with disclosure.paid')
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
    if (op === 'waits') return await au.waits()
    throw new Error('automation: op required — publishWant|enableSeller|status|operations|approvals|budget|policies|waits')
  }
}
