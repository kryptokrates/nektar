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
import type { AutomationStore, TxState, WalletTx } from '../store.js'
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
