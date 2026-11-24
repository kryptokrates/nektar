// F3 — BudgetAuthority: the principal-controlled enforcement point for
// cumulative grant budgets. A local single-writer ledger (spec's honest
// online dependency): new principal-funded spends MUST reserve here first;
// the invariant is committed + outstanding-reserved ≤ totalCapSats.
// Reservations are idempotent on actionId — a crash-retry reuses the slot,
// never double-counts.
import fs from 'node:fs'
import path from 'node:path'
import { sparse } from '@nektar/core'

interface GrantLedger {
  totalCap: number
  committed: number
  reservations: Record<string, number>
  completed?: Record<string, number>
  parents?: string[]
}

/** absent → undefined (first boot is fine); corrupt → throw (fail loud —
 *  silently treating unreadable persisted money state as empty is the
 *  absent-vs-corrupt conflation that un-revokes grants) */
const loadJson = <T>(file: string): T | undefined => {
  if (!fs.existsSync(file)) return undefined
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8')) as T
  } catch (e) {
    throw new Error(`budget: unreadable persisted ledger ${file}: ${e instanceof Error ? e.message : String(e)}`)
  }
}

export class BudgetAuthority {
  private file: string
  private ledger: Record<string, GrantLedger>
  private revoked = new Set<string>()

  constructor(dir: string) {
    this.file = path.join(dir, 'budget.json')
    // absent ≠ corrupt: a missing file is first boot, an unreadable one must
    // stop boot — silently loading {} would un-revoke grants / wipe the caps
    const revoked = loadJson<string[]>(this.revokedFile())
    if (revoked !== undefined) {
      if (!Array.isArray(revoked)) throw new Error(`budget: corrupt revoked ledger ${this.revokedFile()}`)
      this.revoked = new Set(revoked)
    }
    this.ledger = loadJson<Record<string, GrantLedger>>(this.file) ?? {}
  }

  private revokedFile(): string {
    return `${this.file}.revoked`
  }

  /** atomic write — a torn save must never read back as an empty ledger (R2).
   *  A FAILED save throws: in-memory state stays authoritative for this
   *  process, but silently dropping the write would let a restart
   *  under-count committed / re-open a revoked grant — overspend of the cap
   *  this class exists to enforce. Callers see the throw (fail closed). */
  private save(): void {
    const tmp = `${this.file}.tmp`
    fs.writeFileSync(tmp, JSON.stringify(this.ledger, null, 1))
    fs.renameSync(tmp, this.file)
  }

  private saveRevoked(): void {
    const tmp = `${this.revokedFile()}.tmp`
    fs.writeFileSync(tmp, JSON.stringify([...this.revoked]))
    fs.renameSync(tmp, this.revokedFile())
  }

  /** declare a grant's lifetime cap + its ancestor chain */
  setCap(grantId: string, totalCap: number, parents: string[] = []): void {
    if (!sparse(this.ledger[grantId])) this.ledger[grantId] = { totalCap, committed: 0, reservations: {}, parents }
    this.ledger[grantId].totalCap = totalCap
    if (parents.length) this.ledger[grantId].parents = parents
    this.save()
  }

  /** mark a grant revoked — new reservations reject immediately (R2/R7); persisted (N6) */
  revoke(grantId: string): void {
    this.revoked.add(grantId)
    this.saveRevoked()
  }
  isRevoked(grantId: string): boolean {
    return this.revoked.has(grantId)
  }

  /** reserve allInSats under grantId — 'ok' reserved, 'exists' idempotent hit,
   *  'denied' breaches a cap (own or any ancestor's), 'no-cap' unknown grant */
  reserve(grantId: string, actionId: string, allInSats: number): 'ok' | 'exists' | 'denied' | 'no-cap' {
    if (this.revoked.has(grantId)) return 'denied'
    const g = sparse(this.ledger[grantId])
    if (!g) return 'no-cap'
    if (actionId in (g.completed ?? {})) return 'exists' // R2: completed actions replay as idempotent hits, never re-debit
    if (actionId in g.reservations) return 'exists'
    // own + every ancestor's invariant: committed + outstanding ≤ cap (R2/N4).
    // The reservation is written to EVERY ancestor too, under a shared key —
    // a sibling's pending reservation becomes visible to other children.
    const wouldBreach = (lg: GrantLedger) => lg.committed + Object.values(lg.reservations).reduce((a, b) => a + b, 0) + allInSats > lg.totalCap
    if (wouldBreach(g)) return 'denied'
    for (const pid of g.parents ?? []) {
      const p = sparse(this.ledger[pid])
      if (!p) return 'no-cap' // unknown ancestor — fail closed
      if (this.revoked.has(pid) || wouldBreach(p)) return 'denied'
    }
    g.reservations[actionId] = allInSats
    for (const pid of g.parents ?? []) this.ledger[pid].reservations[`${grantId}:${actionId}`] = allInSats
    this.save()
    return 'ok'
  }

  /** commit a reservation — the spend settled; the action lands in `completed`
   *  forever (idempotency record), the reservation frees. Ancestors debit too. */
  commit(grantId: string, actionId: string): boolean {
    const g = sparse(this.ledger[grantId])
    if (!g || !(actionId in g.reservations)) return false
    const amt = g.reservations[actionId]
    delete g.reservations[actionId]
    g.committed += amt
    ;(g.completed ??= {})[actionId] = amt
    for (const pid of g.parents ?? []) {
      const p = sparse(this.ledger[pid])
      if (p) {
        delete p.reservations[`${grantId}:${actionId}`]
        p.committed += amt
        ;(p.completed ??= {})[`${grantId}:${actionId}`] = amt
      }
    }
    this.save()
    return true
  }

  /** release a reservation — the spend never escaped; headroom returns */
  release(grantId: string, actionId: string): boolean {
    const g = sparse(this.ledger[grantId])
    if (!g || !(actionId in g.reservations)) return false
    delete g.reservations[actionId]
    for (const pid of g.parents ?? []) {
      const p = sparse(this.ledger[pid])
      if (p) delete p.reservations[`${grantId}:${actionId}`]
    }
    this.save()
    return true
  }

  status(grantId: string): { totalCap: number; committed: number; reserved: number; headroom: number } | undefined {
    const g = sparse(this.ledger[grantId])
    if (!g) return undefined
    const reserved = Object.values(g.reservations).reduce((a, b) => a + b, 0)
    return { totalCap: g.totalCap, committed: g.committed, reserved, headroom: g.totalCap - g.committed - reserved }
  }
}
