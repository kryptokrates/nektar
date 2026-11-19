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

## XState — pending evaluation

_Unverified — a separate shadow-mode evaluation decides whether XState
fits nektar. The notes below are what the evaluation must check, not an
endorsement._

- **v5 persistence footgun**: restoring a persisted XState machine re-runs
  `invoke`d actors — invoked work that moves money or sends could resubmit
  after a restart. If XState is used, all effects must live in the
  intent/outbox layer (executed after commit by an effector), never in
  invoked actors. This is the single most important constraint the
  evaluation must validate.
- **Graph tooling**: `@xstate/graph` (or successor) provides reachability
  enumeration for the model-based verification layer in `audit.md` — check
  version compatibility with whatever XState release is evaluated.
- **Persisted shape**: machine state must serialize into the existing
  stores (`wallet_transactions.state`, `market_operations.state`) — the
  audit's authority contract is that SQLite owns state; a machine snapshot
  format that competes with the row is a `data/hidden-source-of-truth`
  violation by construction.
- **Non-goals for the evaluation**: do not restructure `market_outbox`,
  do not move money-path effects into actors, do not store a second
  truth — the existing journal is the effect ledger and stays it.
