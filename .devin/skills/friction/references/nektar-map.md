# nektar v0.4 (Pure-Go) — Live Friction Map

The six journeys that decide whether an underdog network gets adopted,
what each pays in the three currencies (Time, Effort, Knowledge) in **v0.4 Pure-Go**,
and where every friction cliff was killed or bounded.

## Table of Contents

- [J1 · Stranger → announced operator](#j1--stranger--announced-operator)
- [J2 · Fresh instance → non-empty first run](#j2--fresh-instance--non-empty-first-run)
- [J3 · External AI client → first tool call](#j3--external-ai-client--first-tool-call)
- [J4 · Agent → settled deal](#j4--agent--settled-deal)
- [J5 · Broken thing → named cause](#j5--broken-thing--named-cause)
- [J6 · Ambient multi-app docking (one brain, many mouths)](#j6--ambient-multi-app-docking-one-brain-many-mouths)

## J1 · Stranger → announced operator

**First value:** signed card visible on the dir lane + `doctor` green. **Target:** <5 min.
**Artifact:** `cmd/nektar/hub_cmd.go`, `deploy/hub/{compose.yaml,install.sh,templates/*}`, `deploy/hub/helm/`.

| Step                     | Time    | Effort                                       | Knowledge | Triage                                                                                                                      |
| ------------------------ | ------- | -------------------------------------------- | --------- | --------------------------------------------------------------------------------------------------------------------------- |
| Install & launch Hub     | ~45 s   | `nektar hub up --auto-ip` (or `install.sh`)  | none      | **KILLED** — single Go binary (`serve -listen :18420 -hub`) replaces MariaDB, `header-shim`, `watch-a/b`, and `bbox-host`   |
| Public HTTPS / NAT       | <10 s   | `--auto-ip` (sslip.io), `--domain`, `--tunnel` | none     | **KILLED** — Caddy Auto-TLS & Cloudflare Tunnel built into `deploy/hub/compose.yaml` + 9 PaaS/Cloud-Init templates          |
| Testnet funding          | <5 s    | 1-click Dashboard faucet or `POST /v1/faucet`| none      | **MOVED** — BRC-100 embedded wallet auto-initializes; Hub faucet has per-IP + 24h per-address + global daily cap            |
| Verify card & metrics    | <5 s    | `GET /v1/health`, `/hub/health`, `/v1/metrics` | none    | **KEEP** — Prometheus 0.0.4 metrics + per-domain Hub density stats + periodic 24h announce refresh                          |

## J2 · Fresh instance → non-empty first run

**First value:** `recall {}` returns seeded `welcome` shard + checkable `nektarr1.*` receipt. **Target:** <10 s.
**Artifact:** `cmd/nektar/main.go`, `internal/memory/welcome.go`, `internal/server/{auth_tokens,metrics}.go`.

| Step                   | Time    | Effort                   | Knowledge | Triage                                                                                                                        |
| ---------------------- | ------- | ------------------------ | --------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Zero-dependency install| ~5 s    | 1-line `install.sh`/`.ps1` | none    | **KILLED** — single static zero-CGO Go binary (`--docker` opt-in); unified port `:18420` across binary, Docker, Compose, Helm |
| Zero-env boot          | <50 ms  | `nektar serve`           | none      | **KILLED** — persistent `testnet` at `~/.nektar/nektar.sqlite` (`-network memory -ephemeral` for RAM), auto-generated tokens  |
| First `recall {}` + TTFV | <10 ms | 1 call (zero args)      | none      | **KILLED** — FTS5 BM25 recall on seeded `welcome` shard; `nektar_ttfv_ms` tracked in `/v1/metrics`                            |
| Dashboard open         | <2 s    | `nektar open`            | none      | **KILLED** — mints single-use `/pair?code=` via `POST /v1/auth/pair-code`; zero manual token paste                            |

## J3 · External AI client → first tool call

**First value:** `recall` + `groundedAnswer` from any IDE, CLI, web connector, or raw OpenAI/Anthropic agent. **Target:** <30 s.
**Artifact:** `cmd/nektar/dock_hook.go`, `internal/server/{proxy,oauth,adapters,mcp}.go`, `integrations/{hermes,openclaw}`.

| Layer | Client / Path                                                                           | Triage                                                                                                                        |
| ----- | --------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| **L0**| Transparent LLM Memory Proxy (`/v1/proxy/openai/v1`, `/v1/proxy/anthropic`)             | **KILLED** — `X-Nektar-Token` or `nkt:<tok>:<key>`, FTS5 recall, Anthropic string+array `system`, zero-buffer SSE, async verify|
| **L1**| MCP Streamable HTTP (`/mcp`) + OAuth 2.1 PKCE + A2A (`/a2a`) + OpenAPI (`/v1/openapi.json`) | **KILLED** — full RFC 9728/8414 + PKCE S256 for hosted web connectors; `nektar mcp` bridges stdio→HTTP transparently        |
| **L2**| Portable Lifecycle Hooks (`nektar hook`) + `SKILL.md`                                   | **KILLED** — Claude Code (`SessionStart`, `UserPromptSubmit`, `Stop` with `stop_hook_active` guard) & Codex `notify --codex`  |
| **L3**| Native Memory Slots (`GET /v1/adapters/bundle/{hermes,openclaw}`)                       | **KILLED** — bundled inside binary/image; `nektar dock` writes by default (`--dry`, `--uninstall`, `--json`) across 13 clients|

## J4 · Agent → settled deal

**First value:** policy preset approved → `want → quote → AtomicBEEF/BRC-29 → BRC-369 key reveal → tainted commit`.
**Artifact:** `internal/market/{broker,workflows,policy}.go`, `internal/hub/liquidity.go`, `internal/store/market_store.go`.

| Failure Mode / Cliff                   | Fix Landed in v0.4                                                                                          |
| -------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| Ephemeral keys invalidated deals/peers | Persistent `node_market_key_v1` shared by `Engine` & `discovery.Service` across restarts                    |
| In-flight sales/claims lost on restart | `pending_sales`, `payment_claims`, `borg_groups`, `market_daily_spend`, and `bbox_cursor` persisted in SQLite|
| Unmatched wants invisible              | `unmatched_wants` demand ledger exposed in Dashboard + Hub `DomainDensityStats` in `/v1/metrics`            |
| Hub bounty drain / below-cost resale   | Mandatory `ProofBundle` + `HDP -> Leaf -> Merkle` check; sustainable resale `max(reward*12/10, 120)`        |
| Tainted purchased facts                | Purchased fragments tagged `Tainted: true`, `Class: "market:<domain>"` and blocked from self-resale         |
| Tight 150ms NAT bbox polling           | Adaptive backoff (`500ms` active / `3s` idle) with persisted per-host cursor                                |

## J5 · Broken thing → named cause

**First value:** operator self-diagnoses in one command (`nektar status` / `nektar doctor` / `/v1/metrics`).
**Artifact:** `cmd/nektar/lock*.go`, `internal/server/{tools_registry,metrics,http}.go`.

| Failure                                | Before                                   | Now in v0.4                                                                                   |
| -------------------------------------- | ---------------------------------------- | --------------------------------------------------------------------------------------------- |
| Concurrent daemon on same SQLite dir   | Silent DB corruption / lock race         | `instance.lock` with cross-platform PID liveness check + clear `nektar mcp`/`nektar open` hint|
| Unauth remote exposure                 | `APIToken==""` ran open on `0.0.0.0`     | Auto-generates mode-0600 `http-token` & `operator-token` on non-loopback or container boot    |
| Superseded receipt citations           | Stayed green after fact superseded       | `VerifyReceipt` checks `SupersededBy` → amber verdict + HTML warning + amber SVG badge        |
| Withholding hosts                      | Silent                                   | Surfaced in `ExecDoctor` (`host_integrity` check) and Dashboard `net.withholdings`            |

## J6 · Ambient multi-app docking (one brain, many mouths)

**First value:** multiple local AI apps (Claude Code, Cursor, VS Code, Hermes, OpenClaw, Codex) share one `:18420` daemon with zero lock collisions, live `recall {}` probe, and shared `daemon.json` (`token` + `authToken`).
**Artifact:** `internal/server/mcp_bridge.go`, `cmd/nektar/dock_hook.go`, `install/{nektar,nektar.ps1}`.
