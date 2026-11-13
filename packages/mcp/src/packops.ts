// packops.ts — the pack publish/fetch edge extracted from NektarMemory:
// PUT shard packs to bstore (content-addressed via chirp closure), commit
// them as bfinger store members, attach the packs store post-construction,
// and resolve stored refs back into ShardPack objects.
import { utf8, toHex, sha256, nektarEnv } from '@nektar/core'
import { storeChirp, fetchObjectByRef, type ObjectRef, type PacksStore, type PackMember } from '@nektar/store'
import type { ShardPack, NektarMemoryOptions } from './types.js'

export interface PackOpsDeps {
  opts: NektarMemoryOptions
  /** shard → last stored pack ref — persisted by the state blob */
  packStores: Map<string, ObjectRef>
  /** members persisted by a previous run's packs store (set on restore) */
  savedPacksStore: () => { members?: PackMember[]; manifestSeq?: number } | undefined
  shardId: (shard: string) => Promise<string>
  persistState: () => void
  metricsInc: (name: string) => void
}

export class PackOps {
  constructor(private deps: PackOpsDeps) {}

  /** PUT a shard pack to bstore when its bytes differ from the stored copy —
   *  the stored ref pins sha256, so a record change must re-store or remote
   *  importers keep resolving the stale pack. Also commits the fresh pack as
   *  a bfinger store member (kind-5 + re-issued manifest) so the identity
   *  record's refs resolve the current set with no /.well-known lookup.
   *  Best-effort: a store failure never fails the caller. Returns the
   *  (possibly pre-existing) ref. */
  async storePack(shard: string, pack: ShardPack, network?: string): Promise<ObjectRef | undefined> {
    const st = this.deps.opts.objectStore
    if (st === undefined) return undefined
    const bytes = utf8(JSON.stringify(pack))
    const sha = toHex(sha256(bytes))
    const existing = this.deps.packStores.get(shard)
    if (existing?.sha256 === sha) return existing
    try {
      const ref = await st.put(
        `packs/${network ?? nektarEnv('NEKTAR_NETWORK') ?? 'regtest'}/${await this.deps.shardId(shard)}/${sha}.json`,
        bytes,
        'application/json'
      )
      // the same bytes land content-addressed: chirp://<root> resolves the
      // whole closure from any store holding chirp/<hash> objects, so an
      // importer is not bound to this operator's bucket layout
      try {
        ref.chirp = await storeChirp(st, bytes)
        ref.endpoint = st.endpoint
      } catch {
        // chirp publish is best-effort — the keyed ref alone still resolves
      }
      this.deps.packStores.set(shard, ref)
      this.deps.persistState()
      try {
        await this.deps.opts.packsStore?.add({
          shard,
          shardId: await this.deps.shardId(shard),
          network: network ?? 'regtest',
          sha256: sha,
          size: bytes.length,
          ...(ref.chirp !== undefined ? { chirp: ref.chirp } : {}),
          ...(ref.endpoint !== undefined ? { endpoint: ref.endpoint } : {}),
          ...(ref.bucket !== undefined ? { bucket: ref.bucket } : {}),
          ...(ref.key !== undefined ? { key: ref.key } : {})
        })
      } catch {
        this.deps.metricsInc('nektar_pack_store_errors_total')
      }
      return ref
    } catch {
      this.deps.metricsInc('nektar_pack_store_errors_total')
      return existing
    }
  }
  /** attach the bfinger packs store post-construction (the automation
   *  runtime builds it once lanes exist); restores members persisted by a
   *  previous run and binds state persistence. */
  attachPacksStore(store: PacksStore): void {
    this.deps.opts.packsStore = store
    store.bindPersist(() => this.deps.persistState())
    store.restore(this.deps.savedPacksStore())
  }
  /** pack-store refs this instance PUT to bstore — the `.well-known` doc
   *  names them so importers can fetch through the store instead of HTTP. */
  packStoreRef(shard: string): ObjectRef | undefined {
    return this.deps.packStores.get(shard)
  }
  /** fetch + verify a pack through the object store — every byte checks
   *  against the ref's digest or the CHIRP closure before importShardPack's
   *  own re-verification. Resolution order: a chirp:// ref resolves through
   *  the signed store first, then unsigned against the advertised endpoint
   *  (no credentials needed on a public-read gateway); the keyed object is
   *  the compatibility fallback, signed then unsigned. */
  async fetchStoredPack(ref: { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string }): Promise<ShardPack> {
    const bytes = await fetchObjectByRef(this.deps.opts.objectStore, ref, () => this.deps.metricsInc('nektar_pack_fetch_chirp_fallback_total'))
    return JSON.parse(new TextDecoder().decode(bytes)) as ShardPack
  }
}
