// catalogops.ts — the catalog read edge extracted from NektarMemory:
// record listing, market plaintext candidates, the __catalog__ manifest
// projection, and content pricing. Reads only; the write path
// (describeShard/storePack/export/import) is a separate cluster.
import type { ShardStore, LogRecord } from '@nektar/log'
import type { ShardManifest } from './memory.js'
import { CATALOG_SHARD } from './memory.js'

export interface CatalogOpsDeps {
  store: ShardStore
  /** plaintext read — decrypts E under the instance's custody (CryptoOps) */
  recordText: (rec: LogRecord) => Promise<string | undefined>
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
}
