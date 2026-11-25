# orchestration — multi-agent review runs (nektar v0.4 Pure-Go)

How a whole-system `arch-review` fans out. Narrow questions (one package, one
failure mode, one metric) run inline and never delegate.

## Roles

- **Parent** — frames Phase 0, runs Phase 1 measurements itself verbatim, owns
  the `slice × lens` coverage matrix, launches hunters, runs the coverage critic,
  launches fresh verifiers for every CRITICAL/HIGH finding, performs the global
  consolidation pass, and writes the final report.
- **Hunters (`research` subagents)** — read-only explorers assigned disjoint
  `slice × lens` units; return structured JSON findings meeting the evidence standard.
- **Coverage Critic (`research` subagent)** — checks the matrix + `reviewed_paths`
  for missed packages, unexamined lifecycle modes, or missed F-catalog families.
- **Fresh Verifiers (`research` subagents)** — independent agents whose sole job
  is to **disprove** every CRITICAL/HIGH finding from source before it enters
  the final report. No hunter ever verifies its own finding.

## Canonical v0.4 slices

| Slice | Packages & paths |
| --- | --- |
| `satoshi-path` | `internal/wallet`, `internal/chain`, `internal/contracts`, `internal/core/spendguard.go`, `internal/memory` (anchoring/recovery) |
| `event-fabric` | `internal/market` (Broker, Restate workflows/client, policy), `internal/hub`, `internal/bstack`, `internal/discovery` |
| `apps` | `cmd/nektar` (boot/shutdown/lock/restate sidecar/wallet CLI), `internal/server` (HTTP, MCP, A2A, Proxy, Paywall, OAuth, Metrics) |
| `libs-ops` | `internal/core`, `internal/store`, `internal/policy`, `internal/reasoning`, `internal/memory` (grounding/packs), `deploy/`, `ops/`, `install/`, `.github/workflows/` |

Lenses: `smells` ([smells.md](smells.md)) and `failure` ([failure-modes.md](failure-modes.md)).

Default full-run plan:
- Dedicated `failure` + `smells` hunters for `satoshi-path`, `event-fabric`, and `apps`
- Combined `smells` + `failure` hunter for `libs-ops`
- Mandatory post-wave Coverage Critic (1–2 re-waves max for genuine gaps)
- Mandatory Fresh Verifier pass challenging every CRITICAL and HIGH finding (rejected findings move to Hypotheses with disproving evidence).

## Structured hunter result contract

Every hunter returns JSON with:
- `units`: `[{ coverage_id, disposition, reviewed_paths, unresolved }]`
- `findings`: `[{ kind, rule, title, where, evidence, severity, failure_detail, recommendation }]`
- `hypotheses`: `[{ claim, settling_check }]`
- `uncovered`: `[{ area, reason }]`
