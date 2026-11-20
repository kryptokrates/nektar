import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './lock.table.json' with { type: 'json' }

export const LOCK_TABLE = table as TransitionTable

/** Pure transition function over the lock resource — shadow mode. */
export const lockMachine = buildMachine(LOCK_TABLE)

export function lockStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(lockMachine, LOCK_TABLE, persistedState, event, payload)
}
