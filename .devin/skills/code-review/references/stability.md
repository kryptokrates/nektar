# stability — production failure semantics

Load for diffs touching outbound I/O (fetch/RPC/hosts/WoC), timers and poll
loops, process lifecycle (locks, respawn, teardown), retries, or capacity
limits. Distilled from Nygard's *Release It!* (adapted under MIT from
ciembor/agent-rules-books). The bias being corrected: **a passing happy path
says nothing about production.** Every box and arrow on the diagram will
eventually fail — slow, partial, or prolonged. Design the failure, don't
inherit it.

## The catalog

| Rule ID | Severity | Rule |
|---|---|---|
| `stab/no-timeout` | MAJOR | An outbound call or wait with no explicit bound — library defaults are not limits. Our convention: `AbortSignal.timeout` on every fetch (4s probes, 10s asset reads); new call sites need the same. |
| `stab/unbounded-retry` | BLOCKER | Retry without a bound on count AND total time, retry of a validation/permanent error, or retry layers stacked so effective attempts multiply. Outbox attempts→dead-letter is the reference shape. |
| `stab/hung-resource` | BLOCKER | A resource held across a remote call or unbounded wait (db txn, lock file, reservation, slot) — one slow dependency pins the scarce resource for everyone. Release-then-call or bound the hold. |
| `stab/shared-fate` | MAJOR | One slow/failing dependency can consume all workers/connections/slots — no bulkhead. Host calls are the example: one hung host must not stall the whole outbox flush (per-destination rows + bounded batches are the existing pattern). |
| `stab/unbounded-queue` | MAJOR | A buffer, backlog, inbox, watch set, or collection that grows without limit and without a defined full-behavior. `market_inbox`, `watched` maps, and outbox rows all have TTLs/size bounds — new accumulators need the same. |
| `stab/load-shed-missing` | MINOR | Under overload, low-value work isn't shed before core paths (funding, escrow) starve — no priority, no demand limit, no graceful refusal. `NEKTAR_MAX_SPEND_*` and slot limits are the existing governors. |
| `stab/fail-slow` | MAJOR | A gate that continues after detecting an unrecoverable condition, holding scarce resources while doing it — fail fast at the boundary (`openRuntime` funding checks, preflight gates, `instance.lock`). |
| `stab/invisible-failure` | MAJOR | A failure mode with no observable surface — swallowed catch, retried-forever state, silently-deferred work. Every failure class needs a signal: log line, `last_error` column, metric, or state transition. |
| `stab/storm-on-recover` | MAJOR | Recovery/restart path that can slam a dependency — N agents or N retries firing simultaneously, thundering-herd on hosts or RPC. Stagger, jitter, or rate-limit the recovery burst. |
| `stab/degraded-unknown` | MAJOR | Code has no defined behavior when a dependency is degraded (host 500s, empty lookups, half-failed quorum). "Down", "slow", "empty", and "lying" are four different faults — the faulthost proxy exists precisely to test them; use it for any new multi-host path. |
| `stab/teardown-leak` | MAJOR | Timers, sockets, watchers, or keep-alive fetches that pin the event loop past shutdown (`unref` where appropriate, explicit stop on teardown — the testkit watcher incident). |
| `stab/latch-unrecoverable` | MAJOR | A one-way latch (dead-letter, abandoned watch, `uncertain` state) with no reviewable or recoverable path — operator intervention must be possible and documented. |
| `stab/optimistic-absence` | MINOR | Treating "dependency responded 200" as "dependency did the thing" — our `/submit` 200+`outputsToAdmit:[]` refusal is the canonical counterexample. Validate the response *content*, not the status. |

## Failure drills (run per changed path)

- **Slow**: what if this call takes 10× the timeout — who waits, what pins?
- **Partial**: what if 2 of 3 hosts accept — which reads see the write?
- **Corrupt**: what if the response parses but is wrong (a flipped byte, a
  stale row, a truncated BEEF) — who verifies *content*, not just presence?
- **Restart**: what replays on next boot — from whose perspective?
- **Overload**: what sheds first when the backlog triples?

The `faulthost` proxy (`packages/bstack/src/testing.ts`) exists for exactly
this — a new host-facing path without a down/slow/withhold/corrupt test is
an unproven claim, not a finished change.

## Evidence conventions for this repo

- A fix to a failure mode should name which drill it answers and how it was
  exercised (fault proxy, killed process, stale row injection).
- "Retried" without attempts/dead-letter evidence is incomplete.
- Timer/poll additions must state their bound and their unref/stop story.
