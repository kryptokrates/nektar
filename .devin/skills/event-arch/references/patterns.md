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

## Quick map: goal → patterns

| Goal (user's words)                 | Patterns that deliver it                                                                      |
| ----------------------------------- | --------------------------------------------------------------------------------------------- |
| as realtime as possible             | 1, 2, 3 — everything waits on signals; polls are floors; `hotUntil` cadence                   |
| max interoperability + seamlessness | 8, 6 + `v:`/`kind:` envelopes + conformance vectors — boring wire, digest refs, vector-proven |
| no timeout                          | 3, 7 — no timer as mechanism; ceilings stay only as leak guards                               |
| as resilient as possible            | 4, 5, 6, 7, 9, 10 — verify-not-trust, fanout, degrade-to-poll, dedup, single substrate        |
