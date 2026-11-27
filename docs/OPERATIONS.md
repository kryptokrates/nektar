# Operations Guide — Nektar v0.1 (Pure-Go)

> **Honesty Rule:** Proofs establish **recorded, unaltered, and derivable by block time** — never that a memory's content is true, and model identity is *declared*, not zkML-proven.

---

## 1. Prerequisites & Build (Zero CGO, Single Binary)

- **Go 1.27+** (zero CGO required — pure-Go SQLite via `modernc.org/sqlite`)
- **Node.js 24+** *(only if rebuilding the embedded Svelte 5 Dashboard UI under `ui/`)*
- **Optional:** Docker / Compose for containerized Hub/Agent stacks (`deploy/hub/`, `deploy/agent/`)

```sh
# Build native static binary
go build -o nektar ./cmd/nektar

# Static cross-compile (Linux / Windows)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o nektar ./cmd/nektar
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o nektar.exe ./cmd/nektar

# Rebuild embedded Svelte 5 Dashboard UI (only when editing ui/)
cd ui && npm install && npm run build
# Sync ui/dist/ -> internal/server/ui_dist/ before rebuilding the Go binary
```

---

## 2. Verification & CI Gates

```sh
go vet ./...
go test -race -count=1 ./...
```

Key subsystem verification commands:

```sh
# Core crypto, SpendGuard, contracts & offline SPV verification
go test -race -v ./internal/core/... ./internal/contracts/... ./internal/chain/...

# Wallet BRC-100 engine, UTXO baskets & reconciliation
go test -race -v ./internal/wallet/...

# Memory, Cedar isolation, Mangle grounding, MemRepo & SQLite store
go test -race -v ./internal/store/... ./internal/memrepo/... ./internal/memory/... ./internal/policy/... ./internal/reasoning/...

# Market broker, Restate durable workflows, Hub BRC-105 fees & Server auth/MCP
go test -race -v ./internal/market/... ./internal/hub/... ./internal/server/... ./cmd/nektar/...

# Mainnet safety gate preflight (exits 0 only when confirm + >=2 non-circular PoW witnesses + settle spec are valid)
NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND \
NEKTAR_HEADERS_SOURCE="woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2" \
NEKTAR_SETTLE_SPEC="broadcaster:main" \
  nektar check-mainnet
```

---

## 3. Running & Operating a Node

### Foreground vs. Background Daemon

```sh
# Flagship 1-command background start + Restate + auto-dock + live receipt check:
nektar up                                              # testnet (~/.nektar, :18420)
nektar up --network main --confirm I_UNDERSTAND        # mainnet (~/.nektar/main, :18420)
nektar up --install-service                            # install per-user login autostart

# Stop cleanly (add --uninstall-service to remove login autostart):
nektar stop
nektar stop --uninstall-service

# Foreground server (for systemd / Docker / debugging):
nektar serve --listen 127.0.0.1:18420
nektar serve --hub --listen :18420                     # with embedded 16-lane Overlay Hub at /hub/
```

### Custody & Funding Operations

- **Desktop-first default (`--wallet auto`):** If BSV Desktop / Metanet Desktop is unlocked on `http://127.0.0.1:3321` for the matching network, Nektar selects `client` custody automatically and stores zero local spending keys. Otherwise it uses the embedded BRC-100 SQLite wallet under `<dataDir>/wallet/`.
- **Strict desktop custody:** `nektar up --wallet client` (refused on `memory`/`regtest`).
- **Treasury / hot-float flow (embedded wallet):**
  ```sh
  nektar wallet status
  nektar wallet address --qr
  nektar wallet fund --from-desktop --sats 25000
  nektar wallet sync
  nektar wallet fuel
  nektar wallet sweep --to-desktop --network main --confirm I_UNDERSTAND
  ```

### Health, Metrics & Self-Diagnosis

```sh
nektar doctor                 # human-readable [GREEN]/[AMBER]/[RED] checks + next commands
nektar doctor --json          # machine-readable JSON
nektar status                 # identity, balance, shards, policy hash, SpendGuard counters
nektar open                   # open paired Dashboard in browser (pass --start to auto-boot)
```

HTTP observability endpoints (port `18420`):

- `GET /v1/health` (`/health`, `/healthz`) — public liveness probe
- `GET /v1/metrics` — Prometheus 0.0.4 metrics (`nektar_uptime_seconds`, `nektar_ttfv_ms`, `nektar_fragments_total`, `nektar_unsettled_anchors`, `nektar_fee_balance_sats`, `nektar_spend_guard_*`, `nektar_restate_attached`, `nektar_hub_*`)
- `GET /v1/coldstart/health` — domain density, fill rate, organic sellers, and bounty taper state
- `GET /v1/verify/{token}` & `GET /v1/badge/{token}` — public receipt verifier (JSON/HTML + SVG badge)

---

## 4. Emergency Controls & Backup / Recovery

### Kill Switch

Immediately blocks all autonomous market spending:

```sh
# Via API (requires operator token if auth is enabled):
curl -X POST http://127.0.0.1:18420/v1/automation/kill \
  -H "Content-Type: application/json" \
  -d '{"enabled":true}'

# Or filesystem sentinel (checked on boot & runtime):
touch ~/.nektar/KILL          # global emergency stop (honored by testnet AND ~/.nektar/main)
touch ~/.nektar/main/KILL     # mainnet-only sentinel
```

### Backup & Restore

Back up the entire data directory (`~/.nektar` on testnet, `~/.nektar/main` on mainnet, or `/data` in Docker/Helm):
- `nektar.sqlite` (fragments, FTS5 index, anchors, receipts, deals, policies, MemRepo commits)
- `wallet/` (BRC-100 embedded wallet identity & UTXO state — absent in `client` mode)
- `objects/` (content-addressed CHIRP blobs & capsules)
- `http-token`, `operator-token`, `restate-internal-token`, `wallet-mode.json`
- `restate-data/` (Restate v1.8.1 durable workflow journals)

Stop the daemon (`nektar stop`) before taking a cold filesystem snapshot, or use SQLite online backup. On restore, start `nektar serve` / `nektar up` — pending anchor crash journals and surviving commercial obligations reconcile automatically on boot.

---

## 5. Hub Operations (`nektar hub`)

```sh
nektar hub up --auto-ip --network testnet
nektar hub up --domain hub.example.com --network main --confirm I_UNDERSTAND --fee-preset hybrid
nektar hub up --native --auto-ip --network main --confirm I_UNDERSTAND --fee-preset hybrid

nektar hub status             # works for both Docker Compose and --native systemd Hubs (~/.nektar-hub)
nektar hub logs
nektar hub down
```

See [DOCKING-AND-HUBS.md](DOCKING-AND-HUBS.md), [JOIN-TESTNET.md](JOIN-TESTNET.md), and [MAINNET-CANARY.md](MAINNET-CANARY.md) for full Hub and Mainnet runbooks.
