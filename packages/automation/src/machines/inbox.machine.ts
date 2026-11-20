import { buildMachine, step, type StepResult, type TransitionTable } from './machine.js'
import table from './inbox.table.json' with { type: 'json' }

export const INBOX_TABLE = table as TransitionTable

/** Pure transition function over the inbox resource — shadow mode. */
export const inboxMachine = buildMachine(INBOX_TABLE)

export function inboxStep(persistedState: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  return step(inboxMachine, INBOX_TABLE, persistedState, event, payload)
}
