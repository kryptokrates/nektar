<p align="center">
  <img src="logo.svg" width="96" alt="nektar"/>
</p>

# nektar — verifiable AI agent memory & knowledge market

**One verifiable brain for all your AI agents. Save what an agent learns, prove where answers came from, and trade knowledge under limits you approve — settled on BSV.**

[![ci](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml/badge.svg)](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

[1. Install (30s)](#1-install--start-30-seconds) · [2. Dock Any Agent (10s)](#2-dock-any-existing-running-agent-10-seconds) · [3. Deploy a Hub (1 Click)](#3-deploy-a-hub-node-1-click) · [Universal Adapter](docs/ADAPTERS.md) · [Trading Guide](docs/MARKET.md) · [Advanced / Dev](docs/ADVANCED-INSTALL.md)

---

## 1. Install & Start (30 seconds)

Everything runs from one official GHCR image (`ghcr.io/kryptokrates/nektar:latest`) with the Restate durable executor baked in — **zero Node.js, zero pnpm, zero `.env` editing, zero config.** Defaults to BSV **testnet** (free test coins) with a seeded `welcome` shard.

**macOS / Linux:**

```sh
curl -fsSL https://nektar.run/install.sh | sh
```

**Windows (PowerShell):**

```powershell
irm https://nektar.run/install.ps1 | iex
```

**Or raw Docker (no installer script):**

```sh
docker run -d --name nektar --restart unless-stopped \
  -p 127.0.0.1:18420:18420 -v nektar-data:/data \
  ghcr.io/kryptokrates/nektar:latest serve
```

> **What happens automatically on first boot:**
>
> 1. Starts one shared local daemon on `http://127.0.0.1:18420` (and auto-generates persistent mode-`0600` auth tokens in `/data`).
> 2. Prints a **Startup Card** with a 1-click browser pairing link (`nektar open` opens your Dashboard already unlocked — no 401 screens, no manual secrets).
> 3. Seeds the `welcome` shard immediately so your first `recall` answers in <1s, even offline.
> 4. Inside the Dashboard Quickstart: click **Run Recall + Receipt** to verify your first receipt and **1-Click Testnet Faucet** to get free test coins.

---

## 2. Dock Any Existing Running Agent (10 seconds)

MCP alone isn't enough for every agent — some agents don't speak MCP at all, and models can lazily skip calling memory tools. Nektar ships a **4-layer Universal Adapter** so _any_ running agent can dock immediately:

### Option A — Coding & Desktop AI Apps (1 command)

Auto-detects and non-destructively wires **Claude Code, Cursor, Codex, Hermes Agent, OpenClaw, Windsurf, VS Code, Zed, Cline, Continue, Goose, OpenCode, and Gemini/Antigravity** (including bundled L3 native plugins + host-portable hooks + live `recall {}` check):

```sh
nektar dock
```

### Option B — ANY Existing Running Agent with ZERO Code Changes (L0 LLM Proxy)

Already running **LangGraph, CrewAI, AutoGen, n8n, Dify, Open WebUI, Aider, or a custom Python/TypeScript agent**? Swap **one** environment variable — no MCP client or code edits required:

```sh
# Any OpenAI-compatible agent:
export OPENAI_BASE_URL="http://127.0.0.1:18420/v1/proxy/openai/v1"
# (If auth is active, run `nektar dock proxy` for the ready-to-paste composite key)

# Any Anthropic-compatible agent:
export ANTHROPIC_BASE_URL="http://127.0.0.1:18420/v1/proxy/anthropic"
```

The proxy deterministically recalls verified memory before each turn, streams responses untouched with zero lag, and asynchronously verifies any `nektarr1.*` receipts. See [docs/ADAPTERS.md](docs/ADAPTERS.md) for the full 4-layer matrix (L0 Proxy · L1 MCP Streamable HTTP + OAuth 2.1 + A2A + REST · L2 Portable Hooks · L3 Bundled Native Plugins).

---

## 3. Deploy a Hub Node (1 Click)

Once your personal agent is running, you can graduate to an independent public **Hub Node** (your agent + redundant market host pair + watch shims + **built-in Caddy Auto-TLS**) — either from the **Hub Node** tab in your Dashboard or with one command:

```sh
# Any VPS — automatic Let's Encrypt HTTPS with ZERO domain purchase (<ip>.sslip.io):
nektar hub up --auto-ip

# Or with your own domain:
nektar hub up --domain hub.yourdomain.com

# Or behind a home router / NAT (zero open ports via Cloudflare Tunnel):
nektar hub up --tunnel
```

### 1-Click Cloud & Home-Server Templates

| Platform                                 | Target                                 | Deploy Template                                                                                                                                 |
| ---------------------------------------- | -------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| **Hetzner / DigitalOcean / Vultr / AWS** | Full Hub Node (Auto-TLS + Auto-Faucet) | [deploy/hub/templates/cloud-init.yaml](deploy/hub/templates/cloud-init.yaml) (paste in User Data)                                               |
| **Coolify**                              | Full Hub Node (1-Click Compose)        | [deploy/hub/templates/coolify.compose.yaml](deploy/hub/templates/coolify.compose.yaml)                                                          |
| **Dokploy**                              | Full Hub Node (1-Click Compose)        | [deploy/hub/templates/dokploy.compose.yaml](deploy/hub/templates/dokploy.compose.yaml)                                                          |
| **Umbrel / CasaOS**                      | Home Server 1-Click App                | [umbrel/](deploy/hub/templates/umbrel/umbrel-app.yml) · [casaos.yaml](deploy/hub/templates/casaos.yaml)                                         |
| **Railway / Render / Fly.io**            | Always-on Cloud Agent (GHCR)           | [railway.json](deploy/hub/templates/railway.json) · [render.yaml](deploy/hub/templates/render.yaml) · [fly.toml](deploy/hub/templates/fly.toml) |
| **Kubernetes / k3s**                     | Production Helm Chart                  | [deploy/hub/helm/](deploy/hub/helm/Chart.yaml) · [deploy/agent/helm/](deploy/agent/helm/Chart.yaml)                                             |

---

## Everyday Commands

```sh
nektar open                        # open Dashboard (auto-paired)
nektar status                      # health, shards, balance & hints
nektar dock                        # wire all detected local AI apps
nektar dock proxy                  # print L0 LLM Proxy env vars for custom agents
nektar policy approve --preset both # approve starter trading policy (buyer + seller)
nektar update                      # pull latest GHCR image (keeps all memory & keys)
nektar hub up --auto-ip            # deploy a 1-click public Hub Node
```

---

## Honest Boundaries & Go Deeper

**Status: MVP.** Verification checks origin, inclusion, and integrity — proofs establish that a claim was _recorded, unaltered, and derivable_, never that it is _true_. Mainnet (`NEKTAR_NETWORK=main`) requires explicit `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND`, encrypted key passphrase, and SpendGuard caps.

- [Universal Agent Adapter (L0–L3)](docs/ADAPTERS.md) · [Trading Guide](docs/MARKET.md) · [Run a Testnet Hub](docs/JOIN-TESTNET.md) · [Go Mainnet](docs/JOIN-MAINNET.md)
- [Advanced Install (BSV Desktop `NEKTAR_WALLET=client`, Native Tarballs, Docker MCP Toolkit, Build from Source, Regtest)](docs/ADVANCED-INSTALL.md)
- [Architecture](docs/ARCHITECTURE.md) · [Operations](docs/OPERATIONS.md) · [Threat Model](docs/THREAT-MODEL.md) · [MIT License](LICENSE)
