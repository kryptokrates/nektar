// Chain-facing interfaces — everything chain-related goes through these so the
// whole system is testable against the regtest stack, and swappable to live
// regtest (NEKTAR_NETWORK=regtest) without code changes.
import type { Transaction, MerklePath } from '@bsv/sdk'

export interface BlockHeader {
  height: number
  hash: string // display hex
  prevHash: string
  merkleRoot: string // display hex
  time: number
  bits: number
  nonce: number
  /** absent from sources that don't serve it — a header without a version
   *  cannot be serialized, so it can never pass headerPowValid */
  version?: number
}

export interface ChainTracker {
  currentHeight: () => Promise<number>
  isValidRootForHeight: (root: string, height: number, signal?: AbortSignal) => Promise<boolean>
  headerAtHeight: (height: number) => Promise<BlockHeader | undefined>
  /** subscribe to tip changes; reorg: blocks removed from tip */
  onReorg?: (cb: (removed: BlockHeader[], newTip: BlockHeader) => void) => void
  /** bypass the header cache and re-ask the source — a witnessed pair uses
   *  this when two sources disagree, so a STALE CACHE cannot masquerade as a
   *  chain split: a same-height hash change runs the reorg walk (onReorg
   *  fires, neighbours evict) before the fresh answer is served */
  refreshHeader?: (height: number) => Promise<BlockHeader | undefined>
  /** the real tip height (currentHeight may carry an offset
   *  100 for the SDK coinbase-maturity rule) — used for confirmation counts */
  tipHeight?: () => Promise<number>
}

export interface BroadcastResult {
  status: 'success' | 'error'
  txid?: string
  description?: string
}

export type TxStatus = 'received' | 'unconfirmed' | 'mined' | 'rejected' | 'double-spent'

export interface TxStatusEvent {
  txid: string
  status: TxStatus
  blockHeight?: number
  merklePath?: MerklePath
}

export interface Broadcaster {
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  /** statuses pushed as the chain progresses */
  onStatus?: (cb: (ev: TxStatusEvent) => void) => void
  /** pull the broadcaster's current word for a txid — a status HINT; never
   *  a verdict (txFacts keeps it in the hint field, structurally) */
  statusHint?: (txid: string) => Promise<import('./txfacts.js').StatusHint>
}

export interface FundingOut {
  tx: Transaction
  vout: number
  satoshis: number
}

/** Supplies funding inputs (P2PKH) for anchor txs. */
export interface Funder {
  /** returns a utxo of at least `satoshis`, or throws */
  take: (satoshis: number) => Promise<FundingOut>
  address: () => Promise<string> | string
}

/** Why a coin is being spent — the allocation floor differs by class: a
 *  `deal` is a live settlement that must always find a spendable input;
 *  `publish`/`upkeep` are background work that yields to deals. */
export type FundPurpose = 'deal' | 'publish' | 'upkeep'
/** upstream/test call sites name it FundsPurpose — same labels */
export type FundsPurpose = FundPurpose

/** The single owner of coin state: selection, reservation, spend marks,
 *  abandonment and adoption. Callers never mark spent or owned directly —
 *  they hand the whole transaction to `spent`/`died`/`adopt` and the
 *  implementation decides which outpoints live or die. */
export interface Funds {
  /** address new funds are paid to */
  address: () => Promise<string> | string
  /** spendable balance in sats (spent and reserved outpoints excluded) */
  balance: () => Promise<number>
  /** the spendable view — owned unspent AND unreserved; the set take()
   *  may choose from. fundingStatus and tests read it. */
  outs: () => Promise<FundingOut[]>
  /** the owned unspent set, reservations included — status views and
   *  watchlist seeding read it */
  list: () => Promise<FundingOut[]>
  /** reserve an output of ≥ `sats` for a `purpose` spend and hand over the
   *  proof we hold. `upkeep`/`publish` may not dip the balance below the
   *  floor that keeps one deal affordable. Throws when nothing fits. */
  take: (purpose: FundPurpose, sats: number) => Promise<FundingOut>
  /** one call after a successful broadcast: consumes our inputs, adopts
   *  outputs that pay us (change and escrow returns). */
  spent: (tx: Transaction) => void | Promise<void>
  /** the tx was signed and exported for someone ELSE to air (a BEEF
   *  payment handed to a seller): inputs are consumed — they are signed
   *  away either way — but outputs paying us are NOT adopted; if the
   *  spend lands, discovery adopts the change then. */
  exported: (tx: Transaction) => void | Promise<void>
  /** the spend was rejected or abandoned: its inputs free again where the
   *  parent still lives, its outputs leave us for good. Accepts the dead
   *  tx or its txid (resolved through the impl's tx view). */
  died: (tx: Transaction | string) => void | Promise<void>
  /** coins sent to us by someone else; refuses outpoints we already spent
   *  or tombstoned — returns false when the output was not adopted */
  adopt: (tx: Transaction, vout: number) => boolean | Promise<boolean>
  /** deliberately bring a tombstoned outpoint back — the caller verified it
   *  is live on-chain (reconcile), so the spend-mark is lifted */
  revive: (tx: Transaction, vout: number) => void | Promise<void>
  /** tombstone one outpoint — observed spend consumed it on-chain, or a
   *  first-party probe reports it spent. Never offered again. */
  burn: (txid: string, vout: number) => void | Promise<void>
  /** reserve a specific outpoint — named-output holds (fee vault, imports)
   *  rather than letting take() choose. Returns false when another spend
   *  already holds it — the caller must pick a different outpoint */
  hold: (txid: string, vout: number) => boolean
  /** un-reserve an outpoint — the spend built on it was never broadcast */
  release: (txid: string, vout: number) => void
  /** pull the backend's spendable view — wallet custody re-queries its
   *  basket; in-memory stores are a no-op */
  refresh?: () => Promise<void>
  /** outpoint lease hook — the automation ledger's prepared-tx leases plug
   *  in here so concurrent takes never pick an input a prepared tx claims */
  leased?: (txid: string, vout: number) => Promise<boolean> | boolean
  /** hint-only pass-over (e.g. a broadcaster says this candidate's parent
   *  was rejected) — never writes bookkeeping, only skips this take */
  hintSkip?: (tx: Transaction, vout: number) => Promise<boolean>
  /** first-party spend probe (chain utxo status / wallet truth): a 'spent'
   *  answer burns the candidate permanently during selection */
  spendCheck?: (txid: string, vout: number) => Promise<boolean>
  /** refusal-class dead parent — the candidate's parent tx is first-party
   *  dead or broadcaster-REJECTED. `take` answers with died(): the
   *  phantom's inputs revive, its owned outputs drop — so stock stranded
   *  by an accepted-then-evicted broadcast recycles instead of leaking.
   *  Unlike hintSkip this seam DOES write (it heals), but only custody
   *  bookkeeping — never a chain verdict */
  deadParent?: (tx: Transaction) => Promise<boolean>
  /** resolve a tx by id for died()'s input-revive walk — hex-stored drafts
   *  lose input sourceTransactions, so reviving a tombstoned input needs a
   *  lookup that can fetch the dead spend's parent (held store, index).
   *  Late-bindable like hintSkip: the wiring instance owns the tx graph */
  txOf?: (txid: string) => Promise<Transaction | undefined> | Transaction | undefined
  /** a purpose's take served by a dedicated pool instead of the shared set —
   *  the fee-vault covenant mints capped outs for `publish` takes; returns
   *  undefined when it has nothing to serve (fall through to the pool) */
  takeOverride?: (purpose: FundPurpose, sats: number) => Promise<FundingOut | undefined>
}
