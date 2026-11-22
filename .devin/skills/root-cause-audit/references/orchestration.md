# orchestration — multi-agent audit runs

How a root-cause-audit fans out. Read this when the run is a swarm run
(whole-repo, explicit "deep/full" request, or a Phase 1 census too wide
for one honest tracing pass). Scoped questions — one file, one helper,
one named pattern — run inline and never delegate.

## Roles

- **Parent** — the agent holding this skill. Frames the run (Phase 0),
  runs the census itself (Phase 1 — deterministic commands, evidence
  travels verbatim; hunters can't run `git log`/exec anyway), owns the
  clustering, runs the drill + fix specs, and writes the report. Only
  the parent clusters — hunters never see each other's candidates.
- **hunter** — read-only delegated explorer (Devin:
  `subagent_explore`). Traces candidates to callers/producers, applies
  the legitimacy test, returns structured candidates.
- **critic** — one fresh `subagent_explore` after the hunter wave.
  Checks coverage, not findings.
- **verifier** — fresh `subagent_explore` per HIGH+ fix spec. Disproof
  only; never the hunter that raised the cluster.

This audit is read-only end to end — no role edits source, so explore
agents suffice everywhere. (If the user later asks to apply a fix spec,
that's a separate task, not this audit.)

## Coverage plan

A unit is **slice × signal-family**. Slices from arch-review's table:

| Slice          | Packages                                                                                                             |
| -------------- | -------------------------------------------------------------------------------------------------------------------- |
| `satoshi-path` | `chain`, `anchor`, `automation` (funding/toolbox/market), `mcp` (funding/txops/runtime)                              |
| `event-fabric` | `bstack/*`, `automation` runtime (watch-shim, outbox, notify)                                                        |
| `apps`         | `mcp` (host/http/memory), `watcher`, `reasoning`, `memory`                                                           |
| `libs`         | `core`, `crypto`, `log`, `datalog`, `policy`, `zk`, `graph`, `identity`, `contracts`, `store`, `verify`, `automaton` |
| `harnesses`    | `e2e`, `fuzz`, `testkit`, `tools`                                                                                    |

Signal-families (from [signals.md](signals.md)):

| Family       | Signatures                                                          |
| ------------ | ------------------------------------------------------------------- |
| `boundary`   | re-normalization, re-validation, defensive optionality              |
| `failure`    | catch→default, retry dialects, fallback paths                       |
| `compat`     | shims, shape-detection branches, migration residue, marker comments |
| `wait-check` | sleep-then-check, read-after-write verify, parallel truth           |

Default full-run plan = each slice × the families its census rows hit —
the census tells you where the signals live, so a slice with zero
`compat` candidates doesn't need a `compat` hunter. `satoshi-path`
units are mandatory whenever their census is nonempty and never merge
into wider assignments. Tight budget: merge families within a slice
before dropping a slice; a slice no hunter reaches is `deferred` and
disclosed in the report.

Clustering, the drill, and fix specs are **not delegated**: they need
every hunter's candidates in one view. This is the same boundary
arch-review draws — agents enumerate and challenge; the parent
synthesizes.

## Hunter prompt contract

Every hunter prompt contains, in this order:

1. Role preamble (two sentences): trace workaround candidates in the
   assigned units to their callers and producers, apply the legitimacy
   test, return exactly one JSON object matching the result contract.
2. The frame: declared seams from Phase 0 (composition roots, env/log
   gates, funds/executor seams) and the AGENTS.md incident catalog —
   pasted verbatim.
3. Assigned coverage IDs, slices, signal-families, and
   repository-relative starting paths. State which slices peer hunters
   own.
4. The signal catalog by path —
   [.devin/skills/root-cause-audit/references/signals.md](signals.md) —
   plus the legitimacy test from SKILL.md. Never send names alone.
5. The relevant Phase 1 census rows for its slice, pasted in — hunters
   trace and classify census rows, they don't re-run the census. They
   may follow a row to a new site it implicates.
6. The evidence standard: every candidate carries traced call sites
   (file:line), quoted code, or counted occurrences, and the legitimacy
   test answered against the producer's **current** code.
7. The structured result contract below.

## Structured hunter result

Return exactly one JSON object, no surrounding prose:

```json
{
  "units": [
    {
      "coverage_id": "apps/failure",
      "disposition": "covered|blocked",
      "reviewed_paths": ["repo/relative/path"],
      "unresolved": ["what blocked a blocked unit"]
    }
  ],
  "candidates": [
    {
      "signature": "renormalize|revalidate|catch-default|retry|compat|optionality|double-check|parallel-truth",
      "title": "one line",
      "where": "packages/x/src/y.ts:42",
      "traced_callers": ["packages/a/src/b.ts:10 — passes already-normalized input"],
      "producer": "packages/p/src/cfg.ts — emits both shapes",
      "observed_variance": "what the site actually sees today",
      "suspected_cause": "the missing guarantee, one line",
      "legitimacy": {
        "verdict": "compensating|load-bearing|hypothesis",
        "evidence": "the test answers — especially #2 (can upstream still produce it) and #4 (is there a ratchet)"
      },
      "est_loc": 15
    }
  ],
  "hypotheses": [{ "claim": "unsettled candidate", "settling_check": "the test/measurement" }],
  "uncovered": [{ "area": "…", "reason": "why it needs its own unit next wave" }]
}
```

`traced_callers` must be **enumerated, not sampled** — "5 callers"
without the 5 file:lines is a coverage gap the critic will reopen. A
`load-bearing` verdict needs the same evidence rigor as `compensating`;
either way it feeds the parent's insurance list or a cluster.

## Coverage critic

After the hunter wave, launch one fresh explore agent. It gets the
slice × family matrix, each unit's disposition + reviewed_paths, census
row counts, and candidate titles (not bodies). It reads source and
returns:

```json
{
  "missing_units": [{ "slice": "…", "family": "…", "reason": "source-backed gap" }],
  "reopen": ["coverage_id closed without evidence"],
  "undispositioned_rows": ["census rows no candidate or insurance entry covers"],
  "stop": false
}
```

It checks: every census row got a disposition (clustered / insurance /
hypothesis / out-of-scope-with-reason); every candidate's callers were
enumerated; producer seams were actually read, not assumed; `satoshi-path`
census rows all resolved. `stop: true` only when it proposes nothing.
One re-wave is normal; two is the ceiling — remaining gaps go in the
report as deferred.

## Verification pass

Every HIGH and CRITICAL fix spec goes to a fresh explore agent with
**two disproof missions**:

1. **Load-bearing disproof** — find a caller/condition that still needs
   the tolerance: a producer path the parent missed, a live old-shape
   writer, a transient that fires in production. `stands` only if no
   such caller exists.
2. **Wrong-cause disproof** — find an alternative explanation for the
   cluster: maybe the sites converge because the _contract_ is right
   and three callers are buggy, or the "missing seam" exists two files
   over and the fix is enforcement, not creation.

The verifier returns `stands` or `rejected` with evidence either way.
The hunter/parent that raised the spec never verifies it. Rejected
specs move to Hypotheses with the disproving evidence — a rejected
claim is still information.

## Anti-patterns

1. Delegating the census — verbatim command output is the evidence; a
   hunter's summary of it is not.
2. Sending agents the family name instead of the signals.md path.
3. Clustering inside a hunter — it sees one slice; cross-package
   clusters are exactly the ones it can't see.
4. A hunter roaming outside its slice — overlap produces duplicate
   candidates the parent must merge.
5. Verifying with the agent that raised the cluster — the disproof only
   has teeth when the challenger is fresh.
6. Treating agent count as coverage — the census-row disposition is the
   coverage claim; agents are how it gets executed.
