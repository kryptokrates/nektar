import { createMachine, getNextSnapshot, setup, type AnyStateMachine } from 'xstate'
import type { AnyEventObject } from 'xstate'

export type { AnyStateMachine }

/** Parsed machine-readable transition table — the library-agnostic spec. */
interface TableRow {
  /** source state, an explicit list of source states, or '*' for all */
  state: string | string[]
  event: string
  guard?: string
  intents?: string[]
  target: string // 'self' = source state
}
export interface TransitionTable {
  resource: string
  /** Row-creation states — graph-test roots (a row may begin life in any of these). */
  roots?: string[]
  states: string[]
  terminal: string[]
  quiescent?: string[]
  parked?: string[]
  nonterminal?: string[]
  notes?: Record<string, string>
  transitions: TableRow[]
}

/** Result of the pure transition step: (persistedState, event) → {next, intents, accepted}. */
export interface StepResult {
  /** The declared next persisted state (== from-state when the event is impossible here). */
  next: string
  /** Effects to persist in the same transaction, run after commit, idempotent + op-keyed. */
  intents: string[]
  /** False when the event is impossible in this state — the shadow FSM logs it, never moves. */
  accepted: boolean
  /** The guard string from the table row, for traceability. */
  guard?: string
}

/**
 * Pure evaluator: the machine is a function, not an actor.
 * SQLite remains authority — hydrate with machine.resolveState({value}), evaluate once,
 * persist next+intents in one transaction, then run effects.
 */
export function step(machine: AnyStateMachine, table: TransitionTable, from: string, event: string, payload: Record<string, unknown> = {}): StepResult {
  // Guarded sibling rows (same state+event, different guards) resolve via the
  // caller-supplied `guard` key on the payload — the effector owns guard truth.
  const rows = table.transitions.filter((r) => rowSources(r, table.states).includes(from) && r.event === event)
  const row = rows.length > 1 && typeof payload.guard === 'string' ? (rows.find((r) => r.guard === payload.guard) ?? rows[0]) : rows[0]
  const snap = machine.resolveState({ value: from, context: {} })
  const next = getNextSnapshot(machine, snap, { ...payload, type: event } as AnyEventObject)
  const nextState = toStateValue(next.value)
  // the row consistent with the machine's outcome owns the intents — a caller
  // that omits the guard still resolves to the first machine-passing sibling
  const matched = rows.find((r) => targetOf(r, from) === nextState)
  if (matched) return { next: nextState, intents: matched.intents ?? [], accepted: true, guard: matched.guard }
  const accepted = nextState !== from || isSelfRow(row, from)
  if (!accepted) return { next: from, intents: [], accepted: false }
  const target = row ? targetOf(row, from) : nextState
  // Machine drifted from the spec — shadow mode surfaces this as a diff, never silently follows.
  return { next: nextState, intents: row?.intents ?? [], accepted: false, guard: `drift:machine=${nextState},table=${target}` }
}

/** The concrete source states a row expands to — '*' stays symbolic here. */
function rowSources(row: TableRow, all?: string[]): string[] {
  if (row.state === '*') return all ?? ['*']
  return ([] as string[]).concat(row.state)
}
function isSelfRow(row: TableRow | undefined, from: string): boolean {
  return !!row && targetOf(row, from) === from
}
function targetOf(row: TableRow, from: string): string {
  return row.target === 'self' ? from : row.target
}
function toStateValue(v: unknown): string {
  return typeof v === 'string' ? v : Object.keys(v as object)[0]
}

/** Build an xstate config from a table — one transition object per (state, event) pair. */
function configFromTable(table: TransitionTable) {
  // No state is declared `type:'final'`: terminals keep self-loop globals, and
  // quiescent/parked states keep revival edges. Terminal classification lives
  // in the table and is enforced by the graph test, not by xstate.
  const states: Record<string, { on: Record<string, { target: string; guard?: { type: string; params: { guard: string } } }[]> }> = {}
  for (const s of table.states) states[s] = { on: {} }
  for (const row of table.transitions) {
    const sources = rowSources(row, table.states)
    for (const src of sources) {
      const st = states[src]
      if (!st) continue
      const target = row.target === 'self' ? src : row.target
      const t: { target: string; guard?: { type: string; params: { guard: string } } } = { target }
      if (row.guard) t.guard = { type: 'guardMatches', params: { guard: row.guard } }
      ;(st.on[row.event] ??= []).push(t)
    }
  }
  return { id: table.resource, initial: table.states[0], states }
}

export function buildMachine(table: TransitionTable): AnyStateMachine {
  // Quiescent/parked states keep outgoing edges → not xstate `final`.
  return setup({
    guards: {
      // The spec's guard strings are opaque to the machine — the effector owns
      // their truth and names the satisfied guard on `event.guard`.
      guardMatches: ({ event }, params: { guard: string }) => event.guard === params.guard
    }
  }).createMachine(configFromTable(table) as never) as unknown as AnyStateMachine
}

/** Enumerate every concrete (state, event) → target the machine declares — for the diff test. */
export function declaredTransitions(table: TransitionTable): { state: string; event: string; target: string }[] {
  const out: { state: string; event: string; target: string }[] = []
  for (const row of table.transitions) {
    for (const src of rowSources(row, table.states)) {
      out.push({ state: src, event: row.event, target: row.target === 'self' ? src : row.target })
    }
  }
  return out
}
