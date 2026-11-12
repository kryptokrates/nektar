/** The packs store: nektar's public shard-pack object set committed through
 *  the operator's bfinger record `refs` — the stock committed-record
 *  discovery path. Each stored pack is a kind-5 sub-record under the operator
 *  identity (its carrier commitment IS the member); the store `root` is the
 *  RFC-6962 root over member commitments. `head` follows the stock rule:
 *  a one-member store heads at its member; more than one heads at the
 *  kind-6 manifest that lists them — so a reader holding only our pubkey
 *  walks ls_finger → record refs → {identityKey, carrier: head} → members,
 *  no /.well-known or operator origin needed. */

import { CborMap, rootOfBytes, type Pair, type Ref, type Value } from '@lightwebinc/bcommon'
import { KindSub, KindManifest } from '@lightwebinc/bfinger-host/dist/record.js'
import { Utils } from '@bsv/sdk'

const PACKS_STORE_NAME = 'nektar-packs'

export interface PackMember {
  /** member commitment — the kind-5 carrier's C, hex */
  c: string
  shard: string
  shardId: string
  network: string
  sha256: string
  size: number
  chirp?: string
  endpoint?: string
  bucket?: string
  key?: string
}

export interface PacksStoreDeps {
  /** publish a kind-5 (member) or kind-6 (manifest) carrier under the
   *  operator identity; resolves to the carrier's commitment + txid. `id` is
   *  the journal idempotency slot — member index for members, a version
   *  counter for manifests (store records are seq-1 create-shaped). */
  publish: (kind: typeof KindSub | typeof KindManifest, id: number, body: CborMap) => Promise<{ commitment: string; txid: string }>
}

const bytes = (hex: string): Uint8Array => new Uint8Array(Utils.toArray(hex, 'hex'))

function memberEntries(m: PackMember, withC: boolean): Pair[] {
  const e: Pair[] = [
    { key: 'v', val: 1n },
    ...(withC ? [{ key: 'c', val: bytes(m.c) }] : []),
    { key: 'shard', val: m.shard },
    { key: 'shardId', val: m.shardId },
    { key: 'network', val: m.network },
    { key: 'sha256', val: bytes(m.sha256) },
    { key: 'size', val: BigInt(m.size) }
  ]
  if (m.chirp !== undefined) e.push({ key: 'chirp', val: m.chirp })
  if (m.endpoint !== undefined) e.push({ key: 'endpoint', val: m.endpoint })
  if (m.bucket !== undefined) e.push({ key: 'bucket', val: m.bucket })
  if (m.key !== undefined) e.push({ key: 'key', val: m.key })
  return e
}

export class PacksStore {
  private members: PackMember[] = []
  private manifestSeq = 0
  /** commitment of the latest manifest — the refs head while count > 1 */
  private manifestC: string | undefined
  private persist?: () => void | Promise<void>
  constructor(private readonly deps: PacksStoreDeps) {}

  /** late-bind the state-snapshot hook — the store can be constructed before
   *  the snapshot owner (NektarMemory) attaches it. */
  bindPersist(fn: () => void | Promise<void>): void {
    this.persist = fn
  }

  restore(s: { members?: PackMember[]; manifestSeq?: number; manifestC?: string } | undefined): void {
    this.members = s?.members ?? []
    this.manifestSeq = s?.manifestSeq ?? 0
    this.manifestC = s?.manifestC
  }
  snapshot(): { members: PackMember[]; manifestSeq: number; manifestC?: string } {
    return { members: this.members, manifestSeq: this.manifestSeq, ...(this.manifestC !== undefined ? { manifestC: this.manifestC } : {}) }
  }

  get size(): number {
    return this.members.length
  }

  /** the record-refs entry for the next finger publish — undefined while the
   *  store is empty (an empty store commits to nothing). */
  ref(): Ref | undefined {
    if (this.members.length === 0) return undefined
    const commits = this.members.map((m) => bytes(m.c))
    // head: the member itself when alone, the manifest listing them when
    // more — matching what a stock reader dereferences at {identityKey, head}
    const head = this.members.length === 1 ? commits[0] : this.manifestC !== undefined ? bytes(this.manifestC) : undefined
    return {
      name: PACKS_STORE_NAME,
      root: rootOfBytes(commits),
      count: BigInt(this.members.length),
      ...(head !== undefined ? { head } : {})
    }
  }

  /** Commit a stored pack as the next member: a kind-5 sub-record carrying
   *  the object locator, then a re-issued kind-6 manifest listing every
   *  member (commitment included) once the store has more than one.
   *  Content-addressed — a pack already stored (same sha256) is a no-op. */
  async add(m: Omit<PackMember, 'c'>): Promise<{ commitment: string; txid?: string } | undefined> {
    if (this.members.some((x) => x.sha256 === m.sha256)) return undefined
    const sub = await this.deps.publish(KindSub, this.members.length + 1, new CborMap(memberEntries({ ...m, c: '' }, false)))
    this.members.push({ ...m, c: sub.commitment })
    let txid: string | undefined
    if (this.members.length > 1) {
      const manifest = new CborMap([
        { key: 'v', val: 1n },
        { key: 'name', val: PACKS_STORE_NAME },
        { key: 'members', val: this.members.map((x): Value => new CborMap(memberEntries(x, true))) }
      ])
      const man = await this.deps.publish(KindManifest, ++this.manifestSeq, manifest)
      this.manifestC = man.commitment
      txid = man.txid
    }
    await this.persist?.()
    return { commitment: sub.commitment, ...(txid !== undefined ? { txid } : {}) }
  }
}
