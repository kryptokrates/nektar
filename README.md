<p align="center">
  <img src="logo.svg" width="96" alt="nektar"/>
</p>

# nektar v0.4 — verifiable AI agent memory & knowledge market

**One static zero-CGO Go binary for all your AI agents. Save what an agent learns, encrypt shards with BRC-369 + Borg epochs, govern reads with AWS Cedar, ground answers with Google Mangle + `nektarr1.*` receipts, and trade knowledge on BSV.**

[![ci](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml/badge.svg)](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

---

## 1. Install, Start & Auto-Dock in 25 Seconds (Zero Docker, Zero Node.js)

```sh
# macOS & Linux — installs static Go binary, starts background daemon + Restate,
# auto-docks Claude Code, Codex CLI, Hermes, OpenClaw, Cursor & VS Code, and verifies a live receipt!
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up
```

```powershell
# Windows PowerShell — same 1-command zero-to-docked flow (auto-detects WSL2 for Restate!)
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Up
```

```sh
# Go live on BSV Mainnet in 1 command (isolated ~/.nektar/main + terminal funding QR + dual-witness PoW)
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up --mainnet --confirm I_UNDERSTAND
```

Add `--install-service` (`-InstallService` on Windows) to keep Nektar running automatically across reboots. Stop anytime with `nektar stop`.

---

## 2. Step-by-Step Guides by AI Agent

Every docked app shares **one local brain** (`http://127.0.0.1:18420`, auto-discovered across both Mainnet and Testnet via `~/.nektar/active-daemon.json`) with zero SQLite lock contention:

| AI Agent / Runtime | Integration Level | What `nektar up` Wires Automatically | Guide |
|---|---|---|---|
| **Claude Code** | **L3** (MCP + Hooks + Skill) | `~/.claude.json` + `SessionStart`/`UserPromptSubmit`/`Stop` receipt verifier hooks + `SKILL.md` | [MANUAL-CLAUDE-CODE.md](docs/MANUAL-CLAUDE-CODE.md) |
| **Codex CLI** | **L2** (MCP + Notify + Skill) | `~/.codex/config.toml` (`[mcp_servers.nektar]` + top-level `notify`) + `SKILL.md` | [MANUAL-CODEX.md](docs/MANUAL-CODEX.md) |
| **Hermes Agent** | **L3** (Native MemoryProvider) | Extracts embedded Python plugin + `SKILL.md` + sets `memory.provider: nektar` | [MANUAL-HERMES.md](docs/MANUAL-HERMES.md) |
| **OpenClaw** | **L3** (Native Memory Slot) | Extracts embedded plugin + wires `plugins.slots.memory` in `openclaw.json` | [MANUAL-HERMES.md](docs/MANUAL-HERMES.md) |
| **Claude Desktop, Cursor, VS Code, Windsurf, Zed, Cline, Gemini CLI** | **L1** (MCP + Universal Skill) | Non-clobbering MCP config + `~/.agents/skills/nektar-memory/SKILL.md` | [DOCKING-AND-HUBS.md](docs/DOCKING-AND-HUBS.md) |
| **Any OpenAI / Anthropic App** | **L0** (Zero-Code Proxy) | `OPENAI_BASE_URL=http://127.0.0.1:18420/v1/proxy/openai/v1` (`nektar dock proxy`) | [DOCKING-AND-HUBS.md](docs/DOCKING-AND-HUBS.md) |

Reversible anytime with `nektar dock --uninstall`, or click **Dock your AI app** in the Dashboard (`nektar open`) for 1-click docking of every client including Codex, Hermes, and OpenClaw.

---

## 3. Earn Sats or Run a Public Hub Node (60 Seconds)

### Knowledge Sellers (Earn BSV from Verified Memory)
```sh
nektar earn                                      # list open Hub bounties & your local shards
nektar earn --bounty bounty-ops --shard notes    # anchor & submit for instant BSV AtomicBEEF payout
nektar policy approve --preset both              # enable autonomous 24/7 peer buying & selling
nektar sell -shard notes -domains ops -price 100 # auto-quote incoming wants on the BSV market
```

### Public Hub Operators (Spine-and-Leaf BRC-105 Fees)
```sh
# Docker + automatic HTTPS (verifies /hub/v1/health & prints Mainnet funding QR):
nektar hub up --auto-ip --network main --confirm I_UNDERSTAND --fee-preset hybrid
nektar hub up --domain hub.example.com --network main --confirm I_UNDERSTAND --fee-preset hybrid

# Zero-Docker native VPS mode (generates .env + systemd unit using this single Go binary!):
nektar hub up --native --auto-ip --network main --confirm I_UNDERSTAND --fee-preset hybrid
```

One-click templates live in `deploy/hub/templates/` (Coolify, Dokploy, Hetzner/DO Cloud-Init, CasaOS, Umbrel, Fly.io, Render, Railway) and `deploy/hub/helm/`. See [docs/DOCKING-AND-HUBS.md](docs/DOCKING-AND-HUBS.md), [docs/COLD-START.md](docs/COLD-START.md), and [docs/MAINNET-CANARY.md](docs/MAINNET-CANARY.md).

---

## What Changed in v0.4 (Pure-Go Core)

Nektar v0.4 collapses the legacy 21-package TypeScript daemon and 12-container Hub stack into **one zero-CGO static Go binary** (`cmd/nektar`):

- **BSV Core & Official Broadcaster (native Go):** `go-sdk` (`transaction/broadcaster` dual-region US/EU official BSV Association Broadcaster, zero GorillaPool), `go-wallet-toolbox`, `lightwebinc/bcommon` (`bwallet` + `wirewallet` BRC-100 custody, `keyed` BRC-369 encryption, `chirp` BRC-167 storage, `headers` dual-witness PoW verification, `payee.Claims` 402 replay protection).
- **Policy & Reasoning (native Go):** `cedar-go` (AWS Cedar authorization + level isolation) and `mangle-go` (Google Mangle stratified Datalog grounding + bi-temporal AORG + `nektarr1.*` receipts).
- **Durable Execution & MCP (native Go):** `restatedev/sdk-go` (`BuyerDeal`, `SellerDeal`, `Custody`, `AnchorCadence`) and `modelcontextprotocol/go-sdk` (stdio auto-bridge + Streamable HTTP `/mcp` + OAuth 2.1).
- **Embedded Everything:** Pure-Go SQLite + FTS5 (`modernc.org/sqlite`), embedded Rúnar covenant artifacts, embedded Svelte 5 dashboard, and embedded Hermes/OpenClaw/`SKILL.md` adapters.
- **Embedded Go Hub (`nektar serve --hub`):** 16 market lanes (`tm_nektar_<net>_0..f` + `ls_nektar`) + Lightweb Go overlay modules + native SSE `/events`.

---

## CLI Commands

```sh
nektar up             # Start background daemon + Restate + auto-dock AI apps + live receipt check
nektar stop           # Gracefully stop the background Nektar daemon
nektar serve          # Run foreground HTTP + MCP + OAuth + A2A + L0 Proxy + Dashboard + optional --hub
nektar mcp            # Stdio MCP bridge (auto-discovers & auto-starts shared daemon; zero lock contention)
nektar open           # Open Dashboard in browser with 1-click pairing code
nektar dock           # Auto-detect & wire AI apps (Claude, Codex, Hermes, OpenClaw, Cursor, VS Code…)
nektar earn           # Inspect open Hub bounties & earn instant BSV payouts from anchored shards
nektar hub            # Deploy/manage a 1-click public Hub (up|status|logs|down|template|run)
nektar hook           # Portable lifecycle hooks (session-start|prefetch|verify-receipt)
nektar policy         # Operator trading policy presets (approve|list|revoke)
nektar wallet         # Embedded BRC-100 wallet (status|address [--qr]|sync|fund|send|sweep|fuel)
nektar remember       # Commit a fragment (-shard, -text, -group, -anchor)
nektar recall         # Recall Cedar-authorized, Borg-decrypted fragments (-shard, -query, -spv)
nektar anchor         # Anchor unanchored fragments via NektarAnchor covenant
nektar verify         # Verify a nektarr1.* receipt offline (-token)
nektar doctor         # Check memory, wallet, Restate, policy, peers & cold-start density health
nektar check-mainnet  # Validate mainnet confirmation + headers + settlement gates
```

---

## Honest Boundaries

Verification checks origin, inclusion, and integrity — proofs establish that a claim was _recorded, unaltered, and derivable_, never that it is _true_. Model identity is _declared_, not zkML-proven. Mainnet (`--network main`) refuses to boot without `--confirm I_UNDERSTAND` (`NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND`) and enforces `SpendGuard` fail-closed caps before every broadcast.
