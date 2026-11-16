// funding + provisioning tools — extracted verbatim from the server.ts dispatchTool switch
import type { ToolHandler } from './shared.js'

export const handlers: Record<string, ToolHandler> = {
  fundingStatus: async (mem, args, d) => {
    // prefer the runtime's live wallet view (spend caps, runway, correct
    // net prefix); fall back to the reserve mem tracks itself
    if (d.ctx.ops?.wallet) {
      const w = (await d.ctx.ops.wallet()) as Record<string, unknown>
      return { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
    }
    return await mem.fundingStatus()
  },
  provisionPlan: async (mem) => {
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    const plan = await au.provisionPlan()
    const w = await mem.fundingStatus()
    return { ...plan, address: w.address }
  },
  provisionPay: async (mem, args) => {
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
  fundingImport: async (mem, args, d) => {
    if (!d.ctx.ops?.importFunding) throw new Error('fundingImport: live network only (regtest funds via the test harness)')
    if (typeof args?.txid !== 'string' || args.txid.length < 8) throw new Error('fundingImport: txid required')
    const res = (await d.ctx.ops.importFunding(args.txid)) as Record<string, unknown>
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
  fundingRefresh: async (mem, args, d) => {
    if (!d.ctx.ops?.refreshFunding) throw new Error('fundingRefresh: live network only')
    return await d.ctx.ops.refreshFunding()
  },
  fund: async (mem, args, d) => {
    const au = mem.automation
    if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
    const w = d.ctx.ops?.wallet ? ((await d.ctx.ops.wallet()) as Record<string, unknown>) : ((await mem.fundingStatus()) as unknown as Record<string, unknown>)
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
  }
}
