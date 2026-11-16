// txops.ts — transaction graph + proof/rejection delivery extracted from
// NektarMemory: fetchTx (graph → backend → throttled network lookups),
// rememberTx/txWire/readTxWire (BEEF/EF wire in+out), verified-proof delivery
// (deliverTxProof attaches to the owning anchor then notifies), rejection
// bookkeeping (txRejected + subscribers), and the txState tri-state read.
// Proofs are pushed (events substrate/broadcaster callbacks); the lookups are
// the single-attempt recovery path for what a push missed while down.
import type { Transaction, MerklePath } from '@bsv/sdk'
import { TxStore, txFactsOf, type TxFacts, type TxStatusEvent, type StatusHint } from '@nektar/chain'
import type { TxObservationInput } from '@nektar/automation'

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
  /** the outside status word for a txid — a HINT, never a verdict; injected
   *  by the runtime (the broadcaster adapter's `statusHint`) so TxOps never
   *  hand-rolls the /tx fetch or owns the endpoint default */
  statusHint?: (txid: string) => Promise<StatusHint>
  /** the durable ledger's own row for a txid — 'rejected'/'conflicted'
   *  states are first-party truth (our own writes), injected by memory
   *  when a ledger exists; absent ledger → the check is skipped */
  ledgerState?: (txid: string) => Promise<string | undefined>
  /** proof pull cadence — the missed-push recovery floor per txid
   *  (default 120_000; NEKTAR_PROOF_PULL_MS at the construction site) */
  proofPullMs?: number
}

export class TxOps {
  private lookupAt = new Map<string, number>()
  private txProofCbs: Array<(txid: string, mp: MerklePath) => void> = []
  private txRejected = new Set<string>()
  private txAbandoned = new Set<string>()
  private txRejectCbs: Array<(txid: string) => void> = []
  /** the one transaction-facts answer — held bytes + proof + first-party
   *  verdicts + abandon horizon compose the verdict; the outside status word
   *  only ever lands in the separate hint field */
  private facts: (txid: string) => Promise<TxFacts>
  constructor(private deps: TxOpsDeps) {
    this.facts = txFactsOf({
      // 'seen' basis: anchor set + backend answers only — a prepared-but-
      // unbroadcast tx in the store is not 'held' (not on the network)
      held: async (t) => {
        const local = this.deps.anchorTxs.get(t) ?? this.deps.chain.getTx?.(t)
        return local instanceof Promise ? await local : local
      },
      // proofs held in the tx store count too — a verified path attached by
      // the push path lives there even when held() has no copy
      proof: (t) => this.deps.txs().get(t)?.merklePath,
      rejected: async (t) =>
        this.txRejected.has(t) ||
        this.deps.chain.isRejected?.(t) === true ||
        ['rejected', 'conflicted'].includes(await this.deps.ledgerState?.(t).catch(() => undefined) ?? ''),
      abandoned: (t) => this.txAbandoned.has(t),
      hint: this.deps.statusHint,
      height: this.deps.height
    })
  }

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
    // proofs arrive by push; this pull is only the missed-push recovery
    // floor — bounded cadence, never the mechanism
    const proofLookup = this.deps.proofLookup()
    if (tx.merklePath === undefined && proofLookup !== undefined && this.lookupDue(`proof:${txid}`, this.deps.proofPullMs ?? 120_000)) {
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
    // a merkle path in hand outranks every recorded verdict — a rejected/
    // abandoned mark that later proves still counts mined (conflict races)
    this.txRejected.delete(txid)
    this.txAbandoned.delete(txid)
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

  /** subscribe to rejection/abandonment wakes — fired for verified chain
   *  'rejected'/'double-spent' events, first-party refusals, abandon-horizon
   *  verdicts AND pushed status hints. A wake is a poke to re-ask txState,
   *  never a verdict in itself. */
  onTxRejected(cb: (txid: string) => void): void {
    this.txRejectCbs.push(cb)
  }
  private notifyTxRejected(txid: string): void {
    for (const cb of this.txRejectCbs) {
      try {
        cb(txid)
      } catch {
        /* listener errors are their own */
      }
    }
  }
  /** record a FIRST-PARTY verdict — fed only by verified chain 'tx' events
   *  and definitive broadcast refusals, never by a pushed status string
   *  (a hint carries no proof object; those go through hintTxRejected) */
  deliverTxRejected(txid: string): void {
    this.txRejected.add(txid)
    this.notifyTxRejected(txid)
  }
  /** a pushed rejection STATUS (webhook / SSE): wake-only — it pokes
   *  subscribers into re-asking txState but writes no durable verdict */
  hintTxRejected(txid: string): void {
    this.notifyTxRejected(txid)
  }
  /** the events substrate's abandon horizon fired — the observer-computed
   *  terminal verdict for a watched tx that never proved */
  deliverTxAbandoned(txid: string): void {
    this.txAbandoned.add(txid)
    this.notifyTxRejected(txid)
  }

  /** what the chain view knows about a tx right now: mined (merkle path in
   *  hand — outranks every recorded verdict), rejected (first-party truth:
   *  a verified chain event, a broadcast refusal, the abandon horizon, or
   *  the ledger's own terminal row), seen (the node knows it), else unknown.
   *  A broadcaster status STRING is a hint — it can wake a reconcile but
   *  never lands 'rejected' on its own. */
  async txState(txid: string): Promise<'mined' | 'seen' | 'rejected' | 'unknown'> {
    const local = this.deps.anchorTxs.get(txid) ?? this.deps.chain.getTx?.(txid)
    const resolved = local instanceof Promise ? await local : local
    // the tx store is consulted ONLY for the merkle-path check — a
    // prepared-but-unbroadcast tx lives there and is not 'seen'
    if (resolved?.merklePath !== undefined || this.deps.txs().get(txid)?.merklePath !== undefined) {
      this.txRejected.delete(txid)
      this.txAbandoned.delete(txid)
      return 'mined'
    }
    if (this.txRejected.has(txid) || this.deps.chain.isRejected?.(txid) === true || this.txAbandoned.has(txid)) return 'rejected'
    const ledger = await this.deps.ledgerState?.(txid).catch(() => undefined)
    if (ledger === 'rejected' || ledger === 'conflicted') return 'rejected'
    if (resolved !== undefined) return 'seen'
    if (this.deps.network === 'regtest' || this.deps.network === undefined) return 'unknown'
    const status = await (this.deps.txStatus?.(txid).catch(() => '') ?? Promise.resolve(''))
    // a rejection status string has no proof object — its terminal path is
    // the abandon horizon or a first-party refusal, never a durable verdict
    if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(status)) return 'unknown'
    // a MINED/IMMUTABLE verdict is a strong hint but carries no proof
    // object — 'mined' is reserved for "merkle path in hand" (the local
    // branch above), so the strongest a status string earns is 'seen'
    if (
      ['MINED', 'IMMUTABLE', 'RECEIVED', 'SENT_TO_NETWORK', 'ACCEPTED_BY_NETWORK', 'SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING'].includes(
        status
      )
    )
      return 'seen'
    return 'unknown'
  }

  /** any tx this instance broadcast and remembers — anchor or attestation
   *  (model-verifier deploy/spend land in attestationTxs too) */
  knownTx(txid: string): Transaction | undefined {
    return this.deps.anchorTxs.get(txid) ?? this.deps.attestationTxs.get(txid)
  }
}

/** watcher-observe mapping — pure so both the runtime closure and tests
 *  exercise the same verdict rules. Only a VERIFIED substrate record may
 *  flip durable state: a pushed 'rejected'/'double-spent' hint and every
 *  broadcaster status string are wake-only (no durable verdict from a hint
 *  — the terminal path is the abandon horizon or a first-party refusal). */
export function watcherObservation(seen: { ev: TxStatusEvent; verified: boolean } | undefined, status: string): TxObservationInput | undefined {
  if (seen?.verified === true) {
    if (seen.ev.status === 'rejected' || seen.ev.status === 'double-spent') return { status: 'rejected', source: 'chain', reason: seen.ev.status }
    if (seen.ev.status === 'mined') return { status: 'proven', source: 'broadcaster' }
  }
  if (seen?.ev.status === 'rejected' || seen?.ev.status === 'double-spent') return undefined
  if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(status)) return undefined
  if (
    ['RECEIVED', 'SENT_TO_NETWORK', 'ACCEPTED_BY_NETWORK', 'SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'MINED', 'IMMUTABLE'].includes(status)
  )
    return { status: 'seen', source: 'broadcaster' }
  return undefined
}

/** re-import gate (spv/reimport-spent): may this outpoint be imported given
 *  the txid a stored tx spends it with, and that spender's ledger row state?
 *  A live or unrecorded spender blocks the import — a first-party record
 *  beats any index answer; a definitively rejected or conflicted spender
 *  frees the outpoint again because that spend never landed. */
export function importable(spender: string | undefined, spenderLedgerState: string | undefined): boolean {
  return spender === undefined || spenderLedgerState === 'rejected' || spenderLedgerState === 'conflicted'
}
