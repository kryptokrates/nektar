// catalogops.ts — the catalog read edge extracted from NektarMemory:
// record listing, market plaintext candidates, the __catalog__ manifest
// projection, and content pricing. Reads only; the write path
// (describeShard/storePack/export/import) is a separate cluster.
import { nektarEnv, type Provenance } from '@nektar/core'
import type { ShardStore, LogRecord } from '@nektar/log'
import type { ObjectRef } from '@nektar/store'
import type { ShardManifest, ShardPack, ManifestOffer, NektarMemoryOptions } from './types.js'
import { CATALOG_SHARD } from './types.js'

export interface CatalogOpsDeps {
  opts: NektarMemoryOptions
  store: ShardStore
  /** plaintext read — decrypts E under the instance's custody (CryptoOps) */
  recordText: (rec: LogRecord) => Promise<string | undefined>
  /** shards the instance has written or anchored (describeShard gate) */
  shards: () => string[]
  shardId: (shard: string) => Promise<string>
  remember: (shard: string, data: string, prov?: Partial<Provenance> & { sourcePub?: string; epoch?: number }, level?: number) => Promise<{ seq: number }>
  exportShardPack: (shard: string) => Promise<ShardPack>
  storePack: (shard: string, pack: ShardPack, network?: string) => Promise<ObjectRef | undefined>
  metricsInc: (name: string) => void
}

export class CatalogOps {
  constructor(private deps: CatalogOpsDeps) {}

  /** the shard's log records (read view — used by prove/disclose tests) */
  async records(shard: string): Promise<LogRecord[]> {
    return await this.deps.store.all(shard)
  }
  /** plaintext candidates for marketplace matching — owner-side decrypt of own
   *  encrypted level-0 records; the sale still delivers only the fragment key */
  async marketCandidates(shard: string): Promise<Array<{ seq: number; text: string }>> {
    const out: Array<{ seq: number; text: string }> = []
    for (const r of await this.records(shard)) {
      if ((r.level ?? 0) !== 0) continue
      const text = await this.deps.recordText(r).catch(() => undefined)
      if (text?.length) out.push({ seq: r.seq, text: text.slice(0, 4000) })
    }
    return out
  }
  /** latest committed manifest per described shard — the catalog projection
   *  the market indexes (one entry per shard, latest record wins). */
  async shardCatalog(): Promise<ShardManifest[]> {
    const by = new Map<string, { m: ShardManifest; seq: number }>()
    for (const rec of await this.deps.store.all(CATALOG_SHARD)) {
      const data = await this.deps.recordText(rec)
      if (!data) continue
      let m: ShardManifest
      try {
        m = JSON.parse(data)
      } catch {
        continue
      }
      if (m?.v !== 1 || m.type !== 'shard-manifest' || typeof m.shard !== 'string') continue
      const cur = by.get(m.shard)
      if (!cur || rec.seq > cur.seq) by.set(m.shard, { m, seq: rec.seq })
    }
    return [...by.values()].map((x) => x.m)
  }
  /** latest committed manifest for one shard */
  async shardManifest(shard: string): Promise<ShardManifest | undefined> {
    return (await this.shardCatalog()).find((m) => m.shard === shard)
  }

  /** the content price a manifest declares for `kind` ('pack' or 'read' via
   *  'answer' offers) — 0 when free/undeclared. Drives the paid-content gate. */
  async contentPrice(shard: string, kind: 'read' | 'pack'): Promise<number> {
    const m = await this.shardManifest(shard).catch(() => undefined)
    const match = m?.offers?.find((o) => (kind === 'read' ? o.kind === 'answer' : o.kind === 'pack') && Number.isSafeInteger(o.priceSats))
    return match?.priceSats ?? 0
  }

  /** land a purchased record locally — call AFTER the sale claim verified
   *  it (leaf-commitment + provenance are the buyer's responsibility; this
   *  only stores what was already checked). The record keeps the seller's
   *  provenance fields; an __imports__ audit record commits the acquisition
   *  (source shard:seq, leaf, settlement txid) so the lineage is anchored. */
  async importRecord(args: {
    data: string
    provenance?: { sourcePub?: string; hash?: string; timestamp?: number; sig?: string }
    sourceShard?: string
    sourceSeq?: number
    leaf?: string
    saleTxid?: string
    shard?: string
  }): Promise<{ seq: number }> {
    const shard = args.shard ?? 'bought'
    const prov = args.provenance?.sourcePub
      ? {
          hash: args.provenance.hash ?? '',
          timestamp: args.provenance.timestamp ?? Date.now(),
          sig: args.provenance.sig ?? '',
          sourcePub: args.provenance.sourcePub
        }
      : undefined
    const { seq } = await this.deps.remember(shard, args.data, prov)
    await this.deps
      .remember(
        '__imports__',
        JSON.stringify({
          type: 'record-import',
          fromShard: args.sourceShard ?? 'unknown',
          fromSeq: args.sourceSeq ?? null,
          ...(args.leaf ? { leaf: args.leaf } : {}),
          ...(args.saleTxid ? { saleTxid: args.saleTxid } : {}),
          toShard: shard,
          seq,
          importedAt: Date.now()
        })
      )
      .catch(() => undefined)
    return { seq }
  }

  /** Commit a shard manifest to __catalog__ — the domain tags another agent
   *  searches by ('who knows about soccer'). The claim is anchored and
   *  timestamped like any memory: false advertising stays attributable.
   *  Re-describing appends a newer manifest; the old one remains history. */
  async describeShard(
    shard: string,
    fields: { domains: string[]; desc?: string; offers?: ManifestOffer[] }
  ): Promise<{ manifest: ShardManifest; seq: number }> {
    if (shard.startsWith('__')) throw new Error('describeShard: system shards (__*) cannot be described')
    if (!this.deps.shards().includes(shard)) throw new Error(`describeShard: unknown shard "${shard}" — describe a shard this instance has written`)
    const domains = [...new Set((fields.domains ?? []).map((d) => String(d).trim().toLowerCase()).filter((d) => /^[a-z0-9][a-z0-9._-]{0,47}$/.test(d)))].slice(
      0,
      32
    )
    if (domains.length === 0) throw new Error('describeShard: at least one valid domain tag required (a-z0-9._-, up to 48 chars)')
    const desc = fields.desc !== undefined ? String(fields.desc).slice(0, 560) : undefined
    const offers = (fields.offers ?? [])
      .map((o) => ({
        kind: String(o?.kind) as ManifestOffer['kind'],
        ...(o?.priceSats !== undefined ? { priceSats: Math.max(0, Math.floor(Number(o.priceSats))) } : {}),
        ...(o?.seq !== undefined ? { seq: Math.floor(Number(o.seq)) } : {})
      }))
      .filter((o) => o.kind === 'answer' || o.kind === 'pack' || o.kind === 'record')
      .slice(0, 64)
    const manifest: ShardManifest = {
      v: 1,
      type: 'shard-manifest',
      shard,
      shardId: await this.deps.shardId(shard),
      domains,
      ...(desc ? { desc } : {}),
      ...(offers.length ? { offers } : {}),
      network: nektarEnv('NEKTAR_NETWORK') ?? 'regtest',
      issuedAt: Date.now()
    }
    const r = await this.deps.remember(CATALOG_SHARD, JSON.stringify(manifest))
    // store the pack when a bstore gateway is configured — only for shards
    // the public-pack rule admits (not private, no level>0 records;
    // exportShardPack enforces the same gates). Best-effort: a store
    // failure never fails the describe, and /v1/pack stays the fallback.
    if (this.deps.opts.objectStore !== undefined) {
      try {
        const pack = await this.deps.exportShardPack(shard)
        await this.deps.storePack(shard, pack, manifest.network ?? 'regtest')
      } catch {
        this.deps.metricsInc('nektar_pack_store_errors_total')
      }
    }
    return { manifest, seq: r.seq }
  }
}
