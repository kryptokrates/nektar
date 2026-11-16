// packs.ts — shard packs, remote imports, stored evidence, doctor and the
// importRecord lineage surface: everything that moves whole bundles, not
// single records.
import { verifyBase, type ToolHandler } from './ctx.js'

export const PACK_HANDLERS: Record<string, ToolHandler> = {
  exportShard: async (d) => {
    const { mem, args } = d
    if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('exportShard: shard required')
    const pack = await mem.exportShardPack(args.shard)
    // the serve endpoint makes the pack a shareable URL — an invite the
    // agent can hand to another operator ('importShard from this URL').
    // The store ref is preferred: it resolves origin-free and re-stores
    // when the shard changed since the last PUT (the ref pins sha256).
    const store = await mem.storePack(args.shard, pack).catch(() => undefined)
    return {
      ...pack,
      ...(store !== undefined ? { store } : {}),
      packUrl: `${verifyBase()}/v1/pack/${encodeURIComponent(args.shard)}`
    }
  },
  importShard: async (d) => {
    const { mem, args } = d
    let pack = args?.pack
    // object-store ref — fetch through getVerified (digest-checked) before
    // the pack's own re-verification decides what lands
    if (pack === undefined && typeof args?.store === 'object' && args.store !== null) {
      const r = args.store as { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string }
      const keyed = typeof r.key === 'string' && typeof r.sha256 === 'string'
      if (!keyed && typeof r.chirp !== 'string') throw new Error('importShard: store ref needs {key, sha256} or {chirp} (bucket/size/endpoint optional)')
      if (r.endpoint !== undefined && !/^https?:\/\//i.test(r.endpoint)) throw new Error('importShard: store endpoint must be http(s)')
      pack = await mem.fetchStoredPack(r)
    }
    if (pack === undefined) {
      const url =
        typeof args?.packUrl === 'string'
          ? args.packUrl
          : typeof args?.from === 'string' && typeof args?.shard === 'string'
            ? `${args.from.replace(/\/+$/, '')}/v1/pack/${encodeURIComponent(args.shard)}`
            : undefined
      if (!url) throw new Error('importShard: give {pack}, {from,shard}, {packUrl}, or {store}')
      if (!/^https?:\/\//i.test(url)) throw new Error('importShard: pack URL must be http(s)')
      const r = await fetch(url, { signal: AbortSignal.timeout(15_000) })
      if (!r.ok) throw new Error(`importShard: fetch ${url} → ${r.status}`)
      pack = await r.json()
    }
    if (typeof pack !== 'object' || pack === null) throw new Error('importShard: pack object required (ShardPack JSON from exportShard or a pack URL)')
    return await mem.importShardPack(pack)
  },
  evidence: async (d) => {
    const { mem, args } = d
    // stored trade-evidence bundles — operator profile only. Fetches the
    // bundle through getVerified (digest/size checked against the ref) and
    // runs the evidence verifier; redacted bundles report which checks the
    // withheld question/disclosure fields make unavailable.
    const { objectStoreFromEnv } = await import('@nektar/store')
    const { fetchObjectByRef } = await import('@nektar/store')
    const { verifyTradeEvidence } = await import('@nektar/automation')
    const st = mem.objectStore ?? mem.automation?.objectStore ?? objectStoreFromEnv()
    let ref = args?.ref as { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string } | undefined
    if (ref === undefined && typeof args?.operationId === 'string') {
      const au = mem.automation
      if (!au) throw new Error('evidence: operationId lookup needs automation (NEKTAR_AUTOMATION=1)')
      ref = (await au.store.operation(args.operationId))?.data.evidenceRef as typeof ref
      if (ref === undefined) throw new Error(`evidence: no stored evidence for ${args.operationId}`)
    }
    if (ref === undefined) throw new Error('evidence: give {ref} or {operationId}')
    // the shared store-ref chain: chirp signed → chirp unsigned → keyed
    // signed → keyed unsigned — evidence resolves with no credentials when
    // the seller's gateway serves public reads
    const bytes = await fetchObjectByRef(st, ref)
    return verifyTradeEvidence(JSON.parse(new TextDecoder().decode(bytes)))
  },
  doctor: async (d) => {
    const { mem, args, ctx } = d
    const doc = await mem.doctor()
    if (ctx.ops?.wallet) {
      const w = (await ctx.ops.wallet()) as Record<string, unknown>
      doc.funding = { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
    }
    // B3 — bootstrap state: 'sealed' = the agent publishes its bfinger
    // record and is discoverable; 'unsealed' = it still needs a configured
    // host set for counterparties to reach it
    const out: Record<string, unknown> = { ...doc, bootstrap: mem.automation?.bootstrap?.() ?? 'unsealed' }
    if (args?.fix) return { ...out, repair: await mem.doctorRepair() }
    return out
  },
  describeShard: async (d) => {
    const { mem, args } = d
    if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('describeShard: shard required')
    if (!Array.isArray(args?.domains)) throw new Error('describeShard: domains[] required — the tags peers search by')
    return await mem.describeShard(args.shard, { domains: args.domains, desc: args.desc, offers: args.offers })
  },
  importRecord: async (d) => {
    const { mem, args } = d
    // land a record bought via sale#claim — caller passes the verified
    // claimed payload; lineage (from shard:seq, leaf, settlement txid)
    // commits on __imports__
    if (typeof args?.data !== 'string') throw new Error('importRecord: data required (the verified claim payload)')
    return await mem.importRecord({
      data: args.data,
      provenance: args.provenance,
      sourceShard: args.sourceShard,
      sourceSeq: args.sourceSeq,
      leaf: args.leaf,
      saleTxid: args.saleTxid,
      shard: args.shard
    })
  }
}
