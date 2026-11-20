import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './host.table.json' with { type: 'json' }

export const HOST_TABLE = table as TransitionTable

/** Pure transition function over the host resource — shadow mode. */
export const hostMachine = buildMachine(HOST_TABLE)

export function hostStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(hostMachine, HOST_TABLE, persistedState, event, payload)
}
