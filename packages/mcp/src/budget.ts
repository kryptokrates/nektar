// F3 — BudgetAuthority: the principal-controlled enforcement point for
// cumulative grant budgets. A local single-writer ledger (spec's honest
// online dependency): new principal-funded spends MUST reserve here first;
// the invariant is committed + outstanding-reserved ≤ totalCapSats.
// Reservations are idempotent on actionId — a crash-retry reuses the slot,
// never double-counts.
import fs from 'node:fs'
import path from 'node:path'

interface GrantLedger { totalCap: number, committed: number, reservations: Record<string, number> }

export class BudgetAuthority {
  private file: string
  private ledger: Record<string, GrantLedger>

  constructor (dir: string) {
    this.file = path.join(dir, 'budget.json')
    try { this.ledger = JSON.parse(fs.readFileSync(this.file, 'utf8')) } catch { this.ledger = {} }
  }

  private save (): void { try { fs.writeFileSync(this.file, JSON.stringify(this.ledger, null, 1)) } catch {} }

  /** declare a grant's lifetime cap — call when a grant is issued */
  setCap (grantId: string, totalCap: number): void {
    if (!this.ledger[grantId]) this.ledger[grantId] = { totalCap, committed: 0, reservations: {} }
    this.ledger[grantId].totalCap = totalCap
    this.save()
  }

  /** reserve allInSats under grantId — 'ok' reserved, 'exists' idempotent hit,
   *  'denied' would breach the invariant, 'no-cap' no ledger yet */
  reserve (grantId: string, actionId: string, allInSats: number): 'ok' | 'exists' | 'denied' | 'no-cap' {
    const g = this.ledger[grantId]
    if (!g) return 'no-cap'
    if (actionId in g.reservations) return 'exists'
    const reserved = Object.values(g.reservations).reduce((a, b) => a + b, 0)
    if (g.committed + reserved + allInSats > g.totalCap) return 'denied'
    g.reservations[actionId] = allInSats
    this.save()
    return 'ok'
  }

  /** commit a reservation — the spend settled; reserved → committed */
  commit (grantId: string, actionId: string): boolean {
    const g = this.ledger[grantId]
    if (!g || !(actionId in g.reservations)) return false
    g.committed += g.reservations[actionId]!
    delete g.reservations[actionId]
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
