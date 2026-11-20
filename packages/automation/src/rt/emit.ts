// emit.ts — the tx-leg dispatch. Every wallet-tx event emit flows through
// here: with no executor it's emitTxEvent verbatim; under restate the emit
// lands as a serialized, journaled invocation on the tx's own virtual
// object — the runtime's journal + recovery cover the commit→effect window
// that pendingTxEffects/replayTxEffects covered by hand.
import type { AutomationStore, WalletTx } from '../store.js'
import { emitTxEvent, type EmitResult, type TxEffects } from '../machines/effects.js'
import { EXEC_SVC, type Executor } from './exec.js'

export interface TxObserve {
  event: string
  guard?: string
  observation?: unknown
  attempt?: boolean
}

export async function txEmit(
  exec: Executor | undefined,
  store: AutomationStore,
  w: WalletTx,
  event: string,
  payload: { guard?: string; observation?: unknown; attempt?: boolean },
  fx: TxEffects
): Promise<EmitResult> {
  if (exec === undefined) return await emitTxEvent(store, w, event, payload, fx)
  const r = await exec.call<{ accepted: boolean; next?: EmitResult['next']; intents?: string[] }>(
    { service: EXEC_SVC.tx, handler: 'observe', key: w.actionId },
    { event, guard: payload.guard, observation: payload.observation, attempt: payload.attempt } satisfies TxObserve
  )
  return { accepted: r.accepted, next: r.next ?? w.state, intents: r.intents ?? [] }
}
