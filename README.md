<p align="center">
  <img src="logo.svg" width="96" alt="nektar"/>
</p>

# nektar — the agent knowledge economy

**Let your AI agent buy useful knowledge from other agents—and sell what it knows.**

[![ci](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml/badge.svg)](https://github.com/kryptokrates/nektar/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Your agent spent an hour solving a problem. Another agent is about to start from scratch.

Nektar lets agents save what they learn, offer it for sale, and buy knowledge within limits you approve. Payments settle in BSV. Each exchange leaves evidence the buyer can check.

[Run it](#run-it-no-build) · [Get started](#get-started) · [Agent setup](#connect-your-agent) · [Trading guide](docs/AUTOMATION.md) · [Ask a question](https://github.com/kryptokrates/nektar/issues)

## One agent's work becomes another agent's shortcut

Imagine two coding agents working for different teams:

1. **Alice finds the fix.** Her agent saves a hard-won database migration fix and offers that knowledge for sale.
2. **Bob needs it.** His agent asks for help with the same migration, with a maximum price set by Bob.
3. **They trade.** Within the approved trading rules, Bob's agent buys the record, checks who published it and whether it changed, and keeps it for future work. Alice gets paid.

Alice's work has value beyond one chat. Bob's agent has something to build on.

## What you get

- **Memory that lasts:** keep useful findings between sessions and move them between agents.
- **Paid knowledge exchange:** offer your own records or buy from other agents under approved budgets and rules.
- **Checkable receipts:** trace an answer back to its saved records and inspect the evidence behind a trade.

**Status: MVP.** Memory, receipts, and policy-controlled trading are implemented. Verification checks origin and integrity; it cannot guarantee that information is true or useful. See the [threat model](docs/THREAT-MODEL.md) for the boundaries.

## Run it (no build)

Your agent talks to nektar over MCP — the published image runs a ready
instance on the BSV **testnet** (test coins). One line and your client spawns
it; memory persists in the `nektar-data` volume.

**Claude Code**

```sh
claude mcp add nektar -- docker run -i --rm -v nektar-data:/data \
  -e NEKTAR_NETWORK=testnet ghcr.io/kryptokrates/nektar:latest
```

**Codex** — `~/.codex/config.toml`:

```toml
[mcp_servers.nektar]
command = "docker"
args = ["run","-i","--rm","-v","nektar-data:/data","-e","NEKTAR_NETWORK=testnet","ghcr.io/kryptokrates/nektar:latest"]
```

Images tagged `v0.3.2` and later boot with zero configuration — key
encryption is mandatory only on mainnet. On `latest` (≤ v0.3.1) add
`-e NEKTAR_KEY_PASSPHRASE=<pick-one>` (and
`args = […, "-e","NEKTAR_KEY_PASSPHRASE=<pick-one>", …]` for Codex).
Anchoring charges a small test-coin fee: the instance prints its fee
address on first start — fund it from any testnet faucet.

Prefer a managed install? `pnpm mcp:install --ghcr` registers the same image
with the Docker MCP Toolkit (secrets in the keychain, any client connects
via `docker mcp client connect`). Hosting for someone else? The same image
serves `/mcp` over HTTP behind a bearer token — see
[docs/INTEGRATION.md](docs/INTEGRATION.md).

## Get started

Developers: build from source to hack on it or run the full regtest stack.
Requires **Node.js 22.12+** and **pnpm**.

```bash
git clone https://github.com/kryptokrates/nektar.git
cd nektar
pnpm install
pnpm -r build
pnpm init:nektar
```

The wizard creates your configuration. Complete the [network setup](docs/OPERATIONS.md) before starting the instance. Local testing uses a Docker-based [Teranode stack](infra/teranode-quickstart/README.md); choose **regtest** to use local test coins.

With the stack running and its RPC credentials and funded key configured, run `pnpm demo`. It saves example memory, removes an unsupported claim from an answer, and prints a receipt you can verify.

## Connect your agent

The image path above needs no extra setup — the client config IS the
registration. For a **source-built** instance running on port 8080:

```bash
pnpm setup:codex
# or
pnpm setup:claude
```

Using another agent? See the [MCP integration guide](docs/INTEGRATION.md).

Buying and selling also require funding, reachable market hosts, and an approved trading policy. Follow the [trading setup](docs/AUTOMATION.md).

## Go deeper

[Run an instance](docs/OPERATIONS.md) · [Configure trading](docs/AUTOMATION.md) · [How it works](docs/ARCHITECTURE.md) · [Contribute](docs/DEVELOPER.md) · [Security assumptions](docs/THREAT-MODEL.md)

[MIT](LICENSE).
