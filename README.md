<p align="center">
  <img src="logo.svg" width="96" alt="nektar"/>
</p>

# nektar v0.4 — verifiable AI agent memory & knowledge market

**One static Go binary for all your AI agents. Save what an agent learns, encrypt shards with BRC-369 + Borg epochs, govern reads with AWS Cedar, ground answers with Google Mangle + `nektarr1.*` receipts, and trade knowledge on BSV.**

[![ci](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml/badge.svg)](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

---

## What Changed in v0.4 (Radical Stack Simplification)

Nektar v0.4 collapses the legacy 21-package (~58,000 LOC) TypeScript daemon and 12-container Hub stack into **one zero-CGO static Go binary** (`cmd/nektar`) built directly on the upstream Go ecosystem:

- **BSV Core & Lightweb Stack (native Go):** `github.com/bsv-blockchain/go-sdk`, `go-chaintracks`, `go-overlay-services`, and `github.com/lightwebinc/{bcommon, bfinger, bbox, blogs, borg, bsecret, bstore, overlay-bridge}` (`bwallet` + `wirewallet` BRC-100 custody, `keyed` BRC-369 encryption, `chirp` BRC-167 content-addressed storage, `headers` PoW verification, `publish` settlement, `payee.Claims` 402 replay protection, and `testchain`).
- **Policy & Reasoning (native Go, zero WASM/JVM):** `github.com/cedar-policy/cedar-go` (AWS Cedar authorization + level isolation) and `codeberg.org/TauCeti/mangle-go` (Google Mangle stratified Datalog grounding + bi-temporal AORG + `nektarr1.*` receipts).
- **Durable Execution & MCP (native Go):** `github.com/restatedev/sdk-go` (`BuyerDeal`, `SellerDeal`, `Custody` Virtual Object) and `github.com/modelcontextprotocol/go-sdk` (stdio + Streamable HTTP `/mcp`).
- **Embedded Assets (zero runtime deps):** Pure-Go SQLite (`modernc.org/sqlite`), embedded Rúnar Bitcoin Script covenant artifacts (`internal/contracts/artifacts/*.json`), and embedded Svelte 5 dashboard (`internal/server/ui_dist/`).
- **Embedded Go Hub (`nektar serve -hub` or `nektar hub`):** Mounts Nektar's 16 market lanes (`tm_nektar_<net>_0..f` + `ls_nektar`) alongside Lightweb's Go overlay modules (`bfinger`, `bbox`, `blogs`, `borg`, `bsecret`, `bstore`) with native SSE `/events` push — retiring MariaDB/MySQL, `header-shim`, `watch-a`, `watch-b`, and `bbox-host`.

---

## Quick Start

### Build from source (Go 1.27+, zero CGO)

```sh
go build -o nektar ./cmd/nektar
./nektar serve
```

### Run with Docker (~35 MB Alpine image)

```sh
docker run -d --name nektar --restart unless-stopped \
  -p 127.0.0.1:8080:8080 -v nektar-data:/data \
  ghcr.io/kryptokrates/nektar:latest serve
```

### Run a Full Hub Node (Agent + Overlay Hub + Native SSE)

```sh
./nektar serve -hub -listen :8080
# or via Docker Compose (2 containers instead of 12):
docker compose -f deploy/hub/compose.yaml up -d
```

---

## Dock Any Agent

```sh
nektar dock
```

- **Stdio MCP (Claude Code, Claude Desktop, Cursor, Codex):** `nektar mcp`
- **Streamable HTTP MCP:** `http://127.0.0.1:8080/mcp`
- **L0 Zero-Code OpenAI Memory Proxy:** `OPENAI_BASE_URL=http://127.0.0.1:8080/v1/proxy/openai/v1`
- **L0 Zero-Code Anthropic Memory Proxy:** `ANTHROPIC_BASE_URL=http://127.0.0.1:8080/v1/proxy/anthropic`
- **A2A Agent Card & JSON-RPC:** `GET /.well-known/agent.json`, `POST /a2a`
- **Svelte 5 Dashboard:** `http://127.0.0.1:8080/dashboard`

---

## CLI Commands

```sh
nektar serve          # Unified HTTP + MCP + A2A + L0 Proxy + Dashboard + optional Hub/Restate
nektar mcp            # Stdio MCP server
nektar hub            # Standalone embedded Go Overlay Hub (tm_nektar_* + Lightweb modules + SSE)
nektar remember       # Commit a fragment (-shard, -text, -group, -anchor)
nektar recall         # Recall Cedar-authorized, Borg-decrypted fragments (-shard, -query, -spv)
nektar anchor         # Anchor unanchored fragments via NektarAnchor covenant
nektar verify         # Verify a nektarr1.* receipt offline (-token)
nektar status         # Node identity, wallet balance, shards, policy hash, SpendGuard stats
nektar check-mainnet  # Validate mainnet confirmation + headers + settlement gates
nektar dock           # Print ready-to-paste MCP / L0 Proxy / A2A connection config
```

---

## Honest Boundaries

Verification checks origin, inclusion, and integrity — proofs establish that a claim was _recorded, unaltered, and derivable_, never that it is _true_. Model identity is _declared_, not zkML-proven. Mainnet (`NEKTAR_NETWORK=main`) refuses to boot without `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` and enforces `SpendGuard` fail-closed caps before every broadcast.
