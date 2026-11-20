import { nektarEnv } from '@nektar/core'
import { opsStep } from './ops.machine.js'
import { opsCauseCandidates } from './shadow.js'

/**
 * The pure spec-side of the machine drivers: flag gates, observation→event
 * mapping and the ops-transition validator. NOTHING here may import the
 * store — the store-bound effect dispatch lives in machines/effects.ts
 * (store.ts → driver.ts → store.ts would be a cycle).
 */

/** kill switch — one release back to the hand-rolled FSM */
export const txFsmLegacy = (): boolean => nektarEnv('NEKTAR_TX_FSM') === 'legacy'

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
