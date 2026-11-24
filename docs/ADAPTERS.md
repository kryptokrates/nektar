# Adapters — connect any AI client to a nektar instance

The universal adapter is not a library per platform — it is **one
contract** expressed on transports every platform already speaks. Pick
the row that matches your client; every path ends at the same tool
surface.

## The contract (compatibility floor)

Tools in the first block are the stable surface — names and required
parameters are frozen; new fields and tools arrive additively only.

| Tool                      | Purpose                                                      | Minimal args                                                  |
| ------------------------- | ------------------------------------------------------------ | ------------------------------------------------------------- |
| `recall`                  | read memory — fragments with provenance                      | `shard` (optional — omit to list/read shards, e.g. `welcome`) |
| `remember`                | write memory — anchored, signed                              | `data`, `shard`                                               |
| `groundedAnswer`          | answer citing verified fragments + receipt footer            | `claims` (`[{text, cites:[{shard, seq}]}]`)                   |
| `verifyReceipt`           | check a receipt token offline/online                         | `token`                                                       |
| `doctor`                  | one-call health: funding, `shardNames`, bootstrap, `hints[]` | —                                                             |
| `automation`              | marketplace ops: `publishWant`, `enableSeller`, `status`, …  | `op` + op args (operator profile)                             |
| `want` / `sell` / `deals` | ergonomic market entry points                                | want: `domains`, `maxSats`                                    |

Everything else on the surface (`prove`, `anchor`, `exportShard`,
`importShard`, `fundingImport`, `sendSats`, `evidence`, …) is extended
surface — stable in practice, not part of the frozen floor.

## Transports

| Transport           | Endpoint / command                                 | Auth                    | For                             |
| ------------------- | -------------------------------------------------- | ----------------------- | ------------------------------- |
| MCP Streamable HTTP | `https://<host>/mcp`                               | `Authorization: Bearer` | remote clients, hosted runtimes |
| MCP stdio           | `docker run -i --rm … ghcr.io/kryptokrates/nektar` | —                       | local agents, per-project       |
| REST tools          | `GET /v1/tools`, `POST /v1/tools/{name}`           | `Authorization: Bearer` | non-MCP platforms               |
| OpenAPI             | `GET /v1/openapi.json`                             | `Authorization: Bearer` | manifest-driven registration    |

`NEKTAR_HTTP_TOKEN` is the bearer on every remote path.

## One-command zero-config dock (`nektar dock`)

Instead of hand-editing JSON/YAML/TOML across every AI app on your machine, run
**one** command (works from both a source checkout and the no-build release
artifact):

```bash
./nektar --port 18420 &          # single shared local brain (zero env vars → testnet + welcome shard)
./nektar dock auto               # detects & wires Hermes, OpenClaw, Claude Code, Codex, Cursor,
                                 # Windsurf, Claude Desktop, OpenCode, Goose + runs live recall {} probe
```

- **One brain, many mouths:** If a second AI app launches `nektar` over stdio
  while the local daemon is already running, `nektar` transparently bridges
  stdio ↔ Streamable HTTP instead of colliding on `instance.lock` or
  cold-starting Restate.
- **Portable hooks (no checkout required):** `nektar hook session-start`,
  `nektar hook prefetch`, and `nektar hook verify-receipt [--codex]` ship inside
  the `nektar` binary itself.
- **Clean exit:** `nektar dock --uninstall` removes nektar entries while leaving
  sibling MCP servers, hooks, and plugins untouched.

## Docking maturity matrix

| Client                                                    | Highest-maturity path                                                             | Level     | What you get automatically                                                                                      |
| --------------------------------------------------------- | --------------------------------------------------------------------------------- | --------- | --------------------------------------------------------------------------------------------------------------- |
| **Hermes Agent**                                          | Native `MemoryProvider` (`integrations/hermes/`) + `nektar dock hermes`           | **L3**    | `prefetch` before each turn, `MEMORY.md`/`USER.md` write mirroring, receipt check, native tools + `mcp_servers` |
| **OpenClaw**                                              | Native `plugins.slots.memory` (`integrations/openclaw/`) + `nektar dock openclaw` | **L3**    | `before_prompt_build` prefetch, `agent_end` receipt check, native tools                                         |
| **Claude Code**                                           | `.claude-plugin/` marketplace bundle + `nektar dock claude`                       | **L3**    | `.mcp.json`, portable `SessionStart` + `UserPromptSubmit` prefetch + `Stop` receipt block, `SKILL.md`           |
| **Codex CLI**                                             | `nektar dock codex` / `pnpm setup:codex`                                          | **L2**    | `[mcp_servers.nektar]`, portable `notify` receipt check, `AGENTS.md`, `SKILL.md`                                |
| **Cursor / Windsurf / Claude Desktop / OpenCode / Goose** | `nektar dock auto` + MCP Registry `server.json`                                   | **L1/L2** | Non-clobbering MCP registration + live `recall {}` check                                                        |
| **Raw HTTP / custom agents**                              | `GET /v1/openapi.json` + `POST /v1/tools/{name}`                                  | **L1**    | Standard OpenAPI + bearer auth                                                                                  |

## Per-platform attach

### Hermes Agent (Native `MemoryProvider` + MCP)

**Option A — Native `MemoryProvider` (recommended, ambient prefetch + `MEMORY.md` mirror):**

```bash
hermes plugins install ./integrations/hermes   # or: pip install ./integrations/hermes
hermes memory setup                            # pick "nektar" (all fields optional)
hermes memory status                           # confirms URL, network, shards & hints
```

**Option B — Hermes MCP (`$HERMES_HOME/config.yaml`):**

```yaml
mcp_servers:
  nektar:
    url: 'http://127.0.0.1:18420/mcp'
    # headers:
    #   Authorization: "Bearer ${env:NEKTAR_HTTP_TOKEN}"
```

Or run `nektar dock hermes` to install the plugin, `SKILL.md`, and `mcp_servers`
stanza in one command. See [integrations/hermes/README.md](../integrations/hermes/README.md).

### OpenClaw (Native `plugins.slots.memory` + MCP)

```bash
nektar dock openclaw                           # or: openclaw plugins install ./integrations/openclaw
```

Binds `nektar-memory` into OpenClaw's exclusive `plugins.slots.memory` slot with
`before_prompt_build` prefetch and `agent_end` receipt checks. See
[integrations/openclaw/README.md](../integrations/openclaw/README.md).

### Claude Code (Plugin bundle or local project)

```bash
# Local project (MCP + portable SessionStart/UserPromptSubmit/Stop hooks + SKILL.md):
nektar dock claude          # or: pnpm setup:claude

# Remote hub:
claude mcp add --transport http nektar https://<you>/mcp \
  --header "Authorization: Bearer <NEKTAR_HTTP_TOKEN>"
```

Repo root also ships `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`
plus the `agentskills.io` skill at [skills/nektar-memory/SKILL.md](../skills/nektar-memory/SKILL.md).

### Codex — `~/.codex/config.toml`

```bash
nektar dock codex           # or: pnpm setup:codex
```

Or manually:

```toml
[mcp_servers.nektar]
url = "https://<you>/mcp"
http_headers = { "Authorization" = "Bearer <NEKTAR_HTTP_TOKEN>" }
```

Local zero-env Docker stdio variant:

```toml
[mcp_servers.nektar]
command = "docker"
args = ["run","-i","--rm","-v","nektar-data:/data","ghcr.io/kryptokrates/nektar:latest"]
```

### Cursor / Windsurf / Claude Desktop / OpenCode / Goose

```bash
nektar dock auto            # detects installed clients and merges non-clobbering config
```

Or paste the standard `mcpServers` block (HTTP or stdio):

```json
{
  "mcpServers": {
    "nektar": {
      "url": "http://127.0.0.1:18420/mcp"
    }
  }
}
```

### Custom function-calling runtimes (REST / OpenAPI)

Register each needed tool as an HTTP function — the manifest is the OpenAPI
doc, so registration can be generated, not hand-written:

```
GET  /v1/openapi.json            → tool manifest (names, schemas)
POST /v1/tools/{name}            → invoke; body = the tool's args
Authorization: Bearer <token>    (only when NEKTAR_HTTP_TOKEN is set)
```

### Anything HTTP-capable

```bash
curl -s https://<you>/v1/tools \
  -H "Authorization: Bearer <NEKTAR_HTTP_TOKEN>"        # manifest

curl -s https://<you>/v1/tools/recall \
  -H "Authorization: Bearer <NEKTAR_HTTP_TOKEN>" \
  -H 'content-type: application/json' \
  -d '{"shard":"welcome"}'                               # call (or {} to list & read all shards)
```

## Which instance do clients attach to?

Each dock keeps its own instance — custody stays where it belongs (a
platform user's agent has its own wallet and memory; a hosted runtime
runs one platform-owned instance). The hub is shared supply + shared
rendezvous — never shared memory. See `docs/JOIN-TESTNET.md` to run a
hub, `deploy/agent/` for a standalone agent.
