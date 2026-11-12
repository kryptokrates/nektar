// §12 temporal logic over consensus time. A timed fact is a formula with the
// consensus time t of its anchor (existence-by block time, see timeBounds).
//   always(φ, t)            □_t φ  — φ holds at or before t (and is never withdrawn by t)
//   eventually(φ, t1, t2)   ◇_(t1,t2) φ
//   obligation(φ, ψ, Δt)    φ →_t ψ — every φ at t is followed by ψ within Δt (SLA / deadline)
//   causalClosure(steps)    no reasoning step cites a fact anchored after the step
export interface TimedFact {
  phi: string
  t: number
  withdrawnAt?: number
}

const holdsAt = (f: TimedFact, t: number) => f.t <= t && (f.withdrawnAt === undefined || f.withdrawnAt > t)

export function always(facts: TimedFact[], phi: string, t: number): boolean {
  return facts.some((f) => f.phi === phi && holdsAt(f, t))
}
export function eventually(facts: TimedFact[], phi: string, t1: number, t2: number): boolean {
  return facts.some((f) => f.phi === phi && f.t >= t1 && f.t <= t2)
}
export interface ObligationResult {
  ok: boolean
  violations: Array<{ trigger: TimedFact; deadline: number }>
}
/** φ →_Δt ψ: each φ-occurrence needs a ψ-occurrence in [t, t + Δt]; `now`
 *  bounds the check (obligations whose deadline is in the future are open, not violated) */
export function obligation(facts: TimedFact[], phi: string, psi: string, dt: number, now: number): ObligationResult {
  const violations = facts
    .filter((f) => f.phi === phi)
    .map((trigger) => ({ trigger, deadline: trigger.t + dt }))
    .filter(({ trigger, deadline }) => deadline < now && !facts.some((g) => g.phi === psi && g.t >= trigger.t && g.t <= deadline))
  return { ok: violations.length === 0, violations }
}
export interface ReasoningStep {
  conclusion: string
  t: number
  cites: TimedFact[]
}
/** causal closure: a step at time t may only cite facts with consensus time ≤ t */
export function causalClosure(steps: ReasoningStep[]): { ok: boolean; reason?: string } {
  for (const s of steps) {
    const late = s.cites.find((c) => c.t > s.t)
    if (late) return { ok: false, reason: `step "${s.conclusion}" at t=${s.t} cites "${late.phi}" anchored at t=${late.t}` }
  }
  return { ok: true }
}
