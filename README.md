<p align="center">
  <img src="logo.svg" width="96" alt="nektar"/>
</p>

# nektar v0.4 — verifiable AI agent memory & knowledge market

**One static zero-CGO Go binary for all your AI agents. Save what an agent learns, encrypt shards with BRC-369 + Borg epochs, govern reads with AWS Cedar, ground answers with Google Mangle + `nektarr1.*` receipts, and trade knowledge on BSV.**

[![ci](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml/badge.svg)](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

---

## 1. Install & Start (Zero Dependencies, < 50 ms boot)

```sh
# macOS / Linux (static Go binary — zero Docker, zero Node.js; add `-- --docker` for GHCR)
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh
nektar serve
```

```powershell
# Windows PowerShell
irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1 | iex
nektar serve
```

```sh
# Go live on BSV Mainnet in 1 command (isolated ~/.nektar/main + terminal funding QR + dual-witness PoW)
nektar serve --network main --confirm I_UNDERSTAND
```

```sh
# Docker / GHCR (~25 MB Alpine image)
docker run -d --name nektar --restart unless-stopped \
  -p 127.0.0.1:18420:18420 -v nektar-data:/data \
  ghcr.io/kryptokrates/nektar:latest serve
```

`nektar serve` boots with **zero env vars** on `http://127.0.0.1:18420` (persistent `testnet` at `~/.nektar`, or isolated Mainnet at `~/.nektar/main` with `--network main --confirm I_UNDERSTAND`), prints a scannable funding QR on Mainnet, and opens a 1-click Dashboard (`nektar open`).

---

## 2. Dock Any Existing Agent (1 Click in Dashboard or `nektar dock`)

**What is docking?** Plugging Nektar into your existing AI apps so they share one encrypted, BSV-anchored memory and can buy/sell verified facts automatically — never clobbering sibling configs and reversible anytime (`nektar dock --uninstall`).

```sh
nektar dock          # auto-detects & wires Claude Code, Claude Desktop, Cursor, VS Code,
                     # Windsurf, Codex, Zed, Cline, Continue, Goose, OpenCode, Gemini, Hermes, OpenClaw
nektar dock proxy    # prints zero-code L0 LLM Memory Proxy exports for any OpenAI/Anthropic agent
```

Or click **Dock your AI app** inside the Dashboard (`nektar open`) for 1-click detection, docking, and a live test probe. See [docs/DOCKING-AND-HUBS.md](docs/DOCKING-AND-HUBS.md) and [docs/ADAPTERS.md](docs/ADAPTERS.md).

---

## 3. Run an Earning Public Hub Node (Docker or Zero-Docker Native)

```sh
# Mainnet Leaf Hub with automatic HTTPS + hybrid BRC-105 fees (connects to nektar.run Spine)
nektar hub up --auto-ip --network main --confirm I_UNDERSTAND --fee-preset hybrid
nektar hub up --domain hub.example.com --network main --confirm I_UNDERSTAND --fee-preset hybrid

# Zero-Docker native VPS mode (generates .env + systemd unit using this single Go binary!)
nektar hub up --native --auto-ip --network main --confirm I_UNDERSTAND --fee-preset hybrid

# Or test a Hub directly on this machine right now:
nektar serve --hub --hub-role leaf --fee-preset hybrid
```

One-click templates also live in `deploy/hub/templates/` (Coolify, Dokploy, Hetzner/DO Cloud-Init, CasaOS, Umbrel, Fly.io, Render, Railway) and `deploy/hub/helm/`. See [docs/DOCKING-AND-HUBS.md](docs/DOCKING-AND-HUBS.md) and [docs/MAINNET-CANARY.md](docs/MAINNET-CANARY.md).

---

## What Changed in v0.4 (Pure-Go Core)

Nektar v0.4 collapses the legacy 21-package TypeScript daemon and 12-container Hub stack into **one zero-CGO static Go binary** (`cmd/nektar`):

- **BSV Core & Official Broadcaster (native Go):** `go-sdk` (`transaction/broadcaster` dual-region US/EU official BSV Association Broadcaster, zero GorillaPool), `go-wallet-toolbox`, `lightwebinc/bcommon` (`bwallet` + `wirewallet` BRC-100 custody, `keyed` BRC-369 encryption, `chirp` BRC-167 storage, `headers` dual-witness PoW verification, `payee.Claims` 402 replay protection).
- **Policy & Reasoning (native Go):** `cedar-go` (AWS Cedar authorization + level isolation) and `mangle-go` (Google Mangle stratified Datalog grounding + bi-temporal AORG + `nektarr1.*` receipts).
- **Durable Execution & MCP (native Go):** `restatedev/sdk-go` (`BuyerDeal`, `SellerDeal`, `Custody`) and `modelcontextprotocol/go-sdk` (stdio + Streamable HTTP `/mcp` + OAuth 2.1).
- **Embedded Assets:** Pure-Go SQLite + FTS5 (`modernc.org/sqlite`), embedded Rúnar covenant artifacts, and embedded Svelte 5 dashboard.
- **Embedded Go Hub (`nektar serve -hub`):** 16 market lanes (`tm_nektar_<net>_0..f` + `ls_nektar`) + Lightweb Go overlay modules + native SSE `/events` — retiring MariaDB, `header-shim`, `watch-a`/`watch-b`, and `bbox-host`.

---

## CLI Commands

```sh
nektar serve          # Unified HTTP + MCP + OAuth + A2A + L0 Proxy + Dashboard + optional -hub
nektar mcp            # Stdio MCP server (transparently bridges to live daemon when running)
nektar open           # Open Dashboard in browser with 1-click pairing code
nektar dock           # Auto-detect & wire AI apps (Claude, Cursor, VS Code, Hermes, OpenClaw...)
nektar hub            # Deploy/manage a 1-click public Hub (up|status|logs|down|template|run)
nektar hook           # Portable lifecycle hooks (session-start|prefetch|verify-receipt)
nektar policy         # Operator trading policy presets (approve|list|revoke)
nektar wallet         # Embedded BRC-100 wallet (status|address|fund|send|import)
nektar remember       # Commit a fragment (-shard, -text, -group, -anchor)
nektar recall         # Recall Cedar-authorized, Borg-decrypted fragments (-shard, -query, -spv)
nektar anchor         # Anchor unanchored fragments via NektarAnchor covenant
nektar verify         # Verify a nektarr1.* receipt offline (-token)
nektar status         # Node identity, wallet balance, shards, policy hash, SpendGuard stats
nektar check-mainnet  # Validate mainnet confirmation + headers + settlement gates
```

---

## Honest Boundaries

Verification checks origin, inclusion, and integrity — proofs establish that a claim was _recorded, unaltered, and derivable_, never that it is _true_. Model identity is _declared_, not zkML-proven. Mainnet (`NEKTAR_NETWORK=main`) refuses to boot without `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` and enforces `SpendGuard` fail-closed caps before every broadcast.
