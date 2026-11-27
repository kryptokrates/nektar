# Join Mainnet — Nektar v0.1 (Pure-Go) Gate Checklist

> **Honesty Rule:** Proofs establish **recorded, unaltered, and derivable by block time** — never that a memory's content is true, and model identity is *declared*, not zkML-proven.

This checklist governs moving a v0.1 Pure-Go Nektar node or Hub from testnet to real BSV Mainnet sats. For the hands-on 5,000–10,000 sat canary walkthrough, see [MAINNET-CANARY.md](MAINNET-CANARY.md).

Every gate below is either **executable** (verified by a command you run and record) or **time/external** (cannot be compressed by code). An unchecked external gate means *not mainnet-ready*, regardless of green unit tests.

---

## 1. Executable Gates (Pure-Go v0.1)

| # | Gate | Command / Procedure | Pass Criterion |
|---|---|---|---|
| **E1** | Vet, build & race tests | `go vet ./... && go test -race -count=1 ./...` | All Go packages green with `-race` |
| **E2** | Static cross-compile | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/nektar` (and `GOOS=windows`) | Zero-CGO single binary builds cleanly |
| **E3** | Mainnet safety gate | `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND NEKTAR_HEADERS_SOURCE="woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2,arcade:https://arcade-v2-us-1.bsvblockchain.tech" NEKTAR_SETTLE_SPEC="broadcaster:main" nektar check-mainnet` | Exits `0` with `>= 2` distinct-origin non-circular PoW header witnesses (default 2-of-3 quorum) |
| **E4** | Mainnet refusal without confirm | `nektar serve --network main` (without `--confirm I_UNDERSTAND`) | Fails closed before opening wallet or HTTP listener |
| **E5** | Dual-witness & SPV hardening | `go test -race -v ./internal/chain/... ./internal/contracts/...` | REFUSED-BUMP foreign-chain rejection, tracker-outage 503 separation, and offline SPV verification pass |
| **E6** | Kill-switch drill | `POST /v1/automation/kill {"enabled":true}` or `touch ~/.nektar/main/KILL` (or global `touch ~/.nektar/KILL`) | New market trades refused immediately; SpendGuard caps remain enforced |
| **E7** | Backup & restore drill | Copy `~/.nektar/main` (`nektar.sqlite`, `wallet/`, tokens) to a fresh host and boot `nektar serve --network main --confirm I_UNDERSTAND` | Node resumes with identical `identityPub`, balance, shards, and re-announces |
| **E8** | Restate v1.8.1 durable plane | `nektar restate-up` (or Compose/Helm `restate:1.8.1` sidecar) + `NEKTAR_RESTATE_REQUIRED=1` | `nektar doctor` shows `restate` **GREEN** (`nektar_restate_attached 1`) |
| **E9** | Custody separation | Personal machine: `nektar up --network main --confirm I_UNDERSTAND --wallet client` (BSV Desktop on `:3321`, zero spending keys in Nektar). Headless 24/7 node: `--wallet embedded` with small SpendGuard hot float funded via `nektar wallet fund --from-desktop --sats 25000` and swept via `nektar wallet sweep --to-desktop --network main --confirm I_UNDERSTAND` | Confirmed custody mode in `nektar doctor` (`walletMode`) and `wallet-mode.json` |
| **E10** | Hub BRC-105 fee & replay gate | `go test -race ./internal/hub/...` + live `curl -i https://<hub>/hub/sync?cursor=0` | Returns HTTP `402 Payment Required` when fees enabled; double-spend / replay rejected |

---

## 2. Time / External Gates (Cannot Be Faked)

| # | Gate | Requirement | Why It Cannot Be Skipped |
|---|---|---|---|
| **X1** | **5-day testnet soak** | >= 120h continuous operation with live anchoring & deals; no wedged Restate workflows, no UTXO fuel exhaustion, no silent stalls | TTL edges, keep-alive intervals, proof compaction, and rate limits only surface on wall-clock time |
| **X2** | **>= 1 independent external Hub operator** | A second team deploys `deploy/hub/` (or `nektar hub up --native`) on their own box with their own keys and PoW header witnesses | Proves real latency, clock skew, NAT bbox relay, and independent SPV verification |
| **X3** | **Real Mainnet canary anchor + SPV** | Anchor 1 fragment on Mainnet (`nektar remember -shard canary -text "..." -anchor`), wait 1 block, verify `nektar recall -shard canary -spv` (`spvVerified: true`) | Exercises live US/EU BSV Broadcaster + dual-witness Chaintracks/WoC BUMP proof end-to-end |
| **X4** | **One real Mainnet trade watched to settlement** | Small-sats `want -> quote -> AtomicBEEF/BRC-29 -> BRC-369 key reveal -> DealReceipt` verified via `nektar verify -token <nektardr1...>` | Exercises live payment settlement, replay registry, and bilateral receipt verification |

---

## 3. Recommended Day-One Mainnet Configuration

```sh
# Isolated Mainnet data directory (~/.nektar/main by default)
export NEKTAR_NETWORK="main"
export NEKTAR_MAINNET_CONFIRM="I_UNDERSTAND"

# Custody: auto (uses BSV Desktop on :3321 if open, else embedded) | client | embedded
export NEKTAR_WALLET="auto"

# Dual-witness non-circular PoW headers (automatically uses 2-of-3 quorum on main when left unset)
# export NEKTAR_HEADERS_SOURCE="woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2,arcade:https://arcade-v2-us-1.bsvblockchain.tech"
# export NEKTAR_SETTLE_SPEC="broadcaster:main"

# Conservative SpendGuard caps (enforced fail-closed before every broadcast)
export NEKTAR_MAX_FEE_SATS_PER_TX=2000
export NEKTAR_MAX_SPEND_SATS_PER_DAY=10000
export NEKTAR_SETTLE_DEPTH=6

# Require Restate v1.8.1 durable workflows on live networks
export NEKTAR_RESTATE_REQUIRED=1

# Optional: pin off-box cold operator pubkey so hot agent cannot weaken spending policies
# export NEKTAR_OPERATOR_PUBKEY="02<66-hex-compressed-pubkey>"
```

Start in 1 command:

```sh
nektar up --network main --confirm I_UNDERSTAND
nektar doctor --network main
```

---

## 4. What Mainnet Guarantees — and Does Not

- **Guaranteed:** fail-closed `NEKTAR_MAINNET_CONFIRM` gate; SpendGuard per-tx and daily satoshi caps; dual-witness non-circular PoW header validation (no single-origin circular header trust); atomic SQLite payment replay protection; Restate v1.8.1 durable deal recovery across crashes; isolated `~/.nektar/main` data directory.
- **Not guaranteed:** counterparty uptime, BSV block cadence, external indexer availability, or objective truth of stored memory text (`TruthNote` always applies).
