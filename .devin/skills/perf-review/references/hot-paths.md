# Hot paths — where nektar's time actually goes

Measured/observed paths, each with the seam to instrument and the known
foot-gun. When a path isn't here, instrument before adding it.

## Automation pass phases (`packages/automation/src/runtime.ts`)

The cycle: `rt.poll()` → `engine.tick()` → `pushAcks()` → `pumpDone`.
Phase timing logs fire when a pass exceeds 2 s (`pass ${ms}ms (poll …
tick … pump …)`). **The deal killer is a pass outrunning the 15 s
`COLLECT_MS` want window** — a want whose collect phase expires before
the offer lands produces `funding_failed`, invisible in any average.
Watch p95 pass time, not mean.

- `hotUntil` keeps the loop hot after live traffic; a settled proof
  waiting only on block depth is excluded from heat by design.
- Serial `await` inside a pass over hosts/rows is the usual regression —
  fan out at the I/O boundary.

## Upkeep (`packages/automation/src/upkeep.ts`)

Serial phases: `laneSpec` → `ensureTrees` → finger publish →
`storeEvidence` → `appendLogEvents`. Runs **detached** on a ~10 s ceiling
(`upkeepRun` dedup), never inline on the deal path. `rt.stopped` bail-outs
between phases keep a late pass from outliving a destroyed pool. Logged
when >2 s (`automation: upkeep ${ms}ms`).

## Lane polls (`packages/automation/src/lanes.ts`)

Every lane read is independent → bounded `Promise.all` per sub, never
serial. Quorum-1 host answer drained at the top of the _next_ pass via
`pendingLane` — a slow host must never gate this pass. Subscription-set
transitions logged once via `subSig`; item counts logged per sub when
non-empty.

## Funds, leases, sweeps (`packages/automation/src/store.ts`, `wallettfunds.ts`)

- **`leaseLive` joins `txs` per lease row** — watchpoint: N+1 inside
  `take()`/`prepare()` loops over many candidates. If a sweep iterates
  hundreds of outpoints, batch the tx-state fetch or pre-load a map.
- Oracles: `spendCheck` (pick-time veto), `liveSet`/`liveCheck` (starve-
  sweep revive), `deadParent`/`tombstonedList` (phantom healing). All
  bookkeeping hints — cheap, never chain claims.
- Sweep-once marking beats per-pick probing; an unanswered oracle revives
  nothing and costs nothing.

## Proof paths (measured by `packages/e2e/src/bench.ts`)

`pnpm bench` → `BENCHMARKS.md`: `verifyFragment`, `authorize`, Datalog,
`proveAccess`/`verifyAccess` (ZK EC math — the expensive crypto),
automaton STARK prove ~35–50 s (priced into deal terms, not a bug).

## HTTP (`packages/mcp/src/http.ts`)

- `/v1/pack`: memoized by shard+content, ETag, in-flight cap — repeat
  fetches are free; cold misses are one export.
- `/v1/verify`: `reverifyMembershipBytes` memoized on
  `sha256(bytes)+anchorTxid` (deterministic, no state consumed);
  fragment citation cap 64.
- Body budget: `Content-Length` pre-check + aggregate bound — pre-auth
  work is the exhaustion surface.
- Reapers: session expiry, invoice sweep (4096), SSE subscriber cap (429).

## Heal-at-read (`bstack/src/box.ts`, `finger.ts`, `automation/src/runtime.ts`)

`journal.put` rows carry `fundingTxid`; `journal.enrich` attaches the
funding parent's BUMP the moment `fetchTx` can answer it. Rows journaled
while the tree was unproven **heal at read** instead of re-serving stale
ancestry — the "move proofs seamlessly" pattern: write once with what you
hold, serve complete forever after.

## The 4GB deploy box

- `tsc` runs serial in deploy (`--workspace-concurrency=1`; two trees peak
  ~1.6 GB under k3s+mysql — parallel OOMs).
- Kafka `txmeta` ~60 s retention: coinbases mined during persister init
  never land → `TX_MISSING_PARENT` (AGENTS.md gotcha).
- `rpc_max_clients` pinned to 128 — default 3 saturates under keep-alive
  callers.
- Windows/Node 24 long evals die with `0xC0000409` libuv teardown:
  split scans into checkpoint phases, `connection: close` on sockets.
- Docker Desktop port proxies go stale after WSL restart — `docker restart`
  re-registers before assuming a service is wedged.
