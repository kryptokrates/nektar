# audit.md — worksheets and procedures for the durable plane

Load when running a restate audit. Each worksheet's empty cells are the
findings — fill them before writing anything.

## Worksheet 1 — handler inventory

One row per handler (object defs in `services.ts`, host defs via
`cfg.rtDefs`, plus any `restate.*` construct):

| service | handler | key shape | exclusive/shared | ctx ops used | sends emitted | errors thrown | rows written |
| ------- | ------- | --------- | ---------------- | ------------ | ------------- | ------------- | ------------ |

- `key shape` = how the key is derived (`actionId`, `ev:<wantId>`,
  `TRANSPORT_DRAIN_KEY`, `'global'`). A shape that can't reach the row's
  identity is `rt/key-collision`.
- `errors thrown` — classify: default retry / `TerminalError` /
  `RetryableError` / `PauseError`. Blank means "never audited" — every
  deterministic throw left on default policy is `rt/poisoned-leg`.
- `rows written` — cross-check against the key→row map (worksheet 3):
  a handler writing a row whose key doesn't cover it needs the row CAS.

## Worksheet 2 — send graph

One row per send site (grep seeds: `send(`, `Send(`, `objectSendClient`,
`serviceSendClient`, `genericSend`, `sendOpts`, `delay:`/`delayMs`,
`idempotencyKey`):

| emitter | target service/handler | key | delay? | detached? | idempotency key? | absorbed by |
| ------- | ---------------------- | --- | ------ | --------- | ---------------- | ----------- |

- `detached?` — every `send` IS detached (survives caller kill/cancel) —
  the column records whether the EMITTER's lifecycle can end while the
  send is armed (a cancellable invocation arming a wake = `rt/orphaned-send`
  candidate).
- `absorbed by` — what makes the second delivery harmless: row-state
  guard, content key, ingress idempotency key, `dedup`. Blank =
  `rt/at-least-once-victim`.
- The graph's cycles are the deadlock map: any request-response `call`
  edge that can loop back to the emitter's own key = `rt/self-deadlock`
  (verify the `inRtScope` escape covers the exact key pair).

## Worksheet 3 — key → row authority map

| domain row/table | writers (handlers) | keys they arrive on | same key? | CAS guard |
| ---------------- | ------------------ | ------------------- | --------- | --------- |

- Any row written from ≥2 different keys without a CAS check =
  `rt/key-collision` (e.g. `ev:<wantId>` event keys and `op:<opId>`
  advance keys both completing inbox rows — `revision`/row-state is the
  guard; name it).
- A fixed key covering unrelated row classes = `rt/over-serialized-key`
  — the old global mutex surviving as `key: 'main'`; measure what the
  key's queue actually serializes.
- Rows written by NON-handler code (the cycle glue, the host poll)
  bypass the key's serialization entirely — those writes need their own
  discipline (the busy flag / the store CAS).

## Worksheet 4 — replay-boundary walk

Per handler, list each statement as inside/outside `ctx.run`:

| statement | inside ctx.run? | re-fires on retry? | safe to re-fire? | fix |
| --------- | --------------- | ------------------ | ---------------- | --- |

- `safe to re-fire` answers: journaled send (re-fires once, deduped),
  idempotent effect (re-fires, absorbed), side-effect WITHOUT dedup
  (`rt/double-effect`), decision-driving read (`rt/replay-fork` — it
  must move inside), ctx op (`rt/nested-ctx` — illegal, won't compile
  but shows up as a runtime structure error under the SDK's own check).
- The store scan rule: a `SELECT` whose result fans sends belongs inside
  `ctx.run` — a re-scan on replay re-reads LIVE rows and can emit a
  different send set than the journal recorded.
- Also hunt: `Math.random`/`new Date()`/`Date.now()` in handler paths
  feeding journal ops (`rt/non-determinism` → RT0016), outer-scope
  mutation inside `ctx.run` read later (`rt/lost-mutation`).

## Worksheet 5 — arm/kill ownership

| armed thing | armer | survives its armer's kill? | absorbed/re-armed by |
| ----------- | ----- | -------------------------- | -------------------- |

- Detached sends and delayed sends survive — name the domain guard that
  makes them no-op when the row moved on (terminal state, cleared dueAt).
- Forced re-register orphans everything armed on the deployment — for
  each self-arming cadence (dealfan, drain, cron, watcher), name the
  re-arm path: the floor self-arm re-derives from the store each fire,
  or a boot poke re-covers it. Nothing named = `rt/deployment-orphan`.
- `ctx.sleep` inside an exclusive handler = `rt/sleep-blocks-key`; the
  long-wait pattern is a delayed send to self, not a sleep.

## Worksheet 6 — clock domain

| stamped field | stamping clock | arm's delay math | same clock? |
| ------------- | -------------- | ---------------- | ----------- |

- `next_at`/`dueAt`/`reconcile_at` stamp on the domain `now` (cfg.now /
  store clock). Every `delayMs = X - Y` must use the SAME `Y` as the
  stamp — a `Date.now()` subtraction against a virtual-clock stamp
  inflates every arm by the skew (`rt/dual-clock` — the delivered-row
  stall was exactly this).
- Restate side: delayed-send wake is computed by the SDK then re-read by
  the server — a skewed SDK/server clock pair fires early/late by the
  same mechanism.

## Kill/cancel semantics — the lookup table

| action                       | effect on the invocation                                              | effect on its detached sends  | effect on armed delayed sends |
| ---------------------------- | --------------------------------------------------------------------- | ----------------------------- | ----------------------------- |
| `cancel`                     | TerminalError at next await point; compensation runs up the call tree | **still execute fully**       | still fire                    |
| `kill`                       | stops immediately, no compensation, state may be inconsistent         | **still execute fully**       | still fire                    |
| `pause`                      | suspends retries until `resume`                                       | armed ones still fire         | still fire                    |
| `purge` (completed only)     | deletes journal+metadata                                              | already executed              | already fired                 |
| `restart-as-new`             | new invocation ID, partial journal copy optional                      | n/a                           | n/a                           |
| deployment `--force` replace | in-flight retries → RT0011/RT0016                                     | queued on the dead deployment | land RT0011                   |

The audit question every row answers: "when this invocation ends
abnormally, which of its emissions still fire, and what absorbs them?"

## Drills — pick per finding, don't run the set

1. **Mid-handler kill** — `kill -9` between journaled steps; restart →
   journaled steps skip, unjournaled sends re-fire once. On the
   `Dispatcher` the same kill = lost leg — the floor must re-cover it
   (the emulation can't journal; that's its honest gap).
2. **Double delivery** — redeliver the same send; second landing no-ops
   (row-state guard, content key, ingress key).
3. **Orphaned arm** — force-replace the deployment mid-arm; assert the
   domain floor re-arms.
4. **Poisoned leg** — deterministic throw → suspended retry on the key;
   the queue wedge is visible (not silent), fixable via pause/resume/
   kill — or `TerminalError` is the right verdict.
5. **Stale-attempt zombie** — sever mid-`ctx.run`; old attempt runs to
   completion — `attemptCompletedSignal` aborts or effects are
   idempotent.
6. **Deployment migration** — `pause` → `resume --deployment` a stuck
   invocation; replay survives (or the fix stayed inside `ctx.run`).
7. **Dispatcher drift** — run the same handler under `Dispatcher` and
   Restate: per-key serialization, delay, at-least-once, close-drains —
   a test passing on the emulator proves nothing about a semantics gap
   the emulator doesn't model (journaling, dedup, terminal-error
   routing). `rt/drift` when a test's green depends on the gap.

## Report footer

- handlers/services reviewed; send-graph completeness (what fraction of
  send sites were found by grep vs enumerated from the defs);
- journal-vs-row coverage limits — which legs' domain-write vs
  journal-write pairing was verified vs assumed;
- whether verification ran against `Dispatcher`, a live server
  (`NEKTAR_RESTATE_URL`), or neither — a Dispatcher run is not a Restate
  run;
- tests actually run.
