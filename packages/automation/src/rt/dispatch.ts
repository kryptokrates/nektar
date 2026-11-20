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
import { EXEC_SVC } from './exec.js'
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

export type DispatchHandler = (arg: unknown, key?: string) => Promise<unknown>

/** the registration surface openAutomation needs to serve bodies on an
 *  injected executor — Dispatcher implements it; RestateExecutor does not
 *  (its handlers live on the endpoint instead) */
export interface LocalDispatch extends Executor {
  register(service: string, handler: string, fn: DispatchHandler): void
  /** cancel armed delayed sends — called on runtime stop */
  close?(): void
}

export const isLocalDispatch = (e: Executor): e is LocalDispatch => typeof (e as LocalDispatch).register === 'function'

export class Dispatcher implements LocalDispatch {
  private readonly handlers = new Map<string, DispatchHandler>()
  private readonly timers = new Set<ReturnType<typeof setTimeout>>()
  private readonly pending = new Set<Promise<unknown>>()
  private inDispatch = 0

  constructor(private readonly log: (line: string) => void = () => {}) {}

  register(service: string, handler: string, fn: DispatchHandler): void {
    this.handlers.set(`${service}/${handler}`, fn)
  }

  private get(t: InvokeTarget): DispatchHandler {
    const fn = this.handlers.get(`${t.service}/${t.handler}`)
    if (fn === undefined) throw new Error(`executor: no handler ${t.service}/${t.handler}`)
    return fn
  }

  send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void> {
    const fn = this.get(target)
    const run = (): void => {
      // dispatched work exits the caller's leg — a send fired inside a
      // bound leg must not hand that deadline to the spawned invocation
      // (ALS propagates through queueMicrotask/setTimeout otherwise)
      const p = detached(async () => {
        this.inDispatch++
        try {
          await fn(arg, target.key)
        } finally {
          this.inDispatch--
        }
      })
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
    for (let i = 0; i < 50 && this.pending.size > 0; i++) await Promise.allSettled([...this.pending])
  }

  async call<T>(target: InvokeTarget, arg: unknown, _opts?: Pick<SendOpts, 'idempotencyKey'>): Promise<T> {
    const r = (await this.get(target)(arg, target.key)) as T
    // a call made INSIDE a dispatched send can't join — the pending set
    // contains that very dispatch; the outer caller's drain covers it.
    if (this.inDispatch === 0) await this.drain()
    return r
  }

  /** flush pending delayed sends */
  close(): void {
    for (const t of this.timers) clearTimeout(t)
    this.timers.clear()
  }
}

/** the fixture's send shim — the same fire-and-forget dispatch the send
 *  clients give inside a handler. */
const rigSend = (ex: LocalDispatch): RtSend => ex.send.bind(ex)

export const wireTx = (ex: LocalDispatch, deps: RtDeps): void => {
  const observe = txObserveBody(deps)
  const expire = txExpireBody(deps)
  const resubmit = txResubmitBody(deps)
  ex.register(EXEC_SVC.tx, 'observe', (arg, key) => observe(arg as TxObserve, key ?? ''))
  ex.register(EXEC_SVC.tx, 'expire', (arg, key) => expire(arg, key ?? ''))
  ex.register(EXEC_SVC.tx, 'resubmit', (arg, key) => resubmit(arg, key ?? ''))
}

export const wireSweep = (ex: LocalDispatch, deps: SweepDeps): void => {
  const tick = sweepTickBody(deps)
  const boot = sweepBootBody(deps)
  ex.register(EXEC_SVC.sweep, 'tick', () => tick(rigSend(ex)))
  ex.register(EXEC_SVC.sweep, 'boot', () => boot(rigSend(ex)))
}

export const wireDeal = (ex: LocalDispatch, deps: DealDeps): void => {
  const leg = dealAdvanceBody(deps)
  ex.register(EXEC_SVC.deal, 'advance', (arg, key) => leg(arg, key ?? ''))
  ex.register(EXEC_SVC.deal, 'due', (arg, key) => leg(arg, key ?? ''))
  ex.register(EXEC_SVC.deal, 'event', (arg) => deps.inboxEvent(arg as DealEvent))
  ex.register(EXEC_SVC.deal, 'reconcile', (arg, key) => deps.reconcileOp(key ?? ''))
}

export const wireDealfan = (ex: LocalDispatch, deps: DealfanDeps): void => {
  const tick = dealfanTickBody(deps)
  ex.register(EXEC_SVC.dealfan, 'tick', () => tick(rigSend(ex)))
}

export const wireCustody = (ex: LocalDispatch, deps: CustodyDeps): void => {
  ex.register(EXEC_SVC.custody, 'submit', (arg) => deps.wallet.submit((arg as CustodyLeg).actionId))
  ex.register(EXEC_SVC.custody, 'markRejected', (arg) => {
    const a = arg as CustodyLeg & { observation: unknown }
    return deps.wallet.markRejectedById(a.actionId, a.observation)
  })
  ex.register(EXEC_SVC.custody, 'resubmitPending', () => deps.wallet.resubmitPending())
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

export { TRANSPORT_DRAIN_KEY }
