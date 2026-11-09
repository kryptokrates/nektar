// F3 — BudgetAuthority: the principal-controlled enforcement point for
// cumulative grant budgets. A local single-writer ledger (spec's honest
// online dependency): new principal-funded spends MUST reserve here first;
// the invariant is committed + outstanding-reserved ≤ totalCapSats.
// Reservations are idempotent on actionId — a crash-retry reuses the slot,
// never double-counts.
import fs from 'node:fs'
import path from 'node:path'

interface GrantLedger { totalCap: number, committed: number, reservations: Record<string, number>, completed?: Record<string, number>, parents?: string[] }

export class BudgetAuthority {
  private file: string
  private ledger: Record<string, GrantLedger>
  private revoked = new Set<string>()

  constructor (dir: string) {
    this.file = path.join(dir, 'budget.json')
    try { this.ledger = JSON.parse(fs.readFileSync(this.file, 'utf8')) } catch { this.ledger = {} }
  }

  /** atomic write — a torn save must never read back as an empty ledger (R2) */
  private save (): void {
    try { const tmp = `${this.file}.tmp`; fs.writeFileSync(tmp, JSON.stringify(this.ledger, null, 1)); fs.renameSync(tmp, this.file) } catch {}
  }

  /** declare a grant's lifetime cap + its ancestor chain */
  setCap (grantId: string, totalCap: number, parents: string[] = []): void {
    if (!this.ledger[grantId]) this.ledger[grantId] = { totalCap, committed: 0, reservations: {}, parents }
    this.ledger[grantId].totalCap = totalCap
    if (parents.length) this.ledger[grantId].parents = parents
    this.save()
  }

  /** mark a grant revoked — new reservations reject immediately (R2/R7) */
  revoke (grantId: string): void { this.revoked.add(grantId) }
  isRevoked (grantId: string): boolean { return this.revoked.has(grantId) }

  /** reserve allInSats under grantId — 'ok' reserved, 'exists' idempotent hit,
   *  'denied' breaches a cap (own or any ancestor's), 'no-cap' unknown grant */
  reserve (grantId: string, actionId: string, allInSats: number): 'ok' | 'exists' | 'denied' | 'no-cap' {
    if (this.revoked.has(grantId)) return 'denied'
    const g = this.ledger[grantId]
    if (!g) return 'no-cap'
    if (actionId in (g.completed ?? {})) return 'exists' // R2: completed actions replay as idempotent hits, never re-debit
    if (actionId in g.reservations) return 'exists'
    // own + every ancestor's invariant: committed + outstanding ≤ cap (R2)
    const wouldBreach = (lg: GrantLedger) => lg.committed + Object.values(lg.reservations).reduce((a, b) => a + b, 0) + allInSats > lg.totalCap
    if (wouldBreach(g)) return 'denied'
    for (const pid of g.parents ?? []) { const p = this.ledger[pid]; if (p && wouldBreach(p)) return 'denied' }
    g.reservations[actionId] = allInSats
    this.save()
    return 'ok'
  }

  /** commit a reservation — the spend settled; the action lands in `completed`
   *  forever (idempotency record), the reservation frees. Ancestors debit too. */
  commit (grantId: string, actionId: string): boolean {
    const g = this.ledger[grantId]
    if (!g || !(actionId in g.reservations)) return false
    const amt = g.reservations[actionId]!
    delete g.reservations[actionId]
    g.committed += amt
    ;(g.completed ??= {})[actionId] = amt
    for (const pid of g.parents ?? []) { const p = this.ledger[pid]; if (p) { p.committed += amt; ;(p.completed ??= {})[`${grantId}:${actionId}`] = amt } }
    this.save()
    return true
  }

  /** release a reservation — the spend never escaped; headroom returns */
  release (grantId: string, actionId: string): boolean {
    const g = this.ledger[grantId]
    if (!g || !(actionId in g.reservations)) return false
    delete g.reservations[actionId]
    this.save()
    return true
  }

  status (grantId: string): { totalCap: number, committed: number, reserved: number, headroom: number } | undefined {
    const g = this.ledger[grantId]
    if (!g) return undefined
    const reserved = Object.values(g.reservations).reduce((a, b) => a + b, 0)
    return { totalCap: g.totalCap, committed: g.committed, reserved, headroom: g.totalCap - g.committed - reserved }
  }
}
