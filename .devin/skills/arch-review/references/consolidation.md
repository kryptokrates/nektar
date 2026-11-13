# consolidation — the leaner-codebase lens

Load after the censuses (measurement #9–#12) have produced their lists.
This file turns "we found duplication / stratigraphy / sprawl / SPoFs"
into root causes, remediation moves, and deletion candidates. The bias
being corrected: **a review that only names symptoms produces patch N+1 —
the deliverable is the missing abstraction and the LOC that goes away.**

## Root-cause drill (run per stratified site)

For each top entry from measurement #10 (fix-density) — and each convergent
cluster from #9 that keeps breaking — ask, in order:

1. **What invariant does this code maintain?** If the answer takes three
   sentences, the file owns more than one concept — split by invariant,
   not by file length.
2. **Which of the last N fixes were the same failure in a new costume?**
   List them. Repeated symptom = one missing abstraction, not N bugs.
3. **What would make the bug class unrepresentable?** A type that can't
   hold the invalid state, a constructor that can't be called wrong, a
   transition table instead of scattered `if state ==` checks — name the
   concrete shape.
4. **Why did the fix land here instead of the seam?** "The seam doesn't
   exist" = the finding. "The seam exists but callers bypass it" = an
   enforcement finding (unenforced invariant, S2).
5. **Who else implements this?** If #9 counted the concept in 3+ places,
   the fix was likely ported incompletely — the dupe is the root cause.

The output per site is one line: `<site> keeps breaking because <missing
abstraction>; the move is <extract / merge / reseam>.` If you can't write
that line, the site goes in the report as a hypothesis, not a finding.

## Remediation patterns (name the move, not the wish)

| Pattern                    | When the finding is…                                                            | What it means here                                                                                                                          |
| -------------------------- | ------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| Extract deep module        | Convergent implementations with the same contract                               | One implementation behind a small interface in `core`/`store`/the owning layer; call sites thin. The Ops-extraction pattern (BondOps/CustodyOps) is the in-repo precedent. |
| Strangler fig              | A live duplicate can't be deleted in one change                                 | New seam routes new traffic; old path drains; deletion is the last commit, not the first. Money paths always migrate this way.             |
| Reseam (composition root)  | Callers construct what only wiring should own                                   | Construction moves to the composition root; everything else takes the seam as a parameter. The SpendGuard/broadcaster construction gate is the template — generalize it to other singletons. |
| Port/adapter               | A package can't be swapped for a stub/alternate impl                            | The interface moves to the consumer side; the concrete impl is an adapter at the edge. Test: can e2e/fuzz substitute without editing the package? |
| Make invalid unrepresentable | Patch stratigraphy — checks re-check the same precondition at N layers        | Encode the valid shape in the type/constructor once; delete the guard layers.                                                               |
| Centralize the wait        | Timer sprawl — same wait, N hardcoded values                                    | One helper owns timeout+jitter+backoff; call sites declare intent (`probe`, `assetRead`, `fundPoll`), not durations.                       |
| Event over poll            | Fixed sleeps waiting on a condition that already emits                          | Replace the poll with the wake + a bounded ceiling; `nektar/fixed-sleep` is the diff-level form of this.                                      |
| Producer-signal            | A wait has no wake path — the producer never announces the awaited condition    | Add the emission at the producer on EVERY path (success and error); the waiter keeps a bounded timeout ceiling. The `unblockOutbox` + watch-shim "wake is a hint, never the load path" pattern is the in-repo template. |
| Single scheduler           | N tick/loop substrates per subsystem (per-wait nudge loops, parallel intervals) | One substrate per subsystem owns polling; waits register interest (`watched`) and the tick fans out. ChainEvents' `waiters` + nudge is the existing model — don't add a second scheduler next to it. |
| Deduplicate retry policy   | N hand-rolled backoff loops (different bases, caps, jitter conventions)         | One `withRetry(fn, {baseMs, max, jitter})` + one rate-limiter; call sites declare intent (`broadcast`, `wocGet`, `probe`). Retry-on-failure is legitimate — the smell is six dialects of it. |
| Bounded drain              | A teardown/cleanup spin-wait on busy flags                                      | Bound the drain + signal completion; an unbounded `while(busy) sleep(25)` turns an orderly stop into a hang.                                   |
| Redundancy plan            | SPoF on a money/durability path                                                 | Pick the tier: active-active (bstack multi-host fanout — the existing model), hot standby, or rebuild-from-source + documented RPO. "None" is only acceptable with a stated reason. |

## Deletion taxonomy (every row carries an LOC estimate)

Rank deletion/consolidation candidates by _LOC removed ÷ risk_:

1. **knip-dead** — `check:knip` output: files/exports nothing references.
   Cheapest deletions; just gate on knip staying green.
2. **Cold, unimported** — `report:cold` ∩ zero importers. Verify with the
   graph, not vibes; a file that cold AND referenced is legacy to review,
   not delete.
3. **Speculative layers** — interfaces/factories/wrappers with exactly one
   implementation and no second caller in history (`struct/speculative-layer`
   at arch altitude). Inline the impl, delete the layer.
4. **Wrapper farms** — call chains where each layer passes arguments
   untouched (`struct/pass-through` at scale). Collapse the middle.
5. **Convergent tails** — after a consolidation lands, the abandoned twin:
   schedule the deletion in the same plan or it becomes tomorrow's cold
   file.
6. **Generated-able sources** — code a tool emits that also lives in source
   form. Keep the generator, delete the checked-in copy (or vice versa —
   pick one truth).

## Resilience mapping (redundancy without bloat)

Redundancy ≠ duplication. Duplication is two copies of the same knowledge;
redundancy is two independent ways to get the same _outcome_. Merging the
former shrinks LoC; adding the latter shrinks blast radius. The report must
say which it is proposing and never propose "redundant" copies of logic as
resilience — that trades one failure mode for two maintenance surfaces.

Map each money/durability path to its layer coverage:

- **Per-call**: every outbound wait bounded (timeouts exist — the census
  counts them; the question is whether money-path calls are all covered).
- **Component**: one slow host can't starve the flush (per-destination
  outbox rows + bounded batches — the existing bulkhead; extend the
  pattern where it's missing rather than inventing a second one).
- **System**: SpendGuard/`NEKTAR_MAX_SPEND_*` are the governor; low-value
  work must shed before funding/escrow starves.
- **Ops/steady-state**: every accumulator (outbox rows, inbox, watch sets,
  journals) has a paired recycler or bound.
- **Recovery**: for each SPoF from #12, the named tier: fanout / standby /
  rebuild — plus the drill that proves it (faulthost for hosts, killed
  process + replay for stores).

## ADR stub (inline, one per major remediation)

Large structural moves get a decision stub in the report's roadmap —
Nygard format, five lines, no new files:

```
ADR-<n>: <decision name>   status: proposed
Context: <the measured evidence that forces this>
Decision: <the move, one sentence>
Consequences: <what gets easier; what now requires coordination; LOC delta>
Rejected: <the alternatives considered and why they lose>
```

## Report glue

The ledger row format (for SKILL.md's Simplification ledger section):

`| Opportunity | Evidence (census #, counts) | Est. LOC | Move (pattern above) |`

Order by LOC removed ÷ risk. A ledger full of LOW-risk deletions beats a
ledger with one heroic rewrite — nektar's ratchets (`check:arch`,
leftovers) reward steady shrinkage, and every landed deletion makes the
next review cheaper.
