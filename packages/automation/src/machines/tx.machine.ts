import { buildMachine, declaredTransitions, step, type StepResult, type TransitionTable } from './machine.js'
import table from './tx.table.json' with { type: 'json' }

export const TX_TABLE = table as TransitionTable

/** Pure transition function over wallet_transactions rows — shadow mode. */
export const txMachine = buildMachine(TX_TABLE)

export function txStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(txMachine, TX_TABLE, persistedState, event, payload)
}

