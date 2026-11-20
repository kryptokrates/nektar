import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './lane.table.json' with { type: 'json' }

export const LANE_TABLE = table as TransitionTable

/** Pure transition function over the lane resource — shadow mode. */
export const laneMachine = buildMachine(LANE_TABLE)

export function laneStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(laneMachine, LANE_TABLE, persistedState, event, payload)
}
