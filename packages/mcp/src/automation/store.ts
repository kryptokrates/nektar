import type { Knex } from 'knex'
import { randomUUID } from 'node:crypto'
import { policyHash, policyError, canonicalJson, type AutomationPolicy, type ActionContext } from './policy.js'

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
  txs: 'wallet_transactions'
} as const

export interface Operation {
  operationId: string
  role: 'buyer' | 'seller' | 'observer'
  kind: string
  dedupeKey: string
  policyHash: string
  network: string
  state: string
  revision: number
  data: Record<string, unknown>
  dueAt?: number
  dueHeight?: number
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
}
export interface BudgetAmounts {
  purchaseSats: number
  feeSats: number
  lockedSats: number
}
export type BudgetResult = { ok: true; existing: boolean } | { ok: false; code: string }
export type TxState = 'prepared' | 'exported' | 'submission_unknown' | 'submitted' | 'observed' | 'confirmed' | 'rejected' | 'conflicted'
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
  status: 'pending' | 'approved' | 'denied' | 'consumed' | 'canceled'
  evidence: unknown
  decidedBy?: string
}

const nat = (n: unknown): n is number => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0
const dayOf = (ms: number) => new Date(ms).toISOString().slice(0, 10)
const sum = async (q: Knex.QueryBuilder, col: string): Promise<number> => Number((await q.sum({ s: col }).first())?.s ?? 0)

export class RevisionConflict extends Error {}

export class AutomationStore {
  constructor(
    private kx: Knex,
    private now: () => number = Date.now
  ) {}

  async close(): Promise<void> {
    await this.kx.destroy()
  }

  async init(): Promise<void> {
    const mk = async (name: string, build: (t: Knex.CreateTableBuilder) => void) => {
      if (!(await this.kx.schema.hasTable(name))) await this.kx.schema.createTable(name, build)
    }
    await mk(T.ops, (t) => {
      t.string('operation_id').primary()
      t.string('role').notNullable()
      t.string('kind').notNullable()
      t.string('dedupe_key').notNullable().unique()
      t.string('policy_hash').notNullable()
      t.string('network').notNullable()
      t.string('state').notNullable()
      t.integer('revision').notNullable()
      t.text('data').notNullable()
      t.bigInteger('due_at')
      t.integer('due_height')
      t.text('last_error')
      t.bigInteger('created_at').notNullable()
      t.bigInteger('updated_at').notNullable()
    })
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
    })
    if (!(await this.kx.schema.hasColumn(T.outbox, 'refs'))) await this.kx.schema.alterTable(T.outbox, (t) => t.text('refs'))
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
    await mk(T.leases, (t) => {
      t.string('outpoint').primary()
      t.string('action_id').notNullable().index()
      t.bigInteger('satoshis').notNullable()
      t.string('state').notNullable()
      t.bigInteger('at').notNullable()
    })
    await mk(T.txs, (t) => {
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
    })
  }

  // ── policies ────────────────────────────────────────────────────────────
  async approvePolicy(policy: AutomationPolicy, approvedBy: string): Promise<string> {
    const hash = policyHash(policy)
    const row = { policy_hash: hash, policy_id: policy.policyId, policy: canonicalJson(policy), approved_at: this.now(), approved_by: approvedBy }
    await this.kx(T.policies).insert(row).onConflict('policy_hash').ignore()
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
      data: JSON.parse(r.data),
      ...(r.due_at != null ? { dueAt: Number(r.due_at) } : {}),
      ...(r.due_height != null ? { dueHeight: r.due_height } : {}),
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
    data: Record<string, unknown>
    dueAt?: number
    dueHeight?: number
    operationId?: string
  }): Promise<{ operation: Operation; created: boolean }> {
    return await this.kx.transaction(async (trx) => {
      const prev = await trx(T.ops).where({ dedupe_key: o.dedupeKey }).first()
      if (prev) return { operation: this.op(prev), created: false }
      const now = this.now()
      const row = {
        operation_id: o.operationId ?? randomUUID(),
        role: o.role,
        kind: o.kind,
        dedupe_key: o.dedupeKey,
        policy_hash: o.policyHash,
        network: o.network,
        state: o.state,
        revision: 1,
        data: canonicalJson(o.data),
        due_at: o.dueAt ?? null,
        due_height: o.dueHeight ?? null,
        created_at: now,
        updated_at: now
      }
      await trx(T.ops).insert(row)
      await trx(T.events).insert({ operation_id: row.operation_id, at: now, from_state: null, to_state: o.state, cause: 'created', evidence: null })
      return { operation: this.op(row), created: true }
    })
  }

  async operation(operationId: string): Promise<Operation | undefined> {
    const r = await this.kx(T.ops).where({ operation_id: operationId }).first()
    return r ? this.op(r) : undefined
  }

  async operationByKey(dedupeKey: string): Promise<Operation | undefined> {
    const r = await this.kx(T.ops).where({ dedupe_key: dedupeKey }).first()
    return r ? this.op(r) : undefined
  }

  async operations(q: { role?: string; states?: string[]; notStates?: string[] } = {}): Promise<Operation[]> {
    let b = this.kx(T.ops).select('*').orderBy('created_at')
    if (q.role) b = b.where({ role: q.role })
    if (q.states) b = b.whereIn('state', q.states)
    if (q.notStates) b = b.whereNotIn('state', q.notStates)
    return (await b).map((r) => this.op(r))
  }

  async transition(
    operationId: string,
    expectedRevision: number,
    to: string,
    change: {
      data?: Record<string, unknown>
      dueAt?: number | null
      dueHeight?: number | null
      lastError?: string | null
      cause: string
      evidence?: unknown
      outbox?: Array<Omit<OutboxItem, 'attempts' | 'operationId'>>
    }
  ): Promise<Operation> {
    return await this.kx.transaction(async (trx) => {
      const cur = await trx(T.ops).where({ operation_id: operationId }).first()
      if (!cur) throw new Error(`unknown operation ${operationId}`)
      if (cur.revision !== expectedRevision) throw new RevisionConflict(`operation ${operationId}: revision ${cur.revision} != expected ${expectedRevision}`)
      const now = this.now()
      const patch: Record<string, unknown> = { state: to, revision: expectedRevision + 1, updated_at: now }
      if (change.data !== undefined) patch.data = canonicalJson({ ...JSON.parse(cur.data), ...change.data })
      if (change.dueAt !== undefined) patch.due_at = change.dueAt
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
            status: 'pending',
            attempts: 0,
            next_at: now
          })
          .onConflict('action_id')
          .ignore()
      }
      return this.op({ ...cur, ...patch })
    })
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

  // ── inbox + cursors ─────────────────────────────────────────────────────
  async cursor(source: string): Promise<string | undefined> {
    return (await this.kx(T.cursors).where({ source }).first())?.cursor
  }

  async ingest(
    source: string,
    items: Array<{ messageId: string; artifactHash: string; kind: string; payload: unknown }>,
    cursor: string
  ): Promise<{ accepted: number; duplicates: number }> {
    return await this.kx.transaction(async (trx) => {
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
  async enqueue(o: Omit<OutboxItem, 'attempts'>): Promise<boolean> {
    return await this.kx.transaction(async (trx) => {
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
        next_at: this.now()
      })
      return true
    })
  }

  async dueOutbox(limit = 50): Promise<OutboxItem[]> {
    return (await this.kx(T.outbox).where({ status: 'pending' }).where('next_at', '<=', this.now()).orderBy('next_at').limit(limit)).map((r) => ({
      actionId: r.action_id,
      operationId: r.operation_id,
      kind: r.kind,
      destination: r.destination,
      payload: JSON.parse(r.payload),
      ...(r.refs != null ? { refs: JSON.parse(r.refs) } : {}),
      attempts: r.attempts
    }))
  }

  async ackOutbox(actionId: string): Promise<void> {
    await this.kx(T.outbox).where({ action_id: actionId }).update({ status: 'acked' })
  }

  /** pull a due-future retry forward to now — used when an event (a proven
   *  funding-tree topup, a slot freeing) removes the cause the backoff was
   *  waiting on. Attempts are untouched: this never hastens a dead-letter. */
  async unblockOutbox(lastErrorLike: string): Promise<number> {
    return await this.kx(T.outbox)
      .where({ status: 'pending' })
      .where('last_error', 'like', `%${lastErrorLike}%`)
      .where('next_at', '>', this.now())
      .update({ next_at: this.now() })
  }

  /** a resource-gated defer — the cause is starvation (a funding slot, not
   *  a delivery failure), so attempts are NOT consumed toward dead-letter.
   *  Short fixed re-check; the real wake-up is unblockOutbox on a freeing
   *  event (topup proven → slots appear). */
  async deferOutbox(actionId: string, error: string, checkInMs = 2_000): Promise<void> {
    await this.kx(T.outbox)
      .where({ action_id: actionId, status: 'pending' })
      .update({ last_error: error.slice(0, 500), next_at: this.now() + checkInMs })
  }

  async retryOutbox(actionId: string, error: string, maxAttempts = 10): Promise<void> {
    await this.kx.transaction(async (trx) => {
      const r = await trx(T.outbox).where({ action_id: actionId }).first()
      if (!r || r.status !== 'pending') return
      const attempts = r.attempts + 1
      await trx(T.outbox)
        .where({ action_id: actionId })
        .update({
          attempts,
          last_error: error.slice(0, 500),
          status: attempts >= maxAttempts ? 'dead' : 'pending',
          next_at: this.now() + Math.min(300_000, 1000 * 2 ** attempts)
        })
    })
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
  async leaseOutpoints(actionId: string, outs: Array<{ txid: string; vout: number; satoshis: number }>): Promise<boolean> {
    return await this.kx.transaction(async (trx) => {
      for (const o of outs) {
        const r = await trx(T.leases)
          .where({ outpoint: `${o.txid}:${o.vout}` })
          .first()
        if (r && r.action_id !== actionId && r.state !== 'released') return false
      }
      const now = this.now()
      for (const o of outs)
        await trx(T.leases)
          .insert({ outpoint: `${o.txid}:${o.vout}`, action_id: actionId, satoshis: o.satoshis, state: 'leased', at: now })
          .onConflict('outpoint')
          .merge()
      return true
    })
  }

  async leaseState(txid: string, vout: number): Promise<{ actionId: string; state: string } | undefined> {
    const r = await this.kx(T.leases)
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
      ...(r.last_observation ? { lastObservation: JSON.parse(r.last_observation) } : {})
    }
  }

  async recordPrepared(t: Omit<WalletTx, 'state' | 'attempts' | 'lastObservation'>): Promise<WalletTx> {
    return await this.kx.transaction(async (trx) => {
      const prev = await trx(T.txs).where({ action_id: t.actionId }).first()
      if (prev) {
        if (prev.txid !== t.txid) throw new Error(`action ${t.actionId} already prepared a different transaction (${prev.txid})`)
        return this.wtx(prev)
      }
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
        at: this.now()
      }
      await trx(T.txs).insert(row)
      return this.wtx(row)
    })
  }

  async walletTx(q: { actionId?: string; txid?: string }): Promise<WalletTx | undefined> {
    const r = await this.kx(T.txs)
      .where(q.actionId ? { action_id: q.actionId } : { txid: q.txid })
      .first()
    return r ? this.wtx(r) : undefined
  }

  async leasedOutpoint(txid: string, vout: number): Promise<boolean> {
    const r = await this.leaseState(txid, vout)
    return r !== undefined && r.state !== 'released'
  }

  async recordBroadcast(
    t: { txid: string; rawHex: string; parents: Record<string, string>; feeSats: number; outs: Array<{ txid: string; vout: number; satoshis: number }> },
    actionId: string,
    operationId: string,
    kind: string
  ): Promise<void> {
    await this.kx.transaction(async (trx) => {
      const now = this.now()
      for (const o of t.outs)
        await trx(T.leases)
          .insert({ outpoint: `${o.txid}:${o.vout}`, action_id: actionId, satoshis: o.satoshis, state: 'leased', at: now })
          .onConflict('outpoint')
          .merge()
      await trx(T.txs)
        .insert({
          action_id: actionId,
          operation_id: operationId,
          kind,
          txid: t.txid,
          raw_hex: t.rawHex,
          parents: JSON.stringify(t.parents),
          fee_sats: t.feeSats,
          state: 'prepared',
          attempts: 0,
          at: now
        })
        .onConflict('action_id')
        .ignore()
    })
  }

  async releaseUnsigned(actionId: string): Promise<boolean> {
    return await this.kx.transaction(async (trx) => {
      const r = await trx(T.txs).where({ action_id: actionId }).first()
      if (!r || r.state !== 'prepared' || r.attempts > 0) return false
      await trx(T.txs)
        .where({ action_id: actionId })
        .update({ state: 'rejected', last_observation: JSON.stringify({ escaped: false, reason: 'never submitted' }) })
      await trx(T.leases).where({ action_id: actionId }).update({ state: 'released' })
      return true
    })
  }

  async walletTxs(states: TxState[]): Promise<WalletTx[]> {
    return (await this.kx(T.txs).whereIn('state', states).orderBy('at')).map((r) => this.wtx(r))
  }

  async setTxState(actionId: string, state: TxState, observation?: unknown, attempt = false): Promise<void> {
    await this.kx.transaction(async (trx) => {
      const r = await trx(T.txs).where({ action_id: actionId }).first()
      if (!r) throw new Error(`unknown wallet action ${actionId}`)
      await trx(T.txs)
        .where({ action_id: actionId })
        .update({ state, attempts: r.attempts + (attempt ? 1 : 0), ...(observation !== undefined ? { last_observation: canonicalJson(observation) } : {}) })
      if (state === 'confirmed') await trx(T.leases).where({ action_id: actionId }).update({ state: 'spent' })
    })
  }

  async releaseLeases(actionId: string): Promise<void> {
    await this.kx(T.leases).where({ action_id: actionId, state: 'leased' }).update({ state: 'released' })
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
      const prev = await trx(T.approvals)
        .where({ operation_id: a.operationId, action_digest: a.actionDigest })
        .whereIn('status', ['pending', 'approved'])
        .first()
      if (prev) return this.appr(prev)
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
