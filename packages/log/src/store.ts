// Shard stores: append-only per-shard record of fragments and their lifecycle.
import type { Bytes } from '@nektar/core'
import type { Fragment } from '@nektar/core'

export type LeafState = 'draft' | 'committed' | 'anchored'

export interface LogRecord {
  shard: string
  seq: number // position within shard (global, across batches)
  leaf: Bytes // computeLeaf(fragment)
  hdp: Bytes // identity hash
  fragment: Fragment
  state: LeafState
  level?: number // access level the fragment was written at (policy/ZK gate)
  batchIndex?: number // n of the batch it was committed into
  leafIndex?: number // position within the batch
  anchorTxid?: string
  authorPub?: string // §19: verified originator key …
  authorEpoch?: number // … and its rotation epoch at write time
}

export interface ShardStore {
  init?: () => Promise<void>
  append: (rec: LogRecord) => Promise<void>
  byIdentity: (shard: string, hdpHex: string) => Promise<LogRecord | undefined>
  bySeq: (shard: string, seq: number) => Promise<LogRecord | undefined>
  byBatch: (shard: string, batchIndex: number) => Promise<LogRecord[]> // batch members, leafIndex order
  nextSeq: (shard: string) => Promise<number>
  pending: (shard: string) => Promise<LogRecord[]> // state 'committed', not anchored
  committedUnbatched: (shard: string) => Promise<LogRecord[]> // committed, no batchIndex yet
  /** batched but never anchored ⇒ the anchor never landed (failed broadcast or
   *  crash between markBatched and markAnchored). Return those records to the
   *  unbatched pool so the next closeBatch re-collects them — at-least-once
   *  anchoring; the head UTXO + hdp dedup absorb the rare double-anchored leaf. */
  resetUnanchored: (shard: string) => Promise<number>
  markBatched: (shard: string, seqs: number[], batchIndex: number) => Promise<void>
  markAnchored: (shard: string, batchIndex: number, txid: string) => Promise<void>
  all: (shard: string) => Promise<LogRecord[]>
  lastBatchIndex: (shard: string) => Promise<number>
  /** names of every shard with at least one stored record — the durable
   *  truth for "shards this instance has written"; memory's touched set is
   *  a snapshot of this and a restart rebuilds from here, not from it */
  shardNames?: () => string[]
}

export class MemoryShardStore implements ShardStore {
  private recs = new Map<string, LogRecord[]>() // shard -> records
  private list(shard: string): LogRecord[] {
    if (!this.recs.has(shard)) this.recs.set(shard, [])
    return this.recs.get(shard)!
  }
  append(rec: LogRecord): Promise<void> {
    this.list(rec.shard).push(rec)
    return Promise.resolve()
  }
  byIdentity(shard: string, hdpHex: string): Promise<LogRecord | undefined> {
    return Promise.resolve(this.list(shard).find((r) => Buffer.from(r.hdp).toString('hex') === hdpHex))
  }
  bySeq(shard: string, seq: number): Promise<LogRecord | undefined> {
    return Promise.resolve(this.list(shard).find((r) => r.seq === seq))
  }
  byBatch(shard: string, batchIndex: number): Promise<LogRecord[]> {
    return Promise.resolve(
      this.list(shard)
        .filter((r) => r.batchIndex === batchIndex)
        .sort((a, b) => (a.leafIndex ?? 0) - (b.leafIndex ?? 0))
    )
  }
  nextSeq(shard: string): Promise<number> {
    return Promise.resolve(this.list(shard).length)
  }
  pending(shard: string): Promise<LogRecord[]> {
    return Promise.resolve(this.list(shard).filter((r) => r.state === 'committed'))
  }
  committedUnbatched(shard: string): Promise<LogRecord[]> {
    return Promise.resolve(this.list(shard).filter((r) => r.state === 'committed' && r.batchIndex === undefined))
  }
  resetUnanchored(shard: string): Promise<number> {
    let n = 0
    for (const r of this.list(shard)) {
      if (r.state === 'committed' && r.batchIndex !== undefined) {
        delete r.batchIndex
        delete r.leafIndex
        n++
      }
    }
    return Promise.resolve(n)
  }
  markBatched(shard: string, seqs: number[], batchIndex: number): Promise<void> {
    const seqSet = new Set(seqs)
    for (const r of this.list(shard)) {
      if (seqSet.has(r.seq)) {
        r.batchIndex = batchIndex
        r.leafIndex = seqs.indexOf(r.seq)
      }
    }
    return Promise.resolve()
  }
  markAnchored(shard: string, batchIndex: number, txid: string): Promise<void> {
    for (const r of this.list(shard)) {
      if (r.batchIndex === batchIndex) {
        r.state = 'anchored'
        r.anchorTxid = txid
      }
    }
    return Promise.resolve()
  }
  all(shard: string): Promise<LogRecord[]> {
    return Promise.resolve(this.list(shard).slice())
  }
  lastBatchIndex(shard: string): Promise<number> {
    let max = -1
    for (const r of this.list(shard)) if (r.batchIndex !== undefined && r.batchIndex > max) max = r.batchIndex
    return Promise.resolve(max)
  }
  shardNames(): string[] {
    return [...this.recs.keys()]
  }
}
