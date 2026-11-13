// txops.ts — transaction graph + proof/rejection delivery extracted from
// NektarMemory: fetchTx (graph → backend → throttled network lookups),
// rememberTx/txWire/readTxWire (BEEF/EF wire in+out), verified-proof delivery
// (deliverTxProof attaches to the owning anchor then notifies), rejection
// bookkeeping (txRejected + subscribers), and the txState tri-state read.
// Proofs are pushed (events substrate/broadcaster callbacks); the lookups are
// the single-attempt recovery path for what a push missed while down.
import type { Transaction, MerklePath } from '@bsv/sdk'
import { TxStore } from '@nektar/chain'

export interface TxOpsDeps {
  /** the instance tx graph (parents + BEEF ancestry live here) */
  txs: () => TxStore
  /** anchor txs keyed by txid — owned by the anchoring side, read here */
  anchorTxs: Map<string, Transaction>
  /** attestation txs keyed by txid (model-verifier deploy/spend) */
  attestationTxs: Map<string, Transaction>
  /** txid → owning shard — restored from persisted anchors at boot */
  txidShard: Map<string, string>
  /** attach a verified proof to its shard anchor */
  attachProof: (shard: string, txid: string, mp: MerklePath) => void
  /** regtest backends answer getTx with the sealed proof; isRejected is a
   *  definitive backend verdict — both probed, not required */
  chain: { getTx?: (t: string) => Transaction | undefined | Promise<Transaction | undefined>; isRejected?: (t: string) => boolean }
  /** SPV check for counterparty wires (the instance's own tracker) */
  sdkTracker: () => import('@bsv/sdk').ChainTracker
  /** network fallbacks — assigned by the live runtime post-construction,
   *  so these are getters, each a single attempt and never a sleep */
  txLookup: () => ((txid: string) => Promise<Transaction | undefined>) | undefined
  proofLookup: () => ((txid: string) => Promise<MerklePath | undefined>) | undefined
  network?: string
  broadcasterUrl?: string
}

export class TxOps {
  private lookupAt = new Map<string, number>()
  private txProofCbs: Array<(txid: string, mp: MerklePath) => void> = []
  private txRejected = new Set<string>()
  private txRejectCbs: Array<(txid: string) => void> = []
  constructor(private deps: TxOpsDeps) {}

  private lookupDue(key: string, everyMs: number): boolean {
    if (Date.now() - (this.lookupAt.get(key) ?? 0) <= everyMs) return false
    if (this.lookupAt.size >= 4096) this.lookupAt.delete(this.lookupAt.keys().next().value!)
    this.lookupAt.set(key, Date.now())
    return true
  }

  async fetchTx(txid: string): Promise<Transaction | undefined> {
    let tx: Transaction | undefined = this.deps.anchorTxs.get(txid) ?? this.deps.txs().get(txid)
    if (tx?.merklePath === undefined) {
      // regtest backends answer for any transaction on their chain, proof included
      const seen = await this.deps.chain.getTx?.(txid)
      if (seen?.merklePath !== undefined) {
        this.deps.txs().setProof(txid, seen.merklePath)
        tx = seen
      } else if (tx === undefined) tx = seen
    }
    const txLookup = this.deps.txLookup()
    if (tx === undefined && txLookup !== undefined && this.lookupDue(`tx:${txid}`, 30_000)) {
      try {
        tx = await txLookup(txid)
      } catch {
        tx = undefined
      }
      if (tx !== undefined) this.deps.txs().put(tx)
    }
    if (tx === undefined) return undefined
    // proofs arrive by push; this pull only covers one missed while the
    // instance was down, so it is tried once and then rarely
    const proofLookup = this.deps.proofLookup()
    if (tx.merklePath === undefined && proofLookup !== undefined && this.lookupDue(`proof:${txid}`, 600_000)) {
      let mp: MerklePath | undefined
      try {
        mp = await proofLookup(txid)
      } catch {
        mp = undefined
      }
      if (mp !== undefined) {
        this.deps.txs().setProof(txid, mp)
        tx.merklePath = mp
      }
    }
    return tx
  }

  /** keep a transaction (and the ancestry it carries) in the instance's graph */
  rememberTx(tx: Transaction): void {
    this.deps.txs().put(tx)
  }
  /** hex to hand a counterparty: BEEF when the ancestry is proven, else EF, else raw */
  txWire(txid: string, maxBytes?: number): string | undefined {
    return this.deps.txs().wire(txid, maxBytes)
  }
  /** read a counterparty's wire transaction and run SPV against our own headers */
  async readTxWire(hex: string): Promise<import('@nektar/chain').Ingested> {
    return await TxStore.read(hex, this.deps.sdkTracker())
  }

  /** subscribe to verified tx proofs — fed by verified broadcaster callbacks
   *  (http layer checks the BUMP first) and by chain 'mined' status events */
  onTxProof(cb: (txid: string, mp: MerklePath) => void): void {
    this.txProofCbs.push(cb)
  }
  /** a merkle proof that was already verified by the caller (callback BUMP
   *  checked against our own tracker) or produced by the chain backend
   *  itself: attach it to the anchor it belongs to, then notify subscribers */
  deliverTxProof(txid: string, mp: MerklePath): void {
    this.deps.txs().setProof(txid, mp)
    try {
      const shard = this.deps.txidShard.get(txid)
      if (shard) this.deps.attachProof(shard, txid, mp)
    } catch {
      /* proof not for us / malformed */
    }
    for (const cb of this.txProofCbs) {
      try {
        cb(txid, mp)
      } catch {
        /* listener errors are their own */
      }
    }
  }

  /** subscribe to definitive transaction rejections — fed by verified
   *  broadcaster callbacks (REJECTED / DOUBLE_SPEND_ATTEMPTED) and by chain
   *  'rejected'/'double-spent' status events */
  onTxRejected(cb: (txid: string) => void): void {
    this.txRejectCbs.push(cb)
  }
  /** record a rejection the backend reported, then notify subscribers */
  deliverTxRejected(txid: string): void {
    this.txRejected.add(txid)
    for (const cb of this.txRejectCbs) {
      try {
        cb(txid)
      } catch {
        /* listener errors are their own */
      }
    }
  }

  /** what the chain view knows about a tx right now: mined (has a merkle
   *  path), seen (known to the node), rejected (the backend explicitly said
   *  so — REJECTED or DOUBLE_SPEND_ATTEMPTED), else unknown. On live networks
   *  the broadcaster's /tx/{txid} verdict is the tiebreaker. */
  async txState(txid: string): Promise<'mined' | 'seen' | 'rejected' | 'unknown'> {
    if (this.txRejected.has(txid) || this.deps.chain.isRejected?.(txid) === true) return 'rejected'
    const local = this.deps.anchorTxs.get(txid) ?? this.deps.chain.getTx?.(txid)
    const resolved = local instanceof Promise ? await local : local
    if (resolved?.merklePath !== undefined) return 'mined'
    if (resolved !== undefined) return 'seen'
    if (this.deps.network === 'regtest' || this.deps.network === undefined) return 'unknown'
    try {
      const st = await fetch(`${this.deps.broadcasterUrl ?? 'https://arcade.gorillapool.io'}/tx/${txid}`, { signal: AbortSignal.timeout(10_000) }).then((r) =>
        r.ok ? r.json() : null
      )
      const status = String(st?.txStatus ?? '').toUpperCase()
      if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(status)) {
        this.txRejected.add(txid)
        return 'rejected'
      }
      if (status === 'MINED' || status === 'IMMUTABLE') return 'mined'
      if (['RECEIVED', 'SENT_TO_NETWORK', 'ACCEPTED_BY_NETWORK', 'SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING'].includes(status)) return 'seen'
    } catch {
      /* undecidable — caller retries */
    }
    return 'unknown'
  }

  /** any tx this instance broadcast and remembers — anchor or attestation
   *  (model-verifier deploy/spend land in attestationTxs too) */
  knownTx(txid: string): Transaction | undefined {
    return this.deps.anchorTxs.get(txid) ?? this.deps.attestationTxs.get(txid)
  }
}
