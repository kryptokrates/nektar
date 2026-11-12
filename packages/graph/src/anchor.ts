// GraphAnchor (AOR-006 finality): commits graph vertices (and edges) into a
// nektar shard via the existing remember→batch→anchor pipeline. Each vertex
// hash is stored as a fragment whose leaf commits to it; proveVertex returns
// the leaf's position + anchor tx so the standard SPV verify path applies.
import { hdp, eq, utf8, type Provenance } from '@nektar/core'
/** structural view of the memory surface GraphAnchor needs — satisfied by
 * NektarMemory without a package dependency (avoids a workspace cycle) */
export interface AnchorMem {
  remember: (
    shard: string,
    d: string,
    prov?: { source?: string; modelId?: string },
    level?: number
  ) => Promise<{ seq: number; anchored?: { txid: string; n: number } }>
  anchorShard: (shard: string) => Promise<{ txid: string; n: number } | null>
  anchorTx: (txid: string) => any
  records: (shard: string) => Promise<Array<{ seq: number; hdp: Uint8Array; fragment: { p: Provenance }; batchIndex?: number; anchorTxid?: string }>>
  prove: (shard: string, seq: number) => Promise<{ result: { ok: boolean } }>
}

export interface AnchoredVertexRef {
  batch: number
  seq: number
  anchorTxid?: string
}

export class GraphAnchor {
  private recs = new Map<string, AnchoredVertexRef>()
  constructor(
    private readonly mem: AnchorMem,
    private readonly shard: string
  ) {}

  /** append a vertex/edge hash as a fragment leaf; returns its log position */
  async record(h: string, meta: Record<string, string> = {}): Promise<AnchoredVertexRef> {
    const out = await this.mem.remember(this.shard, `aorg:${h}`, { source: meta.source ?? 'aorg', modelId: meta.modelId ?? 'aorg' }, 1)
    const ref: AnchoredVertexRef = { batch: out.anchored?.n ?? -1, seq: out.seq, anchorTxid: out.anchored?.txid }
    this.recs.set(h, ref)
    return ref
  }

  /** cut + anchor the pending batch; fills in anchorTxid for recorded refs */
  async anchor(): Promise<string> {
    const res = await this.mem.anchorShard(this.shard)
    if (!res) throw new Error('GraphAnchor.anchor: nothing to anchor')
    // refresh every ref from the log itself: batches may also have closed
    // automatically between record() calls, each with its own anchor
    const bySeq = new Map((await this.mem.records(this.shard)).map((r) => [r.seq, r]))
    for (const r of this.recs.values()) {
      const rec = bySeq.get(r.seq)
      if (rec?.batchIndex !== undefined) {
        r.batch = rec.batchIndex
        r.anchorTxid = rec.anchorTxid
      }
    }
    return res.txid
  }

  /** proveVertex: {ref, anchorTx} for the standard verify path (leaf→R→anchor→BUMP) */
  proveVertex(h: string): { ref: AnchoredVertexRef; anchorTx: any } {
    const ref = this.recs.get(h)
    if (!ref) throw new Error(`vertex ${h.slice(0, 12)} not recorded`)
    const anchorTx = ref.anchorTxid ? this.mem.anchorTx(ref.anchorTxid) : undefined
    return { ref, anchorTx }
  }

  /** a vertex verifies iff its record's identity hash H(d‖p) commits to
   *  exactly `aorg:<h>` (works for plaintext and encrypted stores alike) AND
   *  the record's batch is anchored and SPV-provable */
  async verifyVertex(h: string): Promise<boolean> {
    const ref = this.recs.get(h)
    if (!ref) return false
    const rec = (await this.mem.records(this.shard)).find((r) => r.seq === ref.seq)
    if (!rec || !eq(rec.hdp, hdp({ d: utf8(`aorg:${h}`), p: rec.fragment.p }))) return false
    return (await this.mem.prove(this.shard, ref.seq)).result.ok
  }
}
