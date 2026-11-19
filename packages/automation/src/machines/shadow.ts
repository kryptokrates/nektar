import { opsStep } from './ops.machine.js'
import { txStep } from './tx.machine.js'

/**
 * Shadow-mode replay: fold recorded transitions through the pure machines and
 * diff them against what the live FSM actually did. The machine NEVER moves
 * production state — it only answers "what would the spec have done".
 */

export interface LiveTransition {
  from: string
  to: string
  cause: string
}

export interface Diff {
  kind: 'agree' | 'machine-blocks' | 'machine-diverges' | 'unmodeled'
  from: string
  event: string
  live: string
  machine: string
  cause: string
  guard?: string
  note?: string
}

/** market_events.cause → machine event candidates (state-changing rows only). */
const OPS_CAUSE_MAP: Record<string, { event: string; guards?: string[] }> = {
  want_signed: { event: 'want.signed' },
  offer_selected: { event: 'offers.select', guards: ['policy+budget'] },
  no_qualifying_offer: { event: 'collect.expired' },
  approval_expired: { event: 'approval.expired' },
  human_denied: { event: 'approval.denied' },
  policy_revoked_or_expired: { event: 'policy.revoked' },
  escrow_broadcast: { event: 'escrow.submitted' },
  payment_broadcast: { event: 'escrow.submitted' },
  escrow_refused_before_broadcast: { event: 'escrow.failed' },
  payment_refused_before_broadcast: { event: 'escrow.failed' },
  spend_cap_deferred: { event: 'daily.cap' },
  spend_window_reset: { event: 'cap.reset' },
  spend_guard_refused: { event: 'cap.refused', guards: ['policy.escalate', '!policy.escalate'] },
  escrow_never_built: { event: 'reconciled.dead', guards: ['verified'] },
  escrow_rejected: { event: 'reconciled.dead', guards: ['verified'] },
  reconciled_escrow_observed: { event: 'reconciled.ok', guards: ['verified'] },
  refund_deadline_reached: { event: 'refund.due', guards: ['dueHeight'] },
  refund_broadcast: { event: 'refund.sent' },
  refund_rejected: { event: 'refund.dead', guards: ['verified'] },
  refund_retry_refused: { event: 'refund.dead', guards: ['verified'] },
  refused_escrow_landed: { event: 'escrow.landed', guards: ['verdict=mined'] },
  landed_escrow_gone: { event: 'escrow.gone', guards: ['verdict=dead'] },
  escrow_rejected_late: { event: 'escrow.died', guards: ['verified'] },
  stuck_timeout: { event: 'stuck.timeout', guards: ['maxAgeMs'] },
  stuck_timeout_no_escrow: { event: 'stuck.timeout', guards: ['no-escrow'] },
  stuck_timeout_payment_final: { event: 'stuck.timeout', guards: ['payment-final'] },
  delivery_lapsed_payment_final: { event: 'disclosure.bad' },
  deadline_reached: { event: 'escrow.deadline', guards: ['deadline-passed'] },
  seize_broadcast: { event: 'seize.ok' },
  seize_refused: { event: 'seize.failed' },
  offer_prepared: { event: 'offer.signed' },
  preparation_error: { event: 'prep.failed' },
  accept_refused: { event: 'offer.declined' },
  seller_declined: { event: 'offer.declined' },
  disclosure_broadcast: { event: 'disclosure.submitted' },
  disclosure_proof_observed: { event: 'settlement.proven', guards: ['proof'] },
  disclosure_rejected_late: { event: 'completion.failed' },
  completion_refused: { event: 'completion.failed' },
  quote_expired: { event: 'quote.expired' },
  seller_policy_changed: { event: 'enabled' }
}

/** Replay one op's live transition log through opsMachine. */
export function replayOps(rows: LiveTransition[]): Diff[] {
  const diffs: Diff[] = []
  for (const r of rows) {
    if (!r.from || r.from === r.to) continue // creation + data-update rows carry no edge
    const map = OPS_CAUSE_MAP[r.cause]
    if (!map) {
      diffs.push({ kind: 'unmodeled', from: r.from, event: r.cause, live: r.to, machine: '', cause: r.cause })
      continue
    }
    // try each candidate guard — report the one matching live, else the divergence
    let matched: Diff | null = null
    let best: Diff | null = null
    for (const guard of map.guards ?? [undefined]) {
      const s = opsStep(r.from, map.event, guard ? { guard } : {})
      const d: Diff = { kind: 'agree', from: r.from, event: map.event, live: r.to, machine: s.next, cause: r.cause, guard }
      if (!s.accepted) d.kind = 'machine-blocks'
      else if (s.next !== r.to) d.kind = 'machine-diverges'
      if (d.kind === 'agree') {
        matched = d
        break
      }
      best ??= d
    }
    diffs.push(matched ?? best!)
  }
  return diffs
}

/** Infer a txMachine event from a wallet_transactions (from,to) edge + emit context. */
function txEventFor(from: string, to: string, hint?: 'submit' | 'observe' | 'abandon' | 'export-ttl'): { event: string; guard?: string } {
  if (to === 'submitted') return { event: 'submit.success' }
  if (to === 'submission_unknown') return { event: 'submit.unknown' }
  if (to === 'observed') return { event: 'obs.seen', guard: 'verified' }
  if (to === 'confirmed') return { event: 'obs.proven', guard: 'proof' }
  if (to === 'expired') return { event: 'export.ttl' }
  if (to === 'abandoned') return { event: 'abandon' }
  if (to === 'conflicted') return hint === 'abandon' ? { event: 'abandon' } : { event: 'obs.conflicted', guard: 'verified+spentBy' }
  if (to === 'rejected') return hint === 'submit' ? { event: 'submit.refused', guard: 'definitive' } : { event: 'obs.rejected', guard: 'verified' }
  return { event: `unknown:${from}->${to}` }
}

/** Replay wallet_transactions edges through txMachine. */
export function replayTx(rows: { from: string; to: string; hint?: 'submit' | 'observe' | 'abandon' | 'export-ttl' }[]): Diff[] {
  return rows.map((r) => {
    const { event, guard } = txEventFor(r.from, r.to, r.hint)
    if (event.startsWith('unknown:')) return { kind: 'unmodeled', from: r.from, event, live: r.to, machine: '', cause: event }
    const s = txStep(r.from, event, guard ? { guard } : {})
    const d: Diff = { kind: 'agree', from: r.from, event, live: r.to, machine: s.next, cause: event, guard }
    if (!s.accepted) d.kind = 'machine-blocks'
    else if (s.next !== r.to) d.kind = 'machine-diverges'
    return d
  })
}
