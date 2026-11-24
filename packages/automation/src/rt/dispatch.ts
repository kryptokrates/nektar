// dispatch.ts — an in-process Executor + the registration glue that wires
// the service handler bodies onto it. There is no local mode in production —
// durable execution is Restate, period — but memory-tier tests run several
// automations in one process with no server: each runtime gets its own
// Dispatcher via cfg.exec and openAutomation wires the same bodies the
// endpoint would serve. No port, no registration, no shared handler map.
//
// Semantics mirror the ingress: send() is fire-and-forget dispatch
// (delayMs arms a plain timer — server-side durable timers don't exist
// in-process), call() invokes and then joins the sends it spawned (the
// "one call = the whole pass" contract), drain() awaits every fired send
// so fan-out asserts are deterministic.
import { detached } from '@nektar/core'
import type { Executor, InvokeTarget, SendOpts, RtSend } from './exec.js'
import { EXEC_SVC, inRtScope, PermanentError, rtScopeKey } from './exec.js'
import {
  txObserveBody,
  sweepTickBody,
  sweepBootBody,
  dealAdvanceBody,
  dealfanTickBody,
  transportDrainBody,
  cronTickBody,
  wakeTxProofBody,
  wakeTxRejectedBody,
  wakeTxAbandonedBody,
  wakeHeightBody,
  wakeReorgBody,
  TRANSPORT_DRAIN_KEY,
  opWantId,
  type RtDeps,
  type SweepDeps,
  type DealDeps,
  type DealfanDeps,
  type CustodyDeps,
  type TransportDeps,
  type WakeDeps,
  type CronDeps,
  type NotifyDeps,
  type DealEvent,
  type TickArg
} from './services.js'
import type { CustodyLeg, CustodyResubmitArg, TxObserve } from './emit.js'
import type { OutboxItem } from '../store.js'
import { NONTERMINAL } from '../market.js'

export type DispatchHandler = (arg: unknown, key?: string) => Promise<unknown>

/** the registration surface openAutomation needs to serve bodies on an
 *  injected executor — Dispatcher implements it; RestateExecutor does not
 *  (its handlers live on the endpoint instead) */
export interface LocalDispatch extends Executor {
  register(service: string, handler: string, fn: DispatchHandler): void
  /** cancel armed delayed sends + join in-flight work — called on runtime
   *  stop before the stores close */
  close?(): void | Promise<void>
}

export const isLocalDispatch = (e: Executor): e is LocalDispatch => typeof (e as LocalDispatch).register === 'function'

export class Dispatcher implements LocalDispatch {
  private readonly handlers = new Map<string, DispatchHandler>()
  private readonly timers = new Set<ReturnType<typeof setTimeout>>()
  private readonly pending = new Set<Promise<unknown>>()
  private readonly chains = new Map<string, Promise<unknown>>()
  // ingress idempotency-key semantics, mirrored: the key maps to ONE
  // invocation for the whole retention window — a re-sent send with the
  // same key attaches to the original (no new dispatch), and so does a
  // re-sent call (which resolves to the original's result, error included).
  // The map never expires entries: tests don't outlive the window, and a
  // completed invocation still dedupes later sends — that persistence is
  // exactly the rt/dedup-kills-retry class this models.
  private readonly idempotent = new Map<string, Promise<unknown>>()
  private readonly idemResolve = new Map<string, { res: (v: unknown) => void; rej: (e: unknown) => void }>()
  // the chain scope each pending invocation is queued on — work behind a
  // call-held chain CANNOT run until that call's handler returns, so a
  // drain() must not join it (waiting on work that waits on you is the
  // deadlock this pair exists to refuse). callScopes marks scopes with an
  // in-flight call — global, on purpose: the drain exclusion is about who
  // CAN run, not who is asking.
  private readonly pendingScopes = new Map<Promise<unknown>, string>()
  private readonly callScopes = new Set<string>()
  private inDispatch = 0
  private closed = false

  constructor(private readonly log: (line: string) => void = () => {}) {}

  register(service: string, handler: string, fn: DispatchHandler): void {
    this.handlers.set(`${service}/${handler}`, fn)
  }

  private get(t: InvokeTarget): DispatchHandler {
    const fn = this.handlers.get(`${t.service}/${t.handler}`)
    if (fn === undefined) throw new Error(`executor: no handler ${t.service}/${t.handler}`)
    return fn
  }

  /** per-key mutual exclusion — the VO's exclusive lock, mirrored: two
   *  invocations for the same object key run one-at-a-time in send order,
   *  never concurrently. A failed leg doesn't poison the chain — the next
   *  same-key invocation still runs (the runtime's retry discipline, not
   *  the chain's, carries failures) */
  private chain(scope: string, fn: () => Promise<unknown>): Promise<unknown> {
    const prev = this.chains.get(scope) ?? Promise.resolve()
    const p = prev.then(fn, fn)
    this.chains.set(scope, p)
    // both-branches then: finally() produces a fresh promise that REJECTS
    // when p does — with nothing catching it the cleanup itself becomes an
    // unhandledRejection
    void p.then(
      () => {
        if (this.chains.get(scope) === p) this.chains.delete(scope)
      },
      () => {
        if (this.chains.get(scope) === p) this.chains.delete(scope)
      }
    )
    return p
  }

  private idemKey(t: InvokeTarget, k: string): string {
    return `${t.service}/${t.key ?? ''}/${t.handler}#${k}`
  }

  send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void> {
    // a stopped executor accepts nothing — sends fired between close() and
    // store teardown must not dispatch against a closing runtime (the
    // post-teardown unhandledRejection class)
    if (this.closed) return Promise.resolve()
    const fn = this.get(target) // throws first — an unrouted send records no key
    // ingress dedup — same key = the original invocation, nothing new runs.
    // Recorded at accept-time: a delayed arm is already the invocation, so a
    // dup arriving during the delay dedupes too
    if (opts?.idempotencyKey !== undefined) {
      const k = this.idemKey(target, opts.idempotencyKey)
      if (this.idempotent.has(k)) return Promise.resolve()
      const tracked = new Promise<unknown>((res, rej) => {
        this.idemResolve.set(k, { res, rej })
      })
      this.idempotent.set(k, tracked)
      void tracked.catch(() => undefined) // the stored copy never surfaces as unhandled
    }
    const run = (): void => {
      if (this.closed) return
      // dispatched work exits the caller's leg — a send fired inside a
      // bound leg must not hand that deadline to the spawned invocation
      // (ALS propagates through queueMicrotask/setTimeout otherwise). The rt
      // scope exits too: a fresh invocation holds NO keys — inheriting the
      // spawner's txKey/custodyKey would let it "go direct" past locks it
      // never took, a scope real Restate can't produce.
      const scope = target.key === undefined ? undefined : `${target.service}/${target.key}`
      const invoke = (): Promise<unknown> =>
        detached(async () =>
          inRtScope({ txKey: undefined, custodyKey: undefined, held: scope !== undefined ? [scope] : [] }, async () => {
            if (this.closed) return undefined // queued behind a call that outlived close()
            this.inDispatch++
            try {
              return await fn(arg, target.key)
            } finally {
              this.inDispatch--
            }
          })
        )
      const p = scope === undefined ? invoke() : this.chain(scope, invoke)
      // the tracked dedup record settles with the invocation — a dup call
      // on the same key attaches to this outcome, failure included (an
      // attach that silently resolved undefined would hide a dead send)
      const k = opts?.idempotencyKey !== undefined ? this.idemKey(target, opts.idempotencyKey) : undefined
      void p.then(
        (v) => {
          if (k !== undefined) this.idemResolve.get(k)?.res(v)
        },
        (e) => {
          if (k !== undefined) {
            const waiter = this.idemResolve.get(k)
            if (!(e instanceof PermanentError)) {
              this.idempotent.delete(k)
              this.idemResolve.delete(k)
            }
            waiter?.rej(e)
          }
        }
      )
      this.pending.add(p)
      if (scope !== undefined) this.pendingScopes.set(p, scope)
      void p
        .catch((e: unknown) => {
          this.log(`executor: ${target.service}/${target.handler}: ${(e as Error).message}`)
        })
        .finally(() => {
          this.pending.delete(p)
          this.pendingScopes.delete(p)
        })
    }
    if (opts?.delayMs !== undefined && opts.delayMs > 0) {
      // setTimeout is int32 — a delayed send past ~24.8d (the 180-day
      // refund_pending bound is the live case) overflows to 1ms and warns.
      // Clamp to the platform ceiling: the fired handler re-checks the domain
      // deadline, so an early arm is one wasted wake, never a verdict; the
      // durable executor carries true horizons over the wire.
      const t = setTimeout(
        () => {
          this.timers.delete(t)
          run()
        },
        Math.min(opts.delayMs, 2 ** 31 - 1)
      )
      this.timers.add(t)
    } else {
      queueMicrotask(run)
    }
    return Promise.resolve()
  }

  /** await every fired send — makes fan-out asserts deterministic. Sends
   *  queued behind a chain an in-flight call() still holds are excluded:
   *  they cannot run until that call returns, so joining them from the call
   *  itself is a deadlock by construction. The outer caller joins them once
   *  the key releases (close()'s drain sees them unblocked). */
  async drain(): Promise<void> {
    for (let i = 0; i < 50; i++) {
      const joinable = [...this.pending].filter((p) => {
        const s = this.pendingScopes.get(p)
        return s === undefined || !this.callScopes.has(s)
      })
      if (joinable.length === 0) return
      await Promise.allSettled(joinable)
      if (this.pending.size === 0) return
    }
  }

  async call<T>(target: InvokeTarget, arg: unknown, opts?: Pick<SendOpts, 'idempotencyKey'>): Promise<T> {
    if (this.closed) throw new Error('executor: closed')
    const fn = this.get(target)
    // ingress dedup — a call with an existing key attaches to the original
    // invocation and resolves to ITS result (error included), no new run
    let idemSet: { key: string } | undefined
    if (opts?.idempotencyKey !== undefined) {
      const k = this.idemKey(target, opts.idempotencyKey)
      const prior = this.idempotent.get(k)
      if (prior !== undefined) return (await prior) as T
      idemSet = { key: k }
    }
    // snapshot BEFORE the handler runs: a top-level call must join the sends
    // its handler spawns — checking inDispatch after `r` resolves sees the
    // spawned legs themselves and wrongly skips the join (the legs then run
    // detached past the caller's boundary). A call made INSIDE a dispatched
    // send can't join — pending contains that very dispatch and draining
    // would deadlock on itself; the outer caller's drain covers it.
    const inner = this.inDispatch > 0
    const scope = target.key === undefined ? undefined : `${target.service}/${target.key}`
    // same-key self-call: THIS invocation already holds the chain, so
    // queueing behind it hangs forever — the handler would wait on itself.
    // 'held' is per-invocation (ALS): a scope some OTHER leg holds is not a
    // deadlock, the call just serializes on the object's FIFO — the normal
    // Restate same-key ordering (resubmit's emit + reconcile's markRejected
    // racing one txid both land). Real Restate relies on rtScopeKey to route
    // same-key work direct — a raw call here is a caller bug; a thrown error
    // beats a silent hang.
    if (scope !== undefined && rtScopeKey()?.held?.includes(scope))
      throw new Error(`executor: re-entrant call on held key ${scope} — route same-key work through inRtScope`)
    const parentHeld = rtScopeKey()?.held ?? []
    const nextHeld = scope !== undefined ? [...parentHeld, scope] : [...parentHeld]
    const invoke = (): Promise<unknown> => detached(() => inRtScope({ txKey: undefined, custodyKey: undefined, held: nextHeld }, () => fn(arg, target.key)))
    const p = scope === undefined ? invoke() : this.chain(scope, invoke)
    // the record IS the invocation promise — a dup call attaches mid-flight
    // and resolves to the same result, terminal error included. Transient
    // errors evict so caller retries run fresh (real Restate retries them
    // rather than caching a transient throw as terminal).
    if (idemSet !== undefined) {
      const ik = idemSet.key
      this.idempotent.set(ik, p)
      void p.catch((e: unknown) => {
        if (!(e instanceof PermanentError) && this.idempotent.get(ik) === p) this.idempotent.delete(ik)
      })
    }
    // a call is in-flight work too — close() must join it, or a handler
    // still running fires post-teardown sends against closing stores
    this.pending.add(p)
    if (scope !== undefined) {
      this.pendingScopes.set(p, scope)
      this.callScopes.add(scope)
    }
    try {
      const r = (await p) as T
      if (!inner) await this.drain()
      return r
    } finally {
      this.pending.delete(p)
      this.pendingScopes.delete(p)
      if (scope !== undefined) this.callScopes.delete(scope)
    }
  }

  /** flush pending delayed sends, then join in-flight work — called on
   *  runtime stop before the stores close; a queued dispatch that fires
   *  after kx.destroy() is the post-teardown unhandledRejection class */
  async close(): Promise<void> {
    this.closed = true
    for (const t of this.timers) clearTimeout(t)
    this.timers.clear()
    // a dup call attaching to a never-fired send must not hang on the
    // tracked record after close — settle every outstanding record
    for (const r of this.idemResolve.values()) r.res(undefined)
    this.idemResolve.clear()
    this.idempotent.clear()
    await this.drain()
  }
}

/** the fixture's send shim — the same fire-and-forget dispatch the send
 *  clients give inside a handler. */
const rigSend = (ex: LocalDispatch): RtSend => ex.send.bind(ex)

export const wireTx = (ex: LocalDispatch, deps: RtDeps): void => {
  const observe = txObserveBody(deps)
  // inRtScope mirrors the VO handlers: same-key emits inside the handler
  // must go direct — routing them back through exec.call would wait on
  // this very invocation (the exclusive-key self-deadlock)
  ex.register(EXEC_SVC.tx, 'observe', (arg, key) => inRtScope({ txKey: key ?? '' }, () => observe(arg as TxObserve, key ?? '')))
}

export const wireSweep = (ex: LocalDispatch, deps: SweepDeps): void => {
  const tick = sweepTickBody(deps)
  const boot = sweepBootBody(deps)
  ex.register(EXEC_SVC.sweep, 'tick', () => tick(rigSend(ex)))
  ex.register(EXEC_SVC.sweep, 'boot', () => boot(rigSend(ex)))
}

export const wireDeal = (ex: LocalDispatch, deps: DealDeps): void => {
  const leg = dealAdvanceBody(deps)
  ex.register(EXEC_SVC.deal, 'advance', (arg, key) => leg(arg as { op?: string } | undefined, key ?? ''))
  ex.register(EXEC_SVC.deal, 'due', (arg, key) => leg(arg as { op?: string } | undefined, key ?? ''))
  ex.register(EXEC_SVC.deal, 'event', (arg) => deps.inboxEvent(arg as DealEvent))
  ex.register(EXEC_SVC.deal, 'reconcile', (arg, key) => deps.reconcileOp((arg as { op?: string } | undefined)?.op ?? key ?? ''))
}

export const wireDealfan = (ex: LocalDispatch, deps: DealfanDeps): void => {
  const tick = dealfanTickBody(deps)
  ex.register(EXEC_SVC.dealfan, 'tick', (arg) => tick(rigSend(ex), undefined, arg as TickArg | undefined))
}

export const wireCustody = (ex: LocalDispatch, deps: CustodyDeps): void => {
  // same scope the VO sets: custodyKey records the handler's held key
  // (actionId per tx leg, 'global' for the scan) so nested custody legs
  // for the same id go direct
  const scope = <T>(key: string, fn: () => Promise<T>): Promise<T> => inRtScope({ custodyKey: key }, fn)
  ex.register(EXEC_SVC.custody, 'submit', (arg, key) => scope(key ?? '', () => deps.wallet.submit((arg as CustodyLeg).actionId)))
  ex.register(EXEC_SVC.custody, 'markRejected', (arg, key) => {
    const a = arg as CustodyLeg & { observation: unknown }
    return scope(key ?? '', () => deps.wallet.markRejectedById(a.actionId, a.observation))
  })
  ex.register(EXEC_SVC.custody, 'resubmitPending', async (arg, key) => {
    const ids = await scope(key ?? '', () => deps.wallet.resubmitCandidates())
    if ((arg as CustodyResubmitArg | null | undefined)?.fanOnly === true) {
      for (const actionId of ids) await ex.send({ service: EXEC_SVC.custody, handler: 'submit', key: actionId }, { actionId } satisfies CustodyLeg)
      return [] as Array<{ actionId: string; state: import('../store.js').TxState }>
    }
    const settled = await Promise.allSettled(
      ids.map((actionId) =>
        ex
          .call<{ state: import('../store.js').TxState }>({ service: EXEC_SVC.custody, handler: 'submit', key: actionId }, { actionId } satisfies CustodyLeg)
          .then((r) => ({ actionId, state: r.state }))
      )
    )
    return settled
      .filter((r): r is PromiseFulfilledResult<{ actionId: string; state: import('../store.js').TxState }> => r.status === 'fulfilled')
      .map((r) => r.value)
  })
}

export const wireTransport = (ex: LocalDispatch, deps: TransportDeps): void => {
  const drain = transportDrainBody(deps)
  ex.register(EXEC_SVC.transport, 'deliver', (arg) => deps.deliverItem(arg as OutboxItem))
  ex.register(EXEC_SVC.transport, 'drain', () => drain(rigSend(ex)))
}

export const wireWake = (ex: LocalDispatch, deps: WakeDeps): void => {
  const s: RtSend = rigSend(ex)
  ex.register(EXEC_SVC.wake, 'txProof', (arg) => wakeTxProofBody(deps)(s, arg as { txid: string; mpHex: string }))
  ex.register(EXEC_SVC.wake, 'txRejected', (arg) => wakeTxRejectedBody(deps)(s, arg as { txid: string }))
  ex.register(EXEC_SVC.wake, 'txAbandoned', (arg) => wakeTxAbandonedBody(deps)(s, arg as { txid: string }))
  ex.register(EXEC_SVC.wake, 'height', (arg) => wakeHeightBody(deps)(s, arg as { tip: number }))
  ex.register(EXEC_SVC.wake, 'reorg', (arg) => wakeReorgBody(deps)(s, arg as { from: number }))
}

export const wireCron = (ex: LocalDispatch, service: string, deps: CronDeps): void => {
  ex.register(service, 'tick', (arg) => cronTickBody(service, deps)(rigSend(ex), undefined, arg as TickArg | undefined))
}

export const wireLeg = (ex: LocalDispatch, service: string, leg: () => Promise<void>): void => {
  ex.register(service, 'tick', () => leg())
}

export const wireNotify = (ex: LocalDispatch, deps: NotifyDeps): void => {
  ex.register(EXEC_SVC.notify, 'peer', (arg) => {
    const a = arg as { watch: string; service: string; query?: unknown }
    return deps.peer(a.watch, a.service, a.query)
  })
  ex.register(EXEC_SVC.notify, 'carrier', (arg) => {
    const a = arg as { push: string; beefHex: string }
    return deps.carrier(a.push, a.beefHex)
  })
}

/** the full automation service set on one dispatcher — the wire-path twin
 *  of the def list openEndpoint serves. runtime.ts calls this when cfg.exec
 *  injects a LocalDispatch; tests call the pieces they need directly. */
export interface WireAll {
  tx: RtDeps
  sweep: SweepDeps
  deal: DealDeps
  dealfan: DealfanDeps
  transport: TransportDeps
  custody?: CustodyDeps
  wake: WakeDeps
  /** cron services — [service, deps] pairs (upkeep/watchdog/watches) */
  crons?: Array<[service: string, deps: CronDeps]>
  notify?: NotifyDeps
}

export const wireRuntime = (ex: LocalDispatch, d: WireAll): void => {
  wireTx(ex, d.tx)
  wireSweep(ex, d.sweep)
  wireDeal(ex, d.deal)
  wireDealfan(ex, d.dealfan)
  wireTransport(ex, d.transport)
  if (d.custody !== undefined) wireCustody(ex, d.custody)
  wireWake(ex, d.wake)
  for (const [svc, deps] of d.crons ?? []) wireCron(ex, svc, deps)
  if (d.notify !== undefined) wireNotify(ex, d.notify)
}

/** the subset a standalone MarketEngine needs — derived from the engine's
 *  own surface the way runtime.ts builds them. Tests construct the engine
 *  with `executor: ex` first (the dealfanDeps closures read it lazily), then
 *  wireEngine(ex, engine, {store, fx, ...}) completes the service map. */
export interface WireEngineDeps {
  store: import('../store.js').AutomationStore
  fx: import('../machines/effects.js').TxEffects
  scan?: SweepDeps['scan']
  resubmitRows?: SweepDeps['resubmitRows']
  wake?: WakeDeps
  now?: () => number
  ceilingMs?: () => number
  log?: (line: string) => void
}

interface EngineSurface {
  pendingInbox(): Promise<DealEvent[]>
  resubmitTick(): Promise<void>
  advanceOp(operationId: string): Promise<boolean>
  dealEvent(i: DealEvent): Promise<void>
  reconcileOp(operationId: string): Promise<void>
  deliverOutboxItem(item: OutboxItem): Promise<'acked' | 'retried' | 'deferred' | 'skipped'>
  submitTx(actionId: string): Promise<{ state: import('../store.js').TxState; txid: string; detail?: string } | undefined>
  readonly walletCoordinator?: CustodyDeps['wallet']
}

export const wireEngine = (ex: LocalDispatch, engine: EngineSurface, d: WireEngineDeps): void => {
  const log = d.log ?? (() => {})
  let fanSeq = 0
  const liveOps = async () => (await d.store.operations({ states: NONTERMINAL })).map((o) => ({ operationId: o.operationId, wantId: opWantId(o) }))
  const reconcileOps = async () => {
    // reconcile_at is stamped on the store's clock — compare on the caller's
    // clock seam (d.now) so a virtual `now` selects due rows the same way
    const now = (d.now ?? Date.now)()
    return (await d.store.operations({ role: 'buyer', states: ['funding_failed', 'refunded'], reconcileDue: now })).map((o) => ({
      operationId: o.operationId,
      wantId: opWantId(o)
    }))
  }
  wireRuntime(ex, {
    tx: { store: d.store, fx: d.fx, log },
    sweep: {
      scan: d.scan ?? async function* () {},
      resubmitRows: d.resubmitRows ?? (() => Promise.resolve([])),
      log
    },
    deal: {
      advanceOp: (id) => engine.advanceOp(id),
      inboxEvent: (i) => engine.dealEvent(i),
      reconcileOp: (id) => engine.reconcileOp(id),
      log
    },
    dealfan: {
      pendingInbox: () => engine.pendingInbox(),
      liveOps,
      reconcileOps,
      resubmitPending: () => engine.resubmitTick(),
      ceilingMs: d.ceilingMs ?? (() => 0),
      now: d.now ?? (() => Date.now()),
      tickSeq: () => ++fanSeq,
      log
    },
    transport: {
      dueRows: () => d.store.dueOutbox(),
      nextAt: (excludeIds) => d.store.nextOutboxAt(excludeIds !== undefined && excludeIds.length > 0 ? { excludeIds } : undefined),
      deliverItem: (item) => engine.deliverOutboxItem(item),
      now: d.now ?? (() => Date.now()),
      log
    },
    custody: engine.walletCoordinator !== undefined ? { wallet: engine.walletCoordinator, log } : undefined,
    wake: d.wake ?? { legs: {}, hint: () => {}, log }
  })
}

export { TRANSPORT_DRAIN_KEY }
