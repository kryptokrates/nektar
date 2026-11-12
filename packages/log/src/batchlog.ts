// BatchLog — the write path. remember() appends are O(1) and off-chain;
// closeBatch() cuts a batch (merkle root R_n) and returns leaves+proofs so the
// anchor package can put one anchor tx on-chain per batch.
import { PrivateKey, Signature } from '@bsv/sdk'
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
  constructor(
    private store: ShardStore,
    private signingKey: PrivateKey,
    private policy: ClosePolicy = { maxLeaves: 100 },
    private now: () => number = Date.now
  ) {}

  /**
   * Append a fragment to a shard. Identity-hash dedup: the same (d,p) appended
   * twice returns the existing record. Returns the record + an inclusion
   * promise signed immediately (inclusion happens at next batch close).
   */
  async remember(
    shard: string,
    fragment: Fragment,
    meta: Partial<Pick<LogRecord, 'level' | 'authorPub' | 'authorEpoch'>> = {}
  ): Promise<{ rec: LogRecord; promise: InclusionPromise; duplicate: boolean }> {
    const idh = identityHash(fragment)
    const idHex = toHex(idh)
    const existing = await this.store.byIdentity(shard, idHex)
    if (existing) {
      return { rec: existing, promise: this.signPromise(shard, existing), duplicate: true }
    }
    const rec: LogRecord = {
      shard,
      seq: await this.store.nextSeq(shard),
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
    return { rec, promise: this.signPromise(shard, rec), duplicate: false }
  }

  private signPromise(shard: string, rec: LogRecord): InclusionPromise {
    const msg = concat(TAG_PROMISE, utf8(shard), Uint8Array.of(rec.seq >>> 24, rec.seq >>> 16, rec.seq >>> 8, rec.seq), rec.leaf)
    const sig = this.signingKey.sign([...msg])
    return { shard, seq: rec.seq, leaf: toHex(rec.leaf), hdp: toHex(rec.hdp), signature: toHex(Uint8Array.from(sig.toDER() as number[])) }
  }

  /** Verify an inclusion promise. */
  verifyPromise(p: InclusionPromise, pubkey = this.signingKey.toPublicKey()): boolean {
    const leaf = new Uint8Array(Buffer.from(p.leaf, 'hex'))
    const msg = concat(TAG_PROMISE, utf8(p.shard), Uint8Array.of(p.seq >>> 24, p.seq >>> 16, p.seq >>> 8, p.seq), leaf)

    return pubkey.verify([...msg], Signature.fromDER(p.signature, 'hex'))
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
