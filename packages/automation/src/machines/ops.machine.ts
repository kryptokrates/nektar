import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './ops.table.json' with { type: 'json' }

export const OPS_TABLE = table as TransitionTable

/** Pure transition function over market_operations rows — shadow mode. */
export const opsMachine = buildMachine(OPS_TABLE)

export function opsStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(opsMachine, OPS_TABLE, persistedState, event, payload)
}
