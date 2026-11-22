import { createMachine, isUnhandled, transition, type AnyStateMachine } from 'xstate'
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
  /** False when the event is unhandled in this state — the shadow FSM logs it, never moves. */
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
  const rows = table.transitions.filter((r) => rowSources(r, table.states).includes(from) && r.event === event)
  const snap = machine.resolveState({ value: from, context: {} })
  const result = transition(machine, snap, { ...payload, type: event })
  // isUnhandled is the authoritative accepted signal — an unasserted guard is a
  // refused event, never an absorb. Hydrating an unknown persisted value throws.
  if (isUnhandled(snap, result)) return { next: from, intents: [], accepted: false }
  const nextState = toStateValue(result[0].value)
  // the row consistent with the machine's outcome owns the intents — a caller
  // that omits the guard still resolves to the first machine-passing sibling
  const matched = rows.find((r) => targetOf(r, from) === nextState)
  if (matched) return { next: nextState, intents: matched.intents ?? [], accepted: true, guard: matched.guard }
  // unreachable — the machine's targets are generated from these rows; a hit here
  // is a codegen bug, surfaced like the old drift path instead of silently followed
  return { next: nextState, intents: [], accepted: false, guard: `drift:machine=${nextState}` }
}

/** The concrete source states a row expands to — '*' stays symbolic here. */
function rowSources(row: TableRow, all?: string[]): string[] {
  if (row.state === '*') return all ?? ['*']
  return ([] as string[]).concat(row.state)
}
function targetOf(row: TableRow, from: string): string {
  return row.target === 'self' ? from : row.target
}
function toStateValue(v: unknown): string {
  return typeof v === 'string' ? v : Object.keys(v as object)[0]
}

/** Build an xstate config from a table — one transition fn per (state, event) cell. */
function configFromTable(table: TransitionTable) {
  // Each cell's fn selects the first row in table order that is either guardless
  // or whose guard string the caller names on `event.guard` — guarded siblings
  // disambiguate the same event the way v5's guarded-transition array did.
  // Returning undefined marks the event unhandled (no snapshot is produced),
  // which is what makes `accepted` exact: a guarded row the caller doesn't
  // satisfy is a refusal, never an absorb.
  const states: Record<string, { on: Record<string, (args: { event: AnyEventObject }) => { target: string } | undefined> }> = {}
  for (const s of table.states) states[s] = { on: {} }
  for (const src of table.states) {
    const cells = new Map<string, { target: string; guard?: string }[]>()
    for (const row of table.transitions) {
      if (!rowSources(row, table.states).includes(src)) continue
      const target = row.target === 'self' ? src : row.target
      const cell = cells.get(row.event)
      if (cell) cell.push({ target, guard: row.guard })
      else cells.set(row.event, [{ target, guard: row.guard }])
    }
    for (const [event, cell] of cells) {
      states[src].on[event] = ({ event }: { event: AnyEventObject }) => {
        const hit = cell.find((r) => r.guard === undefined || r.guard === event.guard)
        return hit ? { target: hit.target } : undefined
      }
    }
  }
  return { id: table.resource, initial: table.states[0], states }
}

export function buildMachine(table: TransitionTable): AnyStateMachine {
  // No xstate actors, timers, hierarchy or `type:'final'`: the spec's surface
  // is (state, event) → target only. Terminal classification lives in the
  // table's terminal/quiescent/parked fields, enforced by the graph test —
  // a machine-level `final` would freeze late-evidence revival edges.
  return createMachine(configFromTable(table) as never)
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
