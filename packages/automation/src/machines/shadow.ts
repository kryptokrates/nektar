import { sparse } from '@nektar/core'

/**
 * Shadow-mode cause mapping: the live `cause` vocabulary → machine edges.
 * Used by driver.ts to label emissions and by the shadow-replay census gate.
 * The replay/diff helpers that fold recorded transitions through the pure
 * machines live in test/shadow-replay.test.ts — nothing in src replays.
 */

interface Candidate {
  event: string
  guards?: (string | undefined)[]
}

/** market_events.cause → machine event candidates. A cause may legitimately
 *  land on different edges (role/state-dependent) — candidates are tried in
 *  order and the first agreeing one reports 'agree'. */
const OPS_CAUSE_MAP: Record<string, Candidate[]> = {
  want_signed: [{ event: 'want.signed' }],
  want_republished: [{ event: 'want.republished' }],
  offer_selected: [{ event: 'offers.select', guards: ['policy+budget'] }],
  offer_recorded: [{ event: 'offer.recorded' }],
  offer_rejected: [{ event: 'offer.rejected' }],
  no_qualifying_offer: [{ event: 'collect.expired' }],
  quote_attached: [{ event: 'quote.attached' }],
  approval_expired: [{ event: 'approval.expired' }],
  human_denied: [{ event: 'approval.denied' }],
  policy_revoked_or_expired: [{ event: 'policy.revoked' }],
  escrow_broadcast: [{ event: 'escrow.submitted' }],
  payment_broadcast: [{ event: 'escrow.submitted' }],
  escrow_prepared: [{ event: 'escrow.prepared' }],
  payment_prepared: [{ event: 'payment.prepared' }],
  escrow_resumed_same_bytes: [{ event: 'escrow.resumed' }],
  escrow_refused_before_broadcast: [{ event: 'escrow.failed' }],
  payment_refused_before_broadcast: [{ event: 'escrow.failed' }],
  escrow_build_refused: [{ event: 'escrow.failed' }],
  payment_build_refused: [{ event: 'escrow.failed' }],
  funding_starved: [{ event: 'starved.retry' }],
  funding_starved_exhausted: [{ event: 'starved.exhausted' }],
  spend_cap_deferred: [{ event: 'daily.cap' }],
  spend_window_reset: [{ event: 'cap.reset' }],
  spend_guard_refused: [{ event: 'cap.refused', guards: ['policy.escalate', '!policy.escalate'] }],
  deferred_escrow_aired: [{ event: 'deferred.escrow_aired', guards: ['verified'] }],
  delivered_parked_while_deferred: [{ event: 'delivered.parked' }],
  quote_parked_before_offer: [{ event: 'quote.parked' }],
  delivered_unresolvable: [{ event: 'delivered.parked' }],
  disclosure_verified: [{ event: 'disclosure.ok', guards: ['spv-verify'] }],
  payload_does_not_match_commitment: [{ event: 'disclosure.bad' }],
  escrow_never_built: [{ event: 'reconciled.dead', guards: ['verified'] }],
  escrow_rejected: [{ event: 'reconciled.dead', guards: ['verified'] }],
  reconciled_escrow_observed: [{ event: 'reconciled.ok', guards: ['verified'] }],
  reconcile_pending: [{ event: 'reconcile.pending' }],
  refund_deadline_reached: [{ event: 'refund.due', guards: ['dueHeight', undefined] }],
  refund_broadcast: [{ event: 'refund.sent' }],
  refund_rejected: [{ event: 'refund.dead', guards: ['verified'] }],
  refund_retry_refused: [{ event: 'refund.dead', guards: ['verified'] }],
  refund_retry_immature: [{ event: 'refund.retry' }],
  refund_wait_height: [{ event: 'refund.retry' }],
  refund_retry_spent: [{ event: 'refund.dead', guards: ['verified'] }],
  // chain evidence that the escrow moved: a depositor spend is the refund
  // that already landed; a seller spend parks the disclosure for replay
  refund_observed_on_chain: [{ event: 'refund.sent' }],
  escrow_spent_by_completion: [{ event: 'delivered.parked' }],
  refund_proven: [{ event: 'refund.proven', guards: ['proof'] }],
  refused_escrow_landed: [{ event: 'escrow.landed', guards: ['verdict=mined'] }],
  landed_escrow_gone: [{ event: 'escrow.gone', guards: ['verdict=dead'] }],
  escrow_rejected_late: [{ event: 'escrow.gone', guards: ['verdict=dead'] }],
  stuck_timeout: [{ event: 'stuck.timeout', guards: ['no-escrow', 'payment-final', 'maxAgeMs'] }],
  stuck_timeout_no_escrow: [{ event: 'stuck.timeout', guards: ['no-escrow'] }],
  stuck_timeout_payment_final: [{ event: 'stuck.timeout', guards: ['payment-final'] }],
  delivery_lapsed_payment_final: [{ event: 'disclosure.bad' }],
  deadline_reached: [{ event: 'escrow.deadline', guards: ['deadline-passed'] }],
  seize_broadcast: [{ event: 'seize.ok' }],
  seize_refused: [{ event: 'seize.failed' }],
  resume_after_restart: [{ event: 'resume.restart' }],
  offer_prepared: [{ event: 'offer.signed' }],
  preparation_error: [{ event: 'prep.failed' }],
  accept_refused: [{ event: 'escrow.broadcast_refused' }, { event: 'offer.declined' }],
  seller_declined: [{ event: 'decline.received' }],
  escrow_verified: [{ event: 'escrow.verified', guards: ['spv-verify'] }],
  payment_verified: [{ event: 'payment.verified', guards: ['spv-verify'] }],
  payment_awaiting_proof: [{ event: 'payment.pending', guards: ['spv-verify'] }],
  payment_proven: [{ event: 'payment.proven', guards: ['proof'] }],
  payment_died: [{ event: 'payment.died', guards: ['verdict=dead'] }],
  payment_pending_lapsed: [{ event: 'payment.lapsed' }],
  direct_delivery: [{ event: 'direct.delivery' }],
  disclosure_broadcast: [{ event: 'disclosure.submitted' }],
  disclosure_proof_observed: [{ event: 'settlement.proven', guards: ['proof'] }],
  disclosure_proof_awaiting_depth: [{ event: 'proof.depth' }],
  disclosure_rejected_late: [{ event: 'completion.failed' }],
  completion_refused: [{ event: 'completion.failed' }],
  quote_expired: [{ event: 'quote.expired' }],
  seller_policy_changed: [{ event: 'enabled' }],
  human_approved: [{ event: 'approval.granted' }, { event: 'fund.approved', guards: ['policy+approval+cap'] }],
  policy_allowed: [{ event: 'approval.granted' }, { event: 'fund.approved', guards: ['policy+approval+cap'] }],
  sandbox_evaluation: [{ event: 'marked.useful' }, { event: 'marked.not_useful' }],
  auto_evaluation: [{ event: 'marked.useful' }, { event: 'marked.not_useful' }],
  evidence_stored: [{ event: 'evidence.stored' }],
  restored_from_note: [{ event: 'restored' }],
  tick_error: [{ event: 'tick.error' }]
}

/** Cause vocabulary that never carries an edge — creation rows (from null)
 *  are filtered before the map is consulted. Kept for the census gate. */
export const OPS_NO_EDGE_CAUSES = ['created']

/** Non-CAP deny codes emitted verbatim as the cause (decision.code / b.code) —
 *  each maps to the `policy.denied` edge. CAP-class codes go through
 *  escalate() and arrive as `needs_human:<code>` instead. */
const DENIED_CAUSE =
  /^(invalid_amount|action_released|action_reused_with_different_terms|unknown_policy|policy_revoked|invalid_policy|wrong_network|policy_not_valid_now|role_not_authorized|wrong_agent|shard_not_sellable|public_disclosure_not_authorized|price_outside_policy|domain_outside_policy)$/

/** Resolve a live cause to machine-event candidates (map → prefix → deny-code). */
export function opsCauseCandidates(cause: string): Candidate[] | undefined {
  const hit = sparse(OPS_CAUSE_MAP[cause])
  if (hit) return hit
  if (cause.startsWith('needs_human:')) {
    // unresolved_payment / unresolved_settlement escalate to 'uncertain';
    // every other reason (cap codes, deny escalations, stuck_state_timeout)
    // targets 'escalated' — the guard picks the declared sibling.
    const unresolved = cause.startsWith('needs_human:unresolved_')
    return unresolved ? [{ event: 'needs.human', guards: ['unresolved'] }] : [{ event: 'needs.human', guards: [undefined] }]
  }
  if (DENIED_CAUSE.test(cause)) return [{ event: 'policy.denied' }]
  return undefined
}
