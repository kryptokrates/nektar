# system-design — DDIA rules at authoring time

Distilled from Kleppmann's _Designing Data-Intensive Applications_ for
code that touches persistence (SQLite, journals, `*.json` stores), wire
formats (envelopes, pack manifests, contract JSON), outbox/inbox flows,
chain interaction, or anything restartable. The bias being installed at
authoring time: **nothing is local, ordered, fresh, or exactly-once —
and every byte you write outlives the build that wrote it.**

## Evolvable data shapes — the decision you can't take back

Code is replaced in days; persisted data lives for the life of the
system. Rows written today will be read by builds that don't exist yet
and by builds you forgot exist (rolling upgrades, restarted daemons,
someone's stale regtest wallet). Every persisted/wire shape you create
is a **public contract** — design it like one.

| Flag                          | Symptom while writing                                                         | Rule                                                                                                                                    |
| ----------------------------- | ----------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `data/schema-drift`           | adding/renaming a field on a persisted shape without a reader-tolerance story | Old builds must read new rows (forward compat — unknown fields ignored) and new builds must read old rows (backward compat — defaults). |
| schema-by-convention          | a JSON/SQLite shape whose contract lives only in the writer's head            | Write the contract at the store's boundary: a `schema_version`/`type` field, a decode function that owns all historical shapes.         |
| rename-not-add                | renaming a persisted field for tidiness                                       | Renaming is delete+add — the compat cost is real. Keep the old name or keep a reader for it.                                            |
| `data/hidden-source-of-truth` | two stores can disagree and nothing says which wins                           | Name the arbiter in the design note or a comment: "the chain arbitrates; local stores cache intent." A missing arbiter is a finding.    |

## Durability ordering — the write path as a state machine

For every intent→action→outcome sequence, code the crash semantics
deliberately:

| Step                     | What dies here leaves behind | Design requirement                                  |
| ------------------------ | ---------------------------- | --------------------------------------------------- |
| before intent persisted  | clean no-op                  | nothing to replay                                   |
| after intent, before act | a journaled intent           | replay must re-drive safely (idempotent or dedup'd) |
| after act, before record | act done, outcome unknown    | re-drive must NOT double-apply — the hard case      |
| after outcome recorded   | done                         | no replay at all                                    |

`data/durability-point` (BLOCKER at review): a side effect — broadcast,
send, spend — that executes before the intent is journaled. Order:
**persist intent → act → record outcome.** New write paths reuse
`market_outbox` / `take()` journaling — inventing a fourth scheme is
convergent implementation.

## Delivery & dedup — the end-to-end argument

At-least-once is the substrate's gift, not a bug to hide: retries,
restarts, reconnects all re-deliver. Suppression belongs at the endpoint
that can see the whole operation.

| Flag                           | Symptom while writing                                                             | Rule                                                                                                                                                    |
| ------------------------------ | --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `data/exactly-once-fantasy`    | correctness requires an action to happen exactly once (broadcast, charge, reveal) | Pick one and name it: at-least-once + dedup, or at-most-once + reconcile. "Exactly once" without a mechanism is a fantasy.                              |
| `data/at-least-once-victim`    | consumer applies a delivered message with no dedup                                | Effects must be idempotent — same `action_id`, same result — or gated by a processed-set. The op-ID must originate at the CALLER, not mid-pipeline.     |
| `data/cross-store-transaction` | two stores updated assuming atomicity (sqlite + file, ledger + reserve)           | If they can't commit atomically, one is the intent log and the other the result — and reconcile covers the gap.                                         |
| `data/partial-fanout`          | a write to N hosts/stores where mid-fanout failure leaves disagreement            | Per-destination tracking (outbox rows) or a repair sweep. A fanout without per-destination state can't answer "who got it".                             |
| `data/read-your-writes`        | reading back a write via a lagging path and treating absence as nonexistence      | Lag is normal, not exceptional — quorum reads, host lookups, reserve scans. If the code conflates "not yet" with "never", the logic is wrong by design. |
| `data/ordering-scope`          | assuming global ordering where only per-entity order exists                       | Order-sensitive logic scopes to its key and says what it does with gaps/reorder.                                                                        |
| `data/lost-wakeup`             | a waiter that sleeps its whole interval because the signal fires off-path         | Every producer that can unblock a waiter must signal on every path — success AND failure.                                                               |

## Correctness: the end-to-end argument applied

TCP-style per-hop dedup doesn't survive the boundary — the retry between
user and app is a fresh request to the database. nektar's answer is the
one Kleppmann prescribes: a client-generated **operation ID passed
end-to-end** (the outbox `action_id`), a uniqueness constraint at the
point of application, and the derived state treated as recomputable from
the event. Design every new effect to accept an op-ID; design every new
consumer to record which op-IDs it has applied.

## Auditability — trust, but verify (design for it, don't bolt it on)

- **Self-auditing beats trusting.** For each store you add or extend,
  answer: how would we notice corruption here? A hash, a reconcile loop,
  a re-derivation path. `data/derived-unrebuildable` — derived state
  (indexes, caches, fingerprints, `refsHash`) MUST have a defined
  rebuild path from its source; write it with the index, not later.
- **Immutable beats mutable where you can afford it.** Intent logs and
  event-style journals make crash-replay and auditing deterministic —
  the derivation is repeatable, so bugs are _reproducible_ and fixes are
  _re-runnable_. Mutating a durable record destroys evidence; if you
  need mutation, journal the intent first.
- **The chain arbitrates.** Local stores cache intent; they are never
  proof of an on-chain fact. Any code treating a local row as settled
  truth is `data/hidden-source-of-truth` — reconciliation with the
  arbiter must exist AND be named.

## Operability — design for the operator who isn't you

Humans (and future agents) are unreliable components. Design the system
to absorb their mistakes:

- **Make the right thing easy, the wrong thing loud.** Abstractions and
  defaults should guide toward correct use; invalid states should be
  unrepresentable or refused loudly at the boundary (fail-slow is worse
  than fail-fast).
- **Separate where mistakes are made from where they cost.** Dangerous
  operations get a sandbox/dry-run/regtest path — decouple the place of
  experimentation from the place of consequence.
- **Predictable over clever.** Good defaults + the freedom to override;
  a knob nobody can set correctly is complexity exported to operators
  (that's pull-complexity-downward applied to config).
- **Telemetry isn't optional.** Every failure class needs a visible
  surface — a log line, a `last_error` column, a metric, a state
  transition. `stab/invisible-failure`: if a failure mode has no
  observable signal, it will be diagnosed as "weird" forever.
- **Recovery is a designed path.** Rollback, dead-letter review,
  recompute-from-source — every latch needs a documented way back.
  `stab/latch-unrecoverable` is a design defect, not an ops problem.

## Failure-mode checklist — before declaring done

For each changed path, name the behavior under:

- **Slow** — the call takes 10× its timeout: who waits, what's pinned?
  Every outbound call carries an explicit bound (`AbortSignal.timeout`
  is the repo convention — `stab/no-timeout` at review).
- **Partial** — 2 of 3 hosts accept: which reads see the write?
- **Corrupt** — response parses but is wrong: who verifies _content_,
  not just status? (`stab/optimistic-absence` — a 200 is not "did the
  thing".)
- **Restart** — what replays on next boot, from whose perspective?
- **Overload** — the backlog triples: what sheds first? Every queue,
  buffer, watch-set needs a bound and a named full-behavior
  (`stab/unbounded-queue`, `stab/load-shed-missing`).

`packages/bstack/src/testing.ts` (`faulthost`) exists to exercise
down/slow/withhold/corrupt — a new multi-host path without a fault test
is an unproven claim, not a finished change.
