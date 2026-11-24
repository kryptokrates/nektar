// emit.ts — the tx-leg dispatch. Every wallet-tx event emit flows through
// here: it lands as a serialized, journaled invocation on the tx's own
// virtual object — the runtime's journal + recovery cover the
// commit→effect window that pendingTxEffects/replayTxEffects covered by
// hand. The direct emitTxEvent path is only for same-key emits inside a
// handler (the caller already holds that object's lock).
import { createHash } from 'node:crypto'
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
}

export interface CustodyResubmitArg {
  fanOnly?: boolean
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

const stableObs = (obs: unknown): string => {
  if (obs === undefined) return ''
  if (obs !== null && typeof obs === 'object' && !Array.isArray(obs)) {
    const { at: _at, ...rest } = obs as Record<string, unknown>
    return JSON.stringify(rest)
  }
  return JSON.stringify(obs)
}

/** deterministic content key for deduped egress — guard + observation
 *  hashed so distinct payloads never collide on one key */
const guardKey = (payload: { guard?: string; observation?: unknown }): string =>
  createHash('sha256')
    .update(`${payload.guard ?? ''}${stableObs(payload.observation)}`)
    .digest('hex')
    .slice(0, 16)

export async function txEmit(
  exec: Executor,
  store: AutomationStore,
  w: WalletTx,
  event: string,
  payload: { guard?: string; observation?: unknown; attempt?: boolean },
  fx: TxEffects
): Promise<EmitResult> {
  // inside a handler for this same key, routing back through the object
  // would self-deadlock — go direct (the handler is already serialized)
  if (rtScopeKey()?.txKey === w.actionId) return await emitTxEvent(store, w, event, payload, fx)
  const r = await exec.call<{ accepted: boolean; next?: EmitResult['next']; intents?: string[] }>(
    { service: EXEC_SVC.tx, handler: 'observe', key: w.actionId },
    { event, guard: payload.guard, observation: payload.observation, attempt: payload.attempt } satisfies TxObserve,
    // journaled-step egress: a caller killed between this HTTP call landing
    // and the journal write re-fires on resume — the deterministic key makes
    // the ingress dedupe the re-fire instead of double-observing. Same
    // content → same result, so legitimate repeats collapse harmlessly.
    { idempotencyKey: `observe:${w.actionId}:${event}:${w.attempts}:${guardKey(payload)}` }
  )
  return { accepted: r.accepted, next: r.next ?? w.state, intents: r.intents ?? [] }
}

// ── custody legs ─────────────────────────────────────────────────────────
// the coordinator's custody verbs route to the custody virtual object —
// serialized + journaled, the durable successor of the run() promise chain
// for custody writes. Per-tx legs key on the actionId: one tx's submit +
// markRejected serialize with each other while distinct txs broadcast in
// parallel — a global key would make every deal's funding wait on every
// other deal's broadcast RTT (and a resubmit scan re-airing rows would
// block all new submissions). resubmitPending keeps the fixed 'global'
// key: the scan is one stream by design. Inside a custody handler the
// legs call the coordinator directly — the same body the object's handler
// invokes. A broadcast leg mid-flight is journaled work the runtime
// resumes, not an orphaned promise the resubmit scan has to rediscover.

export async function custodySubmit(exec: Executor, wallet: CustodyWallet, actionId: string): Promise<{ state: TxState; txid: string; detail?: string }> {
  if (rtScopeKey()?.custodyKey === actionId) return await wallet.submit(actionId)
  // No static actionId idempotencyKey: a submission_unknown outcome is a
  // normal return, so a row-only key caches it for the retention window and
  // swallows every retry (boot sweep, funding crash-resume) — per-actionId
  // key FIFO + row-state guard + wire ALREADY absorb repeats instead.
  return await exec.call({ service: EXEC_SVC.custody, handler: 'submit', key: actionId }, { actionId } satisfies CustodyLeg)
}

export async function custodyMarkRejected(exec: Executor, wallet: CustodyWallet, actionId: string, observation: unknown): Promise<void> {
  if (rtScopeKey()?.custodyKey === actionId) return await wallet.markRejectedById(actionId, observation)
  await exec.call(
    { service: EXEC_SVC.custody, handler: 'markRejected', key: actionId },
    {
      actionId,
      observation
    } satisfies CustodyLeg,
    { idempotencyKey: `custody:reject:${actionId}:${guardKey({ observation })}` }
  )
}

export async function custodyResubmitPending(exec: Executor, wallet: CustodyWallet): Promise<Array<{ actionId: string; state: TxState }>> {
  if (rtScopeKey()?.custodyKey === CUSTODY_KEY) return await wallet.resubmitPending()
  return await exec.call({ service: EXEC_SVC.custody, handler: 'resubmitPending', key: CUSTODY_KEY }, null)
}

/** the cadence leg's poke — send, not call: a resubmit scan is one broadcast
 *  per stranded row, and a call would hold the caller's invocation budget
 *  for the whole serialized pass (rt/tick-starvation: a prepared-row backlog
 *  burned a dealfan tick's 120s, the aborted leg retried to pause, and the
 *  paused head wedged the object's whole key). Deduped per cadence window so
 *  back-to-back pokes can't queue duplicate scans behind the custody key. */
export async function custodyResubmitPoke(exec: Executor, wallet: CustodyWallet, window: number): Promise<void> {
  if (rtScopeKey()?.custodyKey === CUSTODY_KEY) return void (await wallet.resubmitPending())
  await exec.send({ service: EXEC_SVC.custody, handler: 'resubmitPending', key: CUSTODY_KEY }, { fanOnly: true } satisfies CustodyResubmitArg, {
    idempotencyKey: `resubmit:${window}`
  })
}
