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

## Per-platform attach

### Claude Code (remote hub)

```bash
claude mcp add --transport http nektar https://<you>/mcp \
  --header "Authorization: Bearer <NEKTAR_HTTP_TOKEN>"
```

Local per-project: `pnpm setup:claude` registers the HTTP MCP server in
`.mcp.json` (accepts `--port`/`--url`/`--token` or reads `.env`) AND
installs the receipt discipline hooks (Stop + SessionStart) into the
project's `.claude/`.

### Codex — `~/.codex/config.toml`

```toml
[mcp_servers.nektar]
url = "https://<you>/mcp"
http_headers = { "Authorization" = "Bearer <NEKTAR_HTTP_TOKEN>" }
```

Local stdio variant:

```toml
[mcp_servers.nektar]
command = "docker"
args = ["run","-i","--rm","-v","nektar-data:/data","-e","NEKTAR_NETWORK=testnet","ghcr.io/kryptokrates/nektar:latest"]
```

### OpenClaw and MCP-native gateways

Any client that takes an `mcpServers` config block:

```json
{
  "mcpServers": {
    "nektar": {
      "transport": "http",
      "url": "https://<you>/mcp",
      "headers": { "Authorization": "Bearer <NEKTAR_HTTP_TOKEN>" }
    }
  }
}
```

### Hermes / function-calling runtimes (no MCP)

Register each needed tool as an HTTP function — the manifest is the
OpenAPI doc, so registration can be generated, not hand-written:

```
GET  /v1/openapi.json            → tool manifest (names, schemas)
POST /v1/tools/{name}            → invoke; body = the tool's args
Authorization: Bearer <token>
```

Minimal runtime shim (~30 lines): `GET /v1/openapi.json` at start → map
each operation to `{name, description, parameters}` in your platform's
function schema → dispatch calls as `POST /v1/tools/{name}` with the arg
object. `POST /v1/tools/doctor` is the health check.

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
