# `NEKTAR_*` Environment Contract — v0.1 Pure-Go

Every variable below is read directly by the single zero-CGO Go binary (`cmd/nektar`) or its embedded packages (`internal/*`). None are required for zero-env local boot (`nektar up` / `nektar serve` default to `testnet`, `127.0.0.1:18420`, `~/.nektar`, and desktop-first `--wallet auto`).

> **Load-Bearing Safety Gates:** `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` is required before any `main` network wallet or daemon will start. SpendGuard caps (`NEKTAR_MAX_FEE_SATS_PER_TX`, `NEKTAR_MAX_SPEND_SATS_PER_DAY`) are enforced fail-closed before every broadcast.

---

## Core Runtime & Custody

| Variable | Default | Purpose | Readers |
|---|---|---|---|
| `NEKTAR_NETWORK` | `testnet` | Target BSV network (`memory` \| `regtest` \| `testnet` \| `main`) | `cmd/nektar/{main,cli_serve,cli_up,cli_memory,cli_ops,hub_cmd,hub_deploy,repo_cmd}.go` |
| `NEKTAR_MAINNET_CONFIRM` | *(unset)* | Must be `I_UNDERSTAND` to boot or spend on `main` | `cmd/nektar/{main,cli_serve,cli_up,cli_mcp,cli_memory,cli_ops,hub_deploy,wallet_ops}.go`, `internal/chain/port.go` |
| `NEKTAR_DATA_DIR` | `~/.nektar` (`~/.nektar/main` on `main`) | Persistent SQLite, BRC-100 `wallet/`, CHIRP objects, and auth tokens | `cmd/nektar/{main,cli_serve_wiring,hub_cmd,hub_deploy}.go` |
| `NEKTAR_EPHEMERAL` | `0` | Set `1` to run with in-memory SQLite (`:memory:`) | `cmd/nektar/{main,cli_serve,cli_mcp}.go` |
| `NEKTAR_WALLET` | `auto` | Custody mode: `auto` (BSV Desktop `:3321` if unlocked, else `embedded`), `client`, or `embedded` | `cmd/nektar/{main,cli_serve,cli_up}.go`, `internal/chain/port.go` |
| `NEKTAR_WALLET_WIRE_URL` | `http://127.0.0.1:3321` | Loopback BRC-100 wire URL for BSV Desktop / Metanet Desktop | `cmd/nektar/{main,cli_serve,cli_up,wallet_ops}.go` |
| `NEKTAR_WALLET_PROFILE` | `standard` | Wallet UTXO pool profile | `cmd/nektar/main.go` |
| `NEKTAR_EXPOSE_WALLET_WIRE` | `0` | Opt-in loopback-only BRC-100 wire endpoint at `/v1/wallet/wire/` | `cmd/nektar/cli_serve.go`, `internal/server/wallet_http.go` |
| `NEKTAR_KILL` | `0` | Emergency kill switch (`1`/`true`/`yes`); also triggered by `<dataDir>/KILL` or global `~/.nektar/KILL` | `cmd/nektar/main.go` |
| `NEKTAR_ISOLATED_DAEMON` | `0` | Set `1` in tests to skip updating `~/.nektar/active-daemon.json` | `internal/server/mcp_bridge.go` |

## HTTP, Auth, Proxy & MCP

| Variable | Default | Purpose | Readers |
|---|---|---|---|
| `NEKTAR_HTTP_PORT` | `18420` | Unified HTTP / MCP / Dashboard / Hub listen port | `cmd/nektar/{main,cli_serve,dock_hook,hub_deploy}.go` |
| `NEKTAR_PUBLIC_URL` | *(empty)* | Public HTTPS base URL for direct A2A trades & verification links (empty = NAT bbox relay) | `cmd/nektar/{main,cli_serve,hub_deploy}.go` |
| `NEKTAR_VERIFY_BASE` | `<NEKTAR_PUBLIC_URL>/v1/verify` | Base URL embedded in `nektarr1.*` receipt verify footers | `cmd/nektar/main.go` |
| `NEKTAR_PUBLIC_VERIFY_RELAY` | *(empty)* | Optional public verify relay URL for loopback nodes (`off` to disable) | `cmd/nektar/main.go` |
| `NEKTAR_TRUST_PROXY` | `0` | Set `1` behind Caddy/Traefik/Ingress to trust `X-Forwarded-Proto`/`Host` | `cmd/nektar/hub_deploy.go`, `internal/server/{http,security_helpers}.go` |
| `NEKTAR_HTTP_TOKEN` / `NEKTAR_API_TOKEN` | *(auto-generated on container/non-loopback)* | Bearer token for `/mcp`, `/v1/tools/*`, `/v1/proxy/*`, and Dashboard | `cmd/nektar/{cli_serve,dock_hook,hub_deploy}.go`, `internal/server/oauth.go` |
| `NEKTAR_OPERATOR_TOKEN` | *(auto-generated on container/non-loopback)* | Higher-privilege token for policies, approvals, pairing codes, host joins, and repo admin mutations | `cmd/nektar/{cli_serve,cli_ops,hub_deploy,repo_cmd,wallet_cmd}.go` |
| `NEKTAR_OPERATOR_PUBKEY` | *(empty)* | Pinned 33-byte compressed cold pubkey required to sign operator spending policies | `cmd/nektar/{main,vault_cmd}.go`, `internal/market/policy.go` |
| `NEKTAR_CONTAINER` | `0` (`1` in Dockerfile) | Forces non-loopback bind and auto-generated mode-0600 auth tokens | `internal/server/auth_tokens.go` |
| `NEKTAR_TOOL_PROFILE` | `operator` (`agent` in `mcp --direct`) | Exposed MCP/REST tool set (`agent` \| `operator`) | `cmd/nektar/{cli_serve,cli_mcp}.go`, `internal/server/tools_registry.go` |
| `NEKTAR_TOOL_PRICES` | *(empty)* | Optional BRC-105 HTTP 402 prices per tool (`recall=5,remember=10`) | `cmd/nektar/cli_serve.go`, `internal/server/security_helpers.go` |
| `NEKTAR_MCP_NO_AUTOSTART` | `0` | Set `1` to prevent `nektar mcp` from auto-spawning a shared background daemon | `cmd/nektar/cli_mcp.go` |
| `NEKTAR_OPENAI_UPSTREAM` | `https://api.openai.com` | Upstream URL for L0 OpenAI memory proxy (`/v1/proxy/openai/v1`) | `cmd/nektar/cli_serve.go` |
| `NEKTAR_ANTHROPIC_UPSTREAM` | `https://api.anthropic.com` | Upstream URL for L0 Anthropic memory proxy (`/v1/proxy/anthropic`) | `cmd/nektar/cli_serve.go` |
| `NEKTAR_MODEL_PROVIDER` / `NEKTAR_MODEL_RELEASE` / `NEKTAR_MODEL_ID` | `nektar` / `0.1.0` | Declared model identity attached to receipts (declared, not zkML-proven) | `cmd/nektar/main.go` |

## Chain, PoW Headers & SpendGuard

| Variable | Default | Purpose | Readers |
|---|---|---|---|
| `NEKTAR_HEADERS_SOURCE` | Auto (`woc:test` on testnet; dual-witness `woc:main` + Chaintracks v2 on `main`) | Witnessed PoW header sources (`>=2` distinct non-circular sources required on `main`) | `cmd/nektar/{main,cli_memory,hub_cmd,hub_deploy,repo_cmd}.go` |
| `NEKTAR_HEADER_ANCHOR` | *(empty)* | Shorthand `anchor:<url>` header witness source | `cmd/nektar/main.go` |
| `NEKTAR_CHAIN_SPEC` | Auto (`woc:test` / `woc:main`) | Chain query endpoint spec | `cmd/nektar/{main,cli_memory,hub_cmd}.go` |
| `NEKTAR_SETTLE_SPEC` | Auto (`broadcaster:testnet` / dual-region US+EU `broadcaster:main`) | Transaction broadcast/settlement endpoint spec | `cmd/nektar/{main,cli_memory,hub_cmd,hub_deploy}.go`, `internal/chain/port_tx.go` |
| `NEKTAR_SETTLE_DEPTH` | Auto (`2` testnet / `6` main) | Required block confirmation depth for value-final settlement | `cmd/nektar/main.go`, `internal/server/metrics.go` |
| `NEKTAR_FEE_RATE_SAT_KB` | Default fee rate | Transaction fee rate override in sats/KB | `cmd/nektar/main.go` |
| `NEKTAR_MAX_FEE_SATS_PER_TX` / `NEKTAR_MAX_FEE_PER_TX` | `10000` | SpendGuard per-transaction max fee cap (sats) | `cmd/nektar/main.go` |
| `NEKTAR_MAX_SPEND_SATS_PER_DAY` / `NEKTAR_MAX_SPEND_PER_DAY` | `100000` | SpendGuard rolling 24h max spend cap (sats) | `cmd/nektar/main.go` |
| `NEKTAR_MIN_BROADCAST_INTERVAL_MS` | `0` | Minimum milliseconds between broadcasts | `cmd/nektar/main.go` |
| `NEKTAR_ARCADE_KEY` / `NEKTAR_WOC_KEY` / `NEKTAR_RPC_USER` / `NEKTAR_RPC_PASS` | *(empty)* | Optional API/RPC credentials for broadcaster, WoC, or regtest node | `cmd/nektar/main.go` |
| `NEKTAR_ANCHOR_CADENCE_SEC` | `300` | Periodic `AnchorCadence` batch interval in seconds | `cmd/nektar/cli_serve_wiring.go` |

## Embedded Durable Execution (`go-workflows` SQLite WAL)

Durable deals (`BuyerDealWorkflow`, `SellerDealWorkflow`, `StandingOrderWorkflow`, `EscrowGuardianWorkflow`) run inside the single Go binary backed by `<NEKTAR_DATA_DIR>/workflows.sqlite` (SQLite WAL, zero external sidecars or ports required).

| Variable | Default | Purpose | Readers |
|---|---|---|---|
| `NEKTAR_FAST_DEALS` | `0` | Optional `1` to run un-suspended 0-conf P2P deals on the synchronous fast path while preserving SQLite obligations | `internal/market/broker_buyer.go` |

## Market, Discovery, Jev Matcher & Embedded Hub

| Variable | Default | Purpose | Readers |
|---|---|---|---|
| `NEKTAR_MARKET_HOSTS` | Network seed hosts | Comma-separated Hub URLs to join (optional) | `cmd/nektar/{main,cli_serve}.go`, `internal/discovery/directory.go` |
| `NEKTAR_SEED_DOMAINS` | `ops,bsv,agents` (`off` to disable) | Domains auto-seeded from verified CHIRP packs at boot | `cmd/nektar/cli_serve_wiring.go` |
| `NEKTAR_BBOX_OFFICE` / `NEKTAR_HANDLE` | `default` / *(empty)* | NAT bbox office name and optional bfinger handle | `cmd/nektar/cli_serve.go` |
| `NEKTAR_ALLOW_LOCAL_PEERS` | `0` | Opt-in loopback peer HTTP outbound for local multi-node testing | `internal/core/safehttp.go` |
| `NEKTAR_EMBED_HUB` | `0` | Set `1` (or pass `-hub`) to mount embedded Go Overlay Hub at `/hub/` | `cmd/nektar/cli_serve.go` |
| `NEKTAR_HUB_DIR` | `~/.nektar-hub` | Directory for `nektar hub up|status|logs|down` configs | `cmd/nektar/hub_deploy.go`, `internal/server/http_market.go` |
| `NEKTAR_HUB_ROLE` | `leaf` | Spine-and-Leaf Hub role (`leaf` \| `spine`) | `cmd/nektar/{cli_serve,hub_cmd,hub_deploy}.go`, `internal/hub/fees.go` |
| `NEKTAR_HUB_FEE_PRESET` | `free` (`hybrid` on `main`) | Hub BRC-105 fee preset (`free` \| `lightbsv_egress` \| `hybrid`) | `cmd/nektar/{cli_serve,cli_serve_wiring,hub_cmd,hub_deploy}.go`, `internal/hub/fees.go` |
| `NEKTAR_HUB_TRANSPORT` / `NEKTAR_UPSTREAM_SPINES` / `NEKTAR_CATCHUP_PEERS` | `relay` / network spine | Hub-to-Hub BRC-149 object plane relay and BRC-24 catch-up peers | `cmd/nektar/{cli_serve,cli_serve_wiring,hub_cmd,hub_deploy}.go`, `internal/hub/{hub,replay_catchup}.go` |
| `NEKTAR_SPINE_PEER_PUBS` / `NEKTAR_HUB_DOMAINS` / `NEKTAR_HUB_ENGINE_LISTEN` | *(empty)* | Bilateral Spine peer pubkeys, subscribed Leaf domains, bare-origin engine listener | `cmd/nektar/{cli_serve_wiring,hub_cmd}.go`, `internal/hub/fees.go` |
| `NEKTAR_HUB_SUBMIT_*_SATS` / `NEKTAR_HUB_BBOX_SEND_SATS` / `NEKTAR_HUB_EGRESS_*` / `NEKTAR_HUB_LOOKUP_PRICES` | Preset defaults | Per-valve BRC-105 Hub fee overrides | `internal/hub/fees.go` |
| `NEKTAR_HUB_RESALE_SATS` / `NEKTAR_FAUCET_DAILY_CAP` | `120` / `50000` | Hub liquidity resale floor and daily testnet faucet cap | `internal/hub/liquidity.go` |
| `NEKTAR_JEV_MODE` / `NEKTAR_JEV_BASE_URL` / `NEKTAR_JEV_MODEL` / `NEKTAR_JEV_PRIVACY` / `NEKTAR_JEV_TIMEOUT_MS` / `NEKTAR_JEV_ALLOWED_HOSTS` | `gated` / local / `disclosable_preview` / `2500` | Optional self-hosted open-weight Jev System One evaluator backing semantic market matching AND JevGraph ontology extraction/alignment (degrades to deterministic ranking / `jev_offline` review candidates unless `-ontology-strict`) | `cmd/nektar/main.go`, `internal/jev/types.go`, `internal/jevgraph` |

---

## NMU-1 Commodity Standard (Zero-Env Defaults)

NMU-1 requires **zero new environment variables** — all commodity pricing, grading, and contract endpoints operate out-of-the-box with deterministic integer arithmetic:

- **Canonical Unit & Quote:** 1 Verified Triple (deduplicated via `TagTripleV1`, restricted to predicates declared in the pinned `OntologyManifest`, capped at 8 per `(subject, predicate)` pair). Quotes normalize to integer `satsPer1kUnits` (milli-sats per triple, zero floats).
- **Deterministic Grades:** `C` (PII-scrubbed text) → `B` (distilled, ontology-pinned, Mangle-consistent triples) → `A` (Grade B + verbatim L1 MPE `EdgesRoot` + active `NektarCheckpointBond`) → `S` (Grade A + authenticated `±60s` replay-protected SSE supersession feed at `GET /v1/market/feeds/{shard}`).
- **Multi-Mode Contracts on Any Listing:** `spot` (`-price`, optional `-per-unit`), `per_query` metered recall (`-query-price`, `POST /v1/market/query` over BRC-105 HTTP 402), and `subscription` Shard Feeds (`-sub-price` / `-sub-days`, `POST /v1/market/subscriptions` + `GET /v1/market/feeds/{shard}`).
- **Commodity Ticker:** `GET /v1/market/ticker`, CLI `nektar ticker`, MCP `marketContract` (`action: "ticker"|"subscriptions"|"revokeSubscription"`), and the embedded Dashboard Commodity Ticker strip.

