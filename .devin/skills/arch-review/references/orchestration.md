# orchestration — multi-agent review runs

How a full arch-review fans out. Read this when the run is a swarm run
(whole-repo, explicit "deep/full" request, or a scoped run still too wide
for one pass). Narrow questions — one package, one failure mode, one
metric — run inline with the main SKILL.md process and never delegate.

## Roles

- **Parent** — the agent holding this skill. Frames the run (Phase 0),
  runs the measurement playbook itself (Phase 1 — deterministic
  commands, evidence travels verbatim, no benefit to delegating), owns
  the coverage plan, launches agents, dedupes findings, does the
  consolidation pass (it needs the global view), and writes the report.
  Only the parent consolidates — agents never see each other's output.
- **research agent** — read-only delegated explorer (Devin:
  `subagent_explore`). Runs the post-wave coverage critic.
- **general agent** — delegated worker with execution access (Devin:
  `subagent_general`). Runs detection hunters and fresh verifiers.

Use the platform's equivalents, preserving three boundaries: each agent
gets exactly its assigned units, agents return structured results
instead of writing shared state, and no agent verifies its own finding.

## Coverage plan

A unit is **subsystem slice × lens**. Canonical slices for this repo:

| Slice | Packages |
|---|---|
| `satoshi-path` | `chain`, `anchor`, `automation` (funding/toolbox/market), `mcp` (funding/txops/runtime) |
| `event-fabric` | `bstack/*`, `automation` runtime (watch-shim, outbox, notify) |
| `apps` | `mcp` (host/http/memory), `watcher`, `reasoning`, `memory` |
| `libs` | `core`, `crypto`, `log`, `datalog`, `policy`, `zk`, `graph`, `identity`, `contracts`, `store`, `verify`, `automaton` |
| `harnesses` | `e2e`, `fuzz`, `testkit`, `tools` |

Lenses: `smells` ([references/smells.md](smells.md)) and `failure`
([references/failure-modes.md](failure-modes.md) — the F-catalog).

Default full-run plan = every slice × both lenses, with
`satoshi-path × failure` and `event-fabric × failure` mandatory and
never merged into a wider assignment — they get dedicated hunters.
`libs` may merge `smells` + `failure` into one hunter when the budget is
tight. When reducing scope, drop `smells` passes before `failure`
passes; a slice no hunter reaches is `deferred` and disclosed in the
report — never silently unassigned.

`consolidation` ([references/consolidation.md](consolidation.md)) is not
delegated: root-cause drills and the deletion ledger need every hunter's
output in view.

## Hunter prompt contract

Every hunter prompt contains, in this order:

1. Role preamble (two sentences): find architecture-level structural
   smells and runtime failure modes in the assigned units; return
   exactly one JSON object matching the result contract below.
2. The frame: lifecycle stage, declared layering, runtime shape, and the
   past-incident catalog from AGENTS.md's "learned the hard way" — the
   parent writes this once in Phase 0 and pastes it verbatim.
3. Assigned coverage IDs, slices, lenses, and repository-relative
   starting paths. State explicitly which slices peer hunters own so
   this hunter does not duplicate them.
4. The reference files for its lenses — load
   [smells.md](smells.md) and/or [failure-modes.md](failure-modes.md) by
   path, plus `../../code-review/references/nektar.md` for any unit
   touching the satoshi path. Never send file names alone.
5. The evidence standard: every finding carries a measured number,
   quoted code, or command output; runtime findings name trigger,
   failure path, blast radius, silence reason, and survive the
   hunter-side challenge step (look for the lock, lease, tombstone,
   retry bound, or test that already prevents it).
6. Relevant Phase 1 measurements pasted in (LOC, edges, Ca/Ce/I,
   census rows for its slice) — the hunter should not re-run the whole
   playbook but may run targeted commands to settle a specific claim.
7. The structured result contract below.

## Structured hunter result

Return exactly one JSON object, no surrounding prose:

```json
{
  "units": [
    {
      "coverage_id": "satoshi-path/failure",
      "disposition": "covered|blocked",
      "reviewed_paths": ["repo/relative/path"],
      "unresolved": ["what blocked a blocked unit"]
    }
  ],
  "findings": [
    {
      "kind": "smell|failure",
      "rule": "S3|F4|ev/timeout-as-mechanism",
      "title": "one line",
      "where": "packages/x/src/y.ts:42",
      "evidence": "measured number, quoted code, or command output",
      "severity": "CRITICAL|HIGH|MEDIUM|LOW",
      "failure_detail": {
        "trigger": "named runtime trigger",
        "failure_path": "what breaks, in order",
        "blast_radius": "how far it travels",
        "silence_reason": "why nothing notices",
        "confidence": "High|Medium|Low",
        "challenge": "the defense looked for + whether it holds"
      },
      "recommendation": "smallest structural move"
    }
  ],
  "hypotheses": [
    {"claim": "unsettled candidate", "settling_check": "the test/measurement"}
  ],
  "uncovered": [
    {"area": "…", "reason": "why it needs its own unit next wave"}
  ]
}
```

`failure_detail` is required for `kind: "failure"`, null otherwise. A
`covered` unit has nonempty `reviewed_paths` and no unresolved entries.
`uncovered` is how a hunter reports a real area outside its assignment —
the parent turns each into a planned unit, not a lost note.

## Coverage critic

After the hunter wave, launch one fresh research agent. It gets the
slice/lens matrix, each unit's disposition + reviewed_paths, and the
finding titles (not bodies — it checks coverage, not findings). It reads
source and returns:

```json
{
  "missing_units": [{"slice": "…", "lens": "…", "reason": "source-backed gap"}],
  "reopen": ["coverage_id closed without evidence"],
  "stop": false
}
```

It checks: every package appeared in some unit's `reviewed_paths`;
every F-catalog family ran against `satoshi-path`; the runtime census
rows (fire-and-forget, catch→default, unbounded collections,
subscriptions, recovery surface) were enumerated, not sampled; lifecycle
modes (boot, steady, restart, shutdown) were each considered on
long-lived paths. `stop: true` only when it proposes nothing. One
re-wave is normal; two is the ceiling — remaining gaps go in the report
as deferred.

## Verification pass

Every CRITICAL and HIGH finding goes to a fresh general agent whose only
job is to disprove it: re-derive the claim from source, hunt the
preventing mechanism (lock, lease, tombstone, retry bound, existing
test, gate), and return `stands` or `rejected` with the evidence either
way. The hunter that raised a finding never verifies it. Rejected
findings are dropped from Findings but listed under Hypotheses with the
disproving evidence — a rejected claim is still information.

## Anti-patterns

1. Delegating Phase 1 measurement — verbatim command output is the
   evidence; a hunter's summary of it is not.
2. Sending agents the lens name instead of the file path and content.
3. Letting a hunter roam outside its slice — overlap produces duplicate
   findings the parent must merge, or worse, contradictory severities.
4. Verifying with the same agent that found the issue — the challenge
   step only has teeth when the challenger is fresh.
5. Treating agent count as coverage — the slice × lens matrix is the
   coverage claim; agents are how it gets executed.
