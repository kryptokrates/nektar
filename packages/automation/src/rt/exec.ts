// exec.ts — the executor seam: who runs a durable call/send.
//
// Restate is the only executor. Every durable send/call goes through the
// Restate sidecar's ingress — the server owns delivery, retries, journaling,
// timers and wakes. There is no local mode and no env opt-out: a nektar
// deployment runs beside a restate server, period.
//
// The handler bodies back the endpoint's Restate handlers — services.ts
// wraps them in ctx.run steps; tests invoke the same bodies directly.
import { AsyncLocalStorage } from 'node:async_hooks'
import { nektarEnv } from '@nektar/core'
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
  call<T>(target: InvokeTarget, arg: unknown, opts?: Pick<SendOpts, 'idempotencyKey'>): Promise<T>
}

/** service/object names — keep in sync with services.ts */
export const EXEC_SVC = {
  tx: 'nektar-tx',
  sweep: 'nektar-sweep',
  deal: 'nektar-deal',
  dealfan: 'nektar-dealfan',
  custody: 'nektar-custody',
  transport: 'nektar-transport',
  wake: 'nektar-wake',
  upkeep: 'nektar-upkeep',
  watchdog: 'nektar-watchdog',
  watches: 'nektar-watches',
  notify: 'nektar-notify',
  /** host-side defs (mcp's watcher region) registered via cfg.rtDefs — same
   *  endpoint, same server, journaled alongside the automation services */
  confirmer: 'nektar-confirmer',
  watcher: 'nektar-watcher'
} as const

/** the custody object's scan key — resubmitPending runs on this single
 *  stream; per-tx legs (submit/markRejected) key on their actionId so one
 *  deal's broadcast never serializes behind another's. */
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
  /** the 'service/key' scopes THIS invocation holds — accumulated as calls
   *  nest (a custody call inside a tx handler holds both). The dispatcher's
   *  re-entrancy guard reads it: a call on a scope this same invocation
   *  already holds queues behind itself — a real deadlock to refuse loudly.
   *  A scope held by a DIFFERENT leg is not here — that call just serializes
   *  on the object's FIFO, the normal same-key ordering. */
  held?: readonly string[]
}

const scope = new AsyncLocalStorage<RtScope>()

// merge, not replace: a nested handler's scope must ACCUMULATE the keys the
// call chain already holds — a tx handler inside a custody invocation still
// holds that custody key, so its custody-side emits must go direct rather
// than calling back into the locked object (the self-deadlock the scope
// exists to prevent). An undefined field clears (a caller can deliberately
// drop a held key); a defined field wins over the parent's.
export const inRtScope = <T>(s: RtScope, fn: () => T): T => scope.run({ ...scope.getStore(), ...s }, fn)

export const rtScopeKey = (): RtScope | undefined => scope.getStore()

/** a journaled send inside a handler — backed by ctx.objectSendClient
 *  (journal-deduped on replay). Callers never use the ingress inside a
 *  handler — unjournaled egress would double-fire on replay. */
export type RtSend = (target: { service: string; handler: string; key?: string }, arg: unknown, opts?: SendOpts) => Promise<unknown>

/** raw HTTP to the restate ingress — /restate/{call,send}/ paths per the
 *  server API; idempotency-key header carries the dedup discipline. */
export class RestateExecutor implements Executor {
  constructor(
    private readonly ingress: string,
    private readonly log: (line: string) => void = () => {}
  ) {}

  private url(verb: 'call' | 'send', t: InvokeTarget, delayMs?: number): string {
    const segs = t.key !== undefined ? `${t.service}/${encodeURIComponent(t.key)}/${t.handler}` : `${t.service}/${t.handler}`
    // sub-second delays stay ms — quantizing up to `1s` would inflate a
    // near-term arm by orders of magnitude (a 250ms poke landing at 1s)
    const q =
      delayMs !== undefined && delayMs > 0
        ? `?delay=${delayMs >= 1000 ? `${Math.max(1, Math.round(delayMs / 1000))}s` : `${Math.max(1, Math.round(delayMs))}ms`}`
        : ''
    return `${this.ingress}/restate/${verb}/${segs}${q}`
  }

  private async post(url: string, arg: unknown, ioMs: number, idempotencyKey?: string): Promise<Response> {
    return await fetch(url, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...(idempotencyKey !== undefined ? { 'idempotency-key': idempotencyKey } : {})
      },
      body: JSON.stringify(arg ?? null),
      // the ingress is local — a dead/wedged one fails fast rather than parking a send
      signal: AbortSignal.timeout(ioMs)
    })
  }

  async send(target: InvokeTarget, arg: unknown, opts?: SendOpts): Promise<void> {
    const r = await this.post(this.url('send', target, opts?.delayMs), arg, WAITS.ingressIo.ms, opts?.idempotencyKey)
    if (!r.ok) throw new Error(`restate send ${target.service}/${target.key ?? ''}/${target.handler}: ${r.status} ${await r.text().catch(() => '')}`)
  }

  async call<T>(target: InvokeTarget, arg: unknown, opts?: Pick<SendOpts, 'idempotencyKey'>): Promise<T> {
    // a call waits for the handler's whole key queue + leg — its abort must
    // outlive the worst serialized leg, not just the socket. An aborted call
    // does NOT cancel the invocation: it keeps running detached and its
    // effect may still land — read abort as "unknown", never "failed"
    const r = await this.post(this.url('call', target), arg, WAITS.callIo.ms, opts?.idempotencyKey)
    if (!r.ok) throw new Error(`restate call ${target.service}/${target.key ?? ''}/${target.handler}: ${r.status} ${await r.text().catch(() => '')}`)
    return (await r.json()) as T
  }
}

// process-wide seam — the shared ingress client for call sites with no
// runtime in scope (wallet.ts, mcp's watcher region). Materializes on first
// use from NEKTAR_RESTATE_URL; setExecutor repoints it in tests.
let active: Executor | undefined

export function executor(): Executor {
  return (active ??= new RestateExecutor(nektarEnv('NEKTAR_RESTATE_URL') ?? 'http://127.0.0.1:8080'))
}

export function setExecutor(e: Executor | undefined): void {
  active = e
}
