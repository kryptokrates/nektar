---
name: restate
description: >
  Use when writing, reviewing, or debugging code on the durable execution
  plane — Restate Virtual Objects, services, handlers, sends, delayed
  sends, ctx.run journaling, retries, awakeables/signals, deployment
  registration and versioning — or when answering "why is this invocation
  suspended / wedged / orphaned / replaying". Trigger surfaces: a new
  handler or send site, a delay/timer arm, an error path inside a handler,
  a key choice, an idempotency question, a restart/deployment interaction,
  or any place the journal and the domain row could disagree. Owns the
  mechanics of the plane; the lifecycle correctness of what rides on it
  belongs to state-audit (this skill's checks plug into its transition
  table). For wait-site/event design use event-arch; for what counts as
  believed use spv-strict; for money-path invariants use code-review.
---

# restate — the journal is the plane

Restate splits every lifecycle into **two records**: the domain row
(SQLite — verdict truth) and the invocation journal (queued / running /
suspended / completed — _delivery_ truth). The single rule everything
here derives from: **a completed invocation proves the handler ran, not
that the domain accepted the effect; a suspended invocation proves a leg
is being retried, not that the row is stuck.** Every bug class this skill
hunts is a disagreement between those two records, or a property the code
assumed the plane has that it doesn't.

The docs' framing: durable execution = a journaled replay where completed
steps skip and uncompleted steps re-run. The audit's job is checking that
what must re-fire does (sends, probes), what must not re-fire can't
(sends with dedup, idempotent effects), and what must survive a crash is
journaled at the right grain.

## Scope

- `--handlers <path>` — enumerate handlers/sends under a path (one object
  or a whole region).
- `--service <name>` — one `EXEC_SVC` entry: its key shape, handler set,
  send graph, error surface.
- `--diff` / `--last [n]` — audit the durable-execution surface a diff
  adds: new sends, new arms, new error paths, new keys.
- `--drift` — audit the local `Dispatcher` against the Restate contract
  (the test seam must emulate the plane or tests prove nothing).
- `--wire` — setup/topology audit: server, endpoint, registration, env
  seams, public/private ingress — see
  [references/integration.md](references/integration.md).

## The model — five load-bearing facts

1. **`ctx.run` is the replay boundary.** Inside: executes once, result
   journaled, replay returns the record. Outside: re-runs on every retry.
   Reads that drive decisions belong inside (replay must see the SAME row
   set — a re-scanned store can fork the replay); sends/side-effects that
   must re-fire belong outside. `ctx` ops are illegal inside `ctx.run`
   (no nested runs, no ctx.get/calls) — a nested op is a compile-visible
   structure error, not a style choice.
2. **A key is a lock, nothing else.** Exclusive handlers on one object
   key run serially in arrival order; shared handlers run concurrently
   against it. Two different keys writing the same domain row is the old
   mutex race in new clothes — the row CAS (`revision`) is the guard.
   A fixed key (`'main'`, `TRANSPORT_DRAIN_KEY`) re-serializes unrelated
   work — name why the breadth is safe (drain scans everything anyway).
3. **Sends are detached; calls are attached.** `send`/`genericSend`/
   delayed send survive the originator's cancel AND kill — the docs are
   explicit: detached sends "will be scheduled and execute fully even if
   the originating invocation is cancelled." Killing an op does not kill
   its armed sends — the row's state must absorb them or they're unowned
   writers. `call` waits, is journaled, and dies with the caller —
   request-response between exclusive handlers deadlocks (A↔B same keys,
   or A→B→C→A cycle). A `call` whose result the producer DISCARDS is an
   anti-pattern — it holds the caller's socket for the handler's whole
   queue+duration when `send` resolves at acceptance and gives the same
   delivery guarantee (`rt/call-holds-producer`; the exception is the
   coalescing pattern where the call's completion bounds queue depth).
4. **Errors are taxonomy, not control flow.** Default is infinite
   exponential retry — an uncaught throw is a durable suspended retry on
   the key, and it wedges every queued invocation behind it.
   `TerminalError` is the only throw that stops (propagates code+metadata
   to the caller — a _verdict_, not a failure). `RetryableError` carries
   `retryAfter` (Rate-limit/`Retry-After` legs). `PauseError` inside
   `ctx.run` parks the invocation for operator intervention. Per-run
   retry policy bounds attempts/duration — an unbounded poisoned leg is
   a key-queue DoS.
5. **Deployments are immutable; invocations pin.** Retries land on the
   deployment that started them. Force-replacing a registration orphans
   armed delayed sends (they land as RT0011 silence — the domain floor
   must re-arm). Code changes reorder/remove/add journal ops →
   RT0016 non-determinism on replay; the only safe in-place edit is
   inside an existing `ctx.run` body. Stuck invocations migrate via
   `pause` → `resume --deployment`.

## The audit — the checks, run on every durable surface

1. **Replay boundary at the right grain.** Walk each handler: what's
   inside `ctx.run` vs outside. Side-effect outside without dedup =
   `rt/double-effect`. Decision-driving read outside = `rt/replay-fork`.
   ctx op inside = `rt/nested-ctx`. Store scans belong inside (replay
   re-sends the same rows); sends belong outside (they re-fire — which is
   the point).
2. **Redelivery absorbed — and only redelivery.** Every send is
   at-least-once. The second landing must no-op: row-state guard,
   content key, or ingress `idempotency-key` (dedupes across independent
   invocations; within one handler execution sends dedupe
   automatically). A handler that appends on redelivery is
   `rt/at-least-once-victim`. The flip side is over-dedup: the key must
   name the ATTEMPT/effect instance, not the row — a stable
   `deliver:<rowId>` key dedupes the domain's own retry of that row
   inside the 24h retention window (`rt/dedup-kills-retry` — outbox rows
   dead-lettered having hit the wire once; shape is
   `deliver:<rowId>:<attempt>`). Event-identity keys (`wake:<event>`)
   suppress recurring evidence — a second reorg at the same height, a
   resubmit→re-reject, a re-mine under a different path —
   `rt/dedup-kills-event`; content-key the evidence (digest of the
   payload) or drop dedup on idempotent legs. And the dedup store
   itself: an in-memory Set/Map that `clear()`s at a cap re-fires every
   pending send in one burst — evict-by-age (expired windows, fired
   arms) and bound the map (`rt/dedup-map-reset`). And the map is only
   ever the cheap pre-filter: it dies at restart while queued
   invocations live on, so the fan-out send carries the same window as
   a bucketed ingress key (`fan:adv:<op>:<floor(now/ms)>`) — a
   post-restart re-fan dedupes against the still-pending leg instead
   of piling one fresh invocation per row (`rt/fan-dedup-restart-gap`).
   And on a LIVE server the same durability applies to the TEST: a
   consumed idempotency key is spent for ~24h and a leftover self-arm
   chain keeps firing into any endpoint that registers its service
   name — live tests asserting on shared names/keys inherit ghosts
   from prior runs (queued deliveries on shared object keys, extra
   ticks in counters). `rt/test-durable-state`: scope service names +
   object keys + destinations with a per-run suffix, or the suite is
   only green on a fresh container — found live in rt-spike.
3. **Same-key discipline — both directions.** Map which keys write
   which rows. Cross-key writers of one row without CAS =
   `rt/key-collision`. A same-key request-response `call` (or A↔B
   cycle) without a scope escape = `rt/self-deadlock` — nektar's escape
   is `inRtScope`/`rtScopeKey` (ALS-carried scope bypasses the object
   hop; the wire must merge not replace nested scopes). `ctx.sleep` or
   a blocked `call` inside an exclusive handler queues the whole key =
   `rt/sleep-blocks-key`. And the inverse: a key serializing MORE than
   one writer domain = `rt/global-lock` — found live: custody legs all
   landed on `custody/'global'`, so every deal's funding serialized
   behind every other deal's broadcast RTT and a resubmit scan blocked
   new submissions. Key scope must match the lock domain the domain
   actually needs — per-row legs get per-row keys
   (`custody/<actionId>`), only genuinely-global scans keep the fixed
   key. The parallelizability check is on the row's invariants (per-row
   reads + CAS'd writes + idempotent broadcasts = parallel-safe), not
   on habit.
4. **Detached-send ownership.** Every `send`/delayed send outlives its
   originator's kill/cancel — name what absorbs it when the domain row
   has moved on (state guard, no-op handler). An arm that outlives its
   kill is `rt/orphaned-send`. A forced deployment replacement orphans
   every armed send on it (`rt/deployment-orphan` — the floor cadence
   must re-arm what the journal lost; we hit this class live). And the
   inverse failure: **every invocation that self-arms adds a chain.** A
   tick/drain body that ends by arming its successor multiplies arms by
   invocation count — every poke/boot/wake-path invocation adds one
   permanent self-arming chain, and the key's delayed-send backlog grows
   without bound (`rt/arm-multiplication` — found live: outboxReady
   pokes + wake-cycle drains each added a chain). The fix is a
   content-derived arm key that converges chains to ≤1 pending arm:
   domain-derived wake (`drain:${nextAt}`) or cadence-quantized
   (`tick:${floor(now/ceil)*ceil+ceil}`). A process-local arm-dedup map
   alone is not enough — it dies at restart while the delayed send
   lives on; the arm send itself needs a content key
   (`due:<op>:<at>`) so re-arms dedupe at the ingress against the
   still-pending invocation (`rt/due-arm-restart-dup`).
5. **Clock domain.** A delayed send's `delayMs` is computed at arm time
   against a clock — it must be the SAME clock that stamped the deadline
   (`next_at`, `dueAt`, `reconcile_at` all stamp on the domain `now`).
   Real-clock arm vs virtual/domain stamp = every arm early or late by
   the skew (we parked a `delivered` row pending exactly this way).
   `rt/dual-clock`. And precision: a wire format that floors delays at
   whole seconds inflates every sub-second arm — `?delay=` accepts ms;
   a 250ms arm encoded as `1s` lands 4× late (`rt/delay-quantization`).
6. **Error classification.** Every throw inside a handler: retried
   forever (default — is that right? does the leg eventually succeed or
   is it deterministically broken?), `TerminalError` (verdict — the
   domain records the terminal outcome itself), `RetryableError`
   (backoff-shaped), `PauseError` (operator gate). A deterministic throw
   left on default policy is a `rt/poisoned-leg` queue wedge — the key
   serializes behind it forever. And the retry BUDGET must match the
   key's temperature: a self-healing floor (drain/cron/dealfan — the
   next arm or poke re-drives the work) should fail fast and release
   the FIFO, not back off 30 times while every queued pass waits
   (`rt/hot-key-poison-window` — `RT_OPTS_FLOOR` maxAttempts:5 on
   floors, :30 on lifecycle/money objects whose retries ARE the
   recovery).
7. **Timeout is not a verdict.** `orTimeout`, inactivity suspend (>1min
   no new journal entries → suspend+retry; legs must fit under it or the
   service raises `options.inactivityTimeout`), and
   `attemptCompletedSignal` aborts are delivery-layer events — a
   timed-out leg may have committed anyway (the old attempt runs to
   completion; its result is discarded, its effects are not).
   `submission_unknown` is the only honest row for it.
   `rt/timeout-as-verdict` when code treats either as failure. A
   monolithic `ctx.run` is the amplifier: a scan pass that legitimately
   exceeds the inactivity bound suspends mid-step, and its retry
   replays the WHOLE pass from zero — a scan that always exceeds never
   completes and the key wedges on its own backlog
   (`rt/heavy-pass-monolith`). Raise `options.inactivityTimeout`/
   `abortTimeout` for the heavy pass (`RT_OPTS_HEAVY`); the real fix
   when it still exceeds is sub-stepping the journal, not a bigger
   number.
8. **No wait is a poll.** Every recurring wait names its producer
   signal — a send, an awakeable/promise/signal, or a call's completion
   — with the self-arm as the bounded ceiling behind it. A cadence that
   fires "every N" to check for work is a poll; a cadence that fires at
   the store-computed `next_at` is the ceiling. `rt/poll-as-mechanism`
   when the mechanism is the interval rather than the event (the
   replacement table is in references/integration.md §3). And the
   producer side of every wake: a send per write/event piles one full
   scan per poke on the key's FIFO — `rt/poke-storm`. Coalesce: a
   flag+`call` for process-side producers (the call resolves at pass
   completion; pokes in flight set wantMore — bounded, and no wake is
   ever lost to a time bucket), or a short bucketed `idempotency-key`
   for journaled hint sends inside handlers (safe only because the
   hint's rows are already committed — a queued pass scans at RUN time
   and sees them).
9. **Ingress exposure is a boundary — and registration is existence.**
   Internal objects (`ingressPrivate: true`) never answer the public
   ingress; endpoint `identityKeys` pin the caller to our Restate
   instance in production. `rt/public-internal` / `rt/open-endpoint`.
   A failed deployment registration must fail the boot — a runtime
   whose sends 404 into an unregistered ingress is up-but-dead
   (`rt/dead-executor`). Registering is a manifest compare: reuse the
   deployment when the service+handler set is identical, force-register
   only on drift — `force:true` per boot orphans every armed send per
   restart (`rt/deployment-orphan`).

## The DB-boundary pattern (docs' own guidance — nektar's exact shape)

The `guides/databases` page prescribes what we built: keep the external
DB authoritative, use Restate's guarantees as the consistency helpers.

- **VO key ↔ row PK.** Serialize a row's writers by routing them through
  the object keyed on the row's identity (tx per `actionId`, deal per
  want id, custody per `CUSTODY_KEY`). Single-writer-per-key replaces the
  process mutex.
- **`ctx.run` the reads that drive decisions.** The journaled value is
  what every retry sees — consistent version/credits/row-set across
  retries and zombies. Then **CAS on the journaled version** in the
  update (`WHERE revision = <read>`) — the docs' "semantic lock" = our
  `transition(op, revision, …)` exactly.
- **Idempotency tokens in the same tx as the effect.** An actionId /
  event row committed with the domain write is the DB-level dedup;
  scheduled expiry is a delayed send (the docs expire theirs with one).
- **Shared handlers for reads** — they run concurrently against the
  exclusive queue; reads that don't need serialization should not queue.
- **Restate K/V vs the DB**: K/V wins for state married to execution
  (session, agent context, lock tokens); the DB wins for core domain
  data, SQL access patterns, and anything other systems must read
  without going through the object. nektar: journal ≠ domain — the ops/
  inbox/outbox/utxo tables stay in SQLite.

## Nektar's plane — the mapping

| Restate concept          | nektar surface                                                                                                                    |
| ------------------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| Virtual Object per key   | `EXEC_SVC` objects — tx/`actionId`, deal/want, custody/`actionId` (legs)                                                          |
| Fixed-key object         | `TRANSPORT_DRAIN_KEY`, `CRON_KEY`, `CUSTODY_KEY` (resubmit scan), sweep/`'main'`, upkeep/watchdog/watches                         |
| `ctx.run` journaled step | handler bodies in `services.ts` — thin wrappers over domain code                                                                  |
| Delayed send             | due arms, drain self-arm at `nextOutboxAt`, dealfan/cron self-arms                                                                |
| Send (detached)          | `rtSend`/`exec.send` — dealfan fan-outs, wake cycle-tails, boot pokes                                                             |
| Same-key call deadlock   | `inRtScope`/`rtScopeKey` — ALS scope bypasses the object hop                                                                      |
| Ingress idempotency-key  | `sendOpts.idempotencyKey` — per-ATTEMPT (`deliver:<id>:<n>`), per-INSTANT arms (`due:<op>:<at>`), poke buckets (`<svc>:poke:<b>`) |
| Orphaned arm (RT0011)    | floor cadence re-arms — self-arm re-derives from the store each fire                                                              |
| `Dispatcher` (test seam) | `cfg.exec` injection — must emulate: per-key serial, delay, at-least-once, close drains                                           |
| Awakeable/approval gate  | policy escalations, `funding_deferred` waits — resolve, don't poll                                                                |
| Service `options:`       | retryPolicy / inactivityTimeout / journalRetention / ingressPrivate knobs                                                         |
| Flow-control scope       | `ctx.scope`/`limitKey` — bounds a class of invocations (e.g. broadcaster legs)                                                    |
| Testcontainers env       | `alwaysReplay`/`disableRetries` — the no-server alternative to `NEKTAR_RESTATE_URL`                                               |
| `@restatedev/xstate`     | machines-as-specs stay ours; the lib exists if a machine ever needs to BE the executor                                            |

## Drills (verification recipes)

The live ones run in `test/rt-live.test.ts` + `rt-spike.test.ts` +
`rt-replay.test.ts` — `pnpm test:rt-live` runs all three (docker one-liner
in the file headers). Endpoint loss is exercised by `RtEndpoint.close()`
force-destroying sessions — crash semantics, in-flight work retries on the
reopened endpoint. Ghost-isolation rule: per-run service names/keys
(`rt/test-durable-state`). `rt-replay` runs `alwaysReplay`/
`disableRetries` through `@restatedev/restate-sdk-testcontainers` —
no `NEKTAR_RESTATE_URL` needed, each test spawns its own container.
Note the mechanism: `alwaysReplay` is `INACTIVITY_TIMEOUT=0s` — replays
engage only at real suspension points (`ctx.sleep`, awakeables, call
responses). A suspension-free body (sends + ctx.run only, like our
fan-out bodies) completes in one execution — the flag cannot exercise
it; crash-replay coverage for those is the endpoint-loss drill.

- **Mid-handler kill** — `kill -9` between journaled steps; restart →
  journaled steps skip (no double effect), unjournaled sends re-fire
  once (target absorbs).
- **Double delivery** — redeliver the same send; second landing no-ops.
- **Orphaned arm** — force-replace the deployment mid-arm; assert the
  domain floor re-arms rather than the cadence dying silently.
- **Poisoned leg** — deterministic throw → invocation suspends; assert
  the wedge is visible and fixable (pause/resume/kill path named).
- **Stale-attempt zombie** — sever the journal mid-`ctx.run`; the old
  attempt runs to completion — assert `attemptCompletedSignal` aborts
  its side effects or its effects are idempotent.
- **Deployment migration** — `pause` → `resume --deployment` a wedged
  invocation; assert replay survives the code change (or the in-place
  fix stayed inside `ctx.run`).

## Report format

Per surface: the send graph (emitter → target/key/delay/detached), the
key→row map, then findings:

```
### <finding title>
invariant: <what should hold>
evidence:  file:line + the call path that reaches it
sequence:  <the concrete interleaving that breaks it>
impact:    <blast radius> — severity: BLOCKER | HIGH | MED
owner:     <who detects/resolves today, or the gap>
fix:       <correction> + <test that fails before it>
status:    confirmed | plausible | lead
```

Footer mandatory: handlers/services reviewed, journal-vs-row coverage
limits, assumptions, tests actually run — never claim replay-safety
without the kill drill (`nektar/fake-evidence` applies here too).

## Sibling skills — load, don't re-derive

| Ask                                              | Skill                                           |
| ------------------------------------------------ | ----------------------------------------------- |
| Transition table, unknown-outcome, late evidence | state-audit — this skill's checks plug in there |
| Wait-site/push-vs-poll/event-plane design        | event-arch                                      |
| What counts as believed (proof truth)            | spv-strict                                      |
| Money-path/custody invariants                    | code-review nektar rules                        |
| Whole-system runtime topology                    | arch-review                                     |

## Sources

docs.restate.dev — key-concepts, services, handlers, invocations,
durable-steps, service-communication, durable-timers, error-handling,
external-events, serving, versioning, managing-invocations, databases
guide, cron guide, configuration, request-lifecycle, testing, flow-
control, sagas, durable-agents, human-in-the-loop; github.com/restatedev
(xstate, examples, ai-examples). Pinned semantics: detached sends survive
kill/cancel; inactivity timeout 1min / abort 10min; immutable
deployments; journal-mismatch RT0016; orphaned invocations RT0011; ctx
ops illegal inside ctx.run; same-key calls deadlock; default retry ~70
attempts→pause (`unlimited` opts out); K/V state per scope; limitKey
re-orders across keys.

Detailed procedures: [references/audit.md](references/audit.md) ·
SDK semantics: [references/sdk.md](references/sdk.md) ·
Setup/integration/no-polling: [references/integration.md](references/integration.md)
