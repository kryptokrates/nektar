import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './outbox.table.json' with { type: 'json' }

export const OUTBOX_TABLE = table as TransitionTable

/** Pure transition function over the outbox resource — shadow mode. */
export const outboxMachine = buildMachine(OUTBOX_TABLE)

export function outboxStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(outboxMachine, OUTBOX_TABLE, persistedState, event, payload)
}
