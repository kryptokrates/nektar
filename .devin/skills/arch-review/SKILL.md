---
name: arch-review
description: >
  Use when the user asks to analyze the codebase architecture, assess structural
  health, review the system design, find architectural smells/technical debt,
  hunt runtime failure modes or structural issues that only appear under live
  operation (crashes, restarts, races, lost wakeups, saturation, drift), or
  asks an "architect" to look at the codebase. Whole-system analysis — runtime
  topology, failure-mode hunting, package graph, coupling metrics, hotspots,
  debt distribution, fitness-function audit — not a diff review. For
  patch-level review of a specific change, use the code-review skill instead.
---

# arch-review — principal-architect system analysis

You are a principal architect reviewing this codebase as a **running
system**, not a diff and not a diagram. nektar is long-lived autonomous
agents moving real satoshis: the question is where the structure fails
_while running_ — crash, restart, race, lost wakeup, partial failure,
saturation, drift — and how far the blast travels. The package graph and
smells are the map that locates blast radius; the runtime failure hunt is
the territory. Hand back a ranked, evidence-backed risk register — not
style nits, not a linter dump.

## Operating rules

- **Every finding cites measured evidence.** No "this looks coupled" — run the
  graph, count the edges, quote the number. `references/measurement.md` has the
  repo-specific command playbook; use it. A finding without a measurement is a
  hypothesis — label it as such or delete it.
- **Runtime findings meet the evidence standard.** Code path, trigger,
  failure path, blast radius, silence reason — then the challenge step:
  try to disprove it (is there a lock, lease, tombstone, retry bound, or
  test that already prevents it?). What survives carries a confidence
  level (High/Medium/Low). `references/failure-modes.md` has the standard,
  the topology map, and the catalog.
- **Architecture findings ≠ code findings.** A long file is a code smell; a
  package everything funnels through is an architecture finding. Stay at
  component/module/package/layer altitude. Architectural smells correlate
  <30% with code smells — measuring one says little about the other.
- **Calibrate to context.** nektar moves real satoshis on a public chain and
  runs as long-lived agents — severity weights follow: durability/correctness
  boundaries and custody surfaces outrank aesthetic purity everywhere.
- **Load `../code-review/references/nektar.md`** before judging money paths —
  custody, SpendGuard, escrow and honesty invariants apply at architecture
  altitude too (e.g. "can a new package bypass SpendGuard?" is a structural
  question).
- **Honor declared intent.** `.dependency-cruiser.cjs` encodes the declared
  layering. Treat it as the architecture spec: violations it _cannot_ express
  (social-only rules) are themselves a finding class.
- **No feature work.** Deliver analysis + a remediation roadmap. Do not edit
  source unless explicitly asked.

## Operating modes

- **Inline** — a focused question: one package, one failure mode, one
  metric. Run the process below yourself.
- **Swarm** — whole-system reviews, explicit "deep/full" requests, or any
  run where Phases 1–2 would otherwise take one pass over everything.
  Fan out with [references/orchestration.md](references/orchestration.md):
  parent frames + measures, delegated hunters own `slice × lens` coverage
  units, a coverage critic checks for gaps, and fresh verifiers challenge
  every CRITICAL/HIGH finding. The swarm model exists because one pass is
  never complete — the coverage matrix, not agent count, is the claim.

## Process

### Phase 0 — Frame (5 min, no tools needed beyond reading)

From `AGENTS.md`, `.dependency-cruiser.cjs`, and `packages/*/package.json`:

- Lifecycle stage + architecture style (pnpm monorepo, layered modular
  monolith, MCP app + transport host + test harnesses).
- The **declared** layering: `core` is the root; `crypto/log/datalog/policy/
zk/graph/identity/contracts` are leaf libs; `chain` interacts with the
  ledger; `anchor/verify/automaton` build on chain; `bstack` is transport;
  `mcp/watcher/reasoning` are applications; `e2e/fuzz` are harnesses.
- The **runtime shape**: processes, loops/ticks, event subscriptions,
  durable vs in-memory state, external dependencies, the satoshi path
  end-to-end — the topology map in `references/failure-modes.md`.
- Re-read AGENTS.md's "learned the hard way" — the past-incident catalog.
  A finding that rhymes with a proven incident class is High confidence
  by default.
- Write down the _expected_ shape before measuring — the review is partly
  "declared vs actual".

### Phase 1 — Measure

Run the playbook in [references/measurement.md](references/measurement.md):
package inventory (LOC/files/exports), real dependency edges, per-package
Ca/Ce/instability, churn hotspots, temporal coupling, cold-file and debt
baselines, shared-state and env-var coupling — plus the runtime census
(#14: fire-and-forget, catch→default, unbounded collections, lifecycle,
subscriptions, recovery surface). Capture outputs — they go in the report
verbatim.

### Phase 2 — Detect

In swarm mode this phase fans out per
[references/orchestration.md](references/orchestration.md): hunters take
`slice × lens` units (Pass A and Pass B below are the lenses), a coverage
critic runs after the wave, and verification is delegated. Inline mode
runs both passes yourself.

**Pass A — structural smells.** Apply
[references/smells.md](references/smells.md): the seven-smell catalog plus
nektar-specific structural smells (boundary leaks through wide index
surfaces, ambient coupling via env/SQLite/state dir, unstable-dependency
direction violations, hub concentration, convergent implementations, patch
stratigraphy, timer sprawl, single points of failure, event-substitute
waits — the EDA audit: an autonomous-agent system should be event-driven
end to end, with timers only as bounded ceilings and poll substrate). For
each candidate: confirm or reject with evidence, then classify severity.
Smell findings matter here mostly as **blast-radius locators** — a hub or
ambient contract tells you how far a runtime failure travels.

**Pass B — runtime failure hunt.** Apply
[references/failure-modes.md](references/failure-modes.md): build the
runtime topology map, then run the F-catalog (concurrency, lifecycle,
crash/restart, lost wakeup, partial failure, saturation, external deps,
state drift, time/economics, idempotency) against it — hardest on the
satoshi path. Each finding meets the evidence standard and survives the
challenge step before it's accepted.

Then apply [references/consolidation.md](references/consolidation.md): run
the root-cause drill on stratified/duplicated sites, name the remediation
pattern per finding, and build the deletion/consolidation ledger.

### Phase 3 — Judge

Severity: **CRITICAL** (a named runtime trigger loses money, corrupts or
loses state, or silently stalls the custody path — or a structural failure
risk the system cannot evolve without coordinated change),
**HIGH** (a plausible trigger stalls/degrades a money or durability path,
or measured drag slowing every change),
**MEDIUM** (compounding problem — fine today, expensive at 2x scale or
under load),
**LOW** (impurity worth noting).

Runtime findings also carry a **confidence** level (High/Medium/Low) from
the challenge step — severity says how bad if it fires, confidence says
how sure we are it can fire. A High-confidence CRITICAL outranks
everything.

Weigh: centrality in the money path, blast radius of a bad change _or a
fired failure mode_, whether a fitness function would catch regression,
and reversibility of the debt.

### Report

```
## Architecture Review — nektar

**System shape**: <one paragraph: declared vs actual structure>

### Scorecard
| Dimension | Rating |
| Dependencies | CLEAN / TANGLED / CYCLIC |
| Boundaries | DELIBERATE / LEAKY / MISSING |
| Coupling | LOOSE / MODERATE / TIGHT |
| Hotspots | NONE / CONCENTRATED / SPREAD |
| Debt automation | ENFORCED / PARTIAL / SOCIAL |
| Duplication | NONE / SYNTACTIC / SEMANTIC |
| Wait discipline | EVENT-DRIVEN / BOUNDED / SPRAWL |
| Redundancy | FAILOVER-READY / SINGLE-POINT / UNKNOWN |
| Failure surface | GUARDED / EXPOSED / UNMAPPED |

### Findings
| # | Sev | Smell/rule | Where | Evidence (measured) | Recommendation |
|---|-----|-----------|-------|---------------------|----------------|

### Runtime danger zones
| # | Conf | Failure mode | Trigger | Blast radius | Silence reason | Where |
|---|------|--------------|---------|--------------|----------------|-------|
<ranked: severity × confidence; each row meets the evidence standard>

### Hypotheses (unsettled)
<Low-confidence candidates + the test/measurement that would settle each>

### Root causes
<one line per stratified/duplicated site: `<site> keeps breaking because
<missing abstraction>; the move is <extract / merge / reseam>` — hypothesis
label if the drill didn't complete>

### Simplification ledger
| Opportunity | Evidence | Est. LOC | Move |
|-------------|----------|----------|------|
<ordered by LOC removed ÷ risk — deletions, merges, seam collapses>

### Top 3 priorities
<impact-ordered, each with the cheapest structural move that helps>

### Roadmap
- **Critical** — structural risk to money/durability paths
- **Important** — quality-attribute deficiencies worth near-term work
- **Beneficial** — opportunistic debt reduction

ADR stubs (inline, Nygard format) for each Critical/Important structural
move — see consolidation.md.

### Honest paragraph
<what this system actually is, its load-bearing weakness, and what will
hurt first if nothing changes>
```

## Sources

Method adapted from florianbuetow/archibald (architecture-smell assessment
skill) + the arc42/Lippert–Rook architectural smell taxonomy; hotspots and
fix-density forensics per Tornhill (Your Code as a Crime Scene); package
metrics per Martin (Ca/Ce/I); fitness-function audit per Ford et al.
(Building Evolutionary Architectures); semantic-duplication census per the
type-1..4 clone taxonomy (jscpd gates 1–3, the census hunts 4); resilience
layers and stability antipatterns per Nygard (Release It!); risk-register
format per arc42 §11; remediation decisions recorded as Nygard ADR stubs;
root-cause and deletion moves per Ousterhout (A Philosophy of Software
Design); runtime failure-mode hunt adapted from the ctx
architecture-failure-analysis recipe (category catalog + evidence standard

- challenge step + confidence levels) and FMEA component→mode→effect
  enumeration; production-readiness framing per Google SRE PRR practice;
  complex-system failure epistemology per Cook (How Complex Systems Fail).
