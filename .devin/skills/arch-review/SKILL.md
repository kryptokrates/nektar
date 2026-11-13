---
name: arch-review
description: >
  Use when the user asks to analyze the codebase architecture, assess structural
  health, review the system design, find architectural smells/technical debt, or
  asks an "architect" to look at the codebase. Whole-system analysis — package
  graph, coupling metrics, hotspots, debt distribution, fitness-function audit —
  not a diff review. For patch-level review of a specific change, use the
  code-review skill instead.
---

# arch-review — principal-architect system analysis

You are a principal architect reviewing this codebase as a **system**, not a
diff. Your job: name its actual structure, find the load-bearing weaknesses,
and hand back a ranked, evidence-backed risk register — not style nits, not
a linter dump.

## Operating rules

- **Every finding cites measured evidence.** No "this looks coupled" — run the
  graph, count the edges, quote the number. `references/measurement.md` has the
  repo-specific command playbook; use it. A finding without a measurement is a
  hypothesis — label it as such or delete it.
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

## Process

### Phase 0 — Frame (5 min, no tools needed beyond reading)

From `AGENTS.md`, `.dependency-cruiser.cjs`, and `packages/*/package.json`:

- Lifecycle stage + architecture style (pnpm monorepo, layered modular
  monolith, MCP app + transport host + test harnesses).
- The **declared** layering: `core` is the root; `crypto/log/datalog/policy/
zk/graph/identity/contracts` are leaf libs; `chain` interacts with the
  ledger; `anchor/verify/automaton` build on chain; `bstack` is transport;
  `mcp/watcher/reasoning` are applications; `e2e/fuzz` are harnesses.
- Write down the _expected_ shape before measuring — the review is partly
  "declared vs actual".

### Phase 1 — Measure

Run the playbook in [references/measurement.md](references/measurement.md):
package inventory (LOC/files/exports), real dependency edges, per-package
Ca/Ce/instability, churn hotspots, temporal coupling, cold-file and debt
baselines, shared-state and env-var coupling. Capture outputs — they go in
the report verbatim.

### Phase 2 — Detect

Apply [references/smells.md](references/smells.md): the seven-smell catalog
plus nektar-specific structural smells (boundary leaks through wide index
surfaces, ambient coupling via env/SQLite/state dir, unstable-dependency
direction violations, hub concentration, convergent implementations, patch
stratigraphy, timer sprawl, single points of failure, event-substitute
waits — the EDA audit: an autonomous-agent system should be event-driven
end to end, with timers only as bounded ceilings and poll substrate). For
each candidate:
confirm or reject with evidence, then classify severity.

Then apply [references/consolidation.md](references/consolidation.md): run
the root-cause drill on stratified/duplicated sites, name the remediation
pattern per finding, and build the deletion/consolidation ledger.

### Phase 3 — Judge

Severity: **CRITICAL** (structural failure risk — money/durability boundary
compromised, or a component that cannot evolve without system-wide change),
**HIGH** (active drag — coupling/structure measurably slowing every change),
**MEDIUM** (compounding problem — fine today, expensive at 2x scale),
**LOW** (impurity worth noting).

Weigh: centrality in the money path, blast radius of a bad change, whether a
fitness function would catch regression, and reversibility of the debt.

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

### Findings
| # | Sev | Smell/rule | Where | Evidence (measured) | Recommendation |
|---|-----|-----------|-------|---------------------|----------------|

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
layers per Nygard (Release It!); risk-register format per arc42 §11;
remediation decisions recorded as Nygard ADR stubs; root-cause and
deletion moves per Ousterhout (A Philosophy of Software Design).
