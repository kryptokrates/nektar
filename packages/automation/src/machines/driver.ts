import { opsStep } from './ops.machine.js'
import { opsCauseCandidates } from './shadow.js'

/**
 * The pure spec-side of the machine drivers: observation→event
 * mapping and the ops-transition validator. NOTHING here may import the
 * store — the store-bound effect dispatch lives in machines/effects.ts
 * (store.ts → driver.ts → store.ts would be a cycle).
 */

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
 *  proof-bearing sources may name 'proven'; a raw broadcaster status string
 *  (source === 'broadcaster') is a 'hint' — it may advance an already-
 *  submitted row to observed, never revive a dead row or fire custody
 *  intents (spv/hint-as-verified). */
export function obsEvent(o: { status: 'seen' | 'proven' | 'rejected' | 'conflicted' | 'abandoned'; source?: string }): { event: string; guard?: string } {
  switch (o.status) {
    case 'proven':
      return { event: 'obs.proven', guard: 'proof' }
    case 'seen':
      return { event: 'obs.seen', guard: o.source === 'broadcaster' ? 'hint' : 'verified' }
    case 'rejected':
      return { event: 'obs.rejected', guard: 'verified' }
    case 'conflicted':
      return { event: 'obs.conflicted', guard: 'verified+spentBy' }
    case 'abandoned':
      return { event: 'abandon' }
  }
}
