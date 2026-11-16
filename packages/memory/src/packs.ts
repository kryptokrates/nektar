// MemPacks — the catalog/pack surface: CatalogOps, PackOps and PackWire
// delegates, shard privacy flags and the public-pack listing gates.
import type { LogRecord } from '@nektar/log'
import type { CatalogOpsPort } from './ports.js'
import type { ManifestOffer, ShardManifest, ShardPack } from './types.js'
import { MemCustody } from './custody.js'

export abstract class MemPacks extends MemCustody {
  /** the shard's log records (read view — used by prove/disclose tests) */
  async records(shard: string) {
    return await this.catalogOps.records(shard)
  }
  /** plaintext candidates for marketplace matching — owner-side decrypt of own
   *  encrypted level-0 records; the sale still delivers only the fragment key */
  async marketCandidates(shard: string): Promise<Array<{ seq: number; text: string }>> {
    return await this.catalogOps.marketCandidates(shard)
  }

  /** read a record's plaintext — decrypting E via the level key when the
   *  instance holds the seed (same path recall/disclose use). */
  protected async recordText(rec: LogRecord): Promise<string | undefined> {
    return await this.cryptoOps.recordText(rec)
  }

  /** latest committed manifest per described shard — the catalog projection
   *  the market indexes (one entry per shard, latest record wins). */
  async shardCatalog(): Promise<ShardManifest[]> {
    return await this.catalogOps.shardCatalog()
  }

  /** latest committed manifest for one shard */
  async shardManifest(shard: string): Promise<ShardManifest | undefined> {
    return await this.catalogOps.shardManifest(shard)
  }

  /** the content price a manifest declares for `kind` ('pack' or 'read' via
   *  'answer' offers) — 0 when free/undeclared. Drives the paid-content gate. */
  async contentPrice(shard: string, kind: 'read' | 'pack'): Promise<number> {
    return await this.catalogOps.contentPrice(shard, kind)
  }

  /** land a purchased record locally — call AFTER the sale claim verified
   *  it (leaf-commitment + provenance are the buyer's responsibility; this
   *  only stores what was already checked). The record keeps the seller's
   *  provenance fields; an __imports__ audit record commits the acquisition
   *   *  (source shard:seq, leaf, settlement txid) so the lineage is anchored. */
  async importRecord(args: Parameters<CatalogOpsPort['importRecord']>[0]): Promise<{ seq: number }> {
    return await this.catalogOps.importRecord(args)
  }

  /** Commit a shard manifest to __catalog__ — the domain tags another agent
   *  searches by ('who knows about soccer'). The claim is anchored and
   *  timestamped like any memory: false advertising stays attributable.
   *  Re-describing appends a newer manifest; the old one remains history. */
  async describeShard(
    shard: string,
    fields: { domains: string[]; desc?: string; offers?: ManifestOffer[] }
  ): Promise<{ manifest: ShardManifest; seq: number }> {
    return await this.catalogOps.describeShard(shard, fields)
  }
  /** PUT a shard pack to bstore when its bytes differ from the stored copy —
   *  the stored ref pins sha256, so a record change must re-store or remote
   *  importers keep resolving the stale pack. Also commits the fresh pack as
   *  a bfinger store member (kind-5 + re-issued manifest) so the identity
   *  record's refs resolve the current set with no /.well-known lookup.
   *  Best-effort: a store failure never fails the caller. Returns the
   *  (possibly pre-existing) ref. */
  async storePack(shard: string, pack: ShardPack, network?: string): Promise<import('@nektar/store').ObjectRef | undefined> {
    return await this.packOps.storePack(shard, pack, network)
  }
  /** attach the bfinger packs store post-construction (the automation
   *  runtime builds it once lanes exist); restores members persisted by a
   *  previous run and binds state persistence. */
  attachPacksStore(store: import('@nektar/store').PacksStore): void {
    this.packOps.attachPacksStore(store)
  }
  /** the configured bstore/S3 object store (undefined = feature off) */
  get objectStore(): import('@nektar/store').S3ObjectStore | undefined {
    return this.opts.objectStore
  }
  get feeRateSatPerKb(): number {
    return this.opts.feeRateSatPerKb ?? 1
  }
  /** confirmations before an anchor counts final — the same depth the
   *  automation engine requires of a settlement proof */
  get finalityDepth(): number {
    return this.opts.finalityDepth ?? 1
  }
  /** pack-store refs this instance PUT to bstore — the `.well-known` doc
   *  names them so importers can fetch through the store instead of HTTP. */
  packStoreRef(shard: string): import('@nektar/store').ObjectRef | undefined {
    return this.packOps.packStoreRef(shard)
  }
  /** fetch + verify a pack through the object store — every byte checks
   *  against the ref's digest or the CHIRP closure before importShardPack's
   *  own re-verification. Resolution order: a chirp:// ref resolves through
   *  the signed store first, then unsigned against the advertised endpoint
   *  (no credentials needed on a public-read gateway); the keyed object is
   *  the compatibility fallback, signed then unsigned. */
  async fetchStoredPack(ref: { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string }): Promise<ShardPack> {
    return await this.packOps.fetchStoredPack(ref)
  }
  /** the committed hash + anchor of a fragment — for redacted (private) cites
   *  that carry the proof value without naming the shard. */
  async fragmentRef(shard: string, seq: number): Promise<{ hdp: string; anchorTxid?: string } | undefined> {
    const rec = await this.store.bySeq(shard, seq)
    return rec ? { hdp: Buffer.from(rec.hdp).toString('hex'), anchorTxid: rec.anchorTxid } : undefined
  }

  /** Export a PUBLIC shard as a portable pack: each record's plaintext +
   *  provenance + the ProofBundle prove() produces for anchored ones.
   *  Private shards and level>0 records refuse — a pack must never carry
   *  gated content. */
  async exportShardPack(shard: string): Promise<ShardPack> {
    return await this.packWire.exportShardPack(shard)
  }

  /** Import a shard pack: records re-land via remember() (byte-identical prov
   *  keeps leaves matching the source bundles), every bundle re-verifies via
   *  verifyFragment against the live chain, and the import event itself is
   *  recorded on __imports__ — the lineage is anchored, not asserted. The
   *  target shard must be empty (no mixing imported + native seqs). */
  async importShardPack(pack: unknown): Promise<{ imported: number; anchored: number; unverifiable: string[]; skipped: number }> {
    return await this.packWire.importShardPack(pack)
  }

  /** is this shard declared private? */
  shardPrivate(shard: string): boolean {
    return this.flags.get(shard)?.private === true
  }

  /** shards an operator can offer as public packs — excludes private shards,
   *  `__*` system shards, and any shard holding level>0 records (the same
   *  gates exportShardPack enforces; listing here means "fetchable"). */
  async publicShards(): Promise<string[]> {
    const out: string[] = []
    for (const s of this.shards()) {
      if (s.startsWith('__') || this.shardPrivate(s)) continue
      if ((await this.store.all(s)).some((r) => (r.level ?? 0) > 0)) continue
      out.push(s)
    }
    return out
  }
  /** declare a shard private — monotone; once set it cannot be unset. */
  setShardPrivate(shard: string): void {
    const f = this.flags.get(shard) ?? {}
    f.private = true
    this.flags.set(shard, f)
    this.persistState()
  }

  /** the shard's on-chain id — hash160 of the genesis owner pubkey, stable
   *  across owner rotations (the anchor head is authoritative when present).
   *  Binds a manifest's label to the anchor chain a verifier can look up. */
  async shardId(shard: string): Promise<string> {
    const ac = this.anchorChain(shard)
    await ac.ready
    return ac.getShardId()
  }
}
