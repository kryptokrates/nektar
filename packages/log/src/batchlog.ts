// BatchLog — the write path. remember() appends are O(1) and off-chain;
// closeBatch() cuts a batch (merkle root R_n) and returns leaves+proofs so the
// anchor package can put one anchor tx on-chain per batch.
import { PrivateKey, PublicKey, Signature } from '@bsv/sdk'
import { toSigner, signerPubHex, type SignerLike } from '@nektar/core'
import { computeLeaf, identityHash, merkleRoot, merkleProof, concat, utf8, toHex, serializeFragment, type Bytes, type Fragment } from '@nektar/core'
import { TAG_PROMISE } from '@nektar/core'
import type { LogRecord, ShardStore } from './store.js'

export interface InclusionPromise {
  shard: string
  seq: number
  leaf: string // hex
  hdp: string // hex
  signature: string // hex, over TAG_PROMISE|shard|seq|leaf
}

export interface Batch {
  shard: string
  n: number
  size: number
  R: Bytes
  leaves: Bytes[]
  records: LogRecord[]
}

/** LOG-006: a batch closes when ANY configured threshold is reached. */
export interface ClosePolicy {
  maxLeaves: number // pending record count
  maxAgeMs?: number // age of the oldest pending record
  maxBytes?: number // canonical bytes pending
  /** cost ceiling: pending bytes × satsPerByte ≥ maxCostSats (bounds the
   *  value at risk in an unanchored batch) */
  maxCostSats?: number
  satsPerByte?: number
}

export class BatchLog {
  private firstPendingAt = new Map<string, number>()
  private pendingBytes = new Map<string, number>()
  /** per-shard write serialization: remember's dedup→nextSeq→append span and
   *  closeBatch's collect→markBatched span must not interleave with another
   *  writer on the same shard, or two calls race a seq (PK violation) or a
   *  dedup check (UNIQUE violation) across awaits. */
  private queues = new Map<string, Promise<unknown>>()
  private enqueue<T>(shard: string, fn: () => Promise<T>): Promise<T> {
    // the stored tail never rejects (errors are swallowed into the chain slot)
    const tail = this.queues.get(shard) ?? Promise.resolve()
    const p = (tail as Promise<void>).then(fn)
    this.queues.set(
      shard,
      p.then(
        () => undefined,
        () => undefined
      )
    )
    return p
  }
  private _signPub?: string
  /** resolves the signer pubkey (DigestSigner custody) — PrivateKey resolves inline. */
  readonly ready: Promise<void>
  constructor(
    private store: ShardStore,
    private signingKey: SignerLike,
    private policy: ClosePolicy = { maxLeaves: 100 },
    private now: () => number = Date.now
  ) {
    this._signPub = signingKey instanceof PrivateKey ? signingKey.toPublicKey().toString() : undefined
    this.ready = this._signPub
      ? Promise.resolve()
      : signerPubHex(signingKey).then((p) => {
          this._signPub = p
        })
  }

  /** Return batched-but-never-anchored records to the pending pool. Call only
   *  while no anchor for this shard is in flight (NektarMemory.anchorShard
   *  holds the per-shard anchor queue for its whole close→broadcast→mark span). */
  async resetUnanchored(shard: string): Promise<number> {
    return await this.enqueue(shard, async () => await this.store.resetUnanchored(shard))
  }

  /**
   * Append a fragment to a shard. Identity-hash dedup: the same (d,p) appended
   * twice returns the existing record. Returns the record + an inclusion
   * promise signed immediately (inclusion happens at next batch close).
   * `prepare` runs inside the shard queue AFTER seq is assigned and BEFORE
   * the leaf is computed — where per-seq mutation belongs (e.g. attaching a
   * ciphertext whose key context binds seq). It runs only on a fresh append.
   */
  async remember(
    shard: string,
    fragment: Fragment,
    meta: Partial<Pick<LogRecord, 'level' | 'authorPub' | 'authorEpoch'>> = {},
    prepare?: (fragment: Fragment, seq: number) => void | Promise<void>
  ): Promise<{ rec: LogRecord; promise: InclusionPromise; duplicate: boolean }> {
    return await this.enqueue(shard, async () => await this.rememberInner(shard, fragment, meta, prepare))
  }
  private async rememberInner(
    shard: string,
    fragment: Fragment,
    meta: Partial<Pick<LogRecord, 'level' | 'authorPub' | 'authorEpoch'>>,
    prepare?: (fragment: Fragment, seq: number) => void | Promise<void>
  ): Promise<{ rec: LogRecord; promise: InclusionPromise; duplicate: boolean }> {
    const idh = identityHash(fragment)
    const idHex = toHex(idh)
    const existing = await this.store.byIdentity(shard, idHex)
    if (existing) {
      return { rec: existing, promise: await this.signPromise(shard, existing), duplicate: true }
    }
    const seq = await this.store.nextSeq(shard)
    await prepare?.(fragment, seq)
    const rec: LogRecord = {
      shard,
      seq,
      leaf: computeLeaf(fragment),
      hdp: idh,
      fragment,
      state: 'committed',
      // persisted WITH the record (a store-backed record cannot be mutated afterwards)
      ...meta
    }
    await this.store.append(rec)
    if (!this.firstPendingAt.has(shard)) this.firstPendingAt.set(shard, this.now())
    this.pendingBytes.set(shard, (this.pendingBytes.get(shard) ?? 0) + serializeFragment(fragment).length)
    return { rec, promise: await this.signPromise(shard, rec), duplicate: false }
  }

  private async signPromise(shard: string, rec: LogRecord): Promise<InclusionPromise> {
    const msg = concat(TAG_PROMISE, utf8(shard), Uint8Array.of(rec.seq >>> 24, rec.seq >>> 16, rec.seq >>> 8, rec.seq), rec.leaf)
    const sig = await toSigner(this.signingKey).sign([...msg])
    return { shard, seq: rec.seq, leaf: toHex(rec.leaf), hdp: toHex(rec.hdp), signature: toHex(Uint8Array.from(sig.toDER() as number[])) }
  }

  /** Verify an inclusion promise. */
  verifyPromise(p: InclusionPromise, pubkey?: PublicKey): boolean {
    const leaf = new Uint8Array(Buffer.from(p.leaf, 'hex'))
    const msg = concat(TAG_PROMISE, utf8(p.shard), Uint8Array.of(p.seq >>> 24, p.seq >>> 16, p.seq >>> 8, p.seq), leaf)
    const pk = pubkey ?? (this._signPub !== undefined ? PublicKey.fromString(this._signPub) : undefined)
    if (!pk) throw new Error('batchlog: signer pubkey unresolved — await log.ready')
    return pk.verify([...msg], Signature.fromDER(p.signature, 'hex'))
  }

  /** Should a batch be closed? */
  async shouldClose(shard: string): Promise<boolean> {
    return (await this.closeReason(shard)) !== null
  }
  /** which threshold fires (null = keep collecting) */
  async closeReason(shard: string): Promise<'leaves' | 'age' | 'bytes' | 'cost' | null> {
    const pending = (await this.store.committedUnbatched(shard)).length
    if (pending === 0) return null
    const p = this.policy
    if (pending >= p.maxLeaves) return 'leaves'
    const since = this.firstPendingAt.get(shard)
    if (p.maxAgeMs !== undefined && since !== undefined && this.now() - since >= p.maxAgeMs) return 'age'
    const bytes = this.pendingBytes.get(shard) ?? 0
    if (p.maxBytes !== undefined && bytes >= p.maxBytes) return 'bytes'
    if (p.maxCostSats !== undefined && bytes * (p.satsPerByte ?? 0.001) >= p.maxCostSats) return 'cost'
    return null
  }

  /**
   * Close a batch: all committed-unbatched leaves → R_n; marks them batched.
   * Returns null if nothing pending. Caller (anchor package) then builds the
   * anchor tx and calls markAnchored(batch.n, txid).
   */
  async closeBatch(shard: string): Promise<Batch | null> {
    return await this.enqueue(shard, async () => await this.closeBatchInner(shard))
  }
  private async closeBatchInner(shard: string): Promise<Batch | null> {
    const recs = await this.store.committedUnbatched(shard)
    if (recs.length === 0) return null
    const n = (await this.store.lastBatchIndex(shard)) + 1
    const leaves = recs.map((r) => r.leaf)
    const R = merkleRoot(leaves)
    await this.store.markBatched(
      shard,
      recs.map((r) => r.seq),
      n
    )
    this.firstPendingAt.delete(shard)
    this.pendingBytes.delete(shard)
    return { shard, n, size: leaves.length, R, leaves, records: recs }
  }

  async markAnchored(shard: string, batchIndex: number, txid: string): Promise<void> {
    await this.store.markAnchored(shard, batchIndex, txid)
  }

  /** merkle proof of leafIndex within a closed batch */
  batchProof(batch: Batch, leafIndex: number) {
    return merkleProof(batch.leaves, leafIndex)
  }
}
