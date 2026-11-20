import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './slot.table.json' with { type: 'json' }

export const SLOT_TABLE = table as TransitionTable

/** Pure transition function over the slot resource — shadow mode. */
export const slotMachine = buildMachine(SLOT_TABLE)

export function slotStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(slotMachine, SLOT_TABLE, persistedState, event, payload)
}
