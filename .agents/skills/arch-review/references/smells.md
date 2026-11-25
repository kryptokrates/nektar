# Smell catalog — nektar v0.4 Pure-Go

Seven canonical architectural smells (Lippert/Rook, arc42) + nektar-specific
`S1–S10`. Every finding must attach measured evidence from `measurement.md`.

## Canonical seven

1. **Cyclic dependency**: Go forbids package cycles at compile time, so hunt
   **logical cycles** (callbacks/closures/atomic pointers wiring `server ↔ market ↔ memory`
   or shared SQLite tables bridging two packages).
2. **Unstable dependency**: stable roots (`core`, `store`, `policy`, `reasoning`,
   `contracts`) depending on volatile service/transport layers (`server`, `market`, `hub`).
3. **Hub-like dependency**: high `Ca` AND high `Ce` on a volatile package.
   (`internal/core` and `internal/store` having high `Ca` with low `Ce` is healthy;
   a mid/top package with both high `Ca` and high `Ce` is an alarm.)
4. **God component**: >3,500 non-test LOC or >60 exports serving unrelated concerns
   (e.g., `internal/market/broker.go`, `internal/server/http.go`, `internal/memory/memory.go`).
5. **Feature concentration / smear**: one user-facing flow (e.g., paid disclosure
   or wallet sync) smeared across 5+ packages with incidental coupling.
6. **Scattered functionality**: BSV tx/BEEF/BRC-29 parsing, payment verification,
   or auth principal enforcement duplicated across `server`, `market`, `hub`, `chain`.
7. **Ambiguous interface**: wide exported surfaces mixing production APIs with
   test/internal helpers or unguarded bypass entrypoints (`ExportShardPack` vs
   `ExportShardPackAuthorized`).

## Nektar-specific smells (`S1–S10`)

- **S1. Ambient coupling (env / fs / SQLite)**: `NEKTAR_*` env vars read in >1
  package, shared data-dir files (`operator-token`, `daemon.json`, `spend-guard.json`),
  or multiple packages querying the same SQLite tables directly.
- **S2. Unenforced invariant**: structural rules enforced only by convention with
  no CI check (e.g., raw key custody outside `internal/wallet`, `contracts/artifacts`
  vs `internal/contracts/artifacts` parity, `ops/alerts.yml` vs `/v1/metrics` parity,
  fail-closed installer checksums).
- **S3. Trust-boundary smearing**: untrusted inputs (tainted market fragments,
  unauthenticated `/bbox/send` envelopes, caller-supplied `Principal` JSON,
  unverified seller SPV bundles) crossing into trusted reasoning/policy/payment
  paths without type or validation separation.
- **S4. Restart-invisible state**: in-memory maps (`publicChirp`, `bboxExpected`,
  `oauthTokens`, `pairCodes`) treated as durable across daemon restarts.
- **S5. Binary / image / artifact skew**: `ui/dist` vs `internal/server/ui_dist`,
  `contracts/artifacts` vs `internal/contracts/artifacts`, release binaries built
  differently from GHCR Docker images, or unpinned `:edge` deployments.
- **S6. Convergent implementation**: same domain contract implemented 2–3+ times
  (dual deal engines, duplicate `RefreshAnchorProofs` loops, duplicate BRC-29
  payment checks across paywall and market purchase).
- **S7. Patch stratigraphy**: high fix-density (>40% fix commits with ≥4 touches)
  indicating a missing core abstraction.
- **S8. Timer sprawl**: uncoordinated hardcoded timeouts/tickers, stacked retry
  layers, or short unary HTTP client timeouts reused on streaming/attach RPCs.
- **S9. Single point of failure**: single SQLite writer without OS lock, single
  unvalidated external indexer, or fail-open config/state file corruption.
- **S10. Event-substitute wait**: `time.Sleep` spin-polling where Restate `Attach`,
  SSE, or channel notification already exists.
