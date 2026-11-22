// Broadcast-verdict vocabulary — what a BroadcastResult's description proves
// about whether the tx actually left for the wire. Destructive actions
// (funds.died, journal tombstones, cap refunds) may only run on a provably-dead
// verdict: 'error' that reached no wire verdict is submission_unknown — the
// bytes may have aired — never a death certificate.
export const NEVER_LEFT = /SpendGuard refused/i
export const DEFINITIVE =
  /rejected|missing inputs|mempool-conflict|double.?spen|utxo already spent|bad-txns|non-mandatory|unlocking scripts invalid|fee too low/i
export const ALREADY = /already (known|in|mined|exists)|txn-already/i

/** provably-dead broadcast verdict: the wire refused it (DEFINITIVE) or the
 *  guard never let it leave (NEVER_LEFT). A transport failure is neither —
 *  the bytes may have left, so 'error' without a wire verdict is
 *  submission_unknown, never a death certificate. died()/tombstone on a
 *  maybe-aired tx frees its inputs for a double-spend. */
export const broadcastProvablyDead = (detail: string): boolean => NEVER_LEFT.test(detail) || (DEFINITIVE.test(detail) && !detail.startsWith('transport:'))
