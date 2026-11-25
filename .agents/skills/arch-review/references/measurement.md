# Measurement playbook — nektar v0.4 Pure-Go

Run these from the repo root before judging. Capture output verbatim — evidence
travels with every finding. **Zero rows is a command failure, not a finding.**

## 1. Package inventory & import graph (`go list`)

```bash
go list -f '{{.ImportPath}} {{len .GoFiles}} {{join .Imports ","}}' ./cmd/... ./internal/...
```

Count non-test Go LOC, files, and exported declarations per package (`cmd/nektar`,
`internal/*`). Compute `Ca` (afferent internal importers), `Ce` (efferent
internal imports), and Instability `I = Ce / (Ca + Ce)`.

Expected shape:
- `internal/core`, `internal/store`, `internal/policy`, `internal/reasoning` → low `I` (stable roots)
- `internal/wallet`, `internal/chain`, `internal/contracts`, `internal/bstack` → mid-layer custody/wire
- `internal/memory`, `internal/discovery`, `internal/hub`, `internal/market`, `internal/server` → higher `I` services
- `cmd/nektar`, `internal/e2e` → `I = 1.0` (composition root / test harness)

Flag any package >3,500 non-test LOC or >60 exported symbols as a god-package candidate.

## 2. Boundary & custody invariants

```bash
# Raw private-key custody boundary (should exist only in internal/wallet + reasoning.MakeDelegation + tests)
rg -n 'ec\.PrivateKey' cmd/ internal/ --glob '!*_test.go'

# Direct SpendGuard / broadcast / wallet bypasses outside internal/chain & internal/wallet
rg -n 'Broadcast|PostFromBEEF|CreateAction|SendToAddress' cmd/ internal/ --glob '!*_test.go'

# Embedded artifact & UI skew checks
diff -rq contracts/artifacts internal/contracts/artifacts
diff -rq ui/dist internal/server/ui_dist
```

## 3. Hotspots — churn × size (Tornhill)

```bash
git log --since="6 months ago" --format="@@@%H" --numstat -- cmd internal \
  | awk '/^@@@/{next} NF==3 && $3 ~ /\.go$/ && $3 !~ /_test\.go$/{n[$3]++} END{for(f in n) print n[f], f}' \
  | sort -rn | head -25
```

Join with non-test file LOC. **Hotspot = high revisions AND high LOC.**

## 4. Temporal coupling (hidden coupling)

Files that change together across commits without an import edge reveal ambient
contracts (env vars, SQLite schemas, HTTP/CBOR wire formats, `.state`/data-dir files):

```bash
git log --since="6 months ago" --name-only --format="@@@%H" -- cmd internal \
  | awk '/^@@@/{if(c>1)for(i in s)for(j in s)if(i<j)p[i"|"j]++;delete s;c=0;next}
         $1 ~ /\.go$/ && $1 !~ /_test\.go$/{s[$1]=1;c++} END{for(k in p)print p[k],k}' \
  | sort -rn | head -20
```

## 5. Ambient coupling (env / filesystem / SQLite)

```bash
# All NEKTAR_* / RESTATE_* env reads across packages
rg -o '(NEKTAR|RESTATE)_[A-Z0-9_]+' cmd/ internal/ deploy/ --glob '!*_test.go' | sort | uniq -c | sort -rn

# All SQLite DB openers and file-path contracts
rg -n 'sql\.Open|modernc\.org/sqlite|os\.WriteFile|os\.OpenFile' cmd/ internal/ --glob '!*_test.go'
```

## 6. Fitness-function & ops parity audit

Run and quote pass/fail:
- `go vet ./...`
- `go test -race ./...`
- `CGO_ENABLED=0 go build ./cmd/nektar`
- Inspect `.github/workflows/{ci,release,deploy,regtest}.yml`, `ops/alerts.yml` vs `internal/server/metrics.go`, and `install/install.{sh,ps1}`.

Any invariant the architecture depends on that CI does not test or enforce is an `S2` finding.

## 7. Concept census — semantic duplication (type-4 clones)

Grep for repeated domain concepts across `cmd/` and `internal/`:
- HTTP client construction (`&http.Client`, `SafeHTTPClient`)
- Transaction / BEEF / BRC-29 parsing (`ParseBEEF`, `NewTransactionFromBEEF`, `DerivePaymentKey`)
- Retry / backoff / polling loops (`time.NewTicker`, `time.Sleep`, `time.After`)
- Auth token loading / verification (`operator-token`, `http-token`, `ConstantTimeCompare`)
- Anchor proof / header verification (`IsValidRootForHeight`, `VerifyFragment`)

## 8. Patch stratigraphy — fix-density

```bash
git log --since="6 months ago" --format="@@@%s" --name-only -- cmd internal \
  | awk '/^@@@/{s=tolower($0); f=(s ~ /fix|bug|regress|patch|broke|harden/);next}
         $1 ~ /\.go$/ && $1 !~ /_test\.go$/{n[$1]++; if(f)fx[$1]++}
         END{for(k in n) if(n[k]>=4) printf "%d/%d %.0f%% %s\n", fx[k]+0, n[k], 100*fx[k]/n[k], k}' \
  | sort -rn | head -20
```

## 9. Timer, wait & EDA census

```bash
rg -n 'time\.Sleep|time\.NewTicker|time\.After|time\.NewTimer' cmd/ internal/ --glob '!*_test.go'
```

Classify every site: **retry-on-failure**, **event-substitute** (spin-polling where
SSE/Restate attach/channel wake exists), or **substrate** (one bounded poll floor
per subsystem).

## 10. Runtime census (Go failure-hunt substrate)

```bash
# F1/F2 Goroutines — every `go func` needs ctx cancellation + WaitGroup or clear ownership
rg -n '\bgo\s+(func|[a-zA-Z_])' cmd/ internal/ --glob '!*_test.go'

# F5 Swallowed errors — check every ignored error on money/store/crypto paths
rg -n '^\s*_\s*=\s*|,\s*_\s*:?=' cmd/ internal/ --glob '!*_test.go'

# F6 Unbounded maps/slices in long-lived structs
rg -n 'make\(map\[' cmd/ internal/ --glob '!*_test.go'

# F2/F6 Lifecycle & shutdown
rg -n 'signal\.Notify|Shutdown\(|Close\(\)|Stop\(\)|exec\.Command' cmd/ internal/ --glob '!*_test.go'

# F3 Crash recovery & reconciliation
rg -n 'Reconcile|Recover|Pending|Abort|UnFail|Awakeable' cmd/ internal/ --glob '!*_test.go'
```
