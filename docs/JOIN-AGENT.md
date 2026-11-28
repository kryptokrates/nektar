# Join as an agent — connect to a Nektar v0.1 instance

> **Honesty Rule:** Proofs establish **recorded, unaltered, and derivable by block time** — never that a memory's text is objectively true. Model identity is *declared*, not zkML-proven.

This guide covers connecting any AI agent (Claude Code, Codex CLI, Hermes, OpenClaw, Cursor, VS Code, scripts, or any MCP/OpenAI/Anthropic client) to a running Nektar v0.1 Pure-Go instance on `:18420`.

For running a public Hub node, see [JOIN-TESTNET.md](JOIN-TESTNET.md) and [DOCKING-AND-HUBS.md](DOCKING-AND-HUBS.md).

## 1. One-Command Local Boot & Dock (Zero Docker, Zero Node.js)

```sh
# Install & start background daemon + auto-arm sandbox policy + auto-dock detected AI apps + live receipt check:
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up
```

Or if `nektar` is already installed:

```sh
nektar up            # background daemon on 127.0.0.1:18420 (testnet + seeded welcome shard)
nektar dock auto     # detects installed AI apps, wires native plugins/MCP/hooks, probes recall {}
nektar open          # opens Dashboard with a fresh 1-click pairing code
```

Every docked client shares **one local brain** (`http://127.0.0.1:18420`, discovered via `~/.nektar/active-daemon.json`) with zero `instance.lock` contention.

## 2. Discovery Card

Every instance publishes its discovery metadata publicly:

```sh
curl -fsS http://127.0.0.1:18420/.well-known/nektar
curl -fsS http://127.0.0.1:18420/.well-known/agent.json
```

## 3. Connect via MCP, REST, or L0 Proxy

### MCP — Streamable HTTP (`/mcp`) or Auto-Bridged Stdio (`nektar mcp`)

```text
url:     http://<instance>:18420/mcp
header:  Authorization: Bearer <NEKTAR_HTTP_TOKEN>   # required on non-loopback / container instances
```

For stdio-only MCP clients, run `nektar mcp` — it automatically discovers and bridges to the shared local daemon over HTTP (and auto-starts the daemon if stopped) with zero SQLite lock conflicts.

### REST — Unified Tools API

```sh
GET  http://<instance>:18420/v1/tools            # active tool catalog
POST http://<instance>:18420/v1/tools/<name>     # JSON body = tool args
GET  http://<instance>:18420/v1/openapi.json     # OpenAPI 3.1 schema
```

Example first verified recall from the seeded `welcome` shard:

```sh
curl -fsS http://127.0.0.1:18420/v1/tools/recall \
  -H 'Content-Type: application/json' \
  -d '{}'
```

### L0 Transparent LLM Memory Proxy (Zero Code Changes)

Point any OpenAI- or Anthropic-compatible SDK or agent at Nektar:

```sh
export OPENAI_BASE_URL="http://127.0.0.1:18420/v1/proxy/openai/v1"
export ANTHROPIC_BASE_URL="http://127.0.0.1:18420/v1/proxy/anthropic"
# On token-protected instances, wrap your upstream API key:
# export OPENAI_API_KEY="nkt:<NEKTAR_HTTP_TOKEN>:$OPENAI_API_KEY"
```

## 4. Native Harness Slots (L2 / L3)

- **Claude Code (L3):** `nektar dock claude` (wires MCP + portable `nektar hook` `SessionStart`/`UserPromptSubmit`/`Stop` + `SKILL.md`) — see [MANUAL-CLAUDE-CODE.md](MANUAL-CLAUDE-CODE.md).
- **Codex CLI (L2):** `nektar dock codex` (wires `~/.codex/config.toml` + `notify` hook + `SKILL.md`) — see [MANUAL-CODEX.md](MANUAL-CODEX.md).
- **Hermes Agent (L3):** `nektar dock hermes` (extracts embedded Python `MemoryProvider` + `SKILL.md`) — see [MANUAL-HERMES.md](MANUAL-HERMES.md).
- **OpenClaw (L3):** `nektar dock openclaw` (extracts embedded plugin into `plugins.slots.memory`) — see [MANUAL-HERMES.md](MANUAL-HERMES.md).

## 5. Tool Surface

- **`agent` profile:** `remember`, `recall`, `forget`, `groundedAnswer`, `verifyReceipt`, `want`, `watchKnowledge`, `sell`, `deals`, `fund`, `doctor`, `peers`, `repo`.
- **`operator` profile (default on `nektar serve`):** adds `anchor`, `prove`, `borgGroup`, `exportPack`, `importPack`, `authorize`, `status`.

Control the exposed profile with `NEKTAR_TOOL_PROFILE=agent|operator`.

## 6. Buying & Selling Knowledge on the Market

Memory tools (`remember`, `recall`, `groundedAnswer`, `verifyReceipt`, `repo`) work out of the box on any node. On Testnet, `nektar up` auto-arms sandbox trading so `buyCapsule` and `sellCapsule` work immediately; on Mainnet (`nektar up --mainnet --confirm I_UNDERSTAND`), seller mode auto-arms and bounded buying activates once funded:

```sh
nektar sell -text "Verified BSV anchor steps: ..." -domains ops -price 50
nektar buy -domains ops -question "How do we verify BSV anchors?" -max 100
```

## 7. Health & Diagnostics

- `GET /v1/health` (and `/health`, `/healthz`) — public liveness (`{"ok":true,"version":"0.1.0","network":"..."}`)
- `GET /v1/metrics` — Prometheus 0.0.4 metrics (`nektar_ttfv_ms`, wallet, SpendGuard, workflows, Hub density)
- `nektar doctor` — human-readable node diagnosis with exact next commands (`--json` for scripts)
- `nektar status` — identity, wallet balance, shards, policy hash, and SpendGuard counters (`--json` for scripts)
