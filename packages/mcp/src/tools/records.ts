// records.ts — the memory surface: remember/recall/prove plus the anchor that
// seals a shard batch. (Named records.ts so it never confuses the reader with
// memory.ts the class — this file is only its tool-facing doors.)
import type { ToolHandler } from './ctx.js'

export const RECORD_HANDLERS: Record<string, ToolHandler> = {
  remember: async (d) => {
    const { mem, args } = d
    if (args.private === false && mem.shardPrivate(args.shard)) throw new Error(`shard "${args.shard}" is private — the flag is monotone and cannot be unset`)
    const r = await mem.remember(args.shard, args.data, { source: args.source, modality: args.modality }, args.level ?? 0, { premiseRefs: args.premiseRefs })
    if (args.private === true) mem.setShardPrivate(args.shard)
    return { ...r, private: mem.shardPrivate(args.shard) || undefined }
  },
  recall: async (d) => {
    const { mem, args } = d
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
  prove: async (d) => {
    const { mem, args } = d
    // fail-closed: a private shard's plain merkle citation is a leak —
    // the same verification goal is met by proveMembership (ZK).
    if (mem.shardPrivate(args.shard))
      return {
        ok: false,
        reason: `shard "${args.shard}" is private — plain citations refused; cite via a ZK membership proof (zkQuery → proveMembership → verifyMembership)`
      }
    return mem.prove(args.shard, args.seq)
  },
  anchor: async (d) => {
    const { mem, args, paid } = d
    const r = await mem.anchorShard(args.shard)
    return paid ? { ...r, paid } : r
  }
}
