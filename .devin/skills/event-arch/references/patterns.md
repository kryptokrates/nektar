# patterns — the fix catalog

Every finding points at one of these moves. Each entry: the smell, the
move, the in-repo precedent to copy. Prefer extending an existing seam
over inventing a second mechanism — a parallel event substrate is
`ev/scheduler-sprawl`, which is worse than the poll it replaced.

## 1. `ev/poll-for-event` — fixed poll waiting on something that emits

**Smell:** `setInterval`/`setTimeout` loop re-reading state that has a
producer (store row flip, registry arrival, host recovery, tx status).

**Move:** subscribe to the producer; keep the poll only as the bounded
floor. Three-tier shape everywhere: push (latency) → poll (correctness
floor) → ceiling (leak guard).

**Precedent:** watch-shim subscriptions wake `deals()` on `changed:true`
(runtime.ts:978); the 15s `refreshWatches` tick stays as re-arm floor;
`hotUntil` keeps cadence hot for 5s after inbound activity.

## 2. `ev/lost-wakeup` — a wait whose producer doesn't signal all paths

**Smell:** consumer waits on condition X; the producer signals on the
success path but not on failure/defer/skip — consumer sleeps till the
next tick, or forever.

**Move:** signal on every exit path, or unblock the waiters directly
when the freeing condition flips. The _freeing event_ is the signal:
host recovery frees outbox rows → watchdog calls `unblockOutbox` per
deferral pattern (runtime.ts:942); tx proof frees funding slots →
`unblockOutbox('funding slot')` (runtime.ts:1041); record resolution
fires the deferred-ack pass (runtime.ts:205).

**Precedent:** `store.unblockOutbox(pattern)` — the generic "the thing
you were waiting on arrived" wake. New deferral classes get a pattern
and a wake at every site that can free them.

## 3. `ev/timeout-as-mechanism` — a timer doing notification's job

**Smell:** "wait N ms then assume state changed"; `await sleep(k)` in a
pipeline; a deadline that _causes_ progress rather than bounding it.

**Move:** find the event the sleep is proxying (a status push, a row
flip, a proof arrival) and subscribe to it; keep the sleep only as the
ceiling. If truly no producer exists, the fix is a producer — not a
shorter sleep.

**Precedent:** `ChainEvents.waitFor` has a deadline (leak guard) but the
resolution path is `observe`-verified status, not elapsed time; the
`probeNow` nudge re-checks on signal, not on the clock.

## 4. `ev/unverified-status` — hint driving a money decision (BLOCKER)

**Smell:** a pushed `TxStatusEvent`/host response flips a spend, reveal,
release, or settle flag without `observe`/proof verification.

**Move:** route the status through `note()` (records for dedup/waiters,
never emits 'tx'); emit truth only from `observe()` or a proof verifier.
Statuses order _work_, never _truth_.

**Precedent:** the `note()`/`observe()` split in `chain/src/events.ts` —
it is the repo's SPV trust model; extending it beats a second channel.

## 5. `ev/absence-as-proof` — host silence read as nonexistence

**Smell:** "query returned empty → the artifact doesn't exist → conclude
(settle/refund/penalize)". A withholding host is indistinguishable from
a missing artifact.

**Move:** absence never concludes; only a verified positive does. Empty
results extend interest (watch stays armed) or trigger fanout re-query —
never close a state machine.

**Precedent:** withheld-offers fault tests (Faulthost `withhold` mode);
DDIA `data/read-your-writes` is the same shape.

## 6. `ev/untracked-fanout` — write to N destinations, no per-dest state

**Smell:** `Promise.all(hosts.map(publish))` with only a merged verdict;
no record of _which_ host got it — can't repair 2-of-3, can't answer
"did everyone see it".

**Move:** per-destination rows (outbox per host or per-host status on
the artifact), plus a repair sweep; `missed[]` in the publish result is
already the contract — persist it, don't just throw.

**Precedent:** `publishArtifact` returns `{missed}` and callers throw —
the suggested upgrade is persisting `missed` into outbox deferral
(`deliver-fault` pattern) so the watchdog's `unblockOutbox` can replay.

## 7. `ev/no-fallback` — subscription that dies on disconnect

**Smell:** a watch/SSE/long-poll that on error just exits; no cursor
resume, no re-arm tick, no poll floor → transient disconnect = permanent
blindness.

**Move:** cursor-resumable reconnect (`?cursor=` continuation — the shim
contract), a slow re-arm tick (`refreshWatches` 15s), and the bounded
poll floor underneath all of it.

**Precedent:** `watchOne`'s loop: `!r.ok` → backoff 5s, keep `cursor`,
continue — failure degrades to ordinary poll, never to stop.

## 8. `ev/location-coupled` — references by location, not digest

**Smell:** a receipt/artifact reference naming a host/URL as the
identity (`host/x/path`), or an artifact fetchable from only the
endpoint that produced it.

**Move:** identity = digest (`chirp/<commit>`); location is a _hint_
alongside (`{endpoint, bucket, digest}`). Resolution tries any copy;
verification is on the bytes. On-chain only ever carries the commitment
(hash + url + expiry per the deck's service layer).

**Precedent:** pack sharing — `GET /v1/pack/{shard}` on the well-known
record means any gateway serves any shard by content name.

## 9. `ev/no-dedup` — applied effect non-idempotent under re-delivery

**Smell:** handler applies an effect on receipt (credit, consume, flag
flip) keyed by arrival — at-least-once delivery replays it.

**Move:** dedup at the _apply_ edge by op-ID — the end-to-end rule:
keyed requests beat per-hop retries. Mark-applied inside the same store
transaction as the effect.

**Precedent:** `take()` journaling / consume-once semantics; market op
journals (`market_outbox`, `market_inbox` op-IDs).

## 10. `ev/scheduler-sprawl` — a second event substrate beside the first

**Smell:** a new per-entity timer/`EventEmitter`/poll loop while
`ChainEvents` (interest-set + single tip-gated poller) already exists
for the same truth source.

**Move:** extend the shared substrate — new interest is a `watch()` call
or a `note()` source, not a new loop. Rotation (`maxPerTick`) and TTL
already solve the oversized-set problem; replicating them is debt.

**Precedent:** `ChainEvents` itself — ONE poller per process, shared
interest set, tip-gating, TTL sweep, abandon-after-blocks. The
`abandon`/`report` events exist precisely so callers don't need their
own trackers.

## 11. `ev/abandon-holds-resource` — bound resolves the wait, work keeps its holds

**Smell:** `Promise.race`/`setTimeout`-bound awaits around work that
holds a serialized resource — a promise-chain mutex (`this.chain =
p.then(fn)`), a queue slot, a lease, a connection. The bound resolves
the _waiter_; JS can't cancel — the abandoned promise keeps running and
keeps its holds. Every later acquirer starves behind a dead link while
the surrounding loop looks healthy.

**Move:** since the work can't be cancelled, it must be _detected_ —
an acquire-bound on the serializer (a `run()` that hasn't acquired
within N turns throws/logs instead of queueing), or a held-too-long
detector that names the holder when a bound fires. A silent orphan is
the finding; a wedged mutex nobody notices is a hang wearing a bound's
clothes.

**Precedent:** `bound()` at `runtime.ts:411` — named-leg logging on
bound-fire is the right detection shape; the missing half is "who held
the chain when it fired". `base.ts:52`'s `this.chain` is the serializing
resource.

## 12. `ev/unmeasured-wake` — static wake contract, no runtime accounting

**Smell:** wake sources are declared (a registry, a map, a coverage
test) but nothing records at runtime which source actually fired each
pass. A producer signal that stops firing degrades the system to
ceiling speed — correct, invisible, indistinguishable from healthy.

**Move:** tag each pass with its firing source (`wakeSource` arg through
the wake seams) and keep a cheap counter or log line — the ratio of
event-driven to ceiling-driven passes is the event plane's SLO.

**Precedent:** `wake-coverage.test.ts` is the static half — the runtime
half is one tag threaded through `workReady`/`dueIn`/`cycle` callers.

## 13. `ev/phase-aligned-cadence` — fixed retry interval, no jitter

**Smell:** `setInterval`/backoff/`next_at` cadences set to fixed literals
(reconcile 30s, resubmit 60s, upkeep interval) — N agents restarted
together fire on the same ticks and stampede the same hosts.

**Move:** multiply the interval by `(0.5 + Math.random())` or spread
`next_at` with a random delta — jitter the cadence, don't lengthen it.

**Precedent:** `watches.ts:28` `Math.min(backoff, 30_000) * (0.5 +
Math.random())` — today the repo's only jittered site.

## 14. `ev/stacked-deadline` — nested bounds each take a fresh budget

**Smell:** a `bound(`/`deliverBounded`/timeout wrapper whose inner work
calls a fresh bound — each layer grants a new budget, so one operation
burns the sum (a `deliver` containing submit+lookup+fetch costs 3× the
written bound).

**Move:** one deadline per operation, computed once, threaded inward —
inner legs bound against `deadline - now()`, never a fresh constant.

**Precedent:** `deliverBounded` (outbox.ts) is the one-layer exemplar —
the fix is a `deadline` arg flowing into the awaits inside it.

## 15. `ev/unarmed-deadline` — computable deadline scanned, not armed

**Smell:** `now() - <stamp> > <bound>` evaluated per pass when the bound
was knowable at commit — `updated_at + maxAgeMs` for stuck-state exits
is the live example: the deadline exists at transition time but the
watchdog re-derives it every tick.

**Move:** store the deadline at the write (`due_at = stamp + bound`) and
arm it through the due-wake seam — the poll stays as floor, but the
watch fires at the deadline, not at cadence.

**Precedent:** the `dueAt`/`dueAtArmed`/`dueIn` plumbing added for deal
deadlines — the same seam arms `STATE_LIMITS` exits.

## 16. `ev/wait-class-blind` — census covers one wait primitive, others unmapped

**Smell:** a coverage gate/registry enumerates `setInterval` jobs while
`setTimeout`/`Promise.race`/`AbortSignal.timeout`/`sleep` waits exist
with no contract — the registry is blind to a whole class, and a new
one-shot bound lands undocumented.

**Move:** extend the census primitive list; every wait gets a row or a
declared classification — a wait the census can't see is a wait without
a contract.

**Precedent:** `wake-coverage.test.ts` greps `setInterval` — extending
its primitive list is the whole fix; the test shape is already proven.

## Quick map: goal → patterns

| Goal (user's words)                 | Patterns that deliver it                                                                         |
| ----------------------------------- | ------------------------------------------------------------------------------------------------ |
| as realtime as possible             | 1, 2, 3 — everything waits on signals; polls are floors; `hotUntil` cadence                      |
| max interoperability + seamlessness | 8, 6 + `v:`/`kind:` envelopes + conformance vectors — boring wire, digest refs, vector-proven    |
| no timeout                          | 3, 7 — no timer as mechanism; ceilings stay only as leak guards                                  |
| as resilient as possible            | 4, 5, 6, 7, 9, 10 — verify-not-trust, fanout, degrade-to-poll, dedup, single substrate           |
| delivery-plane resilience           | `fab/*` in [fabric.md](fabric.md) — numbered streams, repair-by-name, narrow joins, deliver-once |
| wait machinery can't wedge/starve   | 11, 14, 15 — orphan detection, shared budgets, armed deadlines                                   |
| retries that don't stampede         | 13 — jitter on every fixed cadence                                                               |
| the event plane is provable         | 12, 16 — runtime wake accounting + a census that sees every wait class                           |

Delivery/transport fixes (lanes, finger, tree, journal, watch-shim, host
fanout) use the `fab/*` catalog in [fabric.md](fabric.md) — same smell →
move → precedent shape, with the paper→rule map.
