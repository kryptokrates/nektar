// refusals.ts — the refused-ancestor registry: a broadcast refusal that
// names its ancestor is first-party evidence the ancestor is dead to
// this mempool — every outpoint whose unproven ancestry contains it can
// never air, whatever its own status word says. Bookkeeping evidence
// only: it feeds a deadParent oracle (heals stock), never a chain
// verdict.
import { ancestryHit } from '@nektar/bstack'
import { BoundedMap } from '@nektar/core'
import type { Transaction } from '@bsv/sdk'

/** a mark's mempool epoch — a 'conflicting' verdict flips only if the
 *  winner's block reorgs, but a stale mark costs real stock (take
 *  answers it with died()). This bounds the damage; the sealed-verdict
 *  oracle answers whatever outlives it */
export const REFUSED_MARK_MS = 30 * 60_000

// the wire shapes a refusal actually uses to name the dead ancestor:
// Teranode's "utxo <txid>:<vout> belongs to a conflicting transaction"
// (ERR_TX_CONFLICTING — a double-spend-loser parent, dead in this
// mempool epoch) and bitcoind/ARC's "bad-txns-spends-conflicting-tx,
// <wtxid>" — plus the literal "ancestor <txid>" phrasing kept for
// broadcasters whose wording we have not observed. Missing, locked,
// frozen and non-final parents are deliberately unmatched: transient
// holds, none say the ancestor is dead.
const REFUSED_ANCESTOR =
  /\b([0-9a-f]{64})\s*:\s*\d+\s+belongs to a conflicting transaction|bad-txns-spends-conflicting-tx[,:\s]\s*([0-9a-f]{64})\b|ancestor\s*\(?\s*([0-9a-f]{64})\s*\)?/gi

export interface Refusals {
  /** feed a broadcast result's description — matches only refusal
   *  shapes that name a dead ancestor; anything else is a no-op */
  noteRefusal(description: string | undefined): void
  /** live-mark check — expired entries answer false */
  refused(txid: string): boolean
  /** the deadParents oracle entry: the subject's own mark first, then a
   *  bounded ancestry walk; a hit marks the subject itself (it is dead
   *  for its descendants), inheriting the evidence's timestamp so the
   *  mark dies when the refusal does — and making repeat checks O(1) */
  parent(tx: Transaction): Promise<boolean>
}

export const openRefusals = (p: {
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  now?: () => number
  markMs?: number
  cap?: number
}): Refusals => {
  const now = p.now ?? (() => Date.now())
  const markMs = p.markMs ?? REFUSED_MARK_MS
  const marks = new BoundedMap<string, number>(p.cap ?? 4096)
  const refused = (txid: string): boolean => {
    const at = marks.get(txid.toLowerCase())
    return at !== undefined && now() - at < markMs
  }
  return {
    noteRefusal(description) {
      if (description === undefined) return
      for (const m of description.matchAll(REFUSED_ANCESTOR)) {
        const id = m[1] ?? m[2] ?? m[3]
        if (id !== undefined) marks.set(id.toLowerCase(), now())
      }
    },
    refused,
    async parent(tx) {
      if (tx.merklePath !== undefined) return false
      try {
        if (refused(tx.id('hex'))) return true
      } catch {
        // a stand-in parent cannot serialize — its ancestry still answers
      }
      let hitId: string | undefined
      const hit = (id: string): boolean => {
        if (!refused(id)) return false
        hitId = id
        return true
      }
      if (!(await ancestryHit(tx, hit, p.fetchTx))) return false
      // the subject's own ancestry contains a refused tx — it can never
      // air, so it is itself a refused ancestor for its descendants.
      // Mark it with the EVIDENCE's timestamp: the mark dies when the
      // refusal does, never a fresh window later. A directly-refused
      // subject keeps its own (fresher) mark.
      try {
        const id = tx.id('hex')
        const src = hitId === undefined ? now() : (marks.get(hitId) ?? now())
        const cur = marks.get(id)
        marks.set(id, cur !== undefined && cur > src ? cur : src)
      } catch {
        // a stand-in subject carries no id — the hit still stands
      }
      return true
    }
  }
}
