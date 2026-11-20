// services.ts — the Restate-facing surface. One virtual object per wallet
// tx (key = actionId): every observation/emit lands as a serialized,
// journaled invocation on the tx's own object — the per-key serialization
// replaces the sweep mutex + the run() chain for this leg, and the journal
// covers the commit→effect window pendingTxEffects covered by hand.
//
// The handler BODIES are exported plain (txObserveBody/txExpireBody) — the
// Restate handlers are thin ctx.run wrappers around them, and the local
// executor registers the same bodies, so memory-tier tests run the exact
// code the server invokes.
import type * as restate from '@restatedev/restate-sdk'
import type { AutomationStore } from '../store.js'
import { emitTxEvent, type TxEffects } from '../machines/effects.js'
import { EXEC_SVC } from './exec.js'
import type { TxObserve } from './emit.js'

export interface RtDeps {
  store: AutomationStore
  fx: TxEffects
  log: (line: string) => void
}

/** load the row by its object key, then emitTxEvent — the journaled leg. */
export const txObserveBody = (deps: RtDeps) => async (o: TxObserve, actionId: string) => {
  const w = await deps.store.walletTx({ actionId })
  if (w === undefined) return { accepted: false }
  const r = await emitTxEvent(deps.store, w, o.event, { guard: o.guard, observation: o.observation, attempt: o.attempt }, deps.fx)
  return { accepted: r.accepted, next: r.next, intents: r.intents }
}

/** the export-ttl arm — fires only if the row still sits exported. */
export const txExpireBody = (deps: RtDeps) => async (_: unknown, actionId: string) => {
  const w = await deps.store.walletTx({ actionId })
  if (w === undefined || w.state !== 'exported') return { accepted: false }
  const r = await emitTxEvent(deps.store, w, 'export.ttl', {}, deps.fx)
  return { accepted: r.accepted, next: r.next, intents: r.intents }
}

export const txObject = async (deps: RtDeps) => {
  const restate = await import('@restatedev/restate-sdk')
  const observe = txObserveBody(deps)
  const expire = txExpireBody(deps)
  return restate.object({
    name: EXEC_SVC.tx,
    handlers: {
      observe: async (ctx: restate.ObjectContext, o: TxObserve) => await ctx.run('emit', () => observe(o, ctx.key)),
      expire: async (ctx: restate.ObjectContext) => await ctx.run('emit', () => expire(undefined, ctx.key))
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
  ex.register(EXEC_SVC.tx, 'observe', (arg, key) => observe(arg as TxObserve, key!))
  ex.register(EXEC_SVC.tx, 'expire', (arg, key) => expire(arg, key!))
}
