// packwire.ts — the shard-pack wire edge extracted from NektarMemory:
// exportShardPack serializes a shard (records + provenance + AtomicBEEF
// evidence); importShardPack re-lands records byte-identically, re-verifies
// every bundle against the live chain, and audits the import on __imports__.
import { toHex, fromHex, serializeFragment, parseFragment, type Provenance, type Fragment } from '@nektar/core'
import { verifyFragment, type ProofBundle } from '@nektar/verify'
import type { Transaction } from '@bsv/sdk'
import type { LogRecord } from '@nektar/log'
import type { ChainTracker } from '@nektar/chain'
import type { ShardPack, ShardManifest, RememberResult } from './types.js'

export interface PackWireDeps {
  shardPrivate: (shard: string) => boolean
  storeAll: (shard: string) => Promise<LogRecord[]>
  /** prove() — supplies the record's own bundle for the pack */
  prove: (shard: string, seq: number) => Promise<{ bundle?: ProofBundle }>
  shardManifest: (shard: string) => Promise<ShardManifest | undefined>
  /** broadcast anchor txs — evidence BEEF lands here for offline inclusion checks */
  anchorTxs: Map<string, Transaction>
  txidShard: Map<string, string>
  /** 'shard:seq' → verified imported bundle — read by the prove path */
  importedBundles: Map<string, ProofBundle>
  touched: Set<string>
  logRemember: (shard: string, frag: Fragment, meta: Partial<Pick<LogRecord, 'level' | 'authorPub' | 'authorEpoch'>>) => Promise<{ rec: LogRecord }>
  remember: (
    shard: string,
    data: string,
    prov: (Partial<Provenance> & { sourcePub?: string; epoch?: number }) | undefined,
    level: number
  ) => Promise<RememberResult>
  chain: ChainTracker
  persistState: () => void
}

export class PackWire {
  constructor(private deps: PackWireDeps) {}

  async exportShardPack(shard: string): Promise<ShardPack> {
    if (this.deps.shardPrivate(shard)) throw new Error(`exportShard: shard "${shard}" is private — private shards refuse export`)
    const recs = (await this.deps.storeAll(shard)).sort((a, b) => a.seq - b.seq)
    if (recs.length === 0) throw new Error(`exportShard: shard "${shard}" is empty or unknown`)
    if (recs.some((r) => (r.level ?? 0) > 0)) throw new Error(`exportShard: shard "${shard}" holds level>0 records — refuses export`)
    const records: ShardPack['records'] = []
    for (const r of recs) {
      let bundle: ProofBundle | undefined
      if (r.anchorTxid !== undefined && r.batchIndex !== undefined) {
        const p = await this.deps.prove(shard, r.seq)
        bundle = p.bundle
      }
      records.push({
        seq: r.seq,
        data: Buffer.from(r.fragment.d).toString('utf8'),
        // the wire form of the fragment itself — encrypted records have d=''
        // with the payload inside E, so `data` alone can never rebuild them.
        // Carrying the serialized fragment lets an importer re-land the record
        // byte-identically and re-verify its leaf (S3 pack integrity).
        frag: toHex(serializeFragment(r.fragment)),
        prov: {
          source: r.fragment.p.source,
          time: r.fragment.p.time,
          signature: Buffer.from(r.fragment.p.signature).toString('hex'),
          modelId: r.fragment.p.modelId,
          ...(r.fragment.p.modality ? { modality: r.fragment.p.modality } : {})
        },
        level: r.level ?? 0,
        ...(bundle ? { bundle } : {})
      })
    }
    const manifest = await this.deps.shardManifest(shard)
    // F4 portable evidence — the anchor txs' AtomicBEEF (merklePath attached)
    // lets any verifier check inclusion against ITS headers, offline, without
    // trusting this host. Records cite anchorTxid; evidence carries the proof.
    const evidence: Record<string, string> = {}
    for (const r of recs) {
      if (r.anchorTxid === undefined) continue
      const tx = this.deps.anchorTxs.get(r.anchorTxid)
      if (tx) evidence[r.anchorTxid] = toHex(Uint8Array.from(tx.toAtomicBEEF()))
    }
    return {
      v: 1,
      kind: 'nektar-shard-pack',
      shard,
      exportedAt: Date.now(),
      ...(manifest ? { manifest } : {}),
      records,
      ...(Object.keys(evidence).length ? { evidence } : {})
    }
  }

  /** Import a shard pack: records re-land via remember() (byte-identical prov
   *  keeps leaves matching the source bundles), every bundle re-verifies via
   *  verifyFragment against the live chain, and the import event itself is
   *  recorded on __imports__ — the lineage is anchored, not asserted. The
   *  target shard must be empty (no mixing imported + native seqs). */
  async importShardPack(pack: unknown): Promise<{ imported: number; anchored: number; unverifiable: string[]; skipped: number }> {
    const p = pack as ShardPack
    if (p?.v !== 1 || p?.kind !== 'nektar-shard-pack' || typeof p.shard !== 'string' || !Array.isArray(p.records)) {
      throw new Error('importShard: malformed pack — expected {v:1, kind:"nektar-shard-pack", shard, records[]}')
    }
    if ((await this.deps.storeAll(p.shard)).length > 0) throw new Error(`importShard: shard "${p.shard}" already has records — import needs an empty shard`)
    // R10 — consume the evidence map: each anchor's AtomicBEEF (with BUMP once
    // mined) lands in anchorTxs, so verifyFragment checks inclusion offline
    // against THIS instance's headers — never the exporter's word.
    if (p.evidence && typeof p.evidence === 'object') {
      const { Transaction } = await import('@bsv/sdk')
      for (const [txid, beef] of Object.entries(p.evidence)) {
        try {
          const tx = Transaction.fromAtomicBEEF(Array.from(Buffer.from(String(beef), 'hex')))
          if (tx.id('hex') === txid) {
            this.deps.anchorTxs.set(txid, tx)
            this.deps.txidShard.set(txid, p.shard)
          }
        } catch {
          /* malformed evidence — ignored; records still verify by own bundles */
        }
      }
    }
    let imported = 0,
      anchored = 0,
      skipped = 0
    const unverifiable: string[] = []
    for (const rec of [...p.records].sort((a, b) => a.seq - b.seq)) {
      const prov =
        typeof rec.prov === 'object' && rec.prov !== null
          ? ({
              ...(rec.prov as Record<string, unknown>),
              signature:
                typeof (rec.prov as Record<string, unknown>).signature === 'string'
                  ? new Uint8Array(Buffer.from((rec.prov as Record<string, unknown>).signature as string, 'hex'))
                  : new Uint8Array(0)
            } as Partial<Provenance>)
          : undefined
      let r
      if (typeof rec.frag === 'string' && rec.frag.length > 0) {
        // verbatim fragment — preserves E/hdp so the re-landed record's leaf
        // recomputes exactly to the exporter's committed bundle (S3)
        const frag = parseFragment(fromHex(rec.frag))
        const { rec: lr } = await this.deps.logRemember(p.shard, frag, { level: rec.level ?? 0 })
        this.deps.touched.add(p.shard)
        r = { seq: lr.seq, leaf: toHex(lr.leaf) }
        if (frag.d.length > 0 && typeof rec.data === 'string' && Buffer.from(frag.d).toString('utf8') !== rec.data) {
          if (r.seq !== rec.seq) {
            skipped++
            continue
          }
          imported++
          unverifiable.push(`${p.shard}:${r.seq}: pack data does not match the fragment behind the bundle's leaf`)
          continue
        }
      } else {
        r = await this.deps.remember(p.shard, rec.data, prov, rec.level ?? 0)
      }
      if (r.seq !== rec.seq) {
        skipped++
        continue
      } // dedup/position drift — don't fabricate seqs
      imported++
      if (rec.bundle) {
        const recLeaf = (await this.deps.storeAll(p.shard)).find((x) => x.seq === r.seq)?.leaf
        if (!recLeaf || Buffer.from(recLeaf).toString('hex') !== rec.bundle.leaf) {
          unverifiable.push(`${p.shard}:${r.seq}: imported data does not recompute the bundle's leaf`)
          continue
        }
        // N10 — consume the pack's evidence: if the record's own bundle anchor
        // has no BUMP but evidence[txid] supplies one, substitute it. The
        // importer's own header chain still decides mining — never the
        // exporter's word.
        let bundle = rec.bundle
        try {
          const { Transaction } = await import('@bsv/sdk')
          const btx = Transaction.fromAtomicBEEF(bundle.anchorBEEF)
          const evtx = this.deps.anchorTxs.get(btx.id('hex'))
          if (!btx.merklePath && evtx?.merklePath) {
            btx.merklePath = evtx.merklePath
            bundle = { ...bundle, anchorBEEF: Array.from(btx.toAtomicBEEF()) }
          }
        } catch {}
        const v = await verifyFragment(bundle, this.deps.chain)
        if (v.ok) {
          this.deps.importedBundles.set(`${p.shard}:${r.seq}`, bundle)
          anchored++
        } else unverifiable.push(`${p.shard}:${r.seq}: ${v.reason}`)
      }
    }
    this.deps.persistState()
    await this.deps
      .remember(
        '__imports__',
        JSON.stringify({
          type: 'shard-import',
          shard: p.shard,
          exportedAt: p.exportedAt,
          imported,
          anchored,
          unverifiable: unverifiable.length,
          importedAt: Date.now()
        }),
        undefined,
        0
      )
      .catch(() => undefined) // the audit record is best-effort — never block the import
    return { imported, anchored, unverifiable, skipped }
  }
}
