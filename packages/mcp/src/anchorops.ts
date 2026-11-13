// anchorops.ts — the anchor read/attach edge extracted from NektarMemory:
// head/tx/link lookups, the unproven-anchor scan, BUMP attach (deliverTxProof
// lands here), and confirmAnchor. The write path (anchorShard/enqueueAnchor)
// stays in memory.ts — the maps are shared by reference so both sides see
// the same chain state.
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
}
