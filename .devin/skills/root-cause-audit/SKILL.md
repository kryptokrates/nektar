---
name: root-cause-audit
description: >
  Use to audit the codebase for layers of redundant code — patches,
  guards, retries, normalization, fallbacks, compat shims and defensive
  layers that exist because an underlying problem was never fixed — and
  to spec the root-level fix that lets the layers come down. Trigger
  surfaces: "why does every caller normalize this", "the same edge case
  is handled in five places", "is this retry/guard/shim still needed",
  "patches on patches", workaround/compat/legacy/temporary comments,
  the same bug fixed repeatedly in new costumes, N dialects of
  retry/backoff/fallback, "audit for unnecessary code". The deliverable
  is a cluster→cause→fix report: each finding names the affected files,
  why the code exists, the root-level change that replaces it, the
  behavior that must be preserved, and how to test — unproven causes are
  labeled hypotheses, never findings. Audit-only: suggests fixes,
  applies nothing unless asked. For whole-system topology use
  arch-review (this skill is its consolidation drill run standalone);
  for diff review use code-review; for the design rules a fix must
  satisfy use design-guardrails; wait-site workarounds route through
  event-arch; lifecycle/state workarounds through state-audit.
---

# root-cause-audit — find the patch, name the cause, spec the fix

Codebases scar. A defect lands at a boundary nobody owns, so the fix
lands one layer down; the next symptom gets a guard, the third gets a
retry, and three releases later five places each compensate for one
untreated cause. Syntactic duplication gates (`check:dupes`) cannot see
this — the copies aren't textually identical, they're five dialects of
the same accommodation. This audit hunts the accommodation, proves the
common cause, and hands back the fix that deletes the layers.

The unit of work is the **cluster**: a set of sites that compensate for
one suspected cause. A cluster with no proven cause is a hypothesis. A
site whose guard turns out load-bearing is not a finding — it's
insurance, and the report says so.

## Workaround taxonomy (what a candidate looks like)

| Signature             | The accommodation                                                      | Usually compensates for                                                                    |
| --------------------- | ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| Re-validation         | deeper layers re-check a precondition the boundary should enforce      | a boundary that doesn't validate                                                           |
| Re-normalization      | N call sites each fix up the same value shape                          | a producer emitting inconsistent shapes                                                    |
| Catch→default         | a failure is swallowed into a fallback value/path                      | a dependency failure never fixed, or a contract that lies about it                         |
| Retry dialects        | hand-rolled backoff loops, different bases/caps/jitter                 | transient tolerance is legit — the smell is N dialects, or retrying a deterministic defect |
| Compat/legacy shim    | translation for an old schema/wire/record shape                        | a migration that never ran to completion                                                   |
| Defensive optionality | `?.`/`??`/`\|\| default` chains around data that "should always" exist | a producer contract that doesn't guarantee presence                                        |
| Double-check          | sleep-then-read, read-after-write verify, wedged-flag probes           | an event/wake path that can't be trusted — or once couldn't                                |
| Parallel truth        | a second bookkeeping of the same fact kept "just in case"              | a store/ledger the system stopped trusting                                                 |

Census playbook with commands: [references/signals.md](references/signals.md)

## The legitimacy test (run before anything is called redundant)

Most guards exist for a reason that was once true. Before a candidate
enters a cluster, answer against **traced callers and the producer's
current code** — not comments, not git history:

1. What inputs/failures does this layer actually observe today?
   Enumerate the producing call paths.
2. Can the upstream still produce the variance this site handles? Read
   the producer as it is now. Variance reachable only historically =
   the guard is compensating.
3. If the guard were removed AND the root fixed, what concretely breaks?
   "Nothing" = redundant. "X under condition Y" = the guard, or the
   fix, must cover Y.
4. Is there a ratchet? A test/gate that fails without the guard means
   its necessity is pinned — deliberate. Ungated defensive code is
   suspect. (In-repo examples of pinned necessity: the SpendGuard
   construction gate, `markSpent` custody rules, the wait-site map —
   each is a guard whose legitimacy a check enforces.)
5. Transient or deterministic? Retry against genuine transient failure
   (network, fs race) is legitimate; retry against a deterministic
   upstream defect is compensation wearing a retry's clothes.

A guard that survives this test is **load-bearing** — record it in the
report's insurance list so the next audit doesn't re-litigate it.

## Operating rules

- **Every claim cites evidence** — traced call sites (file:line), quoted
  code, counted occurrences. No "looks defensive".
- **Trace callers, never intentions.** "Why the code exists" is answered
  by who calls it with what, not by what the comment says.
- **Fix proposals land at the boundary/seam**, never at the layer where
  the patches live — that's just patch N+1. If the seam doesn't exist,
  "the seam doesn't exist" IS the finding; if it exists but callers
  bypass it, that's an enforcement finding.
- **Every fix spec names its ratchet** — the type, gate, test, or deleted
  seam that keeps the patch class from regrowing. A root fix without a
  ratchet is a suggestion the codebase outgrows in a quarter. In-repo
  ratchet forms: `check:arch` grep gates, `check:leftovers` baselines,
  the wait-site coverage map, a type that can't hold the invalid shape.
- **Unproven causes are hypotheses.** State each with the check that
  settles it — never as a finding.
- **Audit-only.** Suggests and orders fixes; modifies no source unless
  explicitly asked.

## Operating modes

- **Inline** — one file, one package, one named pattern ("why do we
  normalize X everywhere"). Run the process below yourself.
- **Swarm** — whole-repo audits, explicit "deep/full" requests, or any
  scoped run where the census (Phase 1) produces more candidates than
  one pass can trace honestly. Fan out per
  [references/orchestration.md](references/orchestration.md): hunters own
  `slice × signal-family` units and return structured candidates, the
  parent clusters and drills, fresh verifiers challenge every fix spec.
  Swarm is the default for repo-wide runs.

## Process

### Phase 0 — Frame

- Scope: whole repo, a package list, or a named theme.
- Read `AGENTS.md`'s learned-the-hard-way sections — the past-incident
  catalog. A cluster that rhymes with a proven incident class is High
  confidence by default.
- Map the declared seams — the places fixes _should_ land: composition
  roots (`mcp/src/runtime.ts`, `chain/src/port.ts`), the env gate
  (`core/src/env.ts`), the log sink (`core/src/log.ts`), the one coin
  book (`chain` funds/`SqlFunds`), the executor seam (`txEmit`). A
  workaround sitting downstream of a declared seam names its suspect.

### Phase 1 — Census

Run [references/signals.md](references/signals.md): the marker census
(workaround/compat/temporary comments — the `check:leftovers` baseline
already counts TODOs), catch→default, retry dialects, re-normalization,
defensive optionality, double-check, compat shims, parallel truth, and
git fix-density. Output: a candidate list with counts and locations —
candidates, not findings.

### Phase 2 — Cluster

Group candidates by **common suspected cause**: same handled-variance +
same producer = one cluster, even across packages; different variance
handled by the same guard = two clusters sharing a site. Name the
producing seam per cluster. Drop singletons that fail the legitimacy
test into the insurance list.

### Phase 3 — Drill

Per cluster, run the root-cause drill from
[../arch-review/references/consolidation.md](../arch-review/references/consolidation.md)
(what invariant / which fixes were the same failure in costume / what
makes the class unrepresentable / why did the fix land here / who else
implements this) alongside the legitimacy test above. Output per
cluster, one line:

`<cluster> exists because <missing guarantee at seam S>; the fix is
<boundary change>; the k sites then simplify to <shape>.`

Can't write the line → hypothesis, not a finding.

### Phase 4 — Fix spec

Per proven cluster, produce the spec:

- **Root change** — the concrete boundary/seam edit (a type, a
  validated constructor, a producer contract, a deleted alternate path).
- **Removal ledger** — each downstream site with file:line and est. LOC;
  mark which simplify vs delete outright.
- **Preserved behavior** — what the workarounds currently absorb that
  must still be handled, and where that handling moves to.
- **Test plan** — the failing test that proves the seam (write it
  first), plus which existing tests pin the absorbed behavior and must
  stay green through removal.
- **Migration order** — money/durability paths migrate strangler-style:
  root fix lands green alone, then removals, then the ratchet. Never
  one commit that fixes the cause and deletes the witnesses — if the
  root fix is wrong, the witnesses were the only thing absorbing it.
- **Ratchet** — the gate/test/type that keeps the patch class out.

### Verification

Every HIGH+ fix spec goes to a fresh verifier per
[references/orchestration.md](references/orchestration.md) — two disproof
missions: prove a listed site is load-bearing (find the caller that
needs the tolerance), or prove the cause is elsewhere. Rejected specs
move to Hypotheses with the disproving evidence.

## Standard findings format

`RCA-n — [CRITICAL|HIGH|MEDIUM|LOW] — <cluster> — sites: k — cause:
<one line, evidence-cited> — root fix: <one line> — removes ~LOC —
ratchet: <gate>`

Severity: **CRITICAL** = the workaround masks a defect still firing on
a money/custody/durability path (the accommodation is absorbing live
corruption). **HIGH** = cluster on a money/durability path, or large
removal at low risk. **MEDIUM** = compounding sprawl, expensive at 2x.
**LOW** = cosmetic.

## Report template

```
## Root-Cause Audit — <scope>

### Scorecard
| Clusters audited | Causes proven | Hypotheses | Load-bearing cleared | Est. removable LOC |

### Findings
| # | Sev | Cluster | Sites | Root cause (evidence) | Root fix | Est. LOC | Ratchet |

### Fix specs
<per finding: root change; removal ledger w/ file:line; preserved
behavior; test plan; migration order; ratchet>

### Insurance (load-bearing — do not remove)
| Site | What it protects | Evidence it's still needed |

### Hypotheses
| Candidate cluster | Suspected cause | Settling check |

### Removal ledger
<ordered by LOC removed ÷ risk — steady shrinkage beats one heroic
rewrite; the ratchets reward it>
```

## Anti-patterns

1. Calling a guard redundant because its comment is stale — prove it
   against the producer's current code.
2. Proposing "handle it properly at each site" — patch N+1 at the same
   altitude the patches already live.
3. A fix spec with no ratchet — the class regrows.
4. Deleting the last witness: a workaround absorbing a still-firing
   defect gets the root fix FIRST, removal second, separate commits.
5. Clustering by text similarity instead of handled-variance — jscpd
   already owns syntax; this audit owns semantics.
6. Treating transient-failure retries as redundancy.
7. Merging distinct causes into one heroic refactor — the ledger is
   ordered so small proven fixes land first.

## Sources

Method per Ousterhout (A Philosophy of Software Design — define errors
out of existence, deep modules); root-cause drill shared with
arch-review's consolidation reference; strangler-fig migration per
Fowler; preserved-behavior pinning per Feathers (characterization
tests); redundancy ≠ duplication distinction per Nygard (Release It!);
the legitimacy test codifies "every layer of defense must defend against
something real" — a guard with no producing threat is debt, a guard
with one is insurance.
