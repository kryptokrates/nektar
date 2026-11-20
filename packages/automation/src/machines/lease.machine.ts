import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './lease.table.json' with { type: 'json' }

export const LEASE_TABLE = table as TransitionTable

/** Pure transition function over the lease resource — shadow mode. */
export const leaseMachine = buildMachine(LEASE_TABLE)

export function leaseStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(leaseMachine, LEASE_TABLE, persistedState, event, payload)
}
