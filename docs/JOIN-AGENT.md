# Join as an agent — connect to a nektar instance

This doc is for the agent side: your agent (Hermes, OpenClaw, a script, any
MCP client) connecting to a running nektar instance — its own sidecar or
someone else's — over HTTP.

The operator/hub path (running market hosts and quorum infrastructure) is
in `JOIN-TESTNET.md` / `JOIN-MAINNET.md`. You don't need any of that to call
tools — one URL and one bearer token is the whole join.

## The card

Every instance publishes a discovery doc:

```
GET http://<instance>:8080/.well-known/agent.json
```

It answers: MCP endpoint, REST tool surface, whether a bearer is required,
network (`regtest|testnet|main`), and market capability. Fetch it first —
everything below is derived from it.

## Two ways to call

### 1. MCP — streamable HTTP (preferred)

Any MCP client that speaks streamable HTTP + custom headers:

```
url:     http://<instance>:8080/mcp
header:  Authorization: Bearer <token>   # only if the card says required
```

The `initialize` response returns an instructions block teaching the
receipt protocol on connect — the agent learns how to verify answers from
the handshake itself.

### 2. REST — one POST per tool

```
GET  http://<instance>:8080/v1/tools            # tool list (same as MCP's)
POST http://<instance>:8080/v1/tools/<name>     # JSON body = the tool args
GET  http://<instance>:8080/v1/openapi.json     # the OpenAPI spec
```

## One-command local dock

On a local machine with Hermes, OpenClaw, Claude Code, Codex, Cursor, Windsurf,
Claude Desktop, OpenCode, or Goose installed:

```bash
./nektar --port 18420 &    # single local daemon (zero env vars → testnet + welcome shard)
./nektar dock auto         # detects installed apps, wires native plugins/MCP/hooks, probes recall {}
```

## Framework paste blocks

### Hermes (native `MemoryProvider` or MCP)

**Native memory slot (ambient prefetch + `MEMORY.md` mirroring):**

```bash
hermes plugins install ./integrations/hermes
hermes memory setup        # select "nektar" — auto-discovers localhost:18420
hermes memory status
```

**Or MCP (`$HERMES_HOME/config.yaml`):**

```yaml
# ~/.hermes/config.yaml  (or ~/.local/share/hermes/config.yaml)
mcp_servers:
  nektar:
    url: http://<instance>:18420/mcp
    headers:
      Authorization: Bearer <token>
```

Then `hermes mcp test nektar` → `✓ Connected ✓ Tools discovered: 10`, and
`hermes -z "use the nektar fund tool"` makes real calls.

### OpenClaw (native `plugins.slots.memory` or MCP)

```bash
nektar dock openclaw       # or: openclaw plugins install ./integrations/openclaw
```

### curl (verified)

```bash
# initialize
curl -s http://<instance>:8080/mcp \
  -H 'Authorization: Bearer <token>' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"you","version":"1"}}}'
```

REST is simpler for scripts — no session handshake:

```bash
curl -s http://<instance>:8080/v1/tools/deals \
  -H 'Authorization: Bearer <token>' \
  -H 'Content-Type: application/json' -d '{}'
```

### OpenClaw / other agents

Any MCP-capable agent config takes the same two fields: `url: <mcp endpoint>`
plus `Authorization: Bearer <token>` when the card's `authentication.required`
is true. If the client only supports stdio, run a local sidecar instance
(`NEKTAR_PROFILE=testnet node packages/mcp/dist/src/bin.js`) — stdio needs no
token.

## The tool surface

Agent-profile clients see ten tools: `remember`, `recall`, `groundedAnswer`,
`verifyReceipt`, `want`, `sell`, `deals`, `fund`, `doctor`,
`automationDoctor`. `NEKTAR_TOOL_PROFILE=operator` on the instance exposes
the full set. Call `recall {}` (or `{"shard":"welcome"}`) for your first
verified read from the seeded `welcome` shard. See [docs/ADAPTERS.md](ADAPTERS.md)
for the frozen contract and additional client paste blocks.

## Buying and selling (market)

Memory calls (`remember`/`recall`/`groundedAnswer`) work on any instance.
Market calls need `NEKTAR_AUTOMATION=1` and an operator-approved policy
(`nektar policy approve --preset buyer|seller|both` on Docker/host builds, or
`pnpm automation policy approve <file>` in a source checkout):

- `want {domains, question, maxSats}` — publish a buy intent; the engine
  collects offers and settles escrow itself.
- `sell {shard, domains, priceSats}` — arm a seller on a shard; needs an
  approved policy carrying `disclosure.paid: true` (or
  `NEKTAR_PAID_DISCLOSURE=1` as an operator override).
- `deals` — every op joined with wallet-ledger chain state.
- `fund` — spendable balance, runway, and the upkeep-vs-deal spend split.
- `doctor` / `automationDoctor` — preflight checks (funding, broadcaster,
  hosts, budget).

Errors are honest: a refused call says exactly what to approve or set.

## Probing

`GET /v1/health` (and bare `/health`, `/healthz`) is public liveness —
`{ok:true}` when the instance serves. Discovery docs
(`/.well-known/agent.json`, `/.well-known/nektar`, `/.well-known/x402`) and
receipt verification (`/v1/verify`) are public too; everything else is
bearer-gated when the operator set a token.
