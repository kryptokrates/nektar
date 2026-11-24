import { PublicKey, type Transaction } from '@bsv/sdk'
import type { SignedOffer, SignedWant } from '@nektar/bstack'
import type { Ingested } from '@nektar/chain'
import type { SignerLike } from '@nektar/core'
import type { AutomationStore, Operation } from '../store.js'
import type { AIExecutor } from '../ai.js'
import type { Executor } from '../rt/exec.js'

export type MarketMessage =
  | { type: 'quote'; wantId: string; offerId: string; commitment: any; mode: string }
  | {
      type: 'accept'
      wantId: string
      offerId: string
      utxo: { txid: string; vout: number; satoshis: number; script: string }
      depositorPkh: string
      tx?: string
    }
  // a deterministic accept-refusal — without it the buyer only learns the
  // deal is dead when its own refund deadline arrives
  | { type: 'decline'; wantId: string; offerId: string; reason?: string }
  | {
      type: 'delivered'
      wantId: string
      offerId: string
      /** escrow rail: the on-chain disclosure txid; direct rail: the payment txid */
      txid: string
      /** escrow rail: the covenant-spend wire (BEEF/EF/hex); direct rail omits it */
      tx?: string
      /** direct rail: the `wrapped` disclosure value itself — verified against
       *  the quote's hWrapped exactly like an on-chain reveal */
      value?: string
    }
  // encrypted self-note the buyer keeps in its own inbox until the deal ends —
  // enough to rebuild the operation after losing the automation database
  | {
      type: 'note'
      v: 1
      wantId: string
      offerId: string
      buyerOp?: string
      policyHash: string
      want: SignedWant
      offer?: { so: SignedOffer; commitment?: any; mode?: string }
      escrow: any
      utxo: { txid: string; vout: number; satoshis: number; script: string }
      escrowAction: string
      amounts: { purchaseSats: number; feeSats: number; lockedSats: number }
      tx?: string
    }
  // the seller's equivalent — rebuilds the quoted sale after a database loss;
  // signedOffer may be dropped to fit the box limit (the buyer's offer is
  // already published — `offer.offerSig` then proves the quote came from us)
  | {
      type: 'sellerNote'
      v: 1
      wantId: string
      offerId: string
      policyHash: string
      want: SignedWant
      shard: string
      seq: number
      priceSats: number
      offer: any
      signedOffer?: SignedOffer
      dueHeight: number
    }

export interface MarketTransport {
  publishWant: (sw: SignedWant) => Promise<void>
  publishOffer: (so: SignedOffer) => Promise<void>
  /** refs: stock plaintext object references riding the envelope — digest-
   *  pinned locators for external objects (evidence bundles, packs) */
  send: (toPub: string, msg: MarketMessage, refs?: import('@nektar/bstack').BoxRef[]) => Promise<void>
  /** repair a single missed fan-out leg: re-serve the journaled carrier
   *  under `journalKey` to `host` */
  repair?: (host: string, journalKey: string) => Promise<void>
  /** re-announce one journaled carrier (key `hash`) with its funding tree's
   *  BUMP attached — queued per row when `treeTxid` proves */
  republish?: (treeTxid: string, hash: string) => Promise<void>
}

export interface MarketMemory {
  shardManifest: (shard: string) => Promise<{ shardId: string; domains: string[]; offers?: Array<{ kind: string; priceSats?: number }> } | undefined>
  records: (shard: string) => Promise<Array<{ seq: number; level?: number; fragment: { d: Uint8Array } }>>
  marketCandidates?: (shard: string) => Promise<Array<{ seq: number; text: string }>>
  /** bstore locator of a shard's published pack — becomes the stock refs a
   *  `delivered` envelope carries so evidence resolves origin-free */
  packStoreRef?: (shard: string) => { bucket: string; key: string; sha256: string; size: number; endpoint?: string; chirp?: string } | undefined
}

export interface MarketDeps {
  /** this runtime's executor — preferred over the ambient executor()
   *  everywhere engine code runs: two automations in one process must not
   *  dispatch through each other's handlers */
  store: AutomationStore
  call: (tool: string, args: any) => Promise<any>
  memory: MarketMemory
  /** the operator identity — a SignerLike so external custody (BRC-100
   *  substrate, NEKTAR_WALLET=client) drives the same engine; only pub and
   *  envelope signatures were ever needed here, funding spends route
   *  through `call` → the custody-aware sale ops */
  key: SignerLike
  /** operator pubkey hex — required when `key` is a DigestSigner
   *  (pubKeyHex() is async); derived from the key itself for PrivateKey */
  me?: string
  network: string
  height: () => Promise<number>
  tip?: () => Promise<{ tip: string; tipHeight: number } | undefined>
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  /** the one transaction-facts answer — reconciles uncertain payment and
   *  settlement states without waiting on block heights; money decisions
   *  read `verdict`, the outside word stays in `hint` */
  txFacts?: (txid: string) => Promise<import('@nektar/chain').TxFacts>
  /** the verified spender's txid for an outpoint — the refund-vs-completion
   *  discriminator. Absent where the host can't name one; undefined is
   *  'unknown', never 'unspent' */
  spenderOf?: (txid: string, vout: number) => Promise<string | undefined>
  /** confirmations a proof must carry before `completion_submitted` may
   *  settle — a depth-1 proof sits one reorg away from vanishing (default 1:
   *  settle on first sight, the regtest behaviour) */
  settleDepth?: number
  broadcast?: (tx: Transaction) => Promise<import('@nektar/chain').BroadcastResult>
  /** the instance's transaction graph: `txWire` is what a counterparty is
   *  handed (BEEF when the ancestry is proven, else Extended Format, else
   *  raw), `readTx` parses such a payload and runs SPV against the
   *  instance's own headers, `rememberTx` keeps an accepted transaction */
  txWire?: (txid: string, maxBytes?: number) => string | undefined
  readTx?: (hex: string) => Promise<Ingested>
  rememberTx?: (tx: Transaction) => void
  /** mark an output of an accepted transaction as ours — direct-rail payments
   *  become the seller's custody the moment the payment verifies */
  ownOutput?: (tx: Transaction, vout: number) => void | Promise<void>
  /** register a counterparty tx with the chain-event substrate — proof
   *  delivery and the abandon horizon then cover it (the payment_pending
   *  watch: a proof or a conflict wakes the parked sale) */
  watchTx?: (txid: string) => void
  /** the runtime's one custody effect set — the coordinator and the tx
   *  object must run the same handlers */
  txEffects?: import('../machines/effects.js').TxEffects
  afterPrepare?: (actionId: string) => void | Promise<void>
  transport: MarketTransport
  executor?: Executor
  ai: AIExecutor
  /** this agent's blogs deal-journal stream id; signed into wants/offers so
   *  the counterparty (and anyone holding the deal) can follow the journal */
  logStream?: string
  /** operator stop flag — consulted at every new-deal and new-escrow entry
   *  point (fail closed). In-flight deals are never gated: refunds and
   *  settlements keep running so the stop can't strand money. */
  killed?: () => boolean
  /** watchdog flag — every configured host currently unreachable. New deals
   *  and new escrows pause while deaf; in-flight deals continue. */
  deaf?: () => boolean
  now?: () => number
  opts?: Partial<MarketOptions>
  /** diagnostic surface for the serialized-work mutex — a hung run() link
   *  wedges every later run() silently; the held-too-long check names it
   *  here (optional: tests run without the runtime's log) */
  log?: (line: string) => void
}

export interface MarketOptions {
  refundBlocks: number
  offerTtlBlocks: number
  saleMaxFee: number
  escrowFeeReserveSats: number
  /** the direct rail's policy-accounted fee bound — what the P2PKH payment's
   *  fee can cost at the configured rate (a 1-in/~3-out spend is ~450B;
   *  100 covers ≤ ~220 sat/KB). Policy accounting and offer ranking quote
   *  this on a `direct` quote instead of `maxFee + escrowFeeReserveSats` —
   *  a 50-sat outright payment must not carry covenant-class exposure. The
   *  custody take margin derives from the same rate, so raising
   *  NEKTAR_FEE_RATE_SAT_PER_KB past ~220 sat/KB wants a matching raise
   *  here (mcp wires the derived value). */
  directFeeReserveSats: number
  sellerSafetyBlocks: number
  approvalTtlMs: number
  /** buyer-side direct-rail ceiling: when a quote advertises `direct` and the
   *  price is at or under this, the deal settles by outright P2PKH payment
   *  instead of an escrow covenant — payment-final (no refund), so the cap
   *  is the operator's no-recourse exposure. Absent/0 = escrow only. */
  directMaxSats?: number
  /** seller-side opt-in: quotes carry `direct: true` so counterparties may
   *  choose the direct rail for this sale */
  directAccept?: boolean
  /** seller-side direct-rail ceiling — `direct` is advertised only when the
   *  price is at or under this. The direct rail ships the disclosure with an
   *  unproven payment: a post-fact dead verdict can't claw the secret back,
   *  so this cap is the operator's no-recourse exposure (the mirror of the
   *  buyer's directMaxSats). Absent = any price (opt-in already accepts it). */
  directAcceptMaxSats?: number
  /** the largest sale delivered against an unproven payment, both rails —
   *  above it the sale parks in payment_pending until the payment's own
   *  proof arrives; 0 = always wait */
  maxZeroconfSats: number
  /** unproven ancestor txs tolerated behind a zero-conf payment */
  maxUnconfirmedAncestors: number
  /** buyer-side opt-in: a verified disclosure counts as the usefulness
   *  evaluation — deals reach `useful` without an operator call. Default off:
   *  the consumer judges content. */
  autoUseful?: boolean
  /** minimum time a want stays in `collecting` before an acceptable offer may
   *  early-select — a cheaper offer landing inside the window still wins;
   *  `scoring.collectMs` remains the outer ceiling. 0 = select on the first
   *  recorded offer (previous behaviour). */
  minCollectMs: number
  /** quiet-extended collect window — opt-in. While offers keep arriving the
   *  window stays open past `minCollectMs` (early-select is deferred); it
   *  closes `collectQuietMs` after the last recorded offer, or at
   *  `scoring.collectMs`, whichever first. 0 = off (select at minCollectMs +
   *  first offer — the default, unchanged). */
  collectQuietMs: number
  /** extra want lifetime beyond `scoring.collectMs` — the want stays valid
   *  for sellers while the buyer republishes it at each collection boundary
   *  instead of expiring on `no_qualifying_offer`. A seller under load that
   *  misses the first window still lands inside the want's exp; the deal only
   *  dies when the want itself does. 0 = old behaviour (exp == collectMs). */
  wantExtendMs: number
  /** hard bound on one outbox deliver call — every leg inside it is
   *  timeout-capped already, but a leg that never settles must not park
   *  its pump worker (and starve the queue behind it) for as long as it
   *  hangs. The row retries transport-classed like any failed send. */
  deliverBoundMs: number
}

export const DEFAULTS: MarketOptions = {
  refundBlocks: 144,
  offerTtlBlocks: 12,
  saleMaxFee: 10,
  escrowFeeReserveSats: 500,
  directFeeReserveSats: 100,
  sellerSafetyBlocks: 2,
  approvalTtlMs: 86_400_000,
  minCollectMs: 1000,
  collectQuietMs: 0,
  wantExtendMs: 120_000,
  deliverBoundMs: 45_000,
  maxZeroconfSats: 1000,
  maxUnconfirmedAncestors: 2
}
export const pkhOf = (pub: string) => Buffer.from(PublicKey.fromString(pub).toHash() as number[]).toString('hex')
/** a bstore ObjectRef → stock plaintext ref (digest-pinned locator). https
 *  only — uhrp/local http gateways can't ride a stock ref; the envelope
 *  still carries the disclosure fields themselves. */
const packBoxRef = (r: { sha256: string; size: number; endpoint?: string; bucket: string; key: string }): import('@nektar/bstack').BoxRef | undefined => {
  if (r.endpoint === undefined || !r.endpoint.startsWith('https://')) return undefined
  return { url: `${r.endpoint.replace(/\/+$/, '')}/${r.bucket}/${r.key.split('/').map(encodeURIComponent).join('/')}`, sha256: r.sha256, length: r.size }
}
/** the stock refs a `delivered` envelope carries: a digest-pinned locator
 *  for the sold shard's stored pack so the buyer resolves evidence through
 *  bstore/CHIRP rather than the seller's origin */
export const deliveredPackRefs = (d: MarketDeps, op: Operation): import('@nektar/bstack').BoxRef[] | undefined => {
  const shard = op.data.shard
  const r = shard !== undefined ? d.memory.packStoreRef?.(shard) : undefined
  const ref = r !== undefined ? packBoxRef(r) : undefined
  return ref !== undefined ? [ref] : undefined
}
export const CAP = /_cap$|_above_policy$|^domain_outside_policy$/
/** D7 — the stuck-deal rule: no FSM state is unbounded. Every nonterminal
 *  state declares a maxAge (measured against the op's last transition —
 *  activity refreshes it) and a forced exit: abort (nothing on-chain can be
 *  in flight), refund (escrow exists — reclaim it), or escalate:human
 *  ('uncertain' — a broadcast may have escaped; only a person can close it).
 *  Terminal states need no entry; 'escalated'/'uncertain' already ARE the
 *  human exit — their bound is a re-assert, not a transition. */
export type StuckExit = 'abort' | 'refund' | 'escalate:human'
export const STATE_LIMITS: Record<string, { maxAgeMs: number; exit: StuckExit }> = {
  // buyer
  published: { maxAgeMs: 60_000, exit: 'abort' },
  collecting: { maxAgeMs: 86_400_000, exit: 'abort' },
  selected: { maxAgeMs: 15 * 60_000, exit: 'abort' },
  escalated: { maxAgeMs: 7 * 86_400_000, exit: 'escalate:human' },
  funding: { maxAgeMs: 30 * 60_000, exit: 'escalate:human' },
  // a SpendGuard daily-cap refusal is a window, not a no — park the deal
  // until the UTC-midnight reset (or an operator raise that lands first)
  // instead of dying 'funding_failed'. Two days is the stuck bound; the
  // deferral itself lapses at dueAt
  funding_deferred: { maxAgeMs: 2 * 86_400_000, exit: 'escalate:human' },
  awaiting_disclosure: { maxAgeMs: 6 * 3600_000, exit: 'refund' },
  // a refused escrow that landed anyway is a live covenant on our sats —
  // park here until refundAfter, then reclaim. The exit is only the stuck
  // bound: dueHeight is the real driver and can sit months out
  refund_pending: { maxAgeMs: 180 * 86_400_000, exit: 'escalate:human' },
  refunding: { maxAgeMs: 15 * 60_000, exit: 'escalate:human' },
  uncertain: { maxAgeMs: 86_400_000, exit: 'escalate:human' },
  // seller
  preparing: { maxAgeMs: 10 * 60_000, exit: 'abort' },
  quoted: { maxAgeMs: 86_400_000, exit: 'abort' },
  // the payment verified but waits on its own proof — three days is the
  // stuck bound; the abandon horizon and the lapse deadline are the real
  // drivers (payment_died / payment_pending_lapsed)
  payment_pending: { maxAgeMs: 3 * 86_400_000, exit: 'escalate:human' },
  completing: { maxAgeMs: 30 * 60_000, exit: 'escalate:human' },
  completion_submitted: { maxAgeMs: 3 * 86_400_000, exit: 'escalate:human' },
  // observer
  watching: { maxAgeMs: 90 * 86_400_000, exit: 'escalate:human' },
  seizing: { maxAgeMs: 30 * 60_000, exit: 'escalate:human' }
}
export const NONTERMINAL = Object.keys(STATE_LIMITS)
export const TERMINAL = [
  'expired',
  'denied',
  'refunded',
  'useful',
  'not_useful',
  'disputed',
  'canceled',
  'declined',
  'preparation_failed',
  'completion_failed',
  'settled',
  'enabled',
  'funding_failed',
  'seized',
  'seize_failed'
]
/** Money settled, awaiting an external actor (markUsefulness) — no bound by
 *  design, but DECLARED: NONTERMINAL ∪ TERMINAL ∪ PARKED must cover the
 *  state union or the audit has an invisible class */
export const PARKED = ['disclosure_verified']

// StoredOffer moved to ../store.js — it lives beside the op.data merge that
// persists it (the MarketOpData schema); re-exported so engine code keeps
// this import site
export type { StoredOffer } from '../store.js'

/** declared-non-nullish reads that can still be absent at runtime — record-index
 *  lookups and cast-boundary payloads. Widening here keeps the guard sites honest. */
export { sparse } from '@nektar/core'
