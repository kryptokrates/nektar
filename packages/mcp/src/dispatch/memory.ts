// shard record + pack tools — extracted verbatim from the server.ts dispatchTool switch
import { verifyBase } from './shared.js'
import type { ToolHandler } from './shared.js'
import { sparse } from '@nektar/core'

export const handlers: Record<string, ToolHandler> = {
  remember: async (mem, args) => {
    if (args.private === false && mem.shardPrivate(args.shard)) throw new Error(`shard "${args.shard}" is private — the flag is monotone and cannot be unset`)
    // `__*` is the reserved system namespace (__receipts__, __revocation__,
    // __catalog__, __imports__, __identity__, __model__, __corrections__,
    // __access__) — caller writes there forge receipts/revocations or poison
    // recall; only internal modules may write system shards
    if (typeof args.shard !== 'string' || args.shard.startsWith('__'))
      throw new Error('remember: shard names starting with "__" are reserved for system records')
    const level = args.level ?? 0
    // a stored non-integer/negative level throws in the recall loop's BigInt()
    // and permanently poisons the shard on the append-only store
    if (!Number.isInteger(level) || level < 0) throw new Error('remember: level must be a non-negative integer')
    const r = await mem.remember(args.shard, args.data, { source: args.source, modality: args.modality }, level, { premiseRefs: args.premiseRefs })
    if (args.private === true) mem.setShardPrivate(args.shard)
    return { ...r, private: mem.shardPrivate(args.shard) || undefined }
  },
  recall: async (mem, args, d) => {
    // parity with the prove tool's refusal: private/system shards are not
    // served to external callers (the private flag gates citations, packs and
    // listings — it must gate reads too)
    if (d.ctx.external && (mem.shardPrivate(args.shard) || String(args.shard).startsWith('__')))
      return {
        ok: false,
        reason: `shard "${args.shard}" is private — contents refused; verify via a ZK membership proof (zkQuery → proveMembership → verifyMembership)`
      }
    const { decodePresentation } = await import('@nektar/identity')
    const r = await mem.recall(args.shard, { seq: args.seq, identity: args.identity, zk: args.zk === undefined ? undefined : decodePresentation(args.zk) })
    // tell the model how to recover from a wrong seq instead of an empty answer
    if (args.seq !== undefined && r.items.length === 0) {
      const seqs = (await mem.records(args.shard)).map((x) => x.seq)
      return {
        ...r,
        hint: seqs.length
          ? `no record with seq ${args.seq} in shard "${args.shard}"; existing seqs: ${seqs.join(', ')} — or omit seq to recall all`
          : `shard "${args.shard}" is empty`
      }
    }
    return { ...r, shardPrivate: mem.shardPrivate(args.shard) || undefined }
  },
  prove: async (mem, args) => {
    // fail-closed: a private shard's plain merkle citation is a leak —
    // the same verification goal is met by proveMembership (ZK).
    if (mem.shardPrivate(args.shard))
      return {
        ok: false,
        reason: `shard "${args.shard}" is private — plain citations refused; cite via a ZK membership proof (zkQuery → proveMembership → verifyMembership)`
      }
    return mem.prove(args.shard, args.seq)
  },
  anchor: async (mem, args, d) => {
    const r = await mem.anchorShard(args.shard)
    return d.paid ? { ...r, paid: d.paid } : r
  },
  disclose: async (mem, args) => {
    const idm = await import('@nektar/identity')
    const verifyAnchor = async () => (await mem.prove(args.shard, args.seq)).result.ok === true
    const r = await idm.verifyDisclosureAnchored(
      args.envelope,
      args.ownerPub,
      args.verifierPub,
      args.fieldCount,
      Buffer.from(args.fieldRoot, 'hex'),
      (args.siblings as string[]).map((s) => Buffer.from(s, 'hex')),
      args.atTime,
      verifyAnchor
    )
    return { verified: r.ok, reason: r.reason ?? null, receipt: { decision: r.ok ? 'allow' : 'deny', action: 'disclose' } }
  },
  exportShard: async (mem, args) => {
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
  importShard: async (mem, args) => {
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
      const { egressAllowed, nektarEnv } = await import('@nektar/core')
      if (!egressAllowed(url, { allowHttp: true, allowLocal: nektarEnv('NEKTAR_NETWORK') === 'regtest' }))
        throw new Error('importShard: pack URL host refused by egress policy')
      const r = await fetch(url, { signal: AbortSignal.timeout(15_000), redirect: 'error' })
      if (!r.ok) throw new Error(`importShard: fetch ${url} → ${r.status}`)
      pack = await r.json()
    }
    if (typeof pack !== 'object' || pack === null) throw new Error('importShard: pack object required (ShardPack JSON from exportShard or a pack URL)')
    return await mem.importShardPack(pack)
  },
  evidence: async (mem, args) => {
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
      const stored = sparse((await au.store.operation(args.operationId))?.data.evidenceRef)
      if (stored === undefined) throw new Error(`evidence: no stored evidence for ${args.operationId}`)
      ref = stored
    }
    if (ref === undefined) throw new Error('evidence: give {ref} or {operationId}')
    // the shared store-ref chain: chirp signed → chirp unsigned → keyed
    // signed → keyed unsigned — evidence resolves with no credentials when
    // the seller's gateway serves public reads
    const bytes = await fetchObjectByRef(st, ref)
    return verifyTradeEvidence(JSON.parse(new TextDecoder().decode(bytes)))
  },
  describeShard: async (mem, args) => {
    if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('describeShard: shard required')
    if (!Array.isArray(args?.domains)) throw new Error('describeShard: domains[] required — the tags peers search by')
    return await mem.describeShard(args.shard, { domains: args.domains, desc: args.desc, offers: args.offers })
  },
  importRecord: async (mem, args) => {
    // land a record bought via sale#claim — caller passes the verified
    // claimed payload; lineage (from shard:seq, leaf, settlement txid)
    // commits on __imports__
    if (typeof args?.data !== 'string') throw new Error('importRecord: data required (the verified claim payload)')
    if (typeof args.shard === 'string' && args.shard.startsWith('__'))
      throw new Error('importRecord: shard names starting with "__" are reserved for system records')
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
