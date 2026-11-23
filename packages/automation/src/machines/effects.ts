import type { AutomationStore, TxState, WalletTx } from '../store.js'

/**
 * The store-bound half of the tx.machine driver: executes the intent names
 * the table declares. machines/driver.ts stays pure spec (no store edge) —
 * this file is the only place machine intents meet the coin book.
 *
 * The set is TOTAL: every declared effect intent has exactly one handler
 * slot, and a missing slot is a failed effect — the event stays pending
 * for replay instead of silently marking done. custodyTxEffects (ledger.ts)
 * builds the production set; inertTxEffects is the sanctioned "no custody"
 * set for a store with no coin book behind it (tests).
 */
export interface TxEffects {
  /** inputs spent + owned outputs adopted (funds.spent) — fires on submit
   *  success AND on dead→live revive (the counterparty-air path) */
  txSpent: (w: WalletTx) => void | Promise<void>
  /** tombstone adopted outputs, revive live inputs (funds.died) — fires on
   *  every non-confirmed terminal verdict */
  txDied: (w: WalletTx) => void | Promise<void>
  /** exported row timed out unaired — the caller revives provably-live inputs */
  exportExpired: (w: WalletTx) => void | Promise<void>
  /** a refused tx landed anyway — retro-count the spend in the day ledger
   *  (SpendGuard.recordAired) or tomorrow's headroom is overstated */
  recordAired: (w: WalletTx) => void | Promise<void>
  /** reverify/reconcile hook (reorg, boot) */
  reverify?: (w: WalletTx) => void | Promise<void>
}

/** the sanctioned "no custody" set — for a store with no coin book behind
 *  it (tests). Production sites build custodyTxEffects instead; a partial
 *  literal is exactly the bug the total type exists to reject. */
export const inertTxEffects: TxEffects = {
  txSpent: () => {},
  txDied: () => {},
  exportExpired: () => {},
  recordAired: () => {}
}

export interface EmitResult {
  accepted: boolean
  next: TxState
  intents: string[]
  eventId?: number
}

/** intent names that commit INSIDE the row transaction — everything else
 *  dispatches post-commit through TxEffects */
export const IN_TX_INTENTS = new Set(['release-leases', 'leases->spent', 'watch', 'watch-trailing', 'last-error-column'])

/** declared intents nothing runs post-commit: 'broadcast' already happened —
 *  the coordinator airs the tx before it emits the verdict */
export const DESCRIPTIVE_INTENTS = new Set(['broadcast'])

/** post-commit intent name → its TxEffects slot. The census test asserts
 *  every intent the table declares lands in one of the three sets. */
export const TX_EFFECT_OF: Record<string, keyof TxEffects> = {
  'tx-spent': 'txSpent',
  'tx-died': 'txDied',
  'export-expired': 'exportExpired',
  'record-aired': 'recordAired',
  reverify: 'reverify',
  'boot-reconcile': 'reverify'
}

/** dispatch the post-commit intents; returns false if any effect failed —
 *  a throw OR a declared intent with no effector (unknown name or a missing
 *  handler). A failed effect leaves the event uneffected and
 *  replayTxEffects retries it. */
export async function runTxIntents(intents: string[], w: WalletTx, fx: TxEffects, log?: (line: string) => void): Promise<boolean> {
  let ok = true
  for (const i of intents) {
    if (IN_TX_INTENTS.has(i) || DESCRIPTIVE_INTENTS.has(i)) continue
    const handler = fx[TX_EFFECT_OF[i] as keyof TxEffects]
    if (typeof handler !== 'function') {
      ok = false
      log?.(`tx intent '${i}' has no effector — event stays pending (${w.actionId})`)
      continue
    }
    try {
      await handler(w)
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
  fx: TxEffects,
  log?: (line: string) => void
): Promise<EmitResult> {
  const r = await store.applyTxEvent(w.actionId, event, payload)
  if (!r.accepted) return r
  const ok = await runTxIntents(r.intents, w, fx, log)
  if (ok && r.eventId !== undefined) await store.markTxEffects(r.eventId).catch(() => undefined)
  return r
}

/**
 * Replay committed-but-uneffected intents — the crash window between the
 * atomic commit and the effect dispatch. Idempotent: intents re-run in
 * full, so a half-fired event completes and a never-fired one starts.
 * Called at the head of every driver-mode tick and at boot replay.
 */
export async function replayTxEffects(store: AutomationStore, fx: TxEffects, log?: (line: string) => void): Promise<number> {
  let done = 0
  for (const p of await store.pendingTxEffects()) {
    if (p.tx === undefined || (await runTxIntents(p.intents, p.tx, fx, log))) {
      await store.markTxEffects(p.eventId).catch(() => undefined)
      done++
    }
  }
  return done
}
