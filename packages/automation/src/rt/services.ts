// services.ts — the Restate-facing surface. One virtual object per wallet
// tx (key = actionId): every observation/emit lands as a serialized,
// journaled invocation on the tx's own object — the per-key serialization
// replaces the sweep mutex + the run() chain for this leg, and the journal
// covers the commit→effect window pendingTxEffects covered by hand.
//
// The handler bodies are the SAME code paths as the local executor's —
// emitTxEvent/applyTxEvent inside ctx.run legs — so SQLite stays the
// domain authority and the machine table stays the spec validating every
// commit inside the invocation.
import * as restate from '@restatedev/restate-sdk'
import type { AutomationStore } from '../store.js'
import { emitTxEvent, type TxEffects } from '../machines/effects.js'
import { EXEC_SVC } from './exec.js'
import type { TxObserve } from './emit.js'

export interface RtDeps {
  store: AutomationStore
  fx: TxEffects
  log: (line: string) => void
}

export const txObject = (deps: RtDeps) =>
  restate.object({
    name: EXEC_SVC.tx,
    handlers: {
      // an observation lands: load the row, applyTxEvent commit, post-commit
      // intents — all journaled so a crash mid-leg replays from the last
      // completed step, not from scratch.
      observe: async (ctx: restate.ObjectContext, o: TxObserve) => {
        const w = await ctx.run('load', () => deps.store.walletTx({ actionId: ctx.key }))
        if (w === undefined) return { accepted: false }
        const r = await ctx.run('emit', () => emitTxEvent(deps.store, w, o.event, { guard: o.guard, observation: o.observation, attempt: o.attempt }, deps.fx))
        return { accepted: r.accepted, next: r.next, intents: r.intents }
      },

      // the export-ttl arm — armed as a delayed send by intents when the
      // commit lands `exported`; a late emit wins, this fires only if the
      // row still sits exported.
      expire: async (ctx: restate.ObjectContext) => {
        const w = await ctx.run('load', () => deps.store.walletTx({ actionId: ctx.key }))
        if (w === undefined || w.state !== 'exported') return { accepted: false }
        const r = await ctx.run('emit', () => emitTxEvent(deps.store, w, 'export.ttl', {}, deps.fx))
        return { accepted: r.accepted, next: r.next, intents: r.intents }
      }
    }
  })
