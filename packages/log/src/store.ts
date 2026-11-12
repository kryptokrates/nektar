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
  nextSeq: (shard: string) => Promise<number>
  pending: (shard: string) => Promise<LogRecord[]> // state 'committed', not anchored
  committedUnbatched: (shard: string) => Promise<LogRecord[]> // committed, no batchIndex yet
  markBatched: (shard: string, seqs: number[], batchIndex: number) => Promise<void>
  markAnchored: (shard: string, batchIndex: number, txid: string) => Promise<void>
  all: (shard: string) => Promise<LogRecord[]>
  lastBatchIndex: (shard: string) => Promise<number>
}

export class MemoryShardStore implements ShardStore {
  private recs = new Map<string, LogRecord[]>() // shard -> records
  private list(shard: string): LogRecord[] {
    if (!this.recs.has(shard)) this.recs.set(shard, [])
    return this.recs.get(shard)!
  }
  async append(rec: LogRecord): Promise<void> {
    this.list(rec.shard).push(rec)
  }
  async byIdentity(shard: string, hdpHex: string): Promise<LogRecord | undefined> {
    return this.list(shard).find((r) => Buffer.from(r.hdp).toString('hex') === hdpHex)
  }
  async nextSeq(shard: string): Promise<number> {
    return this.list(shard).length
  }
  async pending(shard: string): Promise<LogRecord[]> {
    return this.list(shard).filter((r) => r.state === 'committed')
  }
  async committedUnbatched(shard: string): Promise<LogRecord[]> {
    return this.list(shard).filter((r) => r.state === 'committed' && r.batchIndex === undefined)
  }
  async markBatched(shard: string, seqs: number[], batchIndex: number): Promise<void> {
    const seqSet = new Set(seqs)
    for (const r of this.list(shard)) {
      if (seqSet.has(r.seq)) {
        r.batchIndex = batchIndex
        r.leafIndex = seqs.indexOf(r.seq)
      }
    }
  }
  async markAnchored(shard: string, batchIndex: number, txid: string): Promise<void> {
    for (const r of this.list(shard)) {
      if (r.batchIndex === batchIndex) {
        r.state = 'anchored'
        r.anchorTxid = txid
      }
    }
  }
  async all(shard: string): Promise<LogRecord[]> {
    return this.list(shard).slice()
  }
  async lastBatchIndex(shard: string): Promise<number> {
    let max = -1
    for (const r of this.list(shard)) if (r.batchIndex !== undefined && r.batchIndex > max) max = r.batchIndex
    return max
  }
}
