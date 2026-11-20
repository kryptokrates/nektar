// emit.ts — the tx-leg dispatch. Every wallet-tx event emit flows through
// here: with no executor it's emitTxEvent verbatim; under restate the emit
// lands as a serialized, journaled invocation on the tx's own virtual
// object — the runtime's journal + recovery cover the commit→effect window
// that pendingTxEffects/replayTxEffects covered by hand.
import type { AutomationStore, TxState, WalletTx } from '../store.js'
import { emitTxEvent, type EmitResult, type TxEffects } from '../machines/effects.js'
import { CUSTODY_KEY, EXEC_SVC, rtScopeKey, type Executor } from './exec.js'

export interface TxObserve {
  event: string
  guard?: string
  observation?: unknown
  attempt?: boolean
}

/** the custody legs' arg shape — txScope carries the caller's held tx key
 *  so same-key emits inside the custody handler bypass the object (the
 *  caller's invocation holds that lock; re-entering would deadlock). */
export interface CustodyLeg {
  actionId: string
  observation?: unknown
  txScope?: string
}

/** the coordinator surface the custody legs need — structurally typed so
 *  emit.ts stays below wallet.ts in the dependency graph (wallet.ts imports
 *  txEmit from here; a back-edge would cycle). WalletCoordinator satisfies
 *  it by construction. */
export interface CustodyWallet {
  submit(actionId: string): Promise<{ state: TxState; txid: string; detail?: string }>
  markRejectedById(actionId: string, observation: unknown): Promise<void>
  resubmitPending(): Promise<Array<{ actionId: string; state: TxState }>>
}

export async function txEmit(
  exec: Executor | undefined,
  store: AutomationStore,
  w: WalletTx,
  event: string,
  payload: { guard?: string; observation?: unknown; attempt?: boolean },
  fx: TxEffects
): Promise<EmitResult> {
  // inside a handler for this same key, routing back through the object
  // would self-deadlock — go direct (the handler is already serialized)
  if (exec === undefined || rtScopeKey()?.txKey === w.actionId) return await emitTxEvent(store, w, event, payload, fx)
  const r = await exec.call<{ accepted: boolean; next?: EmitResult['next']; intents?: string[] }>(
    { service: EXEC_SVC.tx, handler: 'observe', key: w.actionId },
    { event, guard: payload.guard, observation: payload.observation, attempt: payload.attempt } satisfies TxObserve
  )
  return { accepted: r.accepted, next: r.next ?? w.state, intents: r.intents ?? [] }
}

// ── custody legs ─────────────────────────────────────────────────────────
// the coordinator's custody verbs route to the single-key custody object
// under restate — serialized + journaled, the durable successor of the
// run() promise chain for custody writes. Local mode calls the coordinator
// directly — the same body the object's handler invokes. A broadcast leg
// mid-flight is journaled work the runtime resumes, not an orphaned
// promise the resubmit scan has to rediscover.

export async function custodySubmit(
  exec: Executor | undefined,
  wallet: CustodyWallet,
  actionId: string
): Promise<{ state: TxState; txid: string; detail?: string }> {
  if (exec === undefined || rtScopeKey()?.custodyKey === CUSTODY_KEY) return await wallet.submit(actionId)
  // txScope forwards the caller's held tx key through the custody hop — a
  // resubmit arriving from tx.resubmit(A) already holds A's object lock, so
  // the emits inside wallet.submit must go direct, not re-enter the object
  return await exec.call({ service: EXEC_SVC.custody, handler: 'submit', key: CUSTODY_KEY }, { actionId, txScope: rtScopeKey()?.txKey } satisfies CustodyLeg)
}

export async function custodyMarkRejected(exec: Executor | undefined, wallet: CustodyWallet, actionId: string, observation: unknown): Promise<void> {
  if (exec === undefined || rtScopeKey()?.custodyKey === CUSTODY_KEY) return await wallet.markRejectedById(actionId, observation)
  await exec.call({ service: EXEC_SVC.custody, handler: 'markRejected', key: CUSTODY_KEY }, {
    actionId,
    observation,
    txScope: rtScopeKey()?.txKey
  } satisfies CustodyLeg)
}

export async function custodyResubmitPending(exec: Executor | undefined, wallet: CustodyWallet): Promise<Array<{ actionId: string; state: TxState }>> {
  if (exec === undefined || rtScopeKey()?.custodyKey === CUSTODY_KEY) return await wallet.resubmitPending()
  return await exec.call({ service: EXEC_SVC.custody, handler: 'resubmitPending', key: CUSTODY_KEY }, null)
}
