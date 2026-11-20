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
// and the local executor registers the same bodies, so memory-tier tests
// run the exact code the server invokes.
import type * as restate from '@restatedev/restate-sdk'
import type { AutomationStore, OutboxItem, TxState, WalletTx } from '../store.js'
import { runTxIntents, type TxEffects } from '../machines/effects.js'
import { CUSTODY_KEY, EXEC_SVC, inRtScope, type RtSend } from './exec.js'
import type { CustodyLeg, TxObserve } from './emit.js'
import type { WalletCoordinator } from '../wallet.js'

export interface RtDeps {
  store: AutomationStore
  fx: TxEffects
  log: (line: string) => void
  /** coordinator submit — the resubmit handler's broadcast leg (engine.submitTx) */
  submit?: (actionId: string) => Promise<{ state: TxState; txid: string } | undefined>
}

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

/** commit → intents → mark — shared by txObserveBody and the expire body. */
const emitSteps = (s: ReturnType<typeof txSteps>) =>
  async function emitStepsRun(w: WalletTx, o: TxObserve): Promise<ObserveResult> {
    const r = await s.commit(w, o)
    if (!r.accepted) return { accepted: false, next: w.state }
    const ok = await s.intents(w, r.intents)
    if (ok && r.eventId !== undefined) await s.mark(r.eventId)
    return { accepted: true, next: r.next, intents: r.intents }
  }

/** load → emit — the sequential body the LocalExecutor runs. */
export const txObserveBody =
  (deps: RtDeps) =>
  async (o: TxObserve, actionId: string): Promise<ObserveResult> => {
    const s = txSteps(deps)
    const w = await s.load(actionId)
    if (w === undefined) return { accepted: false }
    return await emitSteps(s)(w, o)
  }

/** the export-ttl arm — fires only if the row still sits exported. */
export const txExpireBody =
  (deps: RtDeps) =>
  async (_: unknown, actionId: string): Promise<ObserveResult> => {
    const s = txSteps(deps)
    const w = await s.load(actionId)
    if (w === undefined || w.state !== 'exported') return { accepted: false }
    return await emitSteps(s)(w, { event: 'export.ttl' })
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
    handlers: {
      observe: async (ctx: restate.ObjectContext, o: TxObserve) =>
        // inRtScope: same-key emits inside this handler go direct — routing
        // back through the object would self-deadlock the exclusive key
        await inRtScope({ txKey: ctx.key }, async () => {
          const w = await ctx.run('load', () => s.load(ctx.key))
          if (w === undefined) return { accepted: false }
          const r = await ctx.run('commit', () => s.commit(w, o))
          if (!r.accepted) return { accepted: false, next: w.state, intents: [] as string[] }
          const ok = await ctx.run('intents', () => s.intents(w, r.intents))
          const eventId = r.eventId
          if (ok && eventId !== undefined) await ctx.run('mark', () => s.mark(eventId))
          return { accepted: true, next: r.next, intents: r.intents }
        }),
      expire: async (ctx: restate.ObjectContext) =>
        await inRtScope({ txKey: ctx.key }, async () => {
          const w = await ctx.run('load', () => s.load(ctx.key))
          if (w === undefined || w.state !== 'exported') return { accepted: false }
          const r = await ctx.run('commit', () => s.commit(w, { event: 'export.ttl' }))
          if (!r.accepted) return { accepted: false, next: w.state, intents: [] as string[] }
          const ok = await ctx.run('intents', () => s.intents(w, r.intents))
          const eid = r.eventId
          if (ok && eid !== undefined) await ctx.run('mark', () => s.mark(eid))
          return { accepted: true, next: r.next, intents: r.intents }
        }),
      resubmit: async (ctx: restate.ObjectContext) =>
        await inRtScope({ txKey: ctx.key }, async () => {
          if (deps.submit === undefined) return { resubmitted: false }
          const w = await ctx.run('load', () => s.load(ctx.key))
          if (w === undefined) return { resubmitted: false }
          const r = await ctx.run('resubmit', () => s.resubmit(ctx.key))
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
// one virtual object per operation (key = operationId): the op's advance
// leg runs as a serialized, journaled invocation on its own object — the
// per-key serialization replaces the run() chain for the deal legs the
// tick used to run inline. Every write inside the leg is revision-CAS, so
// a replayed leg resumes from its committed prefix — the journal doesn't
// need per-transition steps here.

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
 *  arm firing. */
export const dealAdvanceBody =
  (deps: DealDeps) =>
  async (_: unknown, operationId: string): Promise<{ ran: boolean }> => {
    return { ran: await deps.advanceOp(operationId) }
  }

export const dealObject = async (deps: DealDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const leg = dealAdvanceBody(deps)
  return restate.object({
    name: EXEC_SVC.deal,
    handlers: {
      advance: async (ctx: restate.ObjectContext) => await ctx.run('advance', () => leg({}, ctx.key)),
      due: async (ctx: restate.ObjectContext) => await ctx.run('advance', () => leg({}, ctx.key)),
      event: async (ctx: restate.ObjectContext, item: DealEvent) => await ctx.run('event', () => deps.inboxEvent(item)),
      reconcile: async (ctx: restate.ObjectContext) => await ctx.run('reconcile', () => deps.reconcileOp(ctx.key))
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
  // the forwarded txScope re-establishes the caller's held tx key — a
  // resubmit reaching us via tx.resubmit(A) emits A's events direct, not
  // back through A's locked object (which this call chain now blocks on)
  const scope = <T>(txScope: string | undefined, fn: () => Promise<T>): Promise<T> => inRtScope({ custodyKey: CUSTODY_KEY, txKey: txScope }, fn)
  return restate.object({
    name: EXEC_SVC.custody,
    handlers: {
      submit: async (ctx: restate.ObjectContext, arg: CustodyLeg) => await scope(arg.txScope, () => ctx.run('submit', () => deps.wallet.submit(arg.actionId))),
      markRejected: async (ctx: restate.ObjectContext, arg: CustodyLeg) =>
        await scope(arg.txScope, () => ctx.run('markRejected', () => deps.wallet.markRejectedById(arg.actionId, arg.observation))),
      resubmitPending: async (ctx: restate.ObjectContext) => await scope(undefined, () => ctx.run('resubmit', () => deps.wallet.resubmitPending()))
    }
  })
}

/** the custody bodies on a LocalExecutor — the local-mode wiring. */
export const registerCustodyHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void },
  deps: CustodyDeps
): void => {
  ex.register(EXEC_SVC.custody, 'submit', (arg) => deps.wallet.submit((arg as { actionId: string }).actionId))
  ex.register(EXEC_SVC.custody, 'markRejected', (arg) => {
    const a = arg as { actionId: string; observation: unknown }
    return deps.wallet.markRejectedById(a.actionId, a.observation)
  })
  ex.register(EXEC_SVC.custody, 'resubmitPending', () => deps.wallet.resubmitPending())
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
    const rows = await run('scan', () => deps.dueRows())
    // one journaled send per row, keyed by destination — per-host order +
    // cross-host parallelism; idempotent on actionId so a replayed or raced
    // drain can't double-send
    for (const item of rows)
      await send({ service: EXEC_SVC.transport, handler: 'deliver', key: item.destination }, item, { idempotencyKey: `deliver:${item.actionId}` })
    // self-schedule the ceiling at the earliest pending wake — the durable
    // successor of armOutboxTimer; empty box leaves the loop asleep until
    // outboxReady pokes it
    const next = await run('next', () => deps.nextAt())
    if (next !== undefined)
      await send({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {}, { delayMs: Math.max(0, next - deps.now()) })
    return { sent: rows.length, nextAt: next }
  }

export const transportObject = async (deps: TransportDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const drain = transportDrainBody(deps)
  return restate.object({
    name: EXEC_SVC.transport,
    handlers: {
      deliver: async (ctx: restate.ObjectContext, item: OutboxItem) => await ctx.run('deliver', () => deps.deliverItem(item)),
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
          else if (target.handler === 'drain') await transport.drain(arg, restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0 }))
          else throw new Error(`transport send: no handler ${target.handler}`)
        }
        return await drain(send, (n, fn) => ctx.run(n, fn))
      }
    }
  })
}

/** the transport bodies on a LocalExecutor — the local-mode wiring. */
export const registerTransportHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void } & { send: RtSend },
  deps: TransportDeps
): void => {
  const drain = transportDrainBody(deps)
  ex.register(EXEC_SVC.transport, 'deliver', (arg) => deps.deliverItem(arg as OutboxItem))
  ex.register(EXEC_SVC.transport, 'drain', () => drain((t, a, o) => ex.send(t, a, o)))
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
    const rows = await run('rows', () => deps.resubmitRows())
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
  return restate.service({
    name: EXEC_SVC.sweep,
    handlers: {
      tick: async (ctx: restate.Context) => await tick(send(ctx), (n, fn) => ctx.run(n, fn)),
      boot: async (ctx: restate.Context) => await boot(send(ctx), (n, fn) => ctx.run(n, fn))
    }
  })
}

/** register the same bodies on a LocalExecutor — the local-mode wiring. */
export const registerTxHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void },
  deps: RtDeps
): void => {
  const observe = txObserveBody(deps)
  const expire = txExpireBody(deps)
  const resubmit = txResubmitBody(deps)
  ex.register(EXEC_SVC.tx, 'observe', (arg, key) => observe(arg as TxObserve, key ?? ''))
  ex.register(EXEC_SVC.tx, 'expire', (arg, key) => expire(arg, key ?? ''))
  ex.register(EXEC_SVC.tx, 'resubmit', (arg, key) => resubmit(arg, key ?? ''))
}

/** the deal bodies on a LocalExecutor — the local-mode wiring. */
export const registerDealHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void },
  deps: DealDeps
): void => {
  const leg = dealAdvanceBody(deps)
  ex.register(EXEC_SVC.deal, 'advance', (arg, key) => leg(arg, key ?? ''))
  ex.register(EXEC_SVC.deal, 'due', (arg, key) => leg(arg, key ?? ''))
  ex.register(EXEC_SVC.deal, 'event', (arg) => deps.inboxEvent(arg as DealEvent))
  ex.register(EXEC_SVC.deal, 'reconcile', (arg, key) => deps.reconcileOp(key ?? ''))
}

// ── the dealfan service ──────────────────────────────────────────────────
// the tick's scan+fan-out legs as ONE journaled invocation: pending inbox
// items → deal.event on the deal's correlation key, NONTERMINAL ops →
// deal.advance per op key, due reconcile rows → deal.reconcile per op key.
// Each scan runs inside ctx.run — a replay re-sends the SAME snapshot (no
// row is re-scanned), and the sends go through ctx.objectSendClient so the
// journal dedupes them.

export interface DealfanDeps {
  pendingInbox: () => Promise<DealEvent[]>
  /** NONTERMINAL operation ids */
  liveOps: () => Promise<string[]>
  /** funding_failed/refunded op ids past their reconcile cooldown */
  reconcileOps: () => Promise<string[]>
  log: (line: string) => void
}

// the event key — the deal's correlation id. wantId is on every wire
// shape; an offer without it falls back to its own offerId, and a shape
// we can't read keys by its inbox row id (same FIFO guarantee, narrower
// fan-in).
export const dealEventKey = (i: DealEvent): string => {
  const p = i.payload as { wantId?: string; offerId?: string; msg?: { wantId?: string } } | null
  return `ev:${p?.msg?.wantId ?? p?.wantId ?? p?.offerId ?? i.messageId}`
}

export const dealfanTickBody =
  (deps: DealfanDeps) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    const inbox = await run('inbox', () => deps.pendingInbox())
    for (const i of inbox) await send({ service: EXEC_SVC.deal, handler: 'event', key: dealEventKey(i) }, i)
    const ops = await run('ops', () => deps.liveOps())
    for (const id of ops) await send({ service: EXEC_SVC.deal, handler: 'advance', key: id }, {})
    const rec = await run('reconcile', () => deps.reconcileOps())
    for (const id of rec) await send({ service: EXEC_SVC.deal, handler: 'reconcile', key: id }, {})
    return { events: inbox.length, advanced: ops.length, reconciled: rec.length }
  }

export const dealfanService = async (deps: DealfanDeps, dealDef: unknown) => {
  const restate = await import('@restatedev/restate-sdk')
  const tick = dealfanTickBody(deps)
  const send = (ctx: restate.Context): RtSend => {
    const deal = (key: string) =>
      ctx.objectSendClient(dealDef as never, key) as unknown as {
        advance: (o: unknown) => Promise<void>
        event: (o: DealEvent) => Promise<void>
        reconcile: (o: unknown) => Promise<void>
      }
    return async (target, arg) => {
      if (target.service === EXEC_SVC.deal) {
        const c = deal(target.key ?? '')
        if (target.handler === 'advance') await c.advance(arg)
        else if (target.handler === 'event') await c.event(arg as DealEvent)
        else if (target.handler === 'reconcile') await c.reconcile(arg)
        else throw new Error(`dealfan send: no deal handler ${target.handler}`)
      } else throw new Error(`dealfan send: no route for ${target.service}`)
    }
  }
  return restate.service({
    name: EXEC_SVC.dealfan,
    handlers: {
      tick: async (ctx: restate.Context) => await tick(send(ctx), (n, fn) => ctx.run(n, fn))
    }
  })
}

/** the dealfan bodies on a LocalExecutor — the local-mode wiring. */
export const registerDealfanHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void } & { send: RtSend },
  deps: DealfanDeps
): void => {
  const tick = dealfanTickBody(deps)
  ex.register(EXEC_SVC.dealfan, 'tick', () => tick((t, a, o) => ex.send(t, a, o)))
}

export const registerSweepHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void } & { send: RtSend },
  deps: SweepDeps
): void => {
  const tick = sweepTickBody(deps)
  const boot = sweepBootBody(deps)
  ex.register(EXEC_SVC.sweep, 'tick', () => tick((t, a, o) => ex.send(t, a, o)))
  ex.register(EXEC_SVC.sweep, 'boot', () => boot((t, a, o) => ex.send(t, a, o)))
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
  log: (line: string) => void
}

/** the cycle-equivalent through journaled sends — dealfan.tick is the
 *  deals pass's scan+fan-out, transport.drain is the outbox poke. The
 *  custody resubmit cadence stays on the floor where it belongs — wakes
 *  drive work, not time gates. */
const wakeCycle = async (send: RtSend): Promise<void> => {
  await send({ service: EXEC_SVC.dealfan, handler: 'tick' }, {})
  await send({ service: EXEC_SVC.transport, handler: 'drain', key: TRANSPORT_DRAIN_KEY }, {})
}

export const wakeTxProofBody =
  (deps: WakeDeps) =>
  async (send: RtSend, arg: { txid: string; mpHex: string }, run: StepRun = (_n, fn) => fn()) => {
    if (deps.legs.txProof !== undefined) await run('ledger', () => deps.legs.txProof!(arg.txid, arg.mpHex))
    deps.hint(arg.txid)
    await wakeCycle(send)
    return { done: true }
  }

export const wakeTxRejectedBody = (deps: WakeDeps) => async (send: RtSend, arg: { txid: string }) => {
  deps.hint(arg.txid)
  await wakeCycle(send)
  return { done: true }
}

export const wakeTxAbandonedBody =
  (deps: WakeDeps) =>
  async (send: RtSend, arg: { txid: string }, run: StepRun = (_n, fn) => fn()) => {
    if (deps.legs.txAbandoned !== undefined) await run('ledger', () => deps.legs.txAbandoned!(arg.txid))
    await wakeCycle(send)
    return { done: true }
  }

export const wakeHeightBody =
  (deps: WakeDeps) =>
  async (send: RtSend, arg: { tip: number }, run: StepRun = (_n, fn) => fn()) => {
    const due = deps.legs.dueAtHeight === undefined ? false : await run('due', () => deps.legs.dueAtHeight!(arg.tip))
    if (due) await send({ service: EXEC_SVC.dealfan, handler: 'tick' }, {})
    return { due }
  }

export const wakeReorgBody =
  (deps: WakeDeps) =>
  async (_send: RtSend, arg: { from: number }, run: StepRun = (_n, fn) => fn()) => {
    if (deps.legs.reorg !== undefined) await run('reorg', () => deps.legs.reorg!(arg.from))
    return { done: true }
  }

export const wakeService = async (deps: WakeDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const send = (ctx: restate.Context): RtSend => {
    const dealfan = ctx.serviceSendClient({ name: EXEC_SVC.dealfan } as never) as unknown as { tick: (o: unknown) => Promise<void> }
    const transport = ctx.objectSendClient({ name: EXEC_SVC.transport } as never, TRANSPORT_DRAIN_KEY) as unknown as { drain: (o: unknown) => Promise<void> }
    return async (target, arg) => {
      if (target.service === EXEC_SVC.dealfan && target.handler === 'tick') await dealfan.tick(arg)
      else if (target.service === EXEC_SVC.transport && target.handler === 'drain') await transport.drain(arg)
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
    handlers: {
      txProof: async (ctx: restate.Context, arg: { txid: string; mpHex: string }) => await proof(send(ctx), arg, (n, fn) => ctx.run(n, fn)),
      txRejected: async (ctx: restate.Context, arg: { txid: string }) => await rejected(send(ctx), arg),
      txAbandoned: async (ctx: restate.Context, arg: { txid: string }) => await abandoned(send(ctx), arg, (n, fn) => ctx.run(n, fn)),
      height: async (ctx: restate.Context, arg: { tip: number }) => await height(send(ctx), arg, (n, fn) => ctx.run(n, fn)),
      reorg: async (ctx: restate.Context, arg: { from: number }) => await reorg(send(ctx), arg, (n, fn) => ctx.run(n, fn))
    }
  })
}

/** the wake bodies on a LocalExecutor — the local-mode wiring. */
export const registerWakeHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void } & { send: RtSend },
  deps: WakeDeps
): void => {
  const s: RtSend = (t, a, o) => ex.send(t, a, o)
  ex.register(EXEC_SVC.wake, 'txProof', (arg) => wakeTxProofBody(deps)(s, arg as { txid: string; mpHex: string }))
  ex.register(EXEC_SVC.wake, 'txRejected', (arg) => wakeTxRejectedBody(deps)(s, arg as { txid: string }))
  ex.register(EXEC_SVC.wake, 'txAbandoned', (arg) => wakeTxAbandonedBody(deps)(s, arg as { txid: string }))
  ex.register(EXEC_SVC.wake, 'height', (arg) => wakeHeightBody(deps)(s, arg as { tip: number }))
  ex.register(EXEC_SVC.wake, 'reorg', (arg) => wakeReorgBody(deps)(s, arg as { from: number }))
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
  log: (line: string) => void
}

export const cronTickBody =
  (service: string, deps: CronDeps) =>
  async (send: RtSend, run: StepRun = (_n, fn) => fn()) => {
    await run('tick', () => deps.tick())
    await send({ service, handler: 'tick', key: CRON_KEY }, {}, { delayMs: deps.ceilingMs() })
    return { done: true }
  }

export const cronObject = async (service: string, deps: CronDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const tick = cronTickBody(service, deps)
  return restate.object({
    name: service,
    handlers: {
      tick: async (ctx: restate.ObjectContext) => {
        const self = ctx.objectSendClient({ name: service } as never, CRON_KEY) as unknown as { tick: (o: unknown, opts?: unknown) => Promise<void> }
        const send: RtSend = async (target, arg, opts) => {
          if (target.service !== service || target.handler !== 'tick') throw new Error(`cron send: no route ${target.service}/${target.handler}`)
          await self.tick(arg, restate.rpc.sendOpts({ delay: opts?.delayMs ?? 0 }))
        }
        // a self-SEND on the same key — one-way, no lock wait, no deadlock
        return await tick(send, (n, fn) => ctx.run(n, fn))
      }
    }
  })
}

/** the cron bodies on a LocalExecutor — the local-mode wiring. */
export const registerCronHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void } & { send: RtSend },
  service: string,
  deps: CronDeps
): void => {
  ex.register(service, 'tick', () => cronTickBody(service, deps)((t, a, o) => ex.send(t, a, o)))
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
    handlers: {
      peer: async (ctx: restate.ObjectContext, arg: { watch: string; service: string; query?: unknown }) =>
        await ctx.run('peer', () => deps.peer(arg.watch, arg.service, arg.query)),
      carrier: async (ctx: restate.ObjectContext, arg: { push: string; beefHex: string }) =>
        await ctx.run('carrier', () => deps.carrier(arg.push, arg.beefHex))
    }
  })
}

/** the notify bodies on a LocalExecutor — the local-mode wiring. */
export const registerNotifyHandlers = (
  ex: { register: (service: string, handler: string, fn: (arg: unknown, key?: string) => Promise<unknown>) => void },
  deps: NotifyDeps
): void => {
  ex.register(EXEC_SVC.notify, 'peer', (arg) => {
    const a = arg as { watch: string; service: string; query?: unknown }
    return deps.peer(a.watch, a.service, a.query)
  })
  ex.register(EXEC_SVC.notify, 'carrier', (arg) => {
    const a = arg as { push: string; beefHex: string }
    return deps.carrier(a.push, a.beefHex)
  })
}
