# statecharts.md — the implementation contract

Load when a state-audit fix converges on "declare a machine." Library-
agnostic: everything below is implementable with a switch statement and the
existing journal; no library is assumed or recommended yet.

## The contract

- **The store stays the authority.** The machine is a pure transition
  function: `(persistedState, event) → (nextState, intents)`. It reads
  durable state, decides, and returns — it does not touch the world.
- **State + intents commit atomically.** Write the new state row and the
  intent rows (outbox pattern — `market_outbox` is the reference) in one
  transaction. Effects run _after_ commit, reading intents — never inline
  in the transition, where a crash would split decision from record.
- **Effects are idempotent, keyed by operation ID.** Delivery is
  at-least-once by construction (replays, retries, out-of-order events) —
  every effect must be safe to re-run under its key.
- **Machines don't fix wrong events.** The machine can only be as honest
  as what emits into it: a `confirmed` event fired off a status word
  poisons a perfect machine exactly as it poisons a switch statement.
  Event-source guards (`spv/self-submitted`, `nektar/write-side-verify`)
  are the gate — who may emit a verdict is an audit column, not an
  implementation detail.
- **`unknown` is a state, `expired`-class exits are exits.** A TTL that
  ends an intent is a state transition with consequences (leases released,
  custody revived) — not a verdict, and never conflated with `failed`.

## Rollout rules

- **One machine per resource**, never one app-wide machine. Candidates in
  this repo: `wallet_transactions`/`TxState` (cleanest union, replay
  already exists) and `market_operations` (the late-landing fixes live
  there — `funding_deferred`, `refund_pending`).
- **Shadow mode first.** Replay recorded events through the machine beside
  the existing FSM and diff transitions — procedure in `nektar.md`. Every
  diff gets a named cause before switchover.
- **The transition table the audit produced is the seed.** An audit finding
  of "empty cell" becomes a declared transition or a documented impossibility.

## XState — landed (`automation/src/machines/`)

XState v6 (`6.0.0-beta.3`, pinned) is the spec evaluator for the
declared machines: one `<resource>.table.json` (the audit's transition
table) plus a thin `.machine.ts` per resource, `step()` in `machine.ts`
as the pure transition function, `shadow.ts` for replay diffs,
`driver.ts` for live-write validation (`NEKTAR_TX_FSM`/`NEKTAR_OPS_FSM`
= `legacy` kill switches), `effects.ts` as the store-bound effector.
The constraints below are why the layer is shaped this way — they stay
binding on any change to it.

- **No actors.** `createActor` is never called — the machine is pure
  evaluation, not interpretation. That sidesteps the persistence
  footgun outright: restoring a persisted actor **restarts invoked
  actors** (persisted actions are _not_ re-executed, invoked work is),
  so an `invoke`d effect that moves money or sends would resubmit after
  a restart. v6's headline features — `xstate/durable`, `enq`, invoke
  timeouts, actor registries — are all actor features that stay unused;
  `restoreActor()` from `xstate/persist` is the v6 restore path if
  actors ever land (the old persisted-snapshot options on
  `createActor()` are gone). Second restart vector if actors ever
  land: `reenter: true` transitions stop and re-start the state's
  invoked actors mid-flight — a re-entering self-transition on a
  money-moving state is a resubmit with no process restart. Effects
  stay in the intent/outbox layer either way; nothing executes inside
  the machine.
- **SQLite owns the row.** `step()` hydrates with
  `machine.resolveState({ value: row.state })` — `resolveState`
  survives in v6 and **throws** (E14) on a persisted value the machine
  doesn't know, which is exactly the loud failure an unknown-state
  audit wants. `wallet_transactions.state` stays a string column, not
  an XState snapshot blob (a snapshot format competing with the row is
  `data/hidden-source-of-truth` by construction). This also makes the
  v6 migration hazard moot: v5 persisted snapshots are not
  binary-compatible with v6 — irrelevant here precisely because the
  row, not a snapshot, is what gets persisted. If context is ever
  persisted it must be JSON-serializable: no `bigint` sats, Buffers,
  or class instances — the machines run `context: {}` today.
- **`transition()` + `isUnhandled()` — `accepted` is a library
  guarantee.** In v6 `getNextSnapshot`/`getInitialSnapshot` are
  removed entirely; `transition(machine, snap, event)` returns
  `[snapshot, effects]` (effects as data — the seam intents would ride
  if a table ever declares machine actions). An unhandled event
  returns the same snapshot and `isUnhandled(prev, result)` reports
  it — `step()`'s `accepted` is that check, not the v5-era inference
  from "did the state move." A transition function returning
  `undefined` is how a cell marks an event unhandled.
- **Codegen: one function transition per (state, event) cell.** v6
  rejects the v5 guarded-transition array in `on`; `configFromTable`
  emits a single function per cell that picks the first table row the
  caller satisfies — guardless, or the row's guard string named on
  `event.guard`. No row satisfied → `undefined` → unhandled. The
  consequence worth remembering: **a guarded row the caller doesn't
  satisfy is a refusal, never an absorb** — under v5 a guarded
  self-row could accept _and fire its intents_ without the evidence
  being asserted; that path is gone. If a production caller needs an
  unguarded variant, declare it as its own row (e.g. `resubmit`), not
  by sending a guarded event bare.
- **`xstate/fsm` does not fit this layer.** Its snapshot dedupe makes
  a self-transition that changes nothing report `isUnhandled: true` —
  an accepted self-edge with intents becomes invisible. Full
  `createMachine` is the engine; keep it that way.
- **Empty cells are silent.** XState takes no transition and raises no
  error on an unhandled `(state, event)` — exhaustiveness is not free.
  `step()`'s `accepted: false` (now backed by `isUnhandled`), the
  `drift:` guard strings, and the census/reachability tests are the
  gate; keep them.
- **No `always`/`after`/`invoke`/`spawn`/`type:'final'` in the
  tables.** A state entered and left via `always` inside one microstep
  emits no snapshot — a state the ledger must record can't be a pure
  transient hop. v6 `choice` states are the same class of unobservable
  hop; v6 state-level `timeout`/`onTimeout` are volatile timers —
  durable deadlines stay in `due_at`/`due_height` columns.
  `type:'final'` would freeze late-evidence revival; terminal
  classification lives in the table's `terminal`/`quiescent`/`parked`
  fields, enforced by the graph test.
- **Graph tooling is `xstate/graph`.** The standalone `@xstate/graph`
  and `@xstate/test` packages are deprecated — `getShortestPaths`,
  `getSimplePaths`, `getAdjacencyMap` ship inside `xstate` itself; in
  v6 the old model-testing helpers are gone from that entry point. The
  hand-rolled reachability in `machines.test.ts` stays (it checks the
  quiescent/parked taxonomy, not just reachability); `getAdjacencyMap`
  is there if the checks ever want the library graph.
