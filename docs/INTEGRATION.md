# Using nektar from AI Tools (MCP, Hooks, Native Plugins & OpenAI Proxy)

`nektar` is an official `modelcontextprotocol/go-sdk` MCP server, native Hermes/OpenClaw memory plugin host, and zero-code OpenAI/Anthropic L0 memory proxy — compiled into a single static zero-CGO Go binary (`cmd/nektar`, zero external sidecars).

## 1. Fastest Setup (`nektar up` / `nektar dock`)

```sh
nektar up                     # starts shared background daemon + auto-docks installed AI apps
nektar dock                   # auto-detects & wires Claude Code, Hermes, Codex, Antigravity, Cursor, VS Code…
nektar dock openai            # writes ~/.nektar/openai.env (OPENAI_BASE_URL) + ~/.nektar/openai-mcp.json
nektar dock --uninstall       # cleanly removes Nektar entries anytime
```

## 2. Direct MCP (`nektar mcp` or Streamable HTTP `/mcp`)

```json
{
  "mcpServers": {
    "nektar": {
      "command": "nektar",
      "args": ["mcp"]
    }
  }
}
```

`nektar mcp` automatically discovers your shared background daemon via `~/.nektar/active-daemon.json` (Testnet or Mainnet) so every open IDE window shares one SQLite brain with zero lock contention.

## 3. Focused `agent` Tool Surface

- **Memory & Grounding:** `remember`, `recall`, `forget`, `groundedAnswer` (returns `receipt: nektarr1.*` + `verify:` footer), `verifyReceipt`
- **1-Call Capsule Market:** `buyCapsule` (auto-posts a 0%-CPU `StandingOrder` bounty if 0 offers match), `sellCapsule` (fail-closed PII scrub + commit + anchor + list + `.nkt` export), `importCapsule` (offline SPV verify & import from `.nkt` path or `https://` URL)
- **Discovery & Git Repos:** `peers` (`joinHub` / `leaveHub`), `repo`, `ontology`, `lineage`, `deals`, `fund`, `doctor`

Set `NEKTAR_TOOL_PROFILE=operator` to expose low-level admin tools (`anchor`, `prove`, `borgGroup`, `exportPack`, `importPack`, `authorize`, `status`) directly over MCP.

## 4. Honesty Rule

Every receipt proves that cited records were **recorded, unaltered, and derivable** by block time — never that their content is objectively true, and model identity is _declared_, not zkML-proven.
