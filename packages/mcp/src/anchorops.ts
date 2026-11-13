// anchorops.ts — the anchor edge extracted from NektarMemory: the write path
// (per-shard serialized close→broadcast→markAnchored, LOG-007 checkpoint
// signing, §12 bond mirror) plus the read/attach edge (head/tx/link lookups,
// the unproven-anchor scan, BUMP attach, confirmAnchor). The chain-state maps
// are shared by reference so the whole instance sees one chain.
import { toHex, type SignerLike } from '@nektar/core'
import { signCheckpoint, checkpointDigest, type Checkpoint, type BatchLog } from '@nektar/log'
import type { Transaction, MerklePath } from '@bsv/sdk'
import type { AnchorChain } from '@nektar/anchor'
import type { AnchorLink } from '@nektar/verify'

export interface AnchorOpsDeps {
  anchors: Map<string, AnchorChain>
  tails: Map<string, AnchorLink[]>
  anchorTxs: Map<string, Transaction>
  /** shards the instance has written or anchored (anchors ∪ touched) */
  shards: () => string[]
  persistState: () => void
  // ── write-path deps ────────────────────────────────────────────────────
  log: Pick<BatchLog, 'resetUnanchored' | 'closeBatch' | 'markAnchored'>
  /** lazily resolves (and creates) the shard's AnchorChain — same as
   *  memory.anchorChain; a callback because anchors are created on demand */
  anchorChainFor: (shard: string) => AnchorChain
  txidShard: Map<string, string>
  cps: Map<string, Checkpoint[]>
  logKey: () => SignerLike
  metricsInc: (name: string, labels?: Record<string, string>, n?: number, help?: string) => void
  /** predicate: is promise x covered by a signed checkpoint */
  covered: () => (x: { shard: string; seq: number; issuedAt: number }) => boolean
  /** promise compaction needs read+replace (filter returns a new array) */
  getPromises: () => Array<{ shard: string; seq: number; issuedAt: number }>
  setPromises: (p: Array<{ shard: string; seq: number; issuedAt: number }>) => void
  bondsActive: () => boolean
  bondsMirror: (lastRoot: string) => Promise<boolean>
}

export class AnchorOps {
  constructor(private deps: AnchorOpsDeps) {}

  /** the shard's current anchor head (undefined before the first anchor) */
  anchorHead(shard: string) {
    return this.deps.anchors.get(shard)?.getHead()
  }
  /** an anchor tx by txid (as broadcast, with BUMP when attached) */
  anchorTx(txid: string) {
    return this.deps.anchorTxs.get(txid)
  }
  /** the shard's latest anchor link (tail of the on-chain succession) */
  tailLink(shard: string) {
    return this.deps.tails.get(shard)?.at(-1)
  }
  /** every anchor link of the shard, genesis first (read-only copy) */
  anchorLinks(shard: string): AnchorLink[] {
    return [...(this.deps.tails.get(shard) ?? [])]
  }
  /** anchors still waiting for their BUMP */
  unprovenAnchors(): Array<{ shard: string; txid: string }> {
    return this.deps
      .shards()
      .flatMap((s) => (this.deps.tails.get(s) ?? []).filter((l) => !this.deps.anchorTxs.get(l.txid)?.merklePath).map((l) => ({ shard: s, txid: l.txid })))
  }
  /** attach a BUMP to an anchor (must prove THIS txid) and mark the chain confirmed */
  attachProof(shard: string, txid: string, mp: MerklePath): void {
    const tx = this.deps.anchorTxs.get(txid)
    if (!tx) throw new Error(`attachProof: unknown anchor ${txid}`)
    if (!mp.path[0]?.some((n) => n.txid === true && n.hash === txid)) throw new Error('attachProof: BUMP does not contain this txid')
    tx.merklePath = mp
    this.confirmAnchor(shard, txid)
  }
  /** Mark anchor txs mined (drive from broadcaster status events). */
  confirmAnchor(shard: string, txid: string): void {
    const ac = this.deps.anchors.get(shard)
    if (!ac) return
    ac.confirm(txid, this.deps.anchorTxs.get(txid)?.merklePath)
    this.deps.persistState()
  }

  // ── write path ─────────────────────────────────────────────────────────

  /** per-shard anchor serialization: close→broadcast→markAnchored must not
   *  overlap another anchorShard on the same shard — resetUnanchored could
   *  otherwise return an in-flight batch's records to the pending pool and
   *  commit the same leaves to two on-chain batches. */
  private anchorQueues = new Map<string, Promise<unknown>>()
  private enqueueAnchor<T>(shard: string, fn: () => Promise<T>): Promise<T> {
    const tail = this.anchorQueues.get(shard) ?? Promise.resolve()
    const p = (tail as Promise<void>).then(fn)
    this.anchorQueues.set(
      shard,
      p.then(
        () => undefined,
        () => undefined
      )
    )
    return p
  }

  /** Close the pending batch of a shard and broadcast the anchor. */
  async anchorShard(shard: string): Promise<{ txid: string; n: number } | null> {
    return await this.enqueueAnchor(shard, async () => await this.anchorShardInner(shard))
  }
  private async anchorShardInner(shard: string): Promise<{ txid: string; n: number } | null> {
    // at-least-once: records marked batched by a batch whose anchor never
    // landed (failed broadcast, or crash before markAnchored) go back into the
    // pending pool — without this they are stranded and their signed promises
    // breach the merge delay forever
    await this.deps.log.resetUnanchored(shard)
    const batch = await this.deps.log.closeBatch(shard)
    if (!batch) return null
    let res
    try {
      res = await this.deps.anchorChainFor(shard).anchor(batch.size, batch.R)
    } catch (e) {
      this.deps.metricsInc('nektar_anchor_failures_total', { shard }, 1, 'anchor broadcasts that failed')
      throw e
    }
    this.deps.metricsInc('nektar_anchors_total', {}, 1, 'anchors broadcast')
    const txid = res.tx.id('hex')
    this.deps.anchorTxs.set(txid, res.tx)
    this.deps.txidShard.set(txid, shard)
    await this.deps.log.markAnchored(shard, batch.n, txid)
    // record the link for chain tails / proofs
    const link: AnchorLink = {
      txid,
      outputIndex: 0,
      n: res.head.data.n,
      size: res.head.data.size,
      R: toHex(res.head.data.R),
      Rp: toHex(res.head.data.Rp)
    }
    const tail = this.deps.tails.get(shard) ?? []
    tail.push(link)
    this.deps.tails.set(shard, tail)
    // LOG-007: the log's signed checkpoint for this anchor
    const cps = this.deps.cps.get(shard) ?? []
    cps.push(
      await signCheckpoint(this.deps.logKey(), {
        logId: this.deps.anchorChainFor(shard).getShardId(),
        size: tail.reduce((s, l) => s + l.size, 0),
        n: link.n,
        root: link.R,
        chainedRoot: link.Rp,
        time: Date.now(),
        anchorTxid: txid
      })
    )
    this.deps.cps.set(shard, cps)
    // compaction: a promise covered by a checkpoint can never violate — drop
    // it so state.json, violations() and the lag gauge stay O(outstanding)
    const cov = this.deps.covered()
    this.deps.setPromises(this.deps.getPromises().filter((x) => !cov(x)))
    this.deps.persistState()
    // §12 SLA: the bond covenant mirrors the checkpoint on-chain — lastRoot
    // commits to the FULL signed checkpoint (digest binds logId+size+n+R+R'+
    // time+txid), making the history unforkable and publicly replayable.
    if (this.deps.bondsActive()) {
      const cp = cps[cps.length - 1]
      const lastRoot = toHex(Uint8Array.from(checkpointDigest(cp)))
      const ok = await this.deps.bondsMirror(lastRoot)
      if (!ok) this.deps.metricsInc('nektar_bond_commit_failures_total', {}, 1, 'bond commit broadcasts that failed')
    }
    return { txid, n: res.head.data.n }
  }
}
