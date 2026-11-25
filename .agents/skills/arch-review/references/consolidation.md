# consolidation — the leaner-codebase lens

Load after Phase 1–2 censuses have produced their lists. Turns smells and
runtime failures into root causes, concrete remediation moves, deletion
candidates, and Nygard ADRs. **The deliverable is the missing abstraction and
the dead/redundant paths that go away — not patch N+1.**

## Root-cause drill (per stratified or duplicated site)

1. **What invariant does this code maintain?** If it takes 3 sentences, split by invariant.
2. **Which recent fixes were the same failure in a new costume?** Name the missing abstraction.
3. **What makes the bug class unrepresentable?** Atomic Reserve/Commit, strict `ParseGroundFact`,
   single live engine (Restate-only), single live payment rail (BRC-29-only), kernel `Flock`/`LockFileEx`.
4. **Why did fixes land at callers instead of the seam?** Missing seam vs unenforced invariant (`S2`).
5. **Who else implements this?** Consolidate convergent twins and delete the legacy path.

Output per site: `<site> keeps breaking because <missing abstraction>; the move is <extract / merge / reseam / delete>.`

## Remediation patterns

| Pattern | When the finding is… | Concrete Go v0.4 move |
| --- | --- | --- |
| Extract deep module | Convergent implementations with same contract | Single helper/method in `core`/`chain`/`store`; thin callers |
| Delete legacy twin | Dual runtime engines or dual payment rails | Enforce Restate-only & BRC-29-only on live networks |
| Atomic Reserve/Commit | Spend/payment TOCTOU or post-broadcast rollback | `SpendGuard.Reserve`/`CommitReservation`, pre-broadcast persist |
| Fail-closed boundary | Swallowed diagnostics/errors or corrupt state fallback | Latch `corruptErr`, fail on Cedar `diag.Errors`, fail-closed checksums |
| Kernel lock & awaited drain | TOCTOU PID locks, unawaited `Shutdown`, orphan sidecars | `Flock`/`LockFileEx`, `CloseSSE()` + `<-shutdownDone` + `wg.Wait()` |
| Fitness gate (`S2`) | Unenforced architectural invariant | Add Go unit test (`TestMetricsAlertsParity`) or `ci.yml` check |
| Immutable digest + rollback | Unpinned continuous deployment | Build digest artifact in `release.yml`, pin + auto-rollback in `deploy.yml` |

## ADR stub format (inline Nygard, one per Critical/Important move)

```
ADR-<n>: <decision name>   status: proposed | accepted
Context: <measured evidence forcing this decision>
Decision: <the structural move in one sentence>
Consequences: <what gets simpler/safer; coordination costs; LOC delta>
Rejected: <alternatives considered and why they lose>
```
