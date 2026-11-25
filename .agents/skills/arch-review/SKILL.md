---
name: arch-review
description: >
  Use when the user asks to analyze the codebase architecture, assess structural
  health, review the system design, find architectural smells/technical debt,
  hunt runtime failure modes or structural issues that only appear under live
  operation (crashes, restarts, races, lost wakeups, saturation, drift), or
  asks an "architect" to look at the codebase. Whole-system analysis — runtime
  topology, failure-mode hunting, Go package graph, coupling metrics, hotspots,
  debt distribution, fitness-function audit — not a diff review.
---

# arch-review — principal-architect system analysis (nektar v0.4 Pure-Go)

You are a principal architect reviewing this codebase as a **running
system**, not a diff and not a diagram. nektar v0.4 is a single zero-CGO Go
binary (`cmd/nektar`) running long-lived autonomous agents moving real
satoshis on BSV alongside an embedded SQLite wallet, embedded Go Overlay Hub,
and Restate durable execution plane. The question is where the structure fails
_while running_ — crash, restart, race, lost wakeup, partial failure,
saturation, drift — and how far the blast travels. The package graph and
smells are the map that locates blast radius; the runtime failure hunt is
the territory. Hand back a ranked, evidence-backed risk register — not
style nits, not a linter dump.

## Operating rules

- **Every finding cites measured evidence.** No "this looks coupled" — run `go list`,
  count the edges, quote the number. [references/measurement.md](references/measurement.md)
  has the Go v0.4 command playbook; use it. A finding without a measurement is a
  hypothesis — label it as such or delete it.
- **Runtime findings meet the evidence standard.** Code path, trigger,
  failure path, blast radius, silence reason — then the challenge step:
  try to disprove it (is there a lock, SQLite tx, Restate step, SpendGuard
  reservation, CAS, or test that already prevents it?). What survives carries a
  confidence level (High/Medium/Low). [references/failure-modes.md](references/failure-modes.md)
  has the standard, topology map, and F-catalog.
- **Architecture findings ≠ code findings.** A long function is a code smell;
  a package everything funnels through or an uncoordinated shutdown sequence is
  an architecture finding. Stay at component/package/layer altitude.
- **Calibrate to context.** nektar moves real satoshis on a public chain and
  runs as long-lived autonomous agents — severity weights follow: custody,
  SpendGuard, anchor/deal durability, and honesty invariants outrank aesthetic
  purity everywhere.
- **Honor declared intent.** `AGENTS.md` and `.github/workflows/ci.yml` encode
  the declared layering and fitness gates. Rules that exist only in prose
  (social-only invariants) are themselves a finding class (`S2`).
- **No feature work during an audit.** Deliver analysis + a remediation
  roadmap. Do not edit source unless explicitly asked.

## Operating modes

- **Inline** — a focused question: one package, one failure mode, one metric.
  Run the process below yourself.
- **Swarm** — whole-system reviews, explicit "deep/full" requests, or any run
  where Phases 1–2 would otherwise take one pass over everything. Fan out with
  [references/orchestration.md](references/orchestration.md): parent frames +
  measures, delegated hunters own `slice × lens` coverage units, a coverage
  critic checks for gaps, and fresh verifiers challenge every CRITICAL/HIGH
  finding. The coverage matrix, not agent count, is the claim.

## Process

### Phase 0 — Frame

From `AGENTS.md`, `go.mod`, `cmd/nektar/`, `internal/*`, `deploy/`, and `.github/workflows/`:

- **Architecture style**: single zero-CGO Go binary (`cmd/nektar`) hosting
  MCP + HTTP + A2A + L0 LLM Proxy + embedded Svelte 5 UI (`internal/server`),
  embedded Go Overlay Hub (`internal/hub`), embedded BRC-100 SQLite wallet
  (`internal/wallet`), and Restate Go SDK durable workflows (`internal/market`).
- **Declared layering**:
  - Root/leaf libraries: `internal/core`, `internal/store`, `internal/policy`,
    `internal/reasoning`, `internal/contracts`, `internal/bstack`
  - Custody & chain boundary: `internal/wallet` (sole raw key custody) → `internal/chain`
  - Domain services: `internal/memory`, `internal/discovery`
  - Protocol/app services: `internal/hub`, `internal/market`, `internal/server`
  - Composition root & daemon supervision: `cmd/nektar`
  - Harnesses & ops: `internal/e2e`, `contracts/`, `ui/`, `deploy/`, `ops/`, `install/`, `.github/workflows/`
- **Runtime shape**: processes (`nektar serve`, managed `restate-server` sidecar),
  background goroutines/tickers, SSE streams, SQLite databases (`nektar.db`,
  `wallet.sqlite`, `hub.db`), OS file locks, external dependencies (Arcade/WoC,
  Restate, peer hubs, BSV Desktop `wirewallet`), and the satoshi path end-to-end.

### Phase 1 — Measure

Run the playbook in [references/measurement.md](references/measurement.md):
Go package inventory (LOC/files/exported symbols), real `go list` import DAG,
per-package `Ca`/`Ce`/instability `I`, git churn hotspots, temporal coupling,
env-var / SQLite / filesystem ambient coupling, CI fitness-function audit, and
the Go runtime census (#14: untracked goroutines, ignored errors `_ =`,
unbounded maps/slices, shutdown/lifecycle drains, SSE/subscription gaps,
crash-recovery surfaces). Capture outputs verbatim.

### Phase 2 — Detect

In swarm mode this phase fans out per [references/orchestration.md](references/orchestration.md).
Inline mode runs both passes yourself.

- **Pass A — structural smells** ([references/smells.md](references/smells.md)):
  seven canonical smells + nektar-specific `S1–S10` (ambient coupling, unenforced
  invariants, trust-boundary smearing, restart-invisible state, binary/UI/artifact
  skew, convergent implementations, patch stratigraphy, timer sprawl, SPoFs,
  event-substitute waits).
- **Pass B — runtime failure hunt** ([references/failure-modes.md](references/failure-modes.md)):
  build the runtime topology map, then run `F1–F12` hardest on the satoshi path,
  Restate workflows, wallet custody, Hub/Bbox delivery, and daemon lifecycle.
- **Consolidation** ([references/consolidation.md](references/consolidation.md)):
  root-cause drill on stratified/duplicated sites, named remediation patterns,
  simplification ledger, and Nygard ADR stubs.

### Phase 3 — Judge

- **CRITICAL**: a named runtime trigger loses satoshis, corrupts or loses
  durable state, breaks custody/auth/grounding invariants, or silently stalls
  the money/anchor path — or a structural failure risk requiring coordinated
  system-wide change.
- **HIGH**: a plausible trigger stalls/degrades a money or durability path, or
  measured structural drag slowing every change.
- **MEDIUM**: compounding issue — fine today, expensive at 2× scale or load.
- **LOW**: impurity worth noting.

Runtime findings carry **confidence** (`High`/`Medium`/`Low`) from the fresh
challenge step. A High-confidence CRITICAL outranks everything.

### Report

Deliver the report in the structured format (Scorecard, Structural Findings,
Runtime Danger Zones, Hypotheses/Disproved Claims, Root Causes, Simplification
Ledger, Top 3 Priorities, Roadmap with inline Nygard ADR stubs, and Honest
Paragraph).
