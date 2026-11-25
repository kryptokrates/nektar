# Advanced Install & Developer Setups

The primary user path is the **GHCR-first install** (`curl -fsSL https://nektar.run/install.sh | sh` or `irm https://nektar.run/install.ps1 | iex`). Use the paths below only when you need host-native desktop wallet signing (`NEKTAR_WALLET=client`), Docker Desktop MCP Toolkit, bare stdio containers, or source development against local Teranode regtest.

---

## 1. Host-Native Build (Required for BSV Desktop / `NEKTAR_WALLET=client`)

Desktop BRC-100 wallets (e.g. BSV Desktop / Metanet Desktop) expose their signing API on `localhost:3321` only, which a container cannot reach. Install the self-contained host build (requires Node.js 22.12+; bundles `restate-server`, `integrations/`, and `skills/`):

```sh
# macOS / Linux
curl -fsSL https://nektar.run/install.sh | sh -s -- --native

# Windows PowerShell
& ([scriptblock]::Create((irm https://nektar.run/install.ps1))) -Native
```

Or download directly from [GitHub Releases](https://github.com/kryptokrates/nektar/releases/latest):

- `nektar-mcp-linux-x64.tar.gz`
- `nektar-mcp-darwin-arm64.tar.gz`
- `nektar-mcp-win32-x64.zip` (runs bundled Linux `restate-server` through WSL2)

Boot unlocked BSV Desktop first, then:

```sh
NEKTAR_WALLET=client ./nektar serve
```

---

## 2. Direct Stdio Container (Per-Client Spawn)

If you prefer an MCP client to spawn an ephemeral stdio container directly instead of talking to one shared daemon (`nektar up`):

```sh
# Claude Code
claude mcp add nektar -- docker run -i --rm -v nektar-data:/data ghcr.io/kryptokrates/nektar:latest
```

> Note: do not run a long-lived `nektar` daemon on the same `nektar-data` volume simultaneously with a direct stdio container, or `instance.lock` will reject the second writer. For multiple clients sharing one brain, use `nektar up` + `nektar dock` (HTTP MCP).

---

## 3. Docker Desktop MCP Toolkit

```sh
pnpm mcp:install --ghcr
docker mcp client connect claude-desktop --profile nektar
```

See [docs/INTEGRATION.md](INTEGRATION.md) for regtest/mainnet profiles.

---

## 4. Homebrew (macOS / Linux)

```sh
brew install kryptokrates/tap/nektar
nektar up && nektar dock
```

Formula source: [packaging/homebrew/nektar.rb](../packaging/homebrew/nektar.rb).

---

## 5. Build from Source & Local Teranode Regtest

Requires **Node.js 22.12+**, **pnpm**, and **Docker** (for local Teranode regtest):

```bash
git clone https://github.com/kryptokrates/nektar.git
cd nektar
pnpm install
pnpm -r build
pnpm restate:up        # fetch SHA256-verified restate-server sidecar once
pnpm init:nektar       # interactive wizard
```

Run the local regtest stack and 60-second demo:

```bash
pnpm stack:up
pnpm demo
```

See [docs/DEVELOPER.md](DEVELOPER.md) and [docs/OPERATIONS.md](OPERATIONS.md) for all verification gates, covenants, STARK provers, and `deploy/bstore/`.
