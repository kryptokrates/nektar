import type { Knex } from 'knex'
import { randomUUID } from 'node:crypto'
import type { SignedWant, SignedOffer, BoxRef } from '@nektar/bstack'
import type { ObjectRef } from '@nektar/store'
import { detached, sparse, spread, stable01 } from '@nektar/core'
import { policyHash, policyError, canonicalJson, type AutomationPolicy, type ActionContext } from './policy.js'
import { WAITS } from './waits.js'
import { txStep } from './machines/tx.machine.js'
import { opEdgeAgrees } from './machines/driver.js'

const T = {
  ops: 'market_operations',
  events: 'market_events',
  inbox: 'market_inbox',
  cursors: 'market_source_cursors',
  outbox: 'market_outbox',
  policies: 'market_policies',
  approvals: 'market_approvals',
  budget: 'budget_reservations',
  leases: 'wallet_reservations',
  txs: 'wallet_transactions',
  txEvents: 'wallet_tx_events'
} as const

/** why an outbox row is deferred — the column replaces the error-text LIKE
 *  pattern callers used to re-derive it (a message that merely CONTAINS a
 *  transport keyword no longer changes the row's class) */
export type DeferClass = 'transport' | 'funding-slot'

/** classify a delivery failure once — funding starvation is a resource wait
 *  (no attempts consumed), everything transport-shaped retries with backoff,
 *  anything else stays classless (dead-letters on the normal path) */
export function classifyDeliveryError(msg: string): DeferClass | undefined {
  if (/funding slot/.test(msg)) return 'funding-slot'
  // locktime/not-yet-final verdicts (broadcaster code 476) are a timing wait,
  // not a rejection — resubmit the same bytes once the lock passes
  if (/fetch failed|unreachable|http 5\d\d|timed out|timeout|aborted|ECONN|ENOTFOUND|EAI_AGAIN|socket hang up|network|non-final|lock ?time|\b476\b/i.test(msg))
    return 'transport'
  return undefined
}

/** the escrow cfg bag purchase/sale ops carry — the sale op's wire record;
 *  known fields typed, the rest of the covenant params ride opaque */
type EscrowCfg = {
  sellerPkh?: string
  depositorPkh?: string
  refundAfter?: string | number
  direct?: boolean
} & Record<string, unknown>

export type UtxoRef = { txid: string; vout: number; satoshis: number; script: string }

/** the quote's commitment bag — wire-supplied (types.ts keeps the wire side
 *  opaque): the fields the engine scores/verifies are typed, extra wire
 *  fields ride opaque */
type QuoteCommitment = {
  leaf?: string
  batchR?: unknown
  anchorTxid?: string
  proof?: { path?: unknown }
} & Record<string, unknown>

/** StoredOffer — declared beside the merge that persists it (market/types
 *  re-exports it so engine code keeps one import site) */
export interface StoredOffer {
  so: SignedOffer
  commitment?: QuoteCommitment
  mode?: string
  reject?: string
}

/** the closed op.data schema — every field the FSM merge may carry. All
 *  optional: data accumulates through the FSM, so readers presence-narrow
 *  per state; `null` is the merge's universal clear-marker on writes
 *  (pendingEscrow:null …). checkOpData enforces this field set + shapes on
 *  every write — a typo'd or wrong-typed field fails the transition in the
 *  transaction, not a downstream cast. */
export interface MarketOpData {
  // buyer purchase (escrow/utxo/direct also stamped on accept for sellers)
  want?: SignedWant
  offers?: Record<string, StoredOffer>
  pendingQuotes?: Record<string, { commitment?: QuoteCommitment; mode?: string }>
  selected?: { key: string; scoreBps?: number }
  ranking?: { ranked?: unknown; rejected?: unknown }
  collectingSince?: number
  lastOfferAt?: number
  pendingApproval?: { approvalId?: string; actionDigest?: string; reason?: string }
  pendingEscrow?: { cfg?: EscrowCfg; utxo?: UtxoRef; feeSats?: number }
  pendingDelivered?: { from: string; m: unknown; refs?: BoxRef[] }
  /** buyer: the flat escrow cfg record (sale#escrow's return). Seller: the
   *  nested {cfg, utxo} descriptor — EscrowCfg's index signature absorbs it
   *  structurally; the seller reads .cfg/.utxo through it */
  escrow?: EscrowCfg
  utxo?: UtxoRef
  escrowAction?: string
  escrowTxid?: string
  completeTxid?: string
  refundTxid?: string
  refundSats?: number
  refundTries?: number
  refundProven?: boolean
  lastRefusalReason?: string
  direct?: boolean
  amounts?: BudgetAmounts
  fundingTries?: number
  fundingSince?: number
  provenHeight?: number
  reconcilePing?: boolean
  usefulness?: { useful: boolean; evidence?: unknown }
  result?: unknown
  evidenceRef?: ObjectRef
  restored?: boolean
  // seller sale
  shard?: string
  seq?: number
  priceSats?: number
  ai?: { relevanceBps?: number; model?: { provider: string; modelId: string } }
  /** the seller's quote bag — covenant params are the sale op's wire
   *  contract (price/refundAfter/maxFee/sellerPkh…), validated there */
  offer?: Record<string, any>
  signedOffer?: SignedOffer
  sellerSats?: number
  // enable singleton
  policyHash?: string
  // observer bond-watch
  cfg?: { operatorPkh?: string; bondReleaseHeight?: string; maxFee?: string; epochLen?: string }
  n?: number
  seizeTxid?: string
}

/** a transition's data merge — schema fields, plus `null` as the clear
 *  marker the blind merge applies verbatim */
export type OpDataPatch = { [K in keyof MarketOpData]?: MarketOpData[K] | null }

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const isStr = (v: unknown): v is string => typeof v === 'string'
const isNum = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v)
const isBool = (v: unknown): v is boolean => typeof v === 'boolean'
const isUtxo = (v: unknown): v is UtxoRef => isObj(v) && isStr(v.txid) && isNum(v.vout) && isNum(v.satoshis) && isStr(v.script)
const isObjMap = (v: unknown): boolean => isObj(v) && Object.values(v).every(isObj)

/** one predicate per declared field — Record<keyof MarketOpData> means the
 *  table and the interface cannot drift: a new field without a check is a
 *  type error, an undeclared key at write is a runtime throw. */
const OPDATA_CHECKS: Record<keyof MarketOpData, (v: unknown) => boolean> = {
  want: isObj,
  offers: isObjMap,
  pendingQuotes: isObjMap,
  selected: (v) => isObj(v) && isStr(v.key) && (v.scoreBps === undefined || isNum(v.scoreBps)),
  ranking: isObj,
  collectingSince: isNum,
  lastOfferAt: isNum,
  pendingApproval: isObj,
  pendingEscrow: (v) =>
    isObj(v) && (v.cfg === undefined || isObj(v.cfg)) && (v.utxo === undefined || isUtxo(v.utxo)) && (v.feeSats === undefined || isNum(v.feeSats)),
  pendingDelivered: (v) => isObj(v) && isStr(v.from) && isObj(v.m) && (v.refs === undefined || Array.isArray(v.refs)),
  escrow: isObj,
  utxo: isUtxo,
  escrowAction: isStr,
  escrowTxid: isStr,
  completeTxid: isStr,
  refundTxid: isStr,
  refundSats: isNum,
  refundTries: isNum,
  refundProven: isBool,
  lastRefusalReason: isStr,
  direct: isBool,
  amounts: (v) => isObj(v) && isNum(v.purchaseSats) && isNum(v.feeSats) && isNum(v.lockedSats),
  fundingTries: isNum,
  fundingSince: isNum,
  provenHeight: isNum,
  reconcilePing: isBool,
  usefulness: (v) => isObj(v) && isBool(v.useful),
  result: () => true,
  evidenceRef: isObj,
  restored: isBool,
  shard: isStr,
  seq: isNum,
  priceSats: isNum,
  ai: isObj,
  offer: isObj,
  signedOffer: isObj,
  sellerSats: isNum,
  policyHash: isStr,
  cfg: isObj,
  n: isNum,
  seizeTxid: isStr
}

/** the write-side codec: rejects an undeclared key or a wrong-typed value
 *  before it enters the merge. Runs on createOperation's data and on every
 *  transition's merged bag — inside the write transaction, so a rejection
 *  rolls back the event row and outbox intents with it. `null`/missing
 *  fields pass: null is the merge's clear-marker and data accumulates. */
function checkOpData(data: unknown): void {
  if (!isObj(data)) throw new Error('op.data: not an object')
  for (const [k, v] of Object.entries(data)) {
    const chk = sparse(OPDATA_CHECKS[k as keyof MarketOpData])
    if (chk === undefined) throw new Error(`op.data.${k}: undeclared field — the op.data schema is closed`)
    if (v === null || v === undefined) continue
    if (!chk(v)) throw new Error(`op.data.${k}: malformed value`)
  }
}

export interface Operation {
  operationId: string
  role: 'buyer' | 'seller' | 'observer'
  kind: string
  dedupeKey: string
  policyHash: string
  network: string
  state: string
  revision: number
  /** the accumulated FSM bag — typed by MarketOpData: every write passed
   *  checkOpData, so the hydrated JSON's declared fields are its real ones */
  data: MarketOpData
  dueAt?: number
  dueHeight?: number
  /** next allowed reconcile probe — a persisted throttle so terminal-op
   *  scans skip rows still inside their cooldown (see markReconciled) */
  nextReconcileAt?: number
  lastError?: string
  createdAt: number
  updatedAt: number
}

export interface InboxItem {
  source: string
  messageId: string
  artifactHash: string
  kind: string
  payload: unknown
  attempts: number
}
export interface OutboxItem {
  actionId: string
  operationId: string
  kind: string
  destination: string
  payload: unknown
  /** stock plaintext refs (url/sha256/length[/key]) riding an envelope
   *  message — digest-pinned locators for external evidence */
  refs?: Array<{ url: string; sha256: string; length: number; key?: string }>
  attempts: number
  /** the row's scheduled wake — the drain versions its send idempotency on
   *  it: a defer moves next_at without spending an attempt, and resends must
   *  still be new invocations, not dedupes of the first leg */
  nextAt?: number
}
export interface BudgetAmounts {
  purchaseSats: number
  feeSats: number
  lockedSats: number
}
export type BudgetResult = { ok: true; existing: boolean } | { ok: false; code: string }
export type TxState =
  | 'prepared'
  | 'exported'
  | 'submission_unknown'
  | 'submitted'
  | 'observed'
  | 'confirmed'
  | 'rejected'
  | 'conflicted'
  /** an exported tx whose counterparty never aired it past the TTL —
   *  leases released, custody revives whatever stays live; not a chain
   *  verdict */
  | 'expired'
  /** watched past the horizon with no competing spend — an ABSENCE verdict,
   *  never 'conflicted' (that label means a proven competing spend). Fires
   *  the custody rollback; keeps the trailing watch for a late landing */
  | 'abandoned'
export interface WalletTx {
  actionId: string
  operationId: string
  kind: string
  txid: string
  rawHex: string
  parents: Record<string, string>
  feeSats: number
  state: TxState
  attempts: number
  /** row creation ms — the export-expiry clock reads it */
  at: number
  /** ms the row last changed state — the trailing-watch clock */
  stateAt: number
  lastObservation?: unknown
}
export interface Approval {
  approvalId: string
  operationId: string
  reason: string
  policyHash: string
  actionDigest: string
  maxExtraSats: number
  expiresAt: number
  status: 'pending' | 'approved' | 'denied' | 'consumed' | 'canceled' | 'expired'
  evidence: unknown
  decidedBy?: string
}

const nat = (n: unknown): n is number => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0
const dayOf = (ms: number) => new Date(ms).toISOString().slice(0, 10)
const sum = async (q: Knex.QueryBuilder, col: string): Promise<number> => Number((await q.sum({ s: col }).first())?.s ?? 0)

export class RevisionConflict extends Error {}

/** recordBroadcast claimed an outpoint a foreign LIVE lease still holds —
 *  distinct from a plain Error so the caller (wrapBroadcast) can tell a
 *  claim refusal from a ledger outage. */
export class LeaseConflictError extends Error {}

/** the ops.machine refused a transition() write — the cause resolved to
 *  machine candidates but no declared edge carries (from,cause)→to. Throws
 *  inside the write transaction: state, event row and outbox all roll back.
 *  A spec gap, not a runtime error — fix the table or the caller. */
export class MachineBlocked extends Error {}

export class AutomationStore {
  /** wake fired after any transition/enqueue commits fresh outbox rows —
   *  the runtime wires it to the detached pump; tests leave it unset and
   *  the engine flushes inline at the end of tick() */
  outboxReady?: () => void

  /** wake fired after a commit lands work a later pass must see — a new
   *  operation, a state-changing transition, fresh inbox rows, an accepted
   *  tx event. The runtime wires it to the deals loop; same-state data
   *  stamps and deduped ingests do not fire. */
  workReady?: () => void

  /** a committed deadline — transition/createOperation dueAt values go to
   *  the scheduler's dueIn so a collect window or seize bound fires at its
   *   instant, not the next floor tick (the floor stays the missed-arm
   *  backstop either way). The op id rides along: under the restate
   *  executor the arm is a durable delayed send to the op's own deal
   *  object, not a shared wheel poke */
  dueAtArmed?: (operationId: string, at: number) => void

  /** the residence-bound map — the engine injects STATE_LIMITS so a state
   *  change into a bounded state derives its stuck deadline at COMMIT and
   *  arms the wheel at its instant, rather than waiting for the next
   *  advance() scan to notice the age (the scan stays the floor) */
  residenceBound?: (state: string) => number | undefined

  /** the wallet ledger's connection — txs + leases + tx events are custody
   *  bookkeeping and share records.sqlite's file so take()/leasePredicate and
   *  the resubmit path read one physical truth (the lanes.sqlite precedent);
   *  standalone stores keep the single-connection default */
  private readonly wkx: Knex

  constructor(
    private kx: Knex,
    private now: () => number = Date.now,
    walletKx?: Knex
  ) {
    this.wkx = walletKx ?? kx
  }

  async close(): Promise<void> {
    await this.kx.destroy()
    if (this.wkx !== this.kx) await this.wkx.destroy()
  }

  async init(): Promise<void> {
    const mk = async (name: string, build: (t: Knex.CreateTableBuilder) => void) => {
      if (!(await this.kx.schema.hasTable(name))) await this.kx.schema.createTable(name, build)
    }
    const mkW = async (name: string, build: (t: Knex.CreateTableBuilder) => void) => {
      if (!(await this.wkx.schema.hasTable(name))) await this.wkx.schema.createTable(name, build)
    }
    await mk(T.ops, (t) => {
      t.string('operation_id').primary()
      t.string('role').notNullable()
      t.string('kind').notNullable()
      t.string('dedupe_key').notNullable().unique()
      t.string('policy_hash').notNullable()
      t.string('network').notNullable()
      t.string('state').notNullable().index()
      t.integer('revision').notNullable()
      t.text('data').notNullable()
      t.bigInteger('due_at')
      t.integer('due_height')
      // materialized out of the data JSON so the live-op probe seeks instead
      // of parsing: proven_height IS NULL on a completion_submitted row is
      // the hot-loop "still working" signal
      t.integer('proven_height')
      // probe metadata, NOT FSM state — reconcileGate stamps it directly so
      // terminal-op scans skip rows still inside their probe cooldown
      t.bigInteger('next_reconcile_at')
      t.text('last_error')
      t.bigInteger('created_at').notNullable()
      t.bigInteger('updated_at').notNullable()
    })
    if (!(await this.kx.schema.hasColumn(T.ops, 'proven_height'))) await this.kx.schema.alterTable(T.ops, (t) => t.integer('proven_height'))
    if (!(await this.kx.schema.hasColumn(T.ops, 'next_reconcile_at'))) await this.kx.schema.alterTable(T.ops, (t) => t.bigInteger('next_reconcile_at'))
    // pre-column rows carry the value only inside data — backfill once so
    // hasLiveOps never reads the JSON column
    await this.kx(T.ops)
      .where({ state: 'completion_submitted' })
      .whereNull('proven_height')
      .update({ proven_height: this.kx.raw("json_extract(data, '$.provenHeight')") })
    // the live-op scan (notStates/state IN) runs every hot tick — it must
    // seek, not table-scan, as terminal rows accumulate over an agent's life
    await this.kx.raw(`CREATE INDEX IF NOT EXISTS ${T.ops}_state_index ON ${T.ops} (state)`)
    // height-due probes: hasDueAtHeight narrows on state then filters
    // due_height — the composite turns the post-filter into a range seek
    await this.kx.raw(`CREATE INDEX IF NOT EXISTS ${T.ops}_state_due_height ON ${T.ops} (state, due_height)`)
    // reconcile-due scans (the funding_failed/refunded per-tick probes) —
    // same shape: state prefix, then the cooldown column as the range
    await this.kx.raw(`CREATE INDEX IF NOT EXISTS ${T.ops}_state_next_reconcile ON ${T.ops} (state, next_reconcile_at)`)
    await mk(T.events, (t) => {
      t.increments('seq')
      t.string('operation_id').notNullable().index()
      t.bigInteger('at').notNullable()
      t.string('from_state')
      t.string('to_state').notNullable()
      t.string('cause').notNullable()
      t.text('evidence')
    })
    await mk(T.inbox, (t) => {
      t.string('source').notNullable()
      t.string('message_id').notNullable()
      t.string('artifact_hash').notNullable().index()
      t.string('kind').notNullable()
      t.text('payload').notNullable()
      t.string('status').notNullable().index()
      t.integer('attempts').notNullable()
      t.text('last_error')
      t.bigInteger('received_at').notNullable()
      t.primary(['source', 'message_id'])
    })
    // pendingInbox is an every-tick scan over a table that accumulates
    // done/dead rows forever — (status, received_at) keeps it O(pending)
    await this.kx.raw(`CREATE INDEX IF NOT EXISTS ${T.inbox}_status_received ON ${T.inbox} (status, received_at)`)
    await mk(T.cursors, (t) => {
      t.string('source').primary()
      t.string('cursor').notNullable()
      t.bigInteger('updated_at').notNullable()
    })
    await mk(T.outbox, (t) => {
      t.string('action_id').primary()
      t.string('operation_id').notNullable()
      t.string('kind').notNullable()
      t.string('destination').notNullable()
      t.text('payload').notNullable()
      t.text('refs')
      t.string('status').notNullable().index()
      t.integer('attempts').notNullable()
      t.bigInteger('next_at').notNullable()
      t.text('last_error')
      t.text('defer_class')
    })
    // dueOutbox/nextOutboxAt filter status='pending' and order/min on
    // next_at — the composite turns both into a range seek
    await this.kx.raw(`CREATE INDEX IF NOT EXISTS ${T.outbox}_status_next ON ${T.outbox} (status, next_at)`)
    if (!(await this.kx.schema.hasColumn(T.outbox, 'refs'))) await this.kx.schema.alterTable(T.outbox, (t) => t.text('refs'))
    if (!(await this.kx.schema.hasColumn(T.outbox, 'defer_class'))) {
      await this.kx.schema.alterTable(T.outbox, (t) => t.text('defer_class'))
      // rows deferred before the column existed carry the class only in
      // their error text — backfill so unblockOutbox keys on the column alone
      await this.kx(T.outbox).where({ status: 'pending' }).where('last_error', 'like', '%funding slot%').update({ defer_class: 'funding-slot' })
    }
    await mk(T.policies, (t) => {
      t.string('policy_hash').primary()
      t.string('policy_id').notNullable()
      t.text('policy').notNullable()
      t.bigInteger('approved_at').notNullable()
      t.string('approved_by').notNullable()
      t.bigInteger('revoked_at')
    })
    await mk(T.approvals, (t) => {
      t.string('approval_id').primary()
      t.string('operation_id').notNullable()
      t.string('reason').notNullable()
      t.string('policy_hash').notNullable()
      t.string('action_digest').notNullable().index()
      t.bigInteger('max_extra_sats').notNullable()
      t.bigInteger('expires_at').notNullable()
      t.string('status').notNullable().index()
      t.text('evidence')
      t.string('decided_by')
      t.bigInteger('decided_at')
      t.bigInteger('created_at').notNullable()
    })
    await mk(T.budget, (t) => {
      t.string('action_id').primary()
      t.string('operation_id').notNullable()
      t.string('policy_hash').notNullable().index()
      t.string('day').notNullable()
      t.bigInteger('purchase_sats').notNullable()
      t.bigInteger('fee_sats').notNullable()
      t.bigInteger('locked_sats').notNullable()
      t.string('state').notNullable()
      t.bigInteger('at').notNullable()
    })
    await mkW(T.leases, (t) => {
      t.string('outpoint').primary()
      t.string('action_id').notNullable().index()
      t.bigInteger('satoshis').notNullable()
      t.string('state').notNullable()
      t.bigInteger('at').notNullable()
    })
    await mkW(T.txs, (t) => {
      t.string('action_id').primary()
      t.string('operation_id').notNullable()
      t.string('kind').notNullable()
      t.string('txid').notNullable().unique()
      t.text('raw_hex').notNullable()
      t.text('parents').notNullable()
      t.bigInteger('fee_sats').notNullable()
      t.string('state').notNullable().index()
      t.integer('attempts').notNullable()
      t.text('last_observation')
      t.bigInteger('at').notNullable()
      /** ms the row last CHANGED state — the trailing-watch clock. `at` is
       *  creation time: an export already lived its TTL before 'expired' was
       *  written, so a creation clock can never give the verdict a window */
      t.bigInteger('state_at')
    })
    if (!(await this.wkx.schema.hasColumn(T.txs, 'state_at'))) await this.wkx.schema.table(T.txs, (t) => t.bigInteger('state_at'))
    await this.wkx(T.txs)
      .whereNull('state_at')
      .update({ state_at: this.wkx.ref('at') })
    // the machine's event log — every txStep write records (from,event,guard,
    // to,intents) so the driver's decisions replay and audit like market_events
    if (!(await this.wkx.schema.hasTable(T.txEvents)))
      await this.wkx.schema.createTable(T.txEvents, (t) => {
        t.increments('id').primary()
        t.string('action_id').notNullable().index()
        t.bigInteger('at').notNullable()
        t.string('from_state').notNullable()
        t.string('event').notNullable()
        t.string('guard')
        t.string('to_state').notNullable()
        t.text('intents').notNullable()
        // post-commit intents run after the row commits; effected flips once
        // every declared effect dispatched — a 0 row is an unfinished crash
        // window that replayTxEffects re-runs (intents are idempotent)
        t.integer('effected').notNullable().defaultTo(0)
      })
    // effect-replay bookkeeping (STATE-06): attempts/error/next_at bound the
    // replay's cadence and record WHY a wedge is wedged — an invisible
    // every-tick retry is exactly the unbounded loop this closes
    if (!(await this.wkx.schema.hasColumn(T.txEvents, 'effect_attempts')))
      await this.wkx.schema.alterTable(T.txEvents, (t) => t.integer('effect_attempts').notNullable().defaultTo(0))
    if (!(await this.wkx.schema.hasColumn(T.txEvents, 'effect_error'))) await this.wkx.schema.alterTable(T.txEvents, (t) => t.text('effect_error'))
    if (!(await this.wkx.schema.hasColumn(T.txEvents, 'effect_next_at'))) await this.wkx.schema.alterTable(T.txEvents, (t) => t.bigInteger('effect_next_at'))
  }

  // ── policies ────────────────────────────────────────────────────────────
  async approvePolicy(policy: AutomationPolicy, approvedBy: string): Promise<string> {
    const hash = policyHash(policy)
    const row = { policy_hash: hash, policy_id: policy.policyId, policy: canonicalJson(policy), approved_at: this.now(), approved_by: approvedBy }
    // onConflict-ignore left a revoked row dead while reporting 'approved' —
    // re-approving must make the policy active again
    await this.kx(T.policies).insert(row).onConflict('policy_hash').merge({ revoked_at: null, approved_at: row.approved_at, approved_by: approvedBy })
    return hash
  }

  async revokePolicy(hash: string): Promise<void> {
    await this.kx(T.policies).where({ policy_hash: hash }).whereNull('revoked_at').update({ revoked_at: this.now() })
  }

  async activePolicy(hash: string): Promise<AutomationPolicy | undefined> {
    const r = await this.kx(T.policies).where({ policy_hash: hash }).first()
    if (!r || r.revoked_at != null) return undefined
    const p = JSON.parse(r.policy) as AutomationPolicy
    if (policyError(p) || policyHash(p) !== hash) throw new Error('stored policy does not match its hash')
    const now = this.now()
    return now >= p.notBefore && now < p.notAfter ? p : undefined
  }

  async policies(): Promise<Array<{ policyHash: string; policyId: string; approvedAt: number; approvedBy: string; revokedAt?: number }>> {
    return (await this.kx(T.policies).select('*').orderBy('approved_at')).map((r) => ({
      policyHash: r.policy_hash,
      policyId: r.policy_id,
      approvedAt: Number(r.approved_at),
      approvedBy: r.approved_by,
      ...(r.revoked_at != null ? { revokedAt: Number(r.revoked_at) } : {})
    }))
  }

  // ── operations ──────────────────────────────────────────────────────────
  private op(r: any): Operation {
    return {
      operationId: r.operation_id,
      role: r.role,
      kind: r.kind,
      dedupeKey: r.dedupe_key,
      policyHash: r.policy_hash,
      network: r.network,
      state: r.state,
      revision: r.revision,
      // the one trusted decode: every write path ran checkOpData, so the
      // persisted JSON's declared fields are MarketOpData's real fields
      data: JSON.parse(r.data) as MarketOpData,
      ...(r.due_at != null ? { dueAt: Number(r.due_at) } : {}),
      ...(r.due_height != null ? { dueHeight: r.due_height } : {}),
      ...(r.next_reconcile_at != null ? { nextReconcileAt: Number(r.next_reconcile_at) } : {}),
      ...(r.last_error ? { lastError: r.last_error } : {}),
      createdAt: Number(r.created_at),
      updatedAt: Number(r.updated_at)
    }
  }

  async createOperation(o: {
    role: Operation['role']
    kind: string
    dedupeKey: string
    policyHash: string
    network: string
    state: string
    data: MarketOpData
    dueAt?: number
    dueHeight?: number
    operationId?: string
  }): Promise<{ operation: Operation; created: boolean }> {
    checkOpData(o.data)
    // nulls strip here too — same contract as the transition merge: a null
    // is a clear-marker, never a persisted literal (canonicalJson already
    // drops undefined)
    const data = Object.fromEntries(Object.entries(o.data).filter(([, v]) => v !== null))
    const r = await this.kx.transaction(async (trx) => {
      const prev = await trx(T.ops).where({ dedupe_key: o.dedupeKey }).first()
      if (prev) return { operation: this.op(prev), created: false }
      const now = this.now()
      const bound = o.dueAt === undefined ? this.residenceBound?.(o.state) : undefined
      const row = {
        operation_id: o.operationId ?? randomUUID(),
        role: o.role,
        kind: o.kind,
        dedupe_key: o.dedupeKey,
        policy_hash: o.policyHash,
        network: o.network,
        state: o.state,
        revision: 1,
        data: canonicalJson(data),
        due_at: o.dueAt ?? (bound !== undefined ? now + bound : null),
        due_height: o.dueHeight ?? null,
        proven_height: o.data.provenHeight ?? null,
        created_at: now,
        updated_at: now
      }
      await trx(T.ops).insert(row)
      await trx(T.events).insert({ operation_id: row.operation_id, at: now, from_state: null, to_state: o.state, cause: 'created', evidence: null })
      return { operation: this.op(row), created: true }
    })
    if (r.created) {
      detached(() => this.workReady?.())
      // armed = the persisted deadline — explicit dueAt or the stamped bound
      if (r.operation.dueAt !== undefined) detached(() => this.dueAtArmed?.(r.operation.operationId, r.operation.dueAt!))
    }
    return r
  }

  async operation(operationId: string): Promise<Operation | undefined> {
    const r = await this.kx(T.ops).where({ operation_id: operationId }).first()
    return r ? this.op(r) : undefined
  }

  async operationByKey(dedupeKey: string): Promise<Operation | undefined> {
    const r = await this.kx(T.ops).where({ dedupe_key: dedupeKey }).first()
    return r ? this.op(r) : undefined
  }

  async operations(q: { role?: string; states?: string[]; notStates?: string[]; reconcileDue?: number } = {}): Promise<Operation[]> {
    let b = this.kx(T.ops).select('*').orderBy('created_at')
    if (q.role) b = b.where({ role: q.role })
    if (q.states) b = b.whereIn('state', q.states)
    if (q.notStates) b = b.whereNotIn('state', q.notStates)
    // rows inside their reconcile cooldown never even hydrate — NULL is due
    // so a boot (or a row that never probed) still scans everything once
    if (q.reconcileDue !== undefined)
      b = b.where((w) => {
        w.whereNull('next_reconcile_at').orWhere('next_reconcile_at', '<=', q.reconcileDue as number)
      })
    return (await b).map((r) => this.op(r))
  }

  /** stamp the reconcile cooldown — a direct UPDATE, deliberately outside
   *  transition(): next_reconcile_at is probe metadata, not FSM state, and
   *  a journaled event per op per 30 s would flood market_events. No
   *  revision bump; callers hold no CAS on it. */
  async markReconciled(operationId: string, nextAt: number): Promise<void> {
    await this.kx(T.ops).where({ operation_id: operationId }).update({ next_reconcile_at: nextAt })
  }

  /** retention sweep — terminal-status rows past the cutoff are deleted and
   *  the WAL checkpointed so the file actually shrinks. Journals are audit:
   *  only rows whose PARENT is terminal are eligible (the caller passes the
   *  terminal sets — the store holds no opinion about which states end an
   *  op or a tx), and even those keep `beforeMs` of history. Pending/live
   *  rows are never touched. */
  async prune(beforeMs: number, terminal: { ops: string[]; txs: string[] }): Promise<{ inbox: number; outbox: number; events: number; txEvents: number }> {
    const inbox = await this.kx(T.inbox).whereIn('status', ['done', 'dead', 'duplicate']).where('received_at', '<', beforeMs).del()
    const outbox = await this.kx(T.outbox).whereIn('status', ['acked', 'dead']).where('next_at', '<', beforeMs).del()
    const events =
      terminal.ops.length === 0
        ? 0
        : await this.kx(T.events).where('at', '<', beforeMs).whereIn('operation_id', this.kx(T.ops).select('operation_id').whereIn('state', terminal.ops)).del()
    const txEvents =
      terminal.txs.length === 0
        ? 0
        : await this.wkx(T.txEvents).where('at', '<', beforeMs).whereIn('action_id', this.wkx(T.txs).select('action_id').whereIn('state', terminal.txs)).del()
    await this.kx.raw('PRAGMA wal_checkpoint(TRUNCATE)').catch(() => undefined)
    if (this.wkx !== this.kx) await this.wkx.raw('PRAGMA wal_checkpoint(TRUNCATE)').catch(() => undefined)
    return { inbox, outbox, events, txEvents }
  }

  async transition(
    operationId: string,
    expectedRevision: number,
    to: string,
    change: {
      data?: OpDataPatch
      dueAt?: number | null
      dueHeight?: number | null
      lastError?: string | null
      cause: string
      evidence?: unknown
      outbox?: Array<Omit<OutboxItem, 'attempts' | 'operationId'>>
    }
  ): Promise<Operation> {
    let fromState: string | undefined
    const op = await this.kx.transaction(async (trx) => {
      const cur = await trx(T.ops).where({ operation_id: operationId }).first()
      if (!cur) throw new Error(`unknown operation ${operationId}`)
      fromState = cur.state
      if (cur.revision !== expectedRevision) throw new RevisionConflict(`operation ${operationId}: revision ${cur.revision} != expected ${expectedRevision}`)
      // driver mode: the machine is the spec — a state-changing write must
      // resolve to a declared edge that accepts from the persisted state and
      // lands exactly on the caller's target. Data-stamp writes (same state)
      // still validate; unmodeled causes pass — the census gate owns them.
      const v = opEdgeAgrees(cur.state, to, change.cause)
      if (!v.ok)
        throw new MachineBlocked(
          `operation ${operationId}: ${cur.state} --${change.cause}--> ${to} has no machine edge (${v.event ?? 'unmodeled'}${v.guard !== undefined ? ' @' + v.guard : ''} would land ${v.machine ?? '?'})`
        )
      const now = this.now()
      // updated_at is the residence clock the stuck watchdog measures — only a
      // real state change resets it. Same-state writes (error labels, evidence,
      // data merges) are activity, not residence: they must never extend a
      // state's bound or a poisoned op loops its own watchdog forever.
      const patch: Record<string, unknown> = { state: to, revision: expectedRevision + 1, updated_at: to === cur.state ? cur.updated_at : now }
      if (change.data !== undefined) {
        const merged: Record<string, unknown> = { ...JSON.parse(cur.data) }
        // null in a patch is the clear-marker — it deletes the key outright
        // rather than persisting a literal null, so hydrated op.data fields
        // are honestly T | undefined (never a live null under a typed read)
        for (const [k, v] of Object.entries(change.data)) {
          if (v === null) delete merged[k]
          else merged[k] = v
        }
        // the codec gates the merged bag — an undeclared key or wrong-typed
        // value throws inside this transaction: transition, event row and
        // outbox intents roll back together
        checkOpData(merged)
        patch.data = canonicalJson(merged)
        // the column shadows data.provenHeight so hasLiveOps seeks instead
        // of parsing JSON — written in the same transaction, never drifts
        patch.proven_height = merged.provenHeight ?? null
      }
      if (change.dueAt !== undefined) patch.due_at = change.dueAt
      // derivable-at-commit deadline: entering a bounded state arms its
      // stuck bound at write time instead of the next advance() scan
      // noticing the age. An explicit dueAt wins (collect windows, deferral
      // resumes); a null on a state change clears the OLD state's deadline
      // and re-derives — the bound IS the new state's deadline
      if (to !== cur.state && (change.dueAt === undefined || change.dueAt === null)) {
        const bound = this.residenceBound?.(to)
        if (bound !== undefined) patch.due_at = now + bound
      }
      if (change.dueHeight !== undefined) patch.due_height = change.dueHeight
      if (change.lastError !== undefined) patch.last_error = change.lastError
      const n = await trx(T.ops).where({ operation_id: operationId, revision: expectedRevision }).update(patch)
      if (n !== 1) throw new RevisionConflict(`operation ${operationId}: concurrent update`)
      await trx(T.events).insert({
        operation_id: operationId,
        at: now,
        from_state: cur.state,
        to_state: to,
        cause: change.cause,
        evidence: change.evidence === undefined ? null : canonicalJson(change.evidence)
      })
      for (const o of change.outbox ?? []) {
        await trx(T.outbox)
          .insert({
            action_id: o.actionId,
            operation_id: operationId,
            kind: o.kind,
            destination: o.destination,
            payload: canonicalJson(o.payload),
            refs: o.refs !== undefined ? canonicalJson(o.refs) : null,
            status: 'pending',
            attempts: 0,
            next_at: now
          })
          .onConflict('action_id')
          .ignore()
      }
      return this.op({ ...cur, ...patch })
    })
    if ((change.outbox?.length ?? 0) > 0) detached(() => this.outboxReady?.())
    // a state change is work a later pass must see; same-state writes
    // (error labels, evidence, data merges) are not — they never fire
    if (fromState !== undefined && fromState !== op.state) detached(() => this.workReady?.())
    // arm whatever deadline persisted — explicit dueAt or the stamped
    // residence bound (re-arming an untouched due_at is idempotent)
    if (op.dueAt !== undefined) detached(() => this.dueAtArmed?.(op.operationId, op.dueAt!))
    return op
  }

  async events(operationId: string): Promise<Array<{ seq: number; at: number; from?: string; to: string; cause: string; evidence?: unknown }>> {
    return (await this.kx(T.events).where({ operation_id: operationId }).orderBy('seq')).map((r) => ({
      seq: r.seq,
      at: Number(r.at),
      ...(r.from_state ? { from: r.from_state } : {}),
      to: r.to_state,
      cause: r.cause,
      ...(r.evidence ? { evidence: JSON.parse(r.evidence) } : {})
    }))
  }

  /** events newer than a global seq cursor — the deal journal reads these */
  async eventsAfter(
    afterSeq: number,
    limit = 500
  ): Promise<Array<{ seq: number; operationId: string; at: number; to: string; cause: string; evidence?: unknown }>> {
    return (await this.kx(T.events).where('seq', '>', afterSeq).orderBy('seq').limit(limit)).map((r) => ({
      seq: Number(r.seq),
      operationId: r.operation_id,
      at: Number(r.at),
      to: r.to_state,
      cause: r.cause,
      ...(r.evidence ? { evidence: JSON.parse(r.evidence) } : {})
    }))
  }

  /** how many events an operation has at or below a global seq — the journal
   *  line's per-operation `n` is this + its offset inside the pending batch */
  async eventCount(operationId: string, beforeOrAtSeq: number): Promise<number> {
    const r = await this.kx(T.events).where({ operation_id: operationId }).where('seq', '<=', beforeOrAtSeq).count('* as c').first()
    return Number(r?.c ?? 0)
  }

  /** ops hydrated once for a known id set — replaces the per-row operation()
   *  lookup inside batch loops (the journal assembler's N+1) */
  async opsByIds(ids: string[]): Promise<Map<string, Operation>> {
    if (ids.length === 0) return new Map()
    const rows = await this.kx(T.ops).select('*').whereIn('operation_id', ids)
    return new Map(rows.map((r) => [r.operation_id, this.op(r)]))
  }

  /** per-op event counts at-or-below a global seq — one GROUP BY for the whole
   *  batch instead of an eventCount() per row */
  async eventCountsByOp(ids: string[], atOrBelowSeq: number): Promise<Map<string, number>> {
    if (ids.length === 0) return new Map()
    const rows = await this.kx(T.events)
      .select('operation_id')
      .count('* as c')
      .where('seq', '<=', atOrBelowSeq)
      .whereIn('operation_id', ids)
      .groupBy('operation_id')
    return new Map(rows.map((r) => [r.operation_id as string, Number(r.c)]))
  }

  /** a live op exists — the hot-loop check as one LIMIT 1 row instead of
   *  hydrating every nonterminal op per pass. The completion_submitted
   *  carve-out mirrors the runtime rule: a settled proof awaiting block
   *  depth is not live work. proven_height is the materialized column
   *  transition() stamps from data.provenHeight — the probe never parses
   *  the JSON payload. */
  async hasLiveOps(states: string[]): Promise<boolean> {
    if (states.length === 0) return false
    const r = await this.kx(T.ops)
      .select(this.kx.raw('1'))
      .whereIn('state', states)
      .andWhere((b) => {
        b.where('state', '<>', 'completion_submitted').orWhereNull('proven_height')
      })
      .first()
    return r !== undefined
  }

  /** a nonterminal op whose due_height is at/below tip — the onHeight wake as
   *  one LIMIT 1 row instead of a full ops hydration per block */
  async hasDueAtHeight(states: string[], tip: number): Promise<boolean> {
    if (states.length === 0) return false
    const r = await this.kx(T.ops).select(this.kx.raw('1')).whereIn('state', states).where('due_height', '<=', tip).first()
    return r !== undefined
  }

  // ── inbox + cursors ─────────────────────────────────────────────────────
  async cursor(source: string): Promise<string | undefined> {
    return (await this.kx(T.cursors).where({ source }).first())?.cursor
  }

  async ingest(
    source: string,
    items: Array<{ messageId: string; artifactHash: string; kind: string; payload: unknown }>,
    cursor: string
  ): Promise<{ accepted: number; duplicates: number }> {
    const r = await this.kx.transaction(async (trx) => {
      let accepted = 0,
        duplicates = 0
      const now = this.now()
      for (const i of items) {
        if (await trx(T.inbox).where({ source, message_id: i.messageId }).first()) {
          duplicates++
          continue
        }
        const seen = await trx(T.inbox).where({ artifact_hash: i.artifactHash, kind: i.kind }).first()
        await trx(T.inbox).insert({
          source,
          message_id: i.messageId,
          artifact_hash: i.artifactHash,
          kind: i.kind,
          payload: canonicalJson(i.payload),
          status: seen ? 'duplicate' : 'pending',
          attempts: 0,
          received_at: now
        })
        if (seen) duplicates++
        else accepted++
      }
      await trx(T.cursors).insert({ source, cursor, updated_at: now }).onConflict('source').merge()
      return { accepted, duplicates }
    })
    if (r.accepted > 0) detached(() => this.workReady?.())
    return r
  }

  async pendingInbox(limit = 50, maxAttempts = 8): Promise<InboxItem[]> {
    return (await this.kx(T.inbox).where({ status: 'pending' }).where('attempts', '<', maxAttempts).orderBy('received_at').limit(limit)).map((r) => ({
      source: r.source,
      messageId: r.message_id,
      artifactHash: r.artifact_hash,
      kind: r.kind,
      payload: JSON.parse(r.payload),
      attempts: r.attempts
    }))
  }

  /** inbox rows seen for these message ids, any status — the delivery-dedup
   *  diagnostic: a message ingested twice shows up as two rows */
  async inboxCount(messageIds: string[]): Promise<number> {
    if (messageIds.length === 0) return 0
    const r = await this.kx(T.inbox).whereIn('message_id', messageIds).count('* as n').first()
    return Number((r as { n: number } | undefined)?.n ?? 0)
  }

  async completeInbox(source: string, messageId: string): Promise<void> {
    await this.kx(T.inbox).where({ source, message_id: messageId, status: 'pending' }).update({ status: 'done' })
  }

  async failInbox(source: string, messageId: string, error: string, maxAttempts = 8): Promise<void> {
    await this.kx.transaction(async (trx) => {
      const r = await trx(T.inbox).where({ source, message_id: messageId }).first()
      if (!r || r.status !== 'pending') return
      const attempts = r.attempts + 1
      await trx(T.inbox)
        .where({ source, message_id: messageId })
        .update({ attempts, last_error: error.slice(0, 500), status: attempts >= maxAttempts ? 'dead' : 'pending' })
    })
  }

  // ── outbox ──────────────────────────────────────────────────────────────
  async enqueue(o: Omit<OutboxItem, 'attempts'>, opts?: { delayMs?: number }): Promise<boolean> {
    const added = await this.kx.transaction(async (trx) => {
      if (await trx(T.outbox).where({ action_id: o.actionId }).first()) return false
      await trx(T.outbox).insert({
        action_id: o.actionId,
        operation_id: o.operationId,
        kind: o.kind,
        destination: o.destination,
        payload: canonicalJson(o.payload),
        refs: o.refs !== undefined ? canonicalJson(o.refs) : null,
        status: 'pending',
        attempts: 0,
        next_at: this.now() + (opts?.delayMs ?? 0)
      })
      return true
    })
    if (added) detached(() => this.outboxReady?.())
    return added
  }

  async dueOutbox(limit = 50): Promise<OutboxItem[]> {
    return (await this.kx(T.outbox).where({ status: 'pending' }).where('next_at', '<=', this.now()).orderBy('next_at').limit(limit)).map((r) => ({
      actionId: r.action_id,
      operationId: r.operation_id,
      kind: r.kind,
      destination: r.destination,
      payload: JSON.parse(r.payload),
      ...(r.refs != null ? { refs: JSON.parse(r.refs) } : {}),
      attempts: r.attempts,
      nextAt: Number(r.next_at)
    }))
  }

  /** earliest pending outbox wake — the pump sleeps until this, so a
   *  deferred row fires at its next_at rather than at the next tick */
  async nextOutboxAt(opts?: { excludeIds?: readonly string[] }): Promise<number | undefined> {
    let q = this.kx(T.outbox).where({ status: 'pending' })
    if (opts?.excludeIds !== undefined && opts.excludeIds.length > 0) q = q.whereNotIn('action_id', opts.excludeIds)
    const r = await q.min('next_at as n').first()
    return r?.n === null || r?.n === undefined ? undefined : Number(r.n)
  }

  /** the outbox rows an operation is sitting behind — the `deals` detail
   *  "waiting on" line: pending rows say what it waits for, dead rows say
   *  why it stopped. deferClass is the machine-readable cause, lastError
   *  the plain one. */
  async waitingOn(operationId?: string): Promise<
    Array<{
      actionId: string
      operationId: string
      kind: string
      status: string
      deferClass?: DeferClass
      lastError?: string
      nextAt: number
      attempts: number
    }>
  > {
    const q = this.kx(T.outbox).whereNot({ status: 'acked' }).orderBy('next_at')
    if (operationId !== undefined) void q.where({ operation_id: operationId })
    return (await q).map((r) => ({
      actionId: r.action_id,
      operationId: r.operation_id,
      kind: r.kind,
      status: r.status,
      ...(r.defer_class ? { deferClass: r.defer_class as DeferClass } : {}),
      ...(r.last_error ? { lastError: r.last_error } : {}),
      nextAt: Number(r.next_at),
      attempts: r.attempts
    }))
  }

  async ackOutbox(actionId: string): Promise<void> {
    await this.kx(T.outbox).where({ action_id: actionId }).update({ status: 'acked' })
  }

  /** one row's delivery guard — the transport object reads it before
   *  delivering: an acked/dead/re-scheduled row is skipped, so a replayed
   *  or raced drain can't double-send. */
  async outboxRow(actionId: string): Promise<{ status: string; nextAt: number } | undefined> {
    const r = await this.kx(T.outbox).where({ action_id: actionId }).first()
    return r === undefined ? undefined : { status: r.status, nextAt: Number(r.next_at) }
  }

  /** pull one deferred row forward to now — used when the wait's cause
   *  resolved early (a pending fan-out leg that settled missed). Attempts
   *  are untouched: this never hastens a dead-letter. */
  async pullOutbox(actionId: string): Promise<unknown> {
    const n = await this.kx(T.outbox).where({ action_id: actionId, status: 'pending' }).where('next_at', '>', this.now()).update({ next_at: this.now() })
    if (n > 0) detached(() => this.outboxReady?.())
    return n
  }

  /** pull every due-future row of one defer class forward to now — used when
   *  an event (a proven funding-tree topup, a host coming back) removes the
   *  cause the backoff was waiting on. Class, not error text: a message that
   *  happens to contain the keyword can't pull an unrelated row forward.
   *  Attempts are untouched: this never hastens a dead-letter. */
  async unblockOutbox(cls: DeferClass): Promise<number> {
    const n = await this.kx(T.outbox).where({ status: 'pending' }).where('defer_class', cls).where('next_at', '>', this.now()).update({ next_at: this.now() })
    if (n > 0) detached(() => this.outboxReady?.())
    return n
  }

  /** a resource-gated defer — the cause is starvation (a funding slot, not
   *  a delivery failure), so attempts are NOT consumed toward dead-letter.
   *  Short fixed re-check; the real wake-up is unblockOutbox on a freeing
   *  event (topup proven → slots appear). */
  async deferOutbox(actionId: string, error: string, checkInMs = WAITS.deferCheck.ms, cls: DeferClass = 'funding-slot'): Promise<void> {
    const n = await this.kx(T.outbox)
      .where({ action_id: actionId, status: 'pending' })
      .update({ last_error: error.slice(0, 500), defer_class: cls, next_at: this.now() + checkInMs })
    // the defer MOVED the wake instant — tell the pump: the drain re-arms
    // on the post-defer next_at, a fresh instant its drain:${next} key can
    // never collide with (the arm dedups against the invocation that was
    // itself armed under that instant — rt/drain-arm-dedup). Without this
    // poke a deferred row had no wake path at all once the arm chain broke.
    if (n > 0) detached(() => this.outboxReady?.())
  }

  async retryOutbox(actionId: string, error: string, maxAttempts = 10, cls?: DeferClass): Promise<void> {
    const moved = await this.kx.transaction(async (trx) => {
      const r = await trx(T.outbox).where({ action_id: actionId }).first()
      if (!r || r.status !== 'pending') return 0
      const attempts = r.attempts + 1
      await trx(T.outbox)
        .where({ action_id: actionId })
        .update({
          attempts,
          last_error: error.slice(0, 500),
          defer_class: cls ?? null,
          status: attempts >= maxAttempts ? 'dead' : 'pending',
          // deterministic per-row jitter — N rows failing on the same host
          // flap retry in lockstep without it (phase-aligned cadence);
          // stable01 keeps the spread replayable and restart-stable
          next_at: this.now() + spread(Math.min(WAITS.outboxRetryCap.ms, WAITS.outboxRetryBase.ms * 2 ** attempts), stable01(actionId))
        })
      return 1
    })
    // same moved-wake poke as deferOutbox — a rescheduled row is new work
    if (moved > 0) detached(() => this.outboxReady?.())
  }

  // ── budget ──────────────────────────────────────────────────────────────
  async reserveBudget(ctx: ActionContext, a: BudgetAmounts, humanApproved = false): Promise<BudgetResult> {
    if (![a.purchaseSats, a.feeSats, a.lockedSats].every(nat)) return { ok: false, code: 'invalid_amount' }
    return await this.kx.transaction(async (trx) => {
      const prev = await trx(T.budget).where({ action_id: ctx.actionId }).first()
      if (prev) {
        if (
          prev.policy_hash !== ctx.policyHash ||
          Number(prev.purchase_sats) !== a.purchaseSats ||
          Number(prev.fee_sats) !== a.feeSats ||
          Number(prev.locked_sats) !== a.lockedSats
        )
          return { ok: false, code: 'action_reused_with_different_terms' }
        return prev.state === 'released' ? { ok: false, code: 'action_released' } : { ok: true, existing: true }
      }
      const pr = await trx(T.policies).where({ policy_hash: ctx.policyHash }).first()
      if (!pr) return { ok: false, code: 'unknown_policy' }
      if (pr.revoked_at != null) return { ok: false, code: 'policy_revoked' }
      const p = JSON.parse(pr.policy) as AutomationPolicy
      const now = this.now()
      if (policyError(p) || policyHash(p) !== ctx.policyHash) return { ok: false, code: 'invalid_policy' }
      if (p.network !== ctx.networkId) return { ok: false, code: 'wrong_network' }
      if (now < p.notBefore || now >= p.notAfter) return { ok: false, code: 'policy_not_valid_now' }
      const day = dayOf(now)
      if (humanApproved) {
        await trx(T.budget).insert({
          action_id: ctx.actionId,
          operation_id: ctx.operationId,
          policy_hash: ctx.policyHash,
          day,
          purchase_sats: a.purchaseSats,
          fee_sats: a.feeSats,
          locked_sats: a.lockedSats,
          state: 'reserved',
          at: now
        })
        return { ok: true, existing: false }
      }
      const L = p.limits
      if (a.purchaseSats > L.perPurchaseSats) return { ok: false, code: 'price_above_policy' }
      if (a.feeSats > L.perTxFeeSats) return { ok: false, code: 'fee_above_policy' }
      const live = () => trx(T.budget).where({ policy_hash: ctx.policyHash }).whereNot({ state: 'released' })
      if ((await sum(live().where({ day }), 'purchase_sats')) + a.purchaseSats > L.dailyPurchaseSats) return { ok: false, code: 'daily_purchase_cap' }
      if ((await sum(live().where({ day }), 'fee_sats')) + a.feeSats > L.dailyFeeSats) return { ok: false, code: 'daily_fee_cap' }
      if ((await sum(live(), 'purchase_sats')) + (await sum(live(), 'fee_sats')) + a.purchaseSats + a.feeSats > L.runTotalSats)
        return { ok: false, code: 'run_total_cap' }
      if ((await sum(live().whereIn('state', ['reserved', 'committed']), 'locked_sats')) + a.lockedSats > L.maxLockedSats)
        return { ok: false, code: 'locked_capital_cap' }
      await trx(T.budget).insert({
        action_id: ctx.actionId,
        operation_id: ctx.operationId,
        policy_hash: ctx.policyHash,
        day,
        purchase_sats: a.purchaseSats,
        fee_sats: a.feeSats,
        locked_sats: a.lockedSats,
        state: 'reserved',
        at: now
      })
      return { ok: true, existing: false }
    })
  }

  private async moveBudget(actionId: string, from: string[], to: string): Promise<boolean> {
    return (await this.kx(T.budget).where({ action_id: actionId }).whereIn('state', from).update({ state: to })) === 1
  }

  commitBudget(actionId: string): Promise<boolean> {
    return this.moveBudget(actionId, ['reserved'], 'committed')
  }
  settleBudget(actionId: string): Promise<boolean> {
    return this.moveBudget(actionId, ['reserved', 'committed'], 'settled')
  }
  /** frees a reservation that never settled: 'reserved' (never broadcast) or
   *  'committed' (the broadcast was later rejected — the sats never left) */
  releaseBudget(actionId: string): Promise<boolean> {
    return this.moveBudget(actionId, ['reserved', 'committed'], 'released')
  }

  /** a refunded purchase returned its sats: the purchase amount drops out of
   *  every cap sum (a seller who takes escrow and disappears must not burn the
   *  buyer's daily purchase cap), while the funding fee stays counted — the
   *  fee was really spent */
  async refundBudget(actionId: string): Promise<boolean> {
    return (
      (await this.kx(T.budget).where({ action_id: actionId }).whereIn('state', ['reserved', 'committed']).update({ purchase_sats: 0, state: 'settled' })) === 1
    )
  }

  async budgetStatus(hash: string): Promise<{ day: string; purchaseTodaySats: number; feeTodaySats: number; runTotalSats: number; lockedSats: number }> {
    const live = () => this.kx(T.budget).where({ policy_hash: hash }).whereNot({ state: 'released' })
    const day = dayOf(this.now())
    return {
      day,
      purchaseTodaySats: await sum(live().where({ day }), 'purchase_sats'),
      feeTodaySats: await sum(live().where({ day }), 'fee_sats'),
      runTotalSats: (await sum(live(), 'purchase_sats')) + (await sum(live(), 'fee_sats')),
      lockedSats: await sum(live().whereIn('state', ['reserved', 'committed']), 'locked_sats')
    }
  }

  // ── wallet leases + transactions ────────────────────────────────────────
  /** a lease row is a live claim only while its action's tx is unaired —
   *  prepared, unknown-outcome, or inside the counterparty's export window.
   *  Broadcast-and-closed claims are stale bookkeeping; a 'spent' row keeps
   *  blocking because its outpoint is provably consumed. take()'s unusable()
   *  and prepare()'s conflict check MUST agree — a stale lease take() passes
   *  but prepare() refuses was a real terminal funding_failed on testnet. */
  private leaseLiveRow(lease: { action_id: string; state: string }, w: { state: string } | undefined): boolean {
    // released is never live — without this arm a released row falls through
    // to the tx-state check and a 'released'+live-tx (or no-tx) row blocks
    // prepare() while take() frees it: the take↔prepare divergence this join
    // exists to close, reopened on the released arm
    if (lease.state === 'released') return false
    if (lease.state === 'spent') return true
    return w === undefined || w.state === 'prepared' || w.state === 'submission_unknown' || w.state === 'exported'
  }

  /** resolve the tx rows a batch of lease rows need — one whereIn for the
   *  whole batch instead of one query per outpoint */
  private async leaseTxRows(q: Knex | Knex.Transaction, leases: Array<{ action_id: string; state: string }>): Promise<Map<string, { state: string }>> {
    const ids = [...new Set(leases.filter((l) => l.state !== 'released' && l.state !== 'spent').map((l) => l.action_id))]
    const rows = ids.length ? await q(T.txs).whereIn('action_id', ids).select('action_id', 'state') : []
    return new Map(rows.map((w) => [w.action_id, w]))
  }

  private async leaseLive(q: Knex | Knex.Transaction, lease: { action_id: string; state: string }): Promise<boolean> {
    const rows = await this.leaseTxRows(q, [lease])
    return this.leaseLiveRow(lease, rows.get(lease.action_id))
  }

  /** the ONE lease-claim rule: claim these outpoints for actionId unless a
   *  foreign LIVE lease holds one. Every writer that takes outpoints —
   *  leaseOutpoints, recordPrepared, recordBroadcast — goes through this so
   *  the conflict check and the upsert can never drift apart. */
  private async claimLeases(trx: Knex.Transaction, actionId: string, outs: Array<{ txid: string; vout: number; satoshis: number }>): Promise<boolean> {
    const held = await trx(T.leases).whereIn(
      'outpoint',
      outs.map((o) => `${o.txid}:${o.vout}`)
    )
    const foreign = held.filter((r) => r.action_id !== actionId)
    const txRows = await this.leaseTxRows(trx, foreign)
    if (foreign.some((r) => this.leaseLiveRow(r, txRows.get(r.action_id)))) return false
    const now = this.now()
    for (const o of outs)
      await trx(T.leases)
        .insert({ outpoint: `${o.txid}:${o.vout}`, action_id: actionId, satoshis: o.satoshis, state: 'leased', at: now })
        .onConflict('outpoint')
        .merge()
    return true
  }

  /** TEST SEAM — production never claims bare outpoints: every real claim
   *  rides claimLeases inside the row's write transaction (recordPrepared,
   *  recordBroadcast) so lease+record commit or fail together. This stays
   *  public because the claim rule's tests exercise it directly. */
  async leaseOutpoints(actionId: string, outs: Array<{ txid: string; vout: number; satoshis: number }>): Promise<boolean> {
    return await this.wkx.transaction((trx) => this.claimLeases(trx, actionId, outs))
  }

  async leaseState(txid: string, vout: number): Promise<{ actionId: string; state: string } | undefined> {
    const r = await this.wkx(T.leases)
      .where({ outpoint: `${txid}:${vout}` })
      .first()
    return r ? { actionId: r.action_id, state: r.state } : undefined
  }

  private wtx(r: any): WalletTx {
    return {
      actionId: r.action_id,
      operationId: r.operation_id,
      kind: r.kind,
      txid: r.txid,
      rawHex: r.raw_hex,
      parents: JSON.parse(r.parents),
      feeSats: Number(r.fee_sats),
      state: r.state,
      attempts: r.attempts,
      at: Number(r.at),
      stateAt: Number(r.state_at ?? r.at),
      ...(r.last_observation ? { lastObservation: JSON.parse(r.last_observation) } : {})
    }
  }

  async recordPrepared(
    t: Omit<WalletTx, 'state' | 'attempts' | 'lastObservation' | 'at' | 'stateAt'>,
    leaseOuts?: Array<{ txid: string; vout: number; satoshis: number }>
  ): Promise<WalletTx> {
    return await this.wkx.transaction(async (trx) => {
      const prev = await trx(T.txs).where({ action_id: t.actionId }).first()
      if (prev) {
        if (prev.txid !== t.txid) throw new Error(`action ${t.actionId} already prepared a different transaction (${prev.txid})`)
        return this.wtx(prev)
      }
      // lease+record must commit together: a crash between a committed lease
      // and a failed record leaves 'leased'-with-no-tx — a wedge take() frees
      // but prepare() refuses forever. Same conflict rule as leaseOutpoints.
      if (leaseOuts !== undefined && !(await this.claimLeases(trx, t.actionId, leaseOuts)))
        throw new Error('prepare: an input is already leased to another action')
      const row = {
        action_id: t.actionId,
        operation_id: t.operationId,
        kind: t.kind,
        txid: t.txid,
        raw_hex: t.rawHex,
        parents: JSON.stringify(t.parents),
        fee_sats: t.feeSats,
        state: 'prepared',
        attempts: 0,
        at: this.now(),
        state_at: this.now()
      }
      await trx(T.txs).insert(row)
      return this.wtx(row)
    })
  }

  async walletTx(q: { actionId?: string; txid?: string }): Promise<WalletTx | undefined> {
    const r = await this.wkx(T.txs)
      .where(q.actionId ? { action_id: q.actionId } : { txid: q.txid })
      .first()
    return r ? this.wtx(r) : undefined
  }

  async leasedOutpoint(txid: string, vout: number): Promise<boolean> {
    const r = await this.leaseState(txid, vout)
    if (r === undefined || r.state === 'released') return false
    return await this.leaseLive(this.wkx, { action_id: r.actionId, state: r.state })
  }

  async recordBroadcast(
    t: { txid: string; rawHex: string; parents: Record<string, string>; feeSats: number; outs: Array<{ txid: string; vout: number; satoshis: number }> },
    actionId: string,
    operationId: string,
    kind: string,
    state: TxState = 'prepared'
  ): Promise<void> {
    await this.wkx.transaction(async (trx) => {
      if (!(await this.claimLeases(trx, actionId, t.outs)))
        throw new LeaseConflictError(`recordBroadcast: an input of ${t.txid} is already leased to another live action`)
      const now = this.now()
      await trx(T.txs)
        .insert({
          action_id: actionId,
          operation_id: operationId,
          kind,
          txid: t.txid,
          raw_hex: t.rawHex,
          parents: JSON.stringify(t.parents),
          fee_sats: t.feeSats,
          state,
          attempts: 0,
          at: now,
          state_at: now
        })
        .onConflict('action_id')
        .ignore()
    })
  }

  async walletTxs(states: TxState[]): Promise<WalletTx[]> {
    return (await this.wkx(T.txs).whereIn('state', states).orderBy('at')).map((r) => this.wtx(r))
  }

  async setTxState(actionId: string, state: TxState, observation?: unknown, attempt = false): Promise<void> {
    await this.wkx.transaction(async (trx) => {
      const r = await trx(T.txs).where({ action_id: actionId }).first()
      if (!r) throw new Error(`unknown wallet action ${actionId}`)
      await trx(T.txs)
        .where({ action_id: actionId })
        .update({
          state,
          attempts: r.attempts + (attempt ? 1 : 0),
          ...(state !== r.state ? { state_at: this.now() } : {}),
          ...(observation !== undefined ? { last_observation: canonicalJson(observation) } : {})
        })
      // setTxState is a test-arrangement write — production code may not
      // call it (the check:arch tx-state-direct-write gate). The
      // 'confirmed ⇒ leases spent' side effect stays so arranged rows
      // behave like a committed obs.proven transition
      if (state === 'confirmed') await trx(T.leases).where({ action_id: actionId }).update({ state: 'spent' })
    })
  }

  async releaseLeases(actionId: string): Promise<void> {
    await this.wkx(T.leases).where({ action_id: actionId, state: 'leased' }).update({ state: 'released' })
  }

  /**
   * The tx.machine write path: evaluate (row.state, event) → in ONE
   * transaction write the state, the event row, and the in-tx intents
   * (lease marks). Returns the step result — post-commit effects are the
   * caller's (emitTxEvent). A rejected step writes nothing: the event log
   * only ever holds real, accepted transitions.
   */
  async applyTxEvent(
    actionId: string,
    event: string,
    payload: { guard?: string; observation?: unknown; attempt?: boolean } = {}
  ): Promise<{ accepted: boolean; next: TxState; intents: string[]; eventId?: number }> {
    const r = await this.wkx.transaction(async (trx) => {
      const cur = await trx(T.txs).where({ action_id: actionId }).first()
      if (!cur) throw new Error(`unknown wallet action ${actionId}`)
      const s = txStep(cur.state, event, payload.guard !== undefined ? { guard: payload.guard } : {})
      if (!s.accepted) return { accepted: false, next: cur.state as TxState, intents: [] }
      const now = this.now()
      await trx(T.txs)
        .where({ action_id: actionId })
        .update({
          state: s.next,
          attempts: cur.attempts + (payload.attempt === true ? 1 : 0),
          ...(s.next !== cur.state ? { state_at: now } : {}),
          ...(payload.observation !== undefined ? { last_observation: canonicalJson(payload.observation) } : {})
        })
      const [eventId] = await trx(T.txEvents).insert({
        action_id: actionId,
        at: now,
        from_state: cur.state,
        event,
        guard: payload.guard ?? null,
        to_state: s.next,
        intents: JSON.stringify(s.intents)
      })
      for (const i of s.intents) {
        if (i === 'release-leases') await trx(T.leases).where({ action_id: actionId, state: 'leased' }).update({ state: 'released' })
        else if (i === 'leases->spent') await trx(T.leases).where({ action_id: actionId }).update({ state: 'spent' })
      }
      return { accepted: true, next: s.next as TxState, intents: s.intents, eventId }
    })
    // an accepted transition commits intents — the wtx leg's replay head
    // retries any that went uneffected; the wake makes that immediate
    if (r.accepted) detached(() => this.workReady?.())
    return r
  }

  /** mark an event's post-commit intents dispatched — called only after the
   *  effect loop finishes clean */
  async markTxEffects(eventId: number): Promise<void> {
    await this.wkx(T.txEvents).where({ id: eventId }).update({ effected: 1 })
  }

  /** committed transitions whose post-commit intents never finished — the
   *  crash-between-commit-and-act window, replayed when the row's backoff
   *  says so (effect_next_at spaces retries so a deterministic failure is a
   *  surfaced wedge, not a hot loop) */
  async pendingTxEffects(): Promise<
    Array<{ eventId: number; actionId: string; fromState: TxState; intents: string[]; tx?: WalletTx; attempts: number; lastError?: string }>
  > {
    const now = this.now()
    const rows = await this.wkx(T.txEvents)
      .where({ effected: 0 })
      .where((q) => {
        q.whereNull('effect_next_at').orWhere('effect_next_at', '<=', now)
      })
      .orderBy('id')
    const out: Array<{ eventId: number; actionId: string; fromState: TxState; intents: string[]; tx?: WalletTx; attempts: number; lastError?: string }> = []
    for (const r of rows) {
      const w = await this.walletTx({ actionId: r.action_id })
      out.push({
        eventId: Number(r.id),
        actionId: r.action_id,
        fromState: r.from_state as TxState,
        intents: JSON.parse(r.intents),
        tx: w,
        attempts: Number(r.effect_attempts ?? 0),
        ...(r.effect_error != null ? { lastError: r.effect_error } : {})
      })
    }
    return out
  }

  /** record a failed effect replay — count it, keep the error, and push the
   *  next eligible replay out on a capped exponential back-off (1min → 1h).
   *  The wedge stays retriable forever (a custody effect may not be lost) but
   *  is no longer an unbounded every-tick hammer, and the row now says why. */
  async recordTxEffectFailure(eventId: number, error: string): Promise<void> {
    const cur = await this.wkx(T.txEvents).where({ id: eventId }).first('effect_attempts')
    const attempts = Number(cur?.effect_attempts ?? 0) + 1
    const nextAt =
      this.now() + spread(Math.min(WAITS.txEffectRetryCap.ms, WAITS.txEffectRetryBase.ms * 2 ** Math.min(attempts - 1, 10)), stable01(String(eventId)))
    await this.wkx(T.txEvents)
      .where({ id: eventId })
      .update({ effect_attempts: attempts, effect_error: error.slice(0, 500), effect_next_at: nextAt })
  }

  /** the machine's persisted event log for a wallet action */
  async walletTxEvents(actionId: string): Promise<Array<{ at: number; from: string; event: string; guard?: string; to: string; intents: string[] }>> {
    return (await this.wkx(T.txEvents).where({ action_id: actionId }).orderBy('at')).map((r) => ({
      at: Number(r.at),
      from: r.from_state,
      event: r.event,
      guard: r.guard ?? undefined,
      to: r.to_state,
      intents: JSON.parse(r.intents)
    }))
  }

  // ── approvals ───────────────────────────────────────────────────────────
  private appr(r: any): Approval {
    return {
      approvalId: r.approval_id,
      operationId: r.operation_id,
      reason: r.reason,
      policyHash: r.policy_hash,
      actionDigest: r.action_digest,
      maxExtraSats: Number(r.max_extra_sats),
      expiresAt: Number(r.expires_at),
      status: r.status,
      evidence: r.evidence ? JSON.parse(r.evidence) : undefined,
      ...(r.decided_by ? { decidedBy: r.decided_by } : {})
    }
  }

  async requestApproval(a: {
    operationId: string
    reason: string
    policyHash: string
    actionDigest: string
    maxExtraSats: number
    expiresAt: number
    evidence?: unknown
  }): Promise<Approval> {
    return await this.kx.transaction(async (trx) => {
      const now = this.now()
      const prev = await trx(T.approvals)
        .where({ operation_id: a.operationId, action_digest: a.actionDigest })
        .whereIn('status', ['pending', 'approved'])
        .first()
      if (prev) {
        if (Number(prev.expires_at) > now) return this.appr(prev)
        // a lapsed row must not shadow a fresh request (STATE-08/12): pending
        // rows were never decided — mark the lapse so pending listings stop
        // reporting it; an approved row keeps its status (the decision was
        // real — consumers gate on expiresAt at spend time)
        if (prev.status === 'pending') await trx(T.approvals).where({ approval_id: prev.approval_id }).update({ status: 'expired', decided_at: now })
      }
      const row = {
        approval_id: randomUUID(),
        operation_id: a.operationId,
        reason: a.reason,
        policy_hash: a.policyHash,
        action_digest: a.actionDigest,
        max_extra_sats: a.maxExtraSats,
        expires_at: a.expiresAt,
        status: 'pending',
        evidence: a.evidence === undefined ? null : canonicalJson(a.evidence),
        created_at: this.now()
      }
      await trx(T.approvals).insert(row)
      return this.appr(row)
    })
  }

  async approvals(status?: Approval['status']): Promise<Approval[]> {
    // lazy expiry: undecided rows past their horizon are 'expired', not
    // 'pending' — a stale row must stop shadowing new requests and stop
    // reporting itself as awaiting a decision (STATE-08/12's wedge)
    await this.kx(T.approvals).where({ status: 'pending' }).where('expires_at', '<=', this.now()).update({ status: 'expired', decided_at: this.now() })
    const b = this.kx(T.approvals).select('*').orderBy('created_at')
    return (await (status ? b.where({ status }) : b)).map((r) => this.appr(r))
  }

  async decideApproval(approvalId: string, approve: boolean, decidedBy: string): Promise<Approval> {
    return await this.kx.transaction(async (trx) => {
      const r = await trx(T.approvals).where({ approval_id: approvalId }).first()
      if (!r) throw new Error(`unknown approval ${approvalId}`)
      if (r.status !== 'pending') throw new Error(`approval ${approvalId} is already ${r.status}`)
      if (Number(r.expires_at) <= this.now()) throw new Error(`approval ${approvalId} expired`)
      const patch = { status: approve ? 'approved' : 'denied', decided_by: decidedBy, decided_at: this.now() }
      await trx(T.approvals).where({ approval_id: approvalId }).update(patch)
      return this.appr({ ...r, ...patch })
    })
  }

  async consumeApproval(operationId: string, actionDigest: string): Promise<boolean> {
    return (
      (await this.kx(T.approvals)
        .where({ operation_id: operationId, action_digest: actionDigest, status: 'approved' })
        .where('expires_at', '>', this.now())
        .update({ status: 'consumed' })) >= 1
    )
  }

  /** an escalated operation resolved on its own — the pending human decision
   *  is no longer needed and must stop being listed as open */
  async cancelApproval(approvalId: string): Promise<boolean> {
    return (await this.kx(T.approvals).where({ approval_id: approvalId }).whereIn('status', ['pending', 'approved']).update({ status: 'canceled' })) >= 1
  }
}
