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
import { EXEC_SVC, CUSTODY_KEY, inRtScope } from './exec.js'
import {
  txObserveBody,
  txExpireBody,
  txResubmitBody,
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
  type DealEvent
} from './services.js'
import type { CustodyLeg, TxObserve } from './emit.js'
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

  send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void> {
    // a stopped executor accepts nothing — sends fired between close() and
    // store teardown must not dispatch against a closing runtime (the
    // post-teardown unhandledRejection class)
    if (this.closed) return Promise.resolve()
    const fn = this.get(target)
    const run = (): void => {
      if (this.closed) return
      // dispatched work exits the caller's leg — a send fired inside a
      // bound leg must not hand that deadline to the spawned invocation
      // (ALS propagates through queueMicrotask/setTimeout otherwise)
      const invoke = (): Promise<unknown> =>
        detached(async () => {
          this.inDispatch++
          try {
            await fn(arg, target.key)
          } finally {
            this.inDispatch--
          }
        })
      const p = target.key === undefined ? invoke() : this.chain(`${target.service}/${target.key}`, invoke)
      this.pending.add(p)
      void p
        .catch((e: unknown) => {
          this.log(`executor: ${target.service}/${target.handler}: ${(e as Error).message}`)
        })
        .finally(() => this.pending.delete(p))
    }
    if (opts?.delayMs !== undefined && opts.delayMs > 0) {
      const t = setTimeout(() => {
        this.timers.delete(t)
        run()
      }, opts.delayMs)
      this.timers.add(t)
    } else {
      queueMicrotask(run)
    }
    return Promise.resolve()
  }

  /** await every fired send — makes fan-out asserts deterministic. */
  async drain(): Promise<void> {
    for (let i = 0; i < 50 && this.pending.size > 0; i++) await Promise.allSettled(this.pending)
  }

  async call<T>(target: InvokeTarget, arg: unknown, _opts?: Pick<SendOpts, 'idempotencyKey'>): Promise<T> {
    if (this.closed) throw new Error('executor: closed')
    const fn = this.get(target)
    // snapshot BEFORE the handler runs: a top-level call must join the sends
    // its handler spawns — checking inDispatch after `r` resolves sees the
    // spawned legs themselves and wrongly skips the join (the legs then run
    // detached past the caller's boundary). A call made INSIDE a dispatched
    // send can't join — pending contains that very dispatch and draining
    // would deadlock on itself; the outer caller's drain covers it.
    const inner = this.inDispatch > 0
    const r = (await (target.key === undefined ? fn(arg, target.key) : this.chain(`${target.service}/${target.key}`, () => fn(arg, target.key)))) as T
    if (!inner) await this.drain()
    return r
  }

  /** flush pending delayed sends, then join in-flight work — called on
   *  runtime stop before the stores close; a queued dispatch that fires
   *  after kx.destroy() is the post-teardown unhandledRejection class */
  async close(): Promise<void> {
    this.closed = true
    for (const t of this.timers) clearTimeout(t)
    this.timers.clear()
    await this.drain()
  }
}

/** the fixture's send shim — the same fire-and-forget dispatch the send
 *  clients give inside a handler. */
const rigSend = (ex: LocalDispatch): RtSend => ex.send.bind(ex)

export const wireTx = (ex: LocalDispatch, deps: RtDeps): void => {
  const observe = txObserveBody(deps)
  const expire = txExpireBody(deps)
  const resubmit = txResubmitBody(deps)
  // inRtScope mirrors the VO handlers: same-key emits inside the handler
  // must go direct — routing them back through exec.call would wait on
  // this very invocation (the exclusive-key self-deadlock)
  ex.register(EXEC_SVC.tx, 'observe', (arg, key) => inRtScope({ txKey: key ?? '' }, () => observe(arg as TxObserve, key ?? '')))
  ex.register(EXEC_SVC.tx, 'expire', (arg, key) => inRtScope({ txKey: key ?? '' }, () => expire(arg, key ?? '')))
  ex.register(EXEC_SVC.tx, 'resubmit', (arg, key) => inRtScope({ txKey: key ?? '' }, () => resubmit(arg, key ?? '')))
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
  ex.register(EXEC_SVC.dealfan, 'tick', () => tick(rigSend(ex)))
}

export const wireCustody = (ex: LocalDispatch, deps: CustodyDeps): void => {
  // same scope the VO sets: custodyKey makes nested custodySubmit legs go
  // direct inside the handler; the forwarded txScope re-establishes the
  // caller's held tx key so its emits skip the locked object
  const scope = <T>(txScope: string | undefined, fn: () => Promise<T>): Promise<T> => inRtScope({ custodyKey: CUSTODY_KEY, txKey: txScope }, fn)
  ex.register(EXEC_SVC.custody, 'submit', (arg) => scope((arg as CustodyLeg).txScope, () => deps.wallet.submit((arg as CustodyLeg).actionId)))
  ex.register(EXEC_SVC.custody, 'markRejected', (arg) => {
    const a = arg as CustodyLeg & { observation: unknown }
    return scope(a.txScope, () => deps.wallet.markRejectedById(a.actionId, a.observation))
  })
  ex.register(EXEC_SVC.custody, 'resubmitPending', (arg) => scope((arg as CustodyLeg | undefined)?.txScope, () => deps.wallet.resubmitPending()))
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
  ex.register(service, 'tick', () => cronTickBody(service, deps)(rigSend(ex)))
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
  fx?: import('../machines/effects.js').TxEffects
  submit?: RtDeps['submit']
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
  const liveOps = async () => (await d.store.operations({ states: NONTERMINAL })).map((o) => ({ operationId: o.operationId, wantId: opWantId(o) }))
  const reconcileOps = async () => {
    const now = Date.now()
    return (await d.store.operations({ role: 'buyer', states: ['funding_failed', 'refunded'], reconcileDue: now })).map((o) => ({
      operationId: o.operationId,
      wantId: opWantId(o)
    }))
  }
  wireRuntime(ex, {
    tx: { store: d.store, fx: d.fx ?? {}, log, submit: d.submit ?? ((actionId) => engine.submitTx(actionId)) },
    sweep: {
      scan: d.scan ?? async function* () {},
      resubmitRows: d.resubmitRows ?? (async () => []),
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
      log
    },
    transport: {
      dueRows: () => d.store.dueOutbox(),
      nextAt: () => d.store.nextOutboxAt(),
      deliverItem: (item) => engine.deliverOutboxItem(item),
      now: d.now ?? (() => Date.now()),
      log
    },
    custody: engine.walletCoordinator !== undefined ? { wallet: engine.walletCoordinator, log } : undefined,
    wake: d.wake ?? { legs: {}, hint: () => {}, log }
  })
}

export { TRANSPORT_DRAIN_KEY }
