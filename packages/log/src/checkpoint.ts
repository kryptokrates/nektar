// LOG-007 signed checkpoints and LOG-005 promise monitoring.
// A checkpoint is the log's signed statement "shard <logId> holds <size>
// leaves up to batch n with root R and chained root R'", issued per anchor.
// The monitor flags every inclusion promise that no VALID checkpoint covers
// within the maximum merge delay (the CT SCT/MMD design).
import { PrivateKey, PublicKey, Signature } from '@bsv/sdk'
import { dsha256, concat, utf8, u64le, fromHex, toHex, TAG_CHECKPOINT } from '@nektar/core'

export interface Checkpoint {
  logId: string // shard id (hash160 of the genesis owner), hex
  size: number // total leaves committed in batches 0..n
  n: number // batch index of the anchor this checkpoint describes
  root: string // R_n hex
  chainedRoot: string // R'_n hex
  time: number // unix ms when issued
  anchorTxid: string
  sig: string // DER hex by the log signing key
}

export function checkpointDigest (c: Omit<Checkpoint, 'sig'>): number[] {
  return Array.from(dsha256(concat(TAG_CHECKPOINT, utf8(c.logId), u64le(c.size), u64le(c.n), fromHex(c.root), fromHex(c.chainedRoot), u64le(c.time), fromHex(c.anchorTxid))))
}
export function signCheckpoint (key: PrivateKey, c: Omit<Checkpoint, 'sig'>): Checkpoint {
  return { ...c, sig: toHex(Uint8Array.from(key.sign(checkpointDigest(c)).toDER() as number[])) }
}
export function verifyCheckpoint (c: Checkpoint, pubHex: string): boolean {
  try { return PublicKey.fromString(pubHex).verify(checkpointDigest(c), Signature.fromDER(c.sig, 'hex')) } catch { return false }
}

export interface PromiseRef { shard: string, seq: number, issuedAt: number }
export interface PromiseViolation extends PromiseRef { overdueMs: number }

/** LOG-005: a promise is fulfilled when a valid checkpoint of its shard with
 *  size > seq was issued no later than issuedAt + maxMergeDelayMs. */
export class PromiseMonitor {
  private promises: PromiseRef[] = []
  private cps = new Map<string, Checkpoint[]>()
  constructor (private logPub: string, private shardLogId: (shard: string) => string | undefined) {}
  observePromise (p: PromiseRef): void { this.promises.push(p) }
  /** forged or foreign checkpoints are ignored (returns false) */
  observeCheckpoint (c: Checkpoint): boolean {
    if (!verifyCheckpoint(c, this.logPub)) return false
    const list = this.cps.get(c.logId) ?? []
    list.push(c); this.cps.set(c.logId, list)
    return true
  }
  violations (now: number, maxMergeDelayMs: number): PromiseViolation[] {
    const out: PromiseViolation[] = []
    for (const p of this.promises) {
      const deadline = p.issuedAt + maxMergeDelayMs
      if (now <= deadline) continue
      const logId = this.shardLogId(p.shard)
      const covered = logId !== undefined && (this.cps.get(logId) ?? []).some(c => c.size > p.seq && c.time <= deadline)
      if (!covered) out.push({ ...p, overdueMs: now - deadline })
    }
    return out
  }
}
