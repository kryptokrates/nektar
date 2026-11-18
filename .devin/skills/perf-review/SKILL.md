---
description: >
  Use when asked to make something faster, find why something is slow or
  wedged, cut test/build time, or review a perf-sensitive diff (loops over
  UTXOs/rows/hosts, awaits in series, polls, caches, batch sizes). Also for
  "verify fast": picking the minimal check/test gate set for a change.
  Measure-first, always — a timing claim without a number is a guess.
  For money-path invariants use code-review; for wait-site/event design use
  event-arch; for proof-path truth use spv-strict.
license: Apache-2.0
---

# Perf Review — nektar

## The protocol — measure, change, re-measure

1. **Instrument before theorizing.** The pattern already in the tree:
   `const t0 = Date.now() … if (ms > 2_000) log(\`pass ${ms}ms (poll ${t1-t0} …)\`)`
   — name the segment that ate the time. A slow-pass report that doesn't
   decompose poll / tick / pump / upkeep is useless.
2. **Baseline.** Numbers on _this_ machine: `pnpm bench` writes
   `BENCHMARKS.md` (op costs on the regtest stack). For a code path, count
   iterations × per-iteration cost — e.g. "400 lease rows × 1 join each"
   beats "the sweep is slow".
3. **One change.** The smallest edit that removes the named mechanism —
   not a rewrite while you're in there.
4. **Re-measure.** Report before/after numbers and what was _not_ changed.
   If the number didn't move, the mechanism was wrong — revert and re-name.

## Lean rules — the invariants speed work must never trade

- **Custody and durability are not levers.** SpendGuard sees every spend;
  journals/outboxes persist before broadcast; leases stay until the tx
  state makes them stale (see `leaseLive` — take() and prepare() must
  agree on what a live lease is).
- **SPV truth is not a lever.** A 'mined' verdict still re-derives through
  the kernel (`computeRoot` → `isValidRootForHeight` on our headers).
  Removing a verification to go faster is `spv/hint-as-verified`, not a
  perf win.
- **Polls stay ceilings, never mechanisms.** Wakes/events shorten latency;
  the poll only bounds the worst case (`hotUntil` cadence). Adding a fixed
  sleep is `nektar/fixed-sleep`; removing a needed ceiling is a lost
  wakeup waiting to happen.
- **Bounds and reapers stay.** Body budgets, in-flight caps, session/invoice
  reapers, `hintedTxids`, SSE subscriber cap — a speed fix that removes a
  bound is a resource-exhaustion finding, not a fix.
- **Memoization keys cover every input.** The pack/verify precedent: key on
  `sha256(bytes)+anchorTxid`, and re-apply every time-dependent gate
  (expiry) on a hit.
- **Batching belongs at I/O boundaries.** Host polls, journal puts, lane
  reads — bounded `Promise.all` fanout. Never batch inside a policy or
  custody decision.

## The 1BSV doctrine — where the speed actually is

Grounded in 1bsv.net papers 006/009/011/015 and Wright's SPV essays
(singulargrit.substack.com). The fast path and the correct path are the
same path here:

- **Evidence travels with the transaction; verification is local to the
  relationship.** The fastest fetch is the one never made: attach the
  BUMP/`fundingTxid` at _write_ (`journal.put`), heal ancestry at _read_
  (`journal.enrich` pattern). A counterparty that must re-fetch what you
  already held is a latency bug, not a protocol requirement.
- **Publish once; numbered lanes repaired by the network.** Journal/lane
  fanout beats per-consumer round trips; delivery is idempotent — a resend
  can never pose as a new frame, so retries are free.
- **Unspentness is a hint, not a proof.** Spend-status answers
  (`spendCheck`/`liveSet`/`deadParent`) are bookkeeping oracles — they
  unblock bookkeeping, they never write chain claims. Cheap to consult,
  safe to be wrong (self-corrects at spendCheck/broadcast).
- **Index answers are discovery, not truth** — same shape: they may move
  _bookkeeping_ fast, never verdicts.

## Where the time goes

[references/hot-paths.md](references/hot-paths.md) — the measured hot
paths: pass phases vs the 15s collect window, detached upkeep, lane
fanout, lease/join watchpoints, ZK/STARK op costs, HTTP memoization,
heal-at-read, 4GB-box constraints.

## Verify fast

[references/gates.md](references/gates.md) — change → minimal gate/test
routing. The default "run everything" is how sessions burn minutes; the
table says which `check:*` actually owns each change type.

## Output contract

Name the mechanism. Give before/after numbers (or the instrumented log
line that produces them). State which invariants were held. If a "fix"
weakens a check, say so plainly — that's a security finding filed by a
perf pass, and it wins over the speed.
