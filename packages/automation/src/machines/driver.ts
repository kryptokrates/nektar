import { nektarEnv } from '@nektar/core'
import type { AutomationStore, TxState, WalletTx } from '../store.js'
import { opsStep } from './ops.machine.js'
import { opsCauseCandidates } from './shadow.js'

/**
 * The tx.machine DRIVER: turns intent names into the custody/broadcast calls
 * the hand-rolled path used to inline. The machine decides; this file only
 * executes — every effect must be idempotent (a crash between persist and
 * act replays the intent on the next event).
 *
 * Wiring is per driver site: WalletCoordinator passes its deps, each
 * TransactionWatcher passes its own custody hooks — same spec, two ledgers.
 */
export interface TxEffects {
  broadcast?: (w: WalletTx) => void | Promise<void>
  /** inputs spent + owned outputs adopted (funds.spent) — fires on submit
   *  success AND on dead→live revive (the counterparty-air path) */
  txSpent?: (w: WalletTx) => void | Promise<void>
  /** tombstone adopted outputs, revive live inputs (funds.died) — fires on
   *  every non-confirmed terminal verdict */
  txDied?: (w: WalletTx) => void | Promise<void>
  /** exported row timed out unaired — the caller revives provably-live inputs */
  exportExpired?: (w: WalletTx) => void | Promise<void>
  /** a refused tx landed anyway — retro-count the spend in the day ledger
   *  (SpendGuard.recordAired) or tomorrow's headroom is overstated */
  recordAired?: (w: WalletTx) => void | Promise<void>
  /** reverify/reconcile hook (reorg, boot) */
  reverify?: (w: WalletTx) => void | Promise<void>
}

/** kill switch — one release back to the hand-rolled FSM */
export const txFsmLegacy = (): boolean => nektarEnv('NEKTAR_TX_FSM') === 'legacy'

export interface EmitResult {
  accepted: boolean
  next: TxState
  intents: string[]
  eventId?: number
}

/** intent names that commit INSIDE the row transaction — everything else
 *  dispatches post-commit through TxEffects */
const IN_TX_INTENTS = new Set(['release-leases', 'leases->spent', 'watch', 'watch-trailing', 'last-error-column'])

/** dispatch the post-commit intents; returns false if any effect threw
 *  (the event stays uneffected and replayTxEffects retries it) */
async function runTxIntents(intents: string[], w: WalletTx, fx: TxEffects): Promise<boolean> {
  let ok = true
  for (const i of intents) {
    if (IN_TX_INTENTS.has(i)) continue
    try {
      switch (i) {
        case 'broadcast':
          await fx.broadcast?.(w)
          break
        case 'tx-spent':
          await fx.txSpent?.(w)
          break
        case 'tx-died':
          await fx.txDied?.(w)
          break
        case 'export-expired':
          await fx.exportExpired?.(w)
          break
        case 'record-aired':
          await fx.recordAired?.(w)
          break
        case 'reverify':
        case 'boot-reconcile':
          await fx.reverify?.(w)
          break
        default:
          break
      }
    } catch {
      ok = false
    }
  }
  return ok
}

/**
 * (persistedState, event) → atomic {state + event row + in-tx lease effects},
 * then post-commit effects in declared order. The ONLY machine-mode write
 * path — a rejected step writes nothing (impossible events are caller bugs,
 * not state). A clean effect run flips `effected`; a thrown effect or a
 * crash leaves the row committed and the event pending for replay.
 */
export async function emitTxEvent(
  store: AutomationStore,
  w: WalletTx,
  event: string,
  payload: { guard?: string; observation?: unknown; attempt?: boolean },
  fx: TxEffects
): Promise<EmitResult> {
  const r = await store.applyTxEvent(w.actionId, event, payload)
  if (!r.accepted) return r
  const ok = await runTxIntents(r.intents, w, fx)
  if (ok && r.eventId !== undefined) await store.markTxEffects(r.eventId).catch(() => undefined)
  return r
}

/**
 * Replay committed-but-uneffected intents — the crash window between the
 * atomic commit and the effect dispatch. Idempotent: intents re-run in
 * full, so a half-fired event completes and a never-fired one starts.
 * Called at the head of every driver-mode tick and at boot replay.
 */
export async function replayTxEffects(store: AutomationStore, fx: TxEffects): Promise<number> {
  let done = 0
  for (const p of await store.pendingTxEffects()) {
    if (p.tx === undefined || (await runTxIntents(p.intents, p.tx, fx))) {
      await store.markTxEffects(p.eventId).catch(() => undefined)
      done++
    }
  }
  return done
}

/** kill switch — one release back to unvalidated transition() writes */
export const opsFsmLegacy = (): boolean => nektarEnv('NEKTAR_OPS_FSM') === 'legacy'

export interface OpVerdict {
  ok: boolean
  event?: string
  guard?: string
  /** what the machine would have done — reported on failure so the gap
   *  surfaces loudly instead of silently writing a spec-less row */
  machine?: string
}

/**
 * Driver-mode validation for a transition() write: the cause resolves to
 * machine candidates (same resolution the shadow replay uses); the write is
 * legal iff some candidate edge accepts from `from` and lands exactly on
 * `to`. Unmodeled causes pass through — the census test gates them, and a
 * data-stamp write must not need a transition-table row.
 */
export function opEdgeAgrees(from: string, to: string, cause: string): OpVerdict {
  const candidates = opsCauseCandidates(cause)
  if (candidates === undefined) return { ok: true }
  let best: OpVerdict | undefined
  for (const cand of candidates)
    for (const guard of cand.guards ?? [undefined]) {
      const s = opsStep(from, cand.event, guard !== undefined ? { guard } : {})
      if (s.accepted && s.next === to) return { ok: true, event: cand.event, guard }
      best ??= { ok: false, event: cand.event, guard, machine: s.accepted ? s.next : '(blocked)' }
    }
  return best ?? { ok: false, machine: '(no candidates)' }
}

/** A watcher/coordinator observation → its declared event + guard. Only
 *  proof-bearing sources may name 'proven'; everything else is 'verified'
 *  at best — a hint alone must never reach this function as 'seen'. */
export function obsEvent(o: { status: 'seen' | 'proven' | 'rejected' | 'conflicted' | 'abandoned' }): { event: string; guard?: string } {
  switch (o.status) {
    case 'proven':
      return { event: 'obs.proven', guard: 'proof' }
    case 'seen':
      return { event: 'obs.seen', guard: 'verified' }
    case 'rejected':
      return { event: 'obs.rejected', guard: 'verified' }
    case 'conflicted':
      return { event: 'obs.conflicted', guard: 'verified+spentBy' }
    case 'abandoned':
      return { event: 'abandon' }
  }
}
