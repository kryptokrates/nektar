// exec.ts — the executor seam: who runs a durable call/send.
//
// NEKTAR_EXECUTOR = 'restate' routes every durable send/call through the
// Restate sidecar — the server owns delivery, retries, journaling, timers
// and wakes. Anything else (default) is the local path: today's in-process
// dispatch with no sidecar required — identical semantics to the code this
// plane replaces.
//
// The same handler bodies back both executors — services.ts wraps them for
// the Restate endpoint, LocalExecutor dispatches to them directly — so
// memory-tier tests exercise the exact code the server invokes.
import { AsyncLocalStorage } from 'node:async_hooks'
import { nektarEnv, type EnvSource } from '@nektar/core'
import { WAITS } from '../waits.js'

export interface InvokeTarget {
  /** restate service/object/workflow name */
  service: string
  /** handler name on that service */
  handler: string
  /** the virtual-object / workflow key — omit for a basic service */
  key?: string
}

export interface SendOpts {
  /** delay the send — durable server-side under restate */
  delayMs?: number
  /** dedup the invocation at the ingress — same actionId discipline the outbox used */
  idempotencyKey?: string
}

export interface Executor {
  /** durable one-way delivery — resolves once the runtime accepted it */
  send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void>
  /** request-response — resolves with the handler's result */
  call<T>(target: InvokeTarget, arg: unknown): Promise<T>
}

/** service/object names — keep in sync with services.ts */
export const EXEC_SVC = {
  tx: 'nektar-tx',
  sweep: 'nektar-sweep',
  deal: 'nektar-deal',
  custody: 'nektar-custody',
  transport: 'nektar-transport'
} as const

/** the custody object's fixed key — every wallet custody leg serializes on
 *  this one object (the durable successor of the run() promise chain). */
export const CUSTODY_KEY = 'global'

// the inside-handler scope: a VO handler for key K emitting for K must NOT
// route back through the object — a same-key self-call deadlocks the
// exclusive object (the running handler would wait on itself). Handler
// bodies run inside this scope; emitters consult rtScopeKey() and go
// direct on a same-key hit. Cross-key emits still route — different
// object, no deadlock.
interface RtScope {
  txKey?: string
  /** inside a custody handler — same-key custody calls go direct */
  custodyKey?: string
}

const scope = new AsyncLocalStorage<RtScope>()

export const inRtScope = <T>(s: RtScope, fn: () => T): T => scope.run(s, fn)

export const rtScopeKey = (): RtScope | undefined => scope.getStore()

/** a journaled send inside a handler — restate backs it with
 *  ctx.objectSendClient (journal-deduped on replay); LocalExecutor backs
 *  it with ex.send. Callers never use the ingress inside a handler —
 *  unjournaled egress would double-fire on replay. */
export type RtSend = (target: { service: string; handler: string; key?: string }, arg: unknown, opts?: SendOpts) => Promise<unknown>

type Handler = (arg: unknown, key?: string) => Promise<unknown>

/** in-process dispatch — no sidecar. Delays are plain timers (the local
 *  executor is the test/dev path; the durable timer lives server-side in
 *  restate mode). */
export class LocalExecutor implements Executor {
  private readonly handlers = new Map<string, Handler>()
  private readonly timers = new Set<ReturnType<typeof setTimeout>>()
  private readonly pending = new Set<Promise<unknown>>()

  constructor(private readonly log: (line: string) => void = () => {}) {}

  register(service: string, handler: string, fn: Handler): void {
    this.handlers.set(`${service}/${handler}`, fn)
  }

  private get(t: InvokeTarget): Handler {
    const fn = this.handlers.get(`${t.service}/${t.handler}`)
    if (fn === undefined) throw new Error(`executor: no handler ${t.service}/${t.handler}`)
    return fn
  }

  send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void> {
    const fn = this.get(target)
    const run = (): void => {
      const p = fn(arg, target.key)
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

  /** await every fired send — the test seam that makes fan-out asserts
   *  deterministic (fire-and-forget in prod, joined in tests). */
  async drain(): Promise<void> {
    for (let i = 0; i < 50 && this.pending.size > 0; i++) await Promise.allSettled([...this.pending])
  }

  async call<T>(target: InvokeTarget, arg: unknown): Promise<T> {
    return (await this.get(target)(arg, target.key)) as T
  }

  /** flush pending delayed sends — test seam (stop() equivalent) */
  close(): void {
    for (const t of this.timers) clearTimeout(t)
    this.timers.clear()
  }
}

/** raw HTTP to the restate ingress — /restate/{call,send}/ paths per the
 *  server API; idempotency-key header carries the dedup discipline. */
export class RestateExecutor implements Executor {
  constructor(
    private readonly ingress: string,
    private readonly log: (line: string) => void = () => {}
  ) {}

  private url(verb: 'call' | 'send', t: InvokeTarget, delayMs?: number): string {
    const segs = t.key !== undefined ? `${t.service}/${encodeURIComponent(t.key)}/${t.handler}` : `${t.service}/${t.handler}`
    const q = delayMs !== undefined && delayMs > 0 ? `?delay=${Math.max(1, Math.round(delayMs / 1000))}s` : ''
    return `${this.ingress}/restate/${verb}/${segs}${q}`
  }

  private async post(url: string, arg: unknown, idempotencyKey?: string): Promise<Response> {
    return await fetch(url, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...(idempotencyKey !== undefined ? { 'idempotency-key': idempotencyKey } : {})
      },
      body: JSON.stringify(arg ?? null),
      // the ingress is local — a dead/wedged one fails fast rather than parking a send
      signal: AbortSignal.timeout(WAITS.ingressIo.ms)
    })
  }

  async send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void> {
    const r = await this.post(this.url('send', target, opts?.delayMs), arg, opts?.idempotencyKey)
    if (!r.ok) throw new Error(`restate send ${target.service}/${target.key ?? ''}/${target.handler}: ${r.status} ${await r.text().catch(() => '')}`)
  }

  async call<T>(target: InvokeTarget, arg: unknown): Promise<T> {
    const r = await this.post(this.url('call', target), arg)
    if (!r.ok) throw new Error(`restate call ${target.service}/${target.key ?? ''}/${target.handler}: ${r.status} ${await r.text().catch(() => '')}`)
    return (await r.json()) as T
  }
}

export function executorFromEnv(env: EnvSource = process.env, log?: (line: string) => void): Executor | undefined {
  if (nektarEnv('NEKTAR_EXECUTOR', env) !== 'restate') return undefined
  return new RestateExecutor(nektarEnv('NEKTAR_RESTATE_URL', env) ?? 'http://127.0.0.1:8080', log)
}

// process-wide seam — one executor per dock process, mirroring the
// txFsmLegacy-style env switches. openAutomation populates it; tests
// substitute via setExecutor.
let active: Executor | undefined

export function executor(): Executor | undefined {
  return (active ??= executorFromEnv())
}

export function setExecutor(e: Executor | undefined): void {
  active = e
}
