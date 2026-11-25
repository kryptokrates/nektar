# Advanced Install & Developer Setups (v0.4 Pure-Go)

The primary user path is the **zero-dependency static Go binary**:
`curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh`
(or `irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1 | iex` on Windows).

Use the paths below for Docker/GHCR, BSV Desktop wallet custody, Docker MCP Toolkit, Homebrew, or building from source.

---

## 1. Docker / GHCR (`--docker` opt-in)

```sh
# macOS / Linux thin GHCR launcher (manages ghcr.io/kryptokrates/nektar:latest on :18420)
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --docker
nektar up && nektar dock
```

```powershell
# Windows PowerShell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Docker
nektar up
nektar dock
```

Or run the container directly:

```sh
docker run -d --name nektar --restart unless-stopped \
  -p 127.0.0.1:18420:18420 -v nektar-data:/data \
  ghcr.io/kryptokrates/nektar:latest serve
```

---

## 2. Host-Native Desktop Wallet (`NEKTAR_WALLET=client`)

Desktop BRC-100 wallets (BSV Desktop / Metanet Desktop) listen on loopback `http://127.0.0.1:3321`.
Run the native Go binary on the host alongside your unlocked desktop wallet:

```sh
NEKTAR_WALLET=client nektar serve
```

(`NEKTAR_WALLET=client` is refused on `memory`/`regtest` networks.)

---

## 3. Direct Stdio Container (Per-Client Spawn)

Pass `"mcp"` explicitly so the container speaks MCP over stdio instead of starting the HTTP server:

```sh
claude mcp add nektar -- docker run -i --rm -v nektar-data:/data ghcr.io/kryptokrates/nektar:latest mcp
```

> Note: do not run a long-lived `nektar serve` daemon on the same `nektar-data` volume simultaneously with a direct stdio container, or `instance.lock` will reject the second writer. For multiple clients sharing one brain, run `nektar serve` (or `nektar up`) and dock via `nektar dock` (Streamable HTTP MCP on `:18420`).

---

## 4. Docker Desktop MCP Toolkit

Catalog manifests live under `docker-mcp/` (`nektar.yaml`, `nektar.ghcr.yaml`, `nektar-main.yaml`, `nektar-regtest.yaml`). All stdio entries explicitly invoke `mcp`.

---

## 5. Homebrew (macOS / Linux)

```sh
brew install kryptokrates/tap/nektar
nektar serve
nektar dock
```

Formula source: [packaging/homebrew/nektar.rb](../packaging/homebrew/nektar.rb).

---

## 6. Build from Source (Go 1.27+, Zero CGO)

```bash
git clone https://github.com/kryptokrates/nektar.git
cd nektar
go build -o nektar ./cmd/nektar
go vet ./... && go test -race ./...
./nektar serve
```

Rebuild the embedded Svelte 5 Dashboard UI:

```bash
cd ui && npm install && npm run build
# Sync ui/dist/ -> internal/server/ui_dist/ before rebuilding the Go binary
```
