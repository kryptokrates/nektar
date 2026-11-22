// services.ts — the Restate-facing surface. One virtual object per wallet
// tx (key = actionId): every observation/emit lands as a serialized,
// journaled invocation on the tx's own object — the per-key serialization
// replaces the sweep mutex + the run() chain for this leg, and the journal
// covers the commit→effect window pendingTxEffects covered by hand.
//
// Journal discipline, two rules the bodies encode:
//   - commit and intents are SEPARATE ctx.run steps. A crash replays from
//     the last completed step — if commit journaled, intents re-run; the
//     atomic transition is never re-applied.
//   - sends INSIDE a handler go through ctx.objectSendClient (journaled,
//     deduped on replay), never the ingress — unjournaled egress would
//     double-fire.
// The handler BODIES are exported plain (txObserveBody/sweepTickBody/…)
// — the Restate handlers are thin ctx.run wrappers around the same steps,
// so tests invoke the exact code the server invokes.
import type * as restate from '@restatedev/restate-sdk'
import { WAITS } from '../waits.js'
import type { AutomationStore, MarketOpData, OutboxItem, TxState, WalletTx } from '../store.js'
import { runTxIntents, type TxEffects } from '../machines/effects.js'
import { EXEC_SVC, inRtScope, type RtSend } from './exec.js'
import type { CustodyLeg, TxObserve } from './emit.js'
import type { WalletCoordinator } from '../wallet.js'

export interface RtDeps {
  store: AutomationStore
  fx: TxEffects
  log: (line: string) => void
  /** coordinator submit — the resubmit handler's broadcast leg (engine.submitTx) */
  submit?: (actionId: string) => Promise<{ state: TxState; txid: string } | undefined>
}

/** the service options every def carries.
 *  - inactivityTimeout: the 60s default suspends a leg that emits no journal
 *    entries mid-flight — the suspended attempt keeps running (the zombie
 *    class) while the retry replays from its last journaled step. Our legs
 *    do real I/O inside one ctx.run (lane reads under the 45s deliver
 *    bound, broadcast RTTs, tree mints, confirmer flushes) — headroom.
 *  - retryPolicy pause-on-exhaust: a leg that throws 30 times is poisoned
 *    or the world is down — pausing surfaces it for an operator (resume/
 *    kill path) instead of queue-wedging the key forever, and the floors
 *    re-drive the work anyway (drain re-fans, dealfan re-advances). */
const RT_OPTS = {
  inactivityTimeout: { minutes: 5 },
  abortTimeout: { minutes: 10 },
  retryPolicy: { maxAttempts: 30, onMaxAttempts: 'pause' as const }
}

/** floor keys self-heal — the next arm/poke re-drives the work, so a
 *  poisoned leg should pause fast and free the key's FIFO instead of
 *  backing off through the full attempt budget (30 retries of exponential
 *  backoff is minutes of key-wedge for a scan that redoes itself anyway).
 *  Lifecycle/money objects keep the deep budget: their retries ARE the
 *  recovery. */
const RT_OPTS_FLOOR = {
  ...RT_OPTS,
  retryPolicy: { maxAttempts: 5, onMaxAttempts: 'pause' as const }
}

/** per-caller service options merged over the floor base — heavy cron
 *  passes (confirmer/upkeep) raise the inactivity/abort bounds so a long
 *  but healthy scan isn't suspended+replayed from zero forever. */
type RtOpts = Partial<typeof RT_OPTS>

/** heavy single-step scans (confirmer/upkeep/watcher passes) — a pass that
 *  legitimately runs past the inactivity bound suspends mid-step and its
 *  retry replays the WHOLE pass from zero; a scan that always exceeds
 *  never completes and the key wedges on its own backlog. Raise the
 *  bounds; if a pass still exceeds them the fix is sub-stepping the
 *  journal, not a bigger number. */
export const RT_OPTS_HEAVY: RtOpts = { inactivityTimeout: { minutes: 15 }, abortTimeout: { minutes: 30 } }

export interface ObserveResult {
  accepted: boolean
  next?: string
  intents?: string[]
}

/** the named journaled steps — the VO wraps each in its own ctx.run, the
 *  local bodies call them sequentially. One definition, two schedules. */
export const txSteps = (deps: RtDeps) => ({
  load: (actionId: string) => deps.store.walletTx({ actionId }),
  commit: (w: WalletTx, o: TxObserve) => deps.store.applyTxEvent(w.actionId, o.event, { guard: o.guard, observation: o.observation, attempt: o.attempt }),
  intents: (w: WalletTx, intents: string[]) => runTxIntents(intents, w, deps.fx),
  mark: (eventId: number) => deps.store.markTxEffects(eventId).catch(() => undefined),
  resubmit: (actionId: string) => deps.submit!(actionId)
})

/** commit → intents → mark — the observe handler's step chain. */
const emitSteps = (s: ReturnType<typeof txSteps>) =>
  async function emitStepsRun(w: WalletTx, o: TxObserve): Promise<ObserveResult> {
    const r = await s.commit(w, o)
    if (!r.accepted) return { accepted: false, next: w.state }
    const ok = await s.intents(w, r.intents)
    if (ok && r.eventId !== undefined) await s.mark(r.eventId)
    return { accepted: true, next: r.next, intents: r.intents }
  }

/** load → emit — the sequential body a test dispatcher runs. */
export const txObserveBody =
  (deps: RtDeps) =>
  async (o: TxObserve, actionId: string): Promise<ObserveResult> => {
    const s = txSteps(deps)
    const w = await s.load(actionId)
    if (w === undefined) return { accepted: false }
    return await emitSteps(s)(w, o)
  }

/** the resubmit arm — re-broadcasts a prepared/unknown row from stored
 *  bytes. At-least-once on the wire ('already known' dedupes); the emit
 *  inside submit runs direct via the rt scope. */
export const txResubmitBody =
  (deps: RtDeps) =>
  async (_: unknown, actionId: string): Promise<{ resubmitted: boolean; state?: string }> => {
    const s = txSteps(deps)
    const w = await s.load(actionId)
    if (w === undefined || deps.submit === undefined) return { resubmitted: false }
    const r = await s.resubmit(actionId)
    return { resubmitted: r !== undefined, state: r?.state }
  }

export const txObject = async (deps: RtDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const s = txSteps(deps)
  return restate.object({
    name: EXEC_SVC.tx,
    options: RT_OPTS,
    handlers: {
      observe: async (ctx: restate.ObjectContext, o: TxObserve) =>
        // inRtScope: same-key emits inside this handler go direct — routing
        // back through the object would self-deadlock the exclusive key
        await inRtScope({ txKey: ctx.key }, async () => {
          const w = await ctx.run('load', () => Promise.resolve(s.load(ctx.key)))
          if (w === undefined) return { accepted: false }
          const r = await ctx.run('commit', () => Promise.resolve(s.commit(w, o)))
          if (!r.accepted) return { accepted: false, next: w.state, intents: [] as string[] }
          const ok = await ctx.run('intents', () => Promise.resolve(s.intents(w, r.intents)))
          const eventId = r.eventId
          if (ok && eventId !== undefined) await ctx.run('mark', () => Promise.resolve(s.mark(eventId)))
          return { accepted: true, next: r.next, intents: r.intents }
        }),
      resubmit: async (ctx: restate.ObjectContext) =>
        await inRtScope({ txKey: ctx.key }, async () => {
          if (deps.submit === undefined) return { resubmitted: false }
          const w = await ctx.run('load', () => Promise.resolve(s.load(ctx.key)))
          if (w === undefined) return { resubmitted: false }
          const r = await ctx.run('resubmit', () => Promise.resolve(s.resubmit(ctx.key)))
          return { resubmitted: r !== undefined, state: r?.state }
        })
    }
  })
}

// ── the sweep service ────────────────────────────────────────────────────
// the scan legs the deals pass used to run inline: live/dead row
// observations and the boot resubmit. Under restate each row's emit is a
// journaled send to the row's own tx object — the per-key serialization
// is the mutex the pass's run() chain provided; the scan snapshot is
// journaled (ctx.run) so replay fans out the same sends, never new ones.

export interface SweepObservation {
  actionId: string
  observe: TxObserve
}

// ── the deal object ──────────────────────────────────────────────────────
// one virtual object per DEAL — the want's correlation id (buyer purchase,
// seller sale; non-market ops fall back to their own operationId): the
// op's advance/event/reconcile legs all run as serialized, journaled
// invocations on the same object — the per-key serialization replaces the
// run() chain the tick used to run inline AND is what keeps concurrent
// offer/quote events from racing the op's revision CAS. Every write inside
// a leg is revision-CAS, so a replayed leg resumes from its committed
// prefix — the journal doesn't need per-transition steps here.

/** the inbox row as a wire arg — the deal.event handler re-runs the item's
 *  own leg (dispatch + complete/fail) keyed by the deal's correlation id. */
export interface DealEvent {
  source: string
  messageId: string
  kind: string
  payload: unknown
}

export interface DealDeps {
  /** load op by key + run its advance leg (load + height + the stamped
   *  try/catch) — engine.advanceOp. Returns false when the op is gone or
   *  terminal. */
  advanceOp: (operationId: string) => Promise<boolean>
  /** one inbox item's leg — engine.dealEvent: kind dispatch + complete/fail
   *  inside. Keyed ev:<wantId> — per-key FIFO gives the ordering the tick's
   *  batch sort enforced. */
  inboxEvent: (item: DealEvent) => Promise<void>
  /** one op's reconcile legs — engine.reconcileOp (refused-escrow arm +
   *  refunded rescan), keyed by operationId. */
  reconcileOp: (operationId: string) => Promise<void>
  log: (line: string) => void
}

/** advance and due run the same leg — the op's advance() re-evaluates its
 *  own dueAt/dueHeight conditions. The two handler names exist so the
 *  journal/UI shows whether the wake was a tick fan-out or a deadline
 *  arm firing. The send's key is the want correlation id (a lock token);
 *  the operationId rides in the arg — ctx.key stays the fallback for
 *  sends armed before the arg existed. */
export const dealAdvanceBody =
  (deps: DealDeps) =>
  async (arg: { op?: string } | undefined, key = ''): Promise<{ ran: boolean }> => {
    return { ran: await deps.advanceOp(arg?.op ?? key) }
  }

export const dealObject = async (deps: DealDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const leg = dealAdvanceBody(deps)
  return restate.object({
    name: EXEC_SVC.deal,
    options: RT_OPTS,
    handlers: {
      advance: async (ctx: restate.ObjectContext, arg?: { op?: string }) => await ctx.run('advance', () => Promise.resolve(leg(arg, ctx.key))),
      due: async (ctx: restate.ObjectContext, arg?: { op?: string }) => await ctx.run('advance', () => Promise.resolve(leg(arg, ctx.key))),
      event: async (ctx: restate.ObjectContext, item: DealEvent) => await ctx.run('event', () => Promise.resolve(deps.inboxEvent(item))),
      reconcile: async (ctx: restate.ObjectContext, arg?: { op?: string }) =>
        await ctx.run('reconcile', () => Promise.resolve(deps.reconcileOp(arg?.op ?? ctx.key)))
    }
  })
}

// ── the custody object ───────────────────────────────────────────────────
// one fixed key: every wallet custody leg (broadcast verdicts, their
// coin-book intents, the resubmit scan) serializes on a single object —
// the durable, journaled successor of the run() promise chain for the
// custody surface. A crash mid-broadcast leaves a suspended invocation the
// runtime resumes — not an orphaned promise the resubmit scan has to
// rediscover. The coin-claim side stays in funds.take (its own queue +
// reserved set already serialize picks; mcp-side takes never joined run()
// either) — this object owns the verdict/custody-write legs.

export interface CustodyDeps {
  /** the coordinator whose custody legs the object serializes — submit,
   *  markRejected, resubmitPending (journaled on the custody key; emits
   *  inside route to the tx object as usual) */
  wallet: WalletCoordinator
  log: (line: string) => void
}

export const custodyObject = async (deps: CustodyDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  // the held key is per-leg (actionId for submit/markRejected, 'global'
  // for the resubmit scan) — the scope records it so nested custody legs
  // for the same id go direct instead of self-deadlocking. The forwarded
  // txScope re-establishes the caller's held tx key — a resubmit reaching
  // us via tx.resubmit(A) emits A's events direct, not back through A's
  // locked object (which this call chain now blocks on)
  const scope = <T>(key: string, txScope: string | undefined, fn: () => Promise<T>): Promise<T> => inRtScope({ custodyKey: key, txKey: txScope }, fn)
  return restate.object({
    name: EXEC_SVC.custody,
    options: RT_OPTS,
    handlers: {
      submit: async (ctx: restate.ObjectContext, arg: CustodyLeg) =>
        await scope(ctx.key, arg.txScope, () => Promise.resolve(ctx.run('submit', () => deps.wallet.submit(arg.actionId)))),
      markRejected: async (ctx: restate.ObjectContext, arg: CustodyLeg) =>
        await scope(ctx.key, arg.txScope, () => Promise.resolve(ctx.run('markRejected', () => deps.wallet.markRejectedById(arg.actionId, arg.observation)))),
      resubmitPending: async (ctx: restate.ObjectContext) =>
        await scope(ctx.key, undefined, () => Promise.resolve(ctx.run('resubmit', () => deps.wallet.resubmitPending())))
    }
  })
}

// ── the transport object ─────────────────────────────────────────────────
// the outbox's delivery machinery as journaled work: `deliver` lands on the
// DESTINATION host's key (per-host serialization — the OUTBOX_POOL cap's
// real guarantee was "one dead host can't serialize the batch", which
// per-host keys give natively, plus same-host ordering). `drain` sits on a
// fixed key: it scans due rows (journaled snapshot), fans each row out as
// a journaled send deduped by actionId, then schedules ITS OWN next drain
// at the earliest next_at — the durable-timer successor of the wheel's
// armOutboxTimer. Empty boxes leave no arm; outboxReady pokes re-wake it.
// The rows stay the durable intent (the atomic outbox pattern is SQLite's),
// retryOutbox/deferOutbox keep attempts + dead-letter policy — Restate owns
// scheduling and delivery ordering, not the verdicts.

export const TRANSPORT_DRAIN_KEY = 'drain'

export interface TransportDeps {
  /** due pending rows — the journaled scan input (store.dueOutbox) */
  dueRows: () => Promise<OutboxItem[]>
  /** earliest pending wake — the drain's self-schedule (store.nextOutboxAt) */
  nextAt: () => Promise<number | undefined>
  /** one row's delivery leg — engine.deliverOutboxItem (deliver + ack /
   *  retry / defer + shrink recovery inside) */
  deliverItem: (item: OutboxItem) => Promise<'acked' | 'retried' | 'deferred' | 'skipped'>
  now: () => number
  log: (line: string) => void
}

export const transportDrainBody =
  (deps: TransportDeps) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    const rows = await run('scan', () => Promise.resolve(deps.dueRows()))
    // one journaled send per row, keyed by destination — per-host order +
    // cross-host parallelism; idempotent on actionId so a replayed or raced
    // drain can't double-send
    for (const item of rows)
      // the key must version by attempt: a row the first delivery failed on
      // re-fans on its next_at — deduping by actionId alone would swallow the
      // retry inside the 24h idempotency retention (the row would dead-letter
      // having hit the wire once). Same-attempt re-fans still dedupe; a new
      // attempt is new work. `rt/dedup-kills-retry`
      await send({ service: EXEC_SVC.transport, handler: 'deliver', key: item.destination }, item, {
        idempotencyKey: `deliver:${item.actionId}:${item.attempts}`
      })
    // self-schedule the ceiling at the earliest pending wake — the durable
    // successor of armOutboxTimer; empty box leaves the loop asleep until
    // outboxReady pokes it
    const next = await run('next', () => Promise.resolve(deps.nextAt()))
    if (next !== undefined)
      // EVERY drain invocation arms a successor — outboxReady pokes and
      // wake-cycle drains run this same body, so an unkeyed arm adds one
      // permanent self-arming chain per poke and the delayed-send backlog
      // grows forever (rt/arm-multiplication). Keying the arm on `next` —
      // the domain-derived earliest wake — converges any number of
      // concurrent chains to ≤1 pending arm per wake instant.
      await send(
        { service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY },
        {},
        { delayMs: Math.max(0, next - deps.now()), idempotencyKey: `drain:${next}` }
      )
    return { sent: rows.length, nextAt: next }
  }

export const transportObject = async (deps: TransportDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const drain = transportDrainBody(deps)
  return restate.object({
    name: EXEC_SVC.transport,
    options: RT_OPTS_FLOOR,
    handlers: {
      deliver: async (ctx: restate.ObjectContext, item: OutboxItem) => await ctx.run('deliver', () => Promise.resolve(deps.deliverItem(item))),
      drain: async (ctx: restate.ObjectContext) => {
        const transport = ctx.objectSendClient({ name: EXEC_SVC.transport } as never, TRANSPORT_DRAIN_KEY) as unknown as {
          drain: (o: unknown, opts?: unknown) => Promise<void>
        }
        const send: RtSend = async (target, arg, opts) => {
          if (target.service !== EXEC_SVC.transport) throw new Error(`transport send: no route for ${target.service}`)
          if (target.handler === 'deliver')
            await (
              ctx.objectSendClient({ name: EXEC_SVC.transport } as never, target.key ?? '') as unknown as {
                deliver: (o: OutboxItem, opts?: unknown) => Promise<void>
              }
            ).deliver(arg as OutboxItem, restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0, idempotencyKey: opts?.idempotencyKey }))
          else if (target.handler === 'drain')
            await transport.drain(arg, restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0, idempotencyKey: opts?.idempotencyKey }))
          else throw new Error(`transport send: no handler ${target.handler}`)
        }
        return await drain(send, (n, fn) => ctx.run(n, fn))
      }
    }
  })
}

export interface SweepDeps {
  /** live+dead rows × observeFn → (key, observe) pairs (scanTxObservations) */
  scan: () => AsyncGenerator<SweepObservation>
  /** prepared/submission_unknown rows needing a broadcast re-air at boot */
  resubmitRows: () => Promise<Array<{ actionId: string }>>
  log: (line: string) => void
}

/** collect the scan snapshot, then send each row's observe — the two halves
 *  are journaled separately under restate (the scan's result is journaled so
 *  replay re-sends the SAME snapshot — releaseLeases/txFacts don't re-run —
 *  and the sends dedupe). The optional `run` param is the step boundary:
 *  the VO passes ctx.run, the local executor omits it. */
type StepRun = <T>(name: string, fn: () => Promise<T>) => Promise<T>

export const sweepTickBody =
  (deps: SweepDeps) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    const obs = await run('scan', async () => {
      const out: SweepObservation[] = []
      for await (const o of deps.scan()) out.push(o)
      return out
    })
    for (const o of obs) await send({ service: EXEC_SVC.tx, handler: 'observe', key: o.actionId }, o.observe)
    return { sent: obs.length }
  }

export const sweepBootBody =
  (deps: SweepDeps) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    const rows = await run('rows', () => Promise.resolve(deps.resubmitRows()))
    for (const w of rows) await send({ service: EXEC_SVC.tx, handler: 'resubmit', key: w.actionId }, {})
    return { sent: rows.length }
  }

export const sweepService = async (deps: SweepDeps, txDef: unknown) => {
  const restate = await import('@restatedev/restate-sdk')
  const tick = sweepTickBody(deps)
  const boot = sweepBootBody(deps)
  // journaled sends inside a handler: ctx.objectSendClient — the journal
  // dedupes them on replay; raw ingress egress would double-fire
  const send = (ctx: restate.Context): RtSend => {
    const tx = (key: string) =>
      ctx.objectSendClient(txDef as never, key) as unknown as {
        observe: (o: TxObserve) => Promise<void>
        resubmit: (o: unknown) => Promise<void>
      }
    return async (target, arg) => {
      if (target.service === EXEC_SVC.tx) {
        const c = tx(target.key ?? '')
        if (target.handler === 'observe') await c.observe(arg as TxObserve)
        else if (target.handler === 'resubmit') await c.resubmit(arg)
        else throw new Error(`sweep send: no tx handler ${target.handler}`)
      } else throw new Error(`sweep send: no route for ${target.service}`)
    }
  }
  // an OBJECT on one key, not a basic service: a service runs tick and
  // boot CONCURRENTLY — two journaled scans each fanning the same rows.
  // The fixed key's FIFO gives the floor's single-flight guarantee durably.
  return restate.object({
    name: EXEC_SVC.sweep,
    options: RT_OPTS_FLOOR,
    handlers: {
      tick: async (ctx: restate.ObjectContext) => await tick(send(ctx), (n, fn) => ctx.run(n, fn)),
      boot: async (ctx: restate.ObjectContext) => await boot(send(ctx), (n, fn) => ctx.run(n, fn))
    }
  })
}

// ── the dealfan service ──────────────────────────────────────────────────
// the tick's scan+fan-out legs as ONE journaled invocation: pending inbox
// items → deal.event on the deal's correlation key, NONTERMINAL ops →
// deal.advance per op key, due reconcile rows → deal.reconcile per op key.
// Each scan runs inside ctx.run — a replay re-sends the SAME snapshot (no
// row is re-scanned), and the sends go through ctx.objectSendClient so the
// journal dedupes them.

/** one live op's fan-out id: operationId addresses the leg, wantId is the
 *  deal-object key — events and advances on the same want MUST share it,
 *  distinct keys run concurrently and the op's revision CAS loses writes */
export interface DealOpRef {
  operationId: string
  wantId?: string
}

export interface DealfanDeps {
  pendingInbox: () => Promise<DealEvent[]>
  /** NONTERMINAL operations with their want correlation id */
  liveOps: () => Promise<DealOpRef[]>
  /** funding_failed/refunded ops past their reconcile cooldown */
  reconcileOps: () => Promise<DealOpRef[]>
  /** the resubmit cadence leg — a throttled custody.resubmitPending send;
   *  the deals floor used to carry this inside engine.tick() */
  resubmitPending?: () => Promise<void>
  /** the floor cadence — re-read on every arm so the hot/idle adaptive
   *  ceiling keeps working (the scheduler 'deals' row's successor) */
  ceilingMs: () => number
  /** the domain clock — the self-arm's idempotency window quantizes on
   *  it (store-stamped times and arm math must share one clock) */
  now?: () => number
  /** in-flight send dedup — `id` was already dispatched within `ms` (domain
   *  clock). A pending inbox row or live op is re-fanned every tick; under
   *  restate each send is a durable queued invocation, so unthrottled
   *  re-fanning piles hundreds of identical legs onto the same FIFO key
   *  and starves the new work behind the backlog. True = skip this send;
   *  the window is also the recovery bound for a send whose invocation was
   *  dropped before running (killed/deaf want-gate early-returns ride it). */
  sentRecently?: (id: string, ms: number) => boolean
  /** per-tick monotonic sequence — the durable advance/reconcile key carries
   *  it so DISTINCT ticks are distinct sends: a windowed bucket collapses
   *  every tick in one domain-time window into a single leg, starving ops
   *  whose time/event gates matured between ticks. Journaled via run() so a
   *  replay re-issues the same seq (deduped), while the next tick's sends
   *  land fresh. sentRecently stays the frequency bound — the key dedups
   *  replays, not cadence. In-memory across restarts: a reset seq collides
   *  with a still-pending same-seq leg, which is harmless — the pending leg
   *  IS the same poke. */
  tickSeq?: () => number
  log: (line: string) => void
}

// the event key — the deal's correlation id. wantId nests differently on
// each wire shape (msg.wantId on envelopes, offer.wantId on SignedOffer,
// want.wantId on SignedWant); an offer without it falls back to its own
// offerId, and a shape we can't read keys by its inbox row id (same FIFO
// guarantee, narrower fan-in). Keying every want-related event to the
// SAME id is what serializes the read-modify-write on op.data.offers —
// distinct keys would let concurrent deal.event invocations lose updates.
export const dealEventKey = (i: DealEvent): string => {
  const p = i.payload as {
    wantId?: string
    offerId?: string
    msg?: { wantId?: string }
    offer?: { wantId?: string; offerId?: string }
    want?: { wantId?: string }
  } | null
  return `ev:${p?.msg?.wantId ?? p?.wantId ?? p?.offer?.wantId ?? p?.want?.wantId ?? p?.offer?.offerId ?? p?.offerId ?? i.messageId}`
}

/** the op's deal-object key — the want correlation id when the op carries
 *  one (buyer purchase, seller sale), else its own operationId. Every
 *  deal.* send for the op MUST use this so events/advances/reconciles
 *  serialize on one object key instead of racing the op's revision CAS. */
export const dealOpKey = (o: DealOpRef): string => (o.wantId !== undefined ? `ev:${o.wantId}` : o.operationId)

/** wantId out of an op row — `data.want` is the SignedWant on both buyer
 *  and seller ops; non-market ops have none */
export const opWantId = (o: { data: MarketOpData }): string | undefined => o.data.want?.want?.wantId

export const dealfanTickBody =
  (deps: DealfanDeps, service: string = EXEC_SVC.dealfan) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    const dup = deps.sentRecently ?? (() => false)
    // the fan keys carry the durable dedup layer — sentRecently is the cheap
    // in-memory pre-filter, the ingress key dedups the send that survives a
    // restart (rt/fan-dedup-restart-gap). Event sends stay windowed on the
    // domain clock: a pending inbox row is re-fanned every tick and the
    // window is the right re-fire bound for a gated early-return. Advance/
    // reconcile sends are per-TICK work pokes — a windowed key collapses
    // distinct ticks into one leg and starves ops whose gates matured
    // between ticks, so they carry the tickSeq when one is wired.
    // EVERY decision input lives inside a journaled step: the sentRecently
    // map, the time buckets, the seq, and ceilingMs() all differ across a
    // replay — an unjournaled gate produces a different send stream on
    // replay and the invocation dies JournalMismatchError instead of
    // completing.
    const now0 = await run('now', () => Promise.resolve((deps.now ?? Date.now)()))
    const eb = Math.floor(now0 / WAITS.fanEventResend.ms)
    const ab = deps.tickSeq === undefined ? Math.floor(now0 / WAITS.fanAdvanceResend.ms) : await run('fanseq', () => Promise.resolve(deps.tickSeq!()))
    const inbox = await run('inbox', () => Promise.resolve(deps.pendingInbox()))
    for (const i of inbox)
      if (!(await run(`dup:ev:${i.source}:${i.messageId}`, () => Promise.resolve(dup(`ev:${i.source}:${i.messageId}`, WAITS.fanEventResend.ms)))))
        await send({ service: EXEC_SVC.deal, handler: 'event', key: dealEventKey(i) }, i, {
          idempotencyKey: `fan:ev:${i.source}:${i.messageId}:${eb}`
        })
    const ops = await run('ops', () => Promise.resolve(deps.liveOps()))
    for (const o of ops)
      if (!(await run(`dup:adv:${o.operationId}`, () => Promise.resolve(dup(`adv:${o.operationId}`, WAITS.fanAdvanceResend.ms)))))
        await send(
          { service: EXEC_SVC.deal, handler: 'advance', key: dealOpKey(o) },
          { op: o.operationId },
          { idempotencyKey: `fan:adv:${o.operationId}:${ab}` }
        )
    const rec = await run('reconcile', () => Promise.resolve(deps.reconcileOps()))
    for (const o of rec)
      if (!(await run(`dup:rec:${o.operationId}`, () => Promise.resolve(dup(`rec:${o.operationId}`, WAITS.fanAdvanceResend.ms)))))
        await send(
          { service: EXEC_SVC.deal, handler: 'reconcile', key: dealOpKey(o) },
          { op: o.operationId },
          { idempotencyKey: `fan:rec:${o.operationId}:${ab}` }
        )
    if (deps.resubmitPending !== undefined) await run('resubmit', () => Promise.resolve(deps.resubmitPending!()))
    // the floor: every tick self-arms the next at the adaptive ceiling —
    // durable delayed send under restate, executor timer under local.
    // ceilingMs() <= 0 is the floor's off-switch (intervalMs:0 = event-driven
    // only) — a zero-delay arm would hot-loop the tick forever.
    // The arm is content-keyed on its cadence window: EVERY tick invocation
    // arms (boot kick, wake-cycle sends, engine pokes all run this body),
    // so an unkeyed arm adds a permanent parallel chain per extra tick and
    // the delayed-send backlog grows without bound (rt/arm-multiplication).
    // Quantizing on ceiling windows converges all chains to ≤1 pending arm.
    const ceil = await run('ceiling', () => Promise.resolve(deps.ceilingMs()))
    if (ceil > 0) {
      const now = await run('armNow', () => Promise.resolve((deps.now ?? Date.now)()))
      await send({ service, handler: 'tick', key: CRON_KEY }, {}, { delayMs: ceil, idempotencyKey: `${service}:tick:${Math.floor(now / ceil) * ceil + ceil}` })
    }
    return { events: inbox.length, advanced: ops.length, reconciled: rec.length }
  }

export const dealfanObject = async (deps: DealfanDeps, dealDef: unknown, opts?: RtOpts, service: string = EXEC_SVC.dealfan) => {
  const restate = await import('@restatedev/restate-sdk')
  const tick = dealfanTickBody(deps, service)
  const send = (ctx: restate.Context): RtSend => {
    const deal = (key: string) =>
      ctx.objectSendClient(dealDef as never, key) as unknown as {
        advance: (o: unknown, opts?: unknown) => Promise<void>
        event: (o: DealEvent, opts?: unknown) => Promise<void>
        reconcile: (o: unknown, opts?: unknown) => Promise<void>
      }
    const self = ctx.objectSendClient({ name: service } as never, CRON_KEY) as unknown as {
      tick: (o: unknown, opts?: unknown) => Promise<void>
    }
    return async (target, arg, opts) => {
      const o = restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0, idempotencyKey: opts?.idempotencyKey })
      if (target.service === EXEC_SVC.deal) {
        const c = deal(target.key ?? '')
        if (target.handler === 'advance') await c.advance(arg, o)
        else if (target.handler === 'event') await c.event(arg as DealEvent, o)
        else if (target.handler === 'reconcile') await c.reconcile(arg, o)
        else throw new Error(`dealfan send: no deal handler ${target.handler}`)
      } else if (target.service === service && target.handler === 'tick') await self.tick(arg, o)
      else throw new Error(`dealfan send: no route for ${target.service}`)
    }
  }
  // an OBJECT on one key, not a basic service: the tick is the old run()
  // pass — single-flight by design. A basic service runs concurrent ticks;
  // each scans+fans+arms its own chain, so wake-cycle/boot/poke ticks both
  // duplicate the pass AND multiply the self-arm backlog. The key's FIFO
  // is the durable successor of the tick mutex.
  return restate.object({
    name: service,
    options: { ...RT_OPTS_FLOOR, ...opts },
    handlers: {
      tick: async (ctx: restate.ObjectContext) => await tick(send(ctx), (n, fn) => ctx.run(n, fn))
    }
  })
}

// ── the wake service ─────────────────────────────────────────────────────
// the process-edge event legs (mem.onTxProof/onTxRejected/onTxAbandoned/
// onHeight/onReorg) as durable invocations. Today each callback spawns a
// detached promise: a crash mid-leg drops the wake and the floor has to
// re-cover it. Under restate the callback is a durable send — the leg runs
// inside ctx.run (journaled: replay doesn't re-fire the ledger mutation),
// the cycle-equivalent goes out through ctx send clients (journaled, never
// the ingress), and a leg error propagates so the runtime retries the wake
// rather than log-and-drop it. The legs are naturally idempotent row-ops
// (markProven touches only unproven rows; attachParentPath second-attach is
// a no-op; demotions are range updates), and the ingress idempotency key
// dedupes repeat callbacks for the same logical event. Wakes stay hints:
// the scheduler floor still carries correctness — nothing here removes a
// poll, it only makes the push path survive a crash.

export interface WakeLegs {
  /** txProof domain leg — tree-ledger markProven/attachParentPath + the
   *  funding-slot unblock + queueRepublish. mp crosses the wire as hex
   *  (MerklePath.fromHex inside the leg). */
  txProof?: (txid: string, mpHex: string) => Promise<void>
  /** txAbandoned domain leg — ledger drop + journal reap + ghost + unblock */
  txAbandoned?: (txid: string) => Promise<void>
  /** reorg demotion leg — journal clearPaths + ledger unproveFrom */
  reorg?: (from: number) => Promise<void>
  /** due-height probe — store.hasDueAtHeight; the handler sends dealfan
   *  only when a row is actually due (matches the local wake gate) */
  dueAtHeight?: (tip: number) => Promise<boolean>
}

export interface WakeDeps {
  legs: WakeLegs
  /** the volatile reconcile-hint set — engine.hint. Process-local by
   *  design: it lives wherever reconcileGate runs, journaled replays re-
   *  adding it are harmless (a Set insert). */
  hint: (txid: string) => void
  /** the domain clock — the cycle-tail coalesce bucket quantizes on it */
  now?: () => number
  /** the dealfan service the cycle tails/hight pokes send to — overridable
   *  so live tests can scope the pair off the shared service name (ghost
   *  chains on 'nektar-dealfan/main' would otherwise queue the poke behind
   *  their retries on the 'main' FIFO) */
  fanSvc?: string
  log: (line: string) => void
}

/** the cycle-equivalent through journaled sends — dealfan.tick is the
 *  deals pass's scan+fan-out, transport.drain is the outbox poke. The
 *  custody resubmit cadence stays on the floor where it belongs — wakes
 *  drive work, not time gates.
 * The tails are pure hints (the wake's own journaled leg already committed
 * its row — a queued tick scans at run time and sees the work), so they
 * coalesce on a 1s bucket: a chain-event burst queues ≤1 pass per target
 * per window instead of one full scan per event. Distinct from the
 * self-arm keys — pokes never collide with arms. */
const wakeCycle = async (send: RtSend, now: () => number, fanSvc: string): Promise<void> => {
  const b = Math.floor(now() / 1000)
  await send({ service: fanSvc, handler: 'tick', key: CRON_KEY }, {}, { idempotencyKey: `dealfan:poke:${b}` })
  await send({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {}, { idempotencyKey: `drain:poke:${b}` })
}

export const wakeTxProofBody =
  (deps: WakeDeps) =>
  async (send: RtSend, arg: { txid: string; mpHex: string }, run: StepRun = (_n, fn) => fn()) => {
    if (deps.legs.txProof !== undefined) await run('ledger', () => Promise.resolve(deps.legs.txProof!(arg.txid, arg.mpHex)))
    deps.hint(arg.txid)
    await wakeCycle(send, deps.now ?? Date.now, deps.fanSvc ?? EXEC_SVC.dealfan)
    return { done: true }
  }

export const wakeTxRejectedBody = (deps: WakeDeps) => async (send: RtSend, arg: { txid: string }) => {
  deps.hint(arg.txid)
  await wakeCycle(send, deps.now ?? Date.now, deps.fanSvc ?? EXEC_SVC.dealfan)
  return { done: true }
}

export const wakeTxAbandonedBody =
  (deps: WakeDeps) =>
  async (send: RtSend, arg: { txid: string }, run: StepRun = (_n, fn) => fn()) => {
    if (deps.legs.txAbandoned !== undefined) await run('ledger', () => Promise.resolve(deps.legs.txAbandoned!(arg.txid)))
    await wakeCycle(send, deps.now ?? Date.now, deps.fanSvc ?? EXEC_SVC.dealfan)
    return { done: true }
  }

export const wakeHeightBody =
  (deps: WakeDeps) =>
  async (send: RtSend, arg: { tip: number }, run: StepRun = (_n, fn) => fn()) => {
    const due = deps.legs.dueAtHeight === undefined ? false : await run('due', () => Promise.resolve(deps.legs.dueAtHeight!(arg.tip)))
    if (due) {
      const b = Math.floor((deps.now ?? Date.now)() / 1000)
      await send({ service: deps.fanSvc ?? EXEC_SVC.dealfan, handler: 'tick', key: CRON_KEY }, {}, { idempotencyKey: `dealfan:poke:${b}` })
    }
    return { due }
  }

export const wakeReorgBody =
  (deps: WakeDeps) =>
  async (_send: RtSend, arg: { from: number }, run: StepRun = (_n, fn) => fn()) => {
    if (deps.legs.reorg !== undefined) await run('reorg', () => Promise.resolve(deps.legs.reorg!(arg.from)))
    return { done: true }
  }

export const wakeService = async (deps: WakeDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const fanSvc = deps.fanSvc ?? EXEC_SVC.dealfan
  const send = (ctx: restate.Context): RtSend => {
    const dealfan = ctx.objectSendClient({ name: fanSvc } as never, CRON_KEY) as unknown as {
      tick: (o: unknown, opts?: unknown) => Promise<void>
    }
    const transport = ctx.objectSendClient({ name: EXEC_SVC.transport } as never, TRANSPORT_DRAIN_KEY) as unknown as {
      drain: (o: unknown, opts?: unknown) => Promise<void>
    }
    return async (target, arg, opts) => {
      const o = restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0, idempotencyKey: opts?.idempotencyKey })
      if (target.service === fanSvc && target.handler === 'tick') await dealfan.tick(arg, o)
      else if (target.service === EXEC_SVC.transport && target.handler === 'drain') await transport.drain(arg, o)
      else throw new Error(`wake send: no route for ${target.service}/${target.handler}`)
    }
  }
  const proof = wakeTxProofBody(deps)
  const rejected = wakeTxRejectedBody(deps)
  const abandoned = wakeTxAbandonedBody(deps)
  const height = wakeHeightBody(deps)
  const reorg = wakeReorgBody(deps)
  return restate.service({
    name: EXEC_SVC.wake,
    options: RT_OPTS_FLOOR,
    handlers: {
      txProof: async (ctx: restate.Context, arg: { txid: string; mpHex: string }) => await proof(send(ctx), arg, (n, fn) => ctx.run(n, fn)),
      txRejected: async (ctx: restate.Context, arg: { txid: string }) => await rejected(send(ctx), arg),
      txAbandoned: async (ctx: restate.Context, arg: { txid: string }) => await abandoned(send(ctx), arg, (n, fn) => ctx.run(n, fn)),
      height: async (ctx: restate.Context, arg: { tip: number }) => await height(send(ctx), arg, (n, fn) => ctx.run(n, fn)),
      reorg: async (ctx: restate.Context, arg: { from: number }) => await reorg(send(ctx), arg, (n, fn) => ctx.run(n, fn))
    }
  })
}

// ── the cadence objects ──────────────────────────────────────────────────
// the periodic loops the scheduler wheel used to drive — one fixed-key
// object per cadence: `tick` runs the whole pass inside one ctx.run (the
// pass is already serialized and replay-idempotent — ensureTrees dedupes
// on ledger state, evidence is keyed, the log cursor advances on quorum,
// hostBack/unblock are row updates) and then SELF-ARMS via a journaled
// delayed send — the durable successor of scheduler.add's ceiling row. If
// the chain breaks the journal holds the suspended invocation — nothing
// silently stops ticking. The object's single key replaces the
// single-flight guards (upkeepRun/busy) with per-key serialization.

export const CRON_KEY = 'main'

export interface CronDeps {
  /** the pass — journaled as one ctx.run */
  tick: () => Promise<void>
  /** the cadence — re-read on every arm so adaptive ceilings keep working */
  ceilingMs: () => number
  /** the domain clock — the self-arm's idempotency window quantizes on it */
  now?: () => number
  log: (line: string) => void
}

export const cronTickBody =
  (service: string, deps: CronDeps) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    await run('tick', () => Promise.resolve(deps.tick()))
    // ceilingMs() <= 0 is the cadence's off-switch — a zero-delay arm is a
    // hot infinite loop, not a floor
    // The arm is content-keyed on its cadence window — every tick invocation
    // arms (boot kicks and event pokes run this same body), so unkeyed arms
    // add one permanent parallel chain per poke (rt/arm-multiplication).
    // The window converges all chains to ≤1 pending arm. The ceiling and
    // clock are journaled too — replaying a tick re-arms with the journaled
    // values, not a different command stream.
    const ceil = await run('ceiling', () => Promise.resolve(deps.ceilingMs()))
    if (ceil > 0) {
      const now = await run('armNow', () => Promise.resolve((deps.now ?? Date.now)()))
      await send({ service, handler: 'tick', key: CRON_KEY }, {}, { delayMs: ceil, idempotencyKey: `${service}:tick:${Math.floor(now / ceil) * ceil + ceil}` })
    }
    return { done: true }
  }

export const cronObject = async (service: string, deps: CronDeps, opts?: RtOpts) => {
  const restate = await import('@restatedev/restate-sdk')
  const tick = cronTickBody(service, deps)
  return restate.object({
    name: service,
    options: { ...RT_OPTS_FLOOR, ...opts },
    handlers: {
      tick: async (ctx: restate.ObjectContext) => {
        const self = ctx.objectSendClient({ name: service } as never, CRON_KEY) as unknown as { tick: (o: unknown, opts?: unknown) => Promise<void> }
        const send: RtSend = async (target, arg, opts) => {
          if (target.service !== service || target.handler !== 'tick') throw new Error(`cron send: no route ${target.service}/${target.handler}`)
          await self.tick(arg, restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0, idempotencyKey: opts?.idempotencyKey }))
        }
        // a self-SEND on the same key — one-way, no lock wait, no deadlock
        return await tick(send, (n, fn) => ctx.run(n, fn))
      }
    }
  })
}

/** a keyed event-sink object — one journaled ctx.run leg per 'tick'
 *  invocation, no self-arm: the event plane's own signals are the cadence
 *  (the journaled successor of a detached() listener body a crash dropped) */
export const legObject = async (service: string, leg: () => Promise<void>, opts?: RtOpts) => {
  const restate = await import('@restatedev/restate-sdk')
  return restate.object({
    name: service,
    options: { ...RT_OPTS_FLOOR, ...opts },
    handlers: {
      tick: async (ctx: restate.ObjectContext) => {
        await ctx.run('tick', leg)
        return { done: true }
      }
    }
  })
}

// ── the notify object ────────────────────────────────────────────────────
// the counterparty push legs — notifyPeer's presence announce and
// pushCarrier's direct beef POST — as durable invocations keyed by
// destination host. Today each is a detached() fire-and-forget fetch a
// crash silently drops (the peer just misses the hint until the next
// announce). As journaled work the delivery is at-least-once: a leg
// throws → the runtime retries; per-host keys keep the same ordering +
// dead-host isolation the transport object's deliver keys give.

export interface NotifyDeps {
  /** notifyPeer's HTTP leg — POST {service, query?} to <watch>/notify.
   *  Throws on failure → the handler propagates → the runtime retries. */
  peer: (watch: string, service: string, query?: unknown) => Promise<void>
  /** pushCarrier's HTTP leg — POST the carrier hex to the recipient's
   *  /v1/lane push endpoint */
  carrier: (push: string, beefHex: string) => Promise<void>
  log: (line: string) => void
}

export const notifyObject = async (deps: NotifyDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  return restate.object({
    name: EXEC_SVC.notify,
    options: RT_OPTS,
    handlers: {
      peer: async (ctx: restate.ObjectContext, arg: { watch: string; service: string; query?: unknown }) =>
        await ctx.run('peer', () => Promise.resolve(deps.peer(arg.watch, arg.service, arg.query))),
      carrier: async (ctx: restate.ObjectContext, arg: { push: string; beefHex: string }) =>
        await ctx.run('carrier', () => Promise.resolve(deps.carrier(arg.push, arg.beefHex)))
    }
  })
}
