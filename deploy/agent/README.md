# Nektar v0.1 Agent — Deployable Anywhere (Pure-Go + Restate v1.8.1)

Runs the published zero-CGO single binary image `ghcr.io/kryptokrates/nektar:latest` alongside the Restate v1.8.1 durable execution sidecar on port `:18420` (MCP Streamable HTTP, REST API, A2A, L0 LLM Proxy, and embedded Svelte 5 Dashboard).

> **Note:** On a personal laptop or workstation, you don't need Docker at all — install the static Go binary directly with `curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up`. Use this `deploy/agent/` directory for VPS, Docker Compose, or Kubernetes deployments.

---

## What Lives Where

| Piece | What It Is | Hosted Where | Image |
|---|---|---|---|
| **Your agent** | Identity, BRC-100 embedded wallet, SQLite FTS5 memory, Cedar, Mangle, HTTP/MCP/Proxy/Dashboard | Your VPS / cluster | `ghcr.io/kryptokrates/nektar:latest` |
| **Durable executor** | Restate v1.8.1 (`BuyerDeal`, `SellerDeal`, `Custody`, `AnchorCadence`) | Same Compose project / Kubernetes pod | `docker.restate.dev/restatedev/restate:1.8.1` |
| **Agent state** | SQLite DB, BRC-100 `wallet/`, CHIRP objects, auth tokens | `agent-data` volume / PVC (`/data`) | Your backups |
| **Market Hubs** | Untrusted overlay relays (signature & SPV verified client-side) | Public Hubs or your own (`deploy/hub/`) | `ghcr.io/kryptokrates/nektar:latest` (`-hub`) |
| **Chain truth** | Dual-witness PoW headers (WoC + Chaintracks) & official US/EU BSV Broadcaster | Public internet (SPV-verified, never trusted) | — |

---

## 1. Bare Linux Box (One Command)

```bash
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
```

Installs Docker if missing, generates `NEKTAR_HTTP_TOKEN` and `NEKTAR_OPERATOR_TOKEN` into `.env`, and starts `agent` + `restate` on `127.0.0.1:18420`.

Mainnet one-liner (real BSV — requires explicit `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND`; leaving `NEKTAR_HEADERS_SOURCE` unset automatically enables dual-witness non-circular PoW headers):

```bash
NEKTAR_NETWORK=main NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND \
  curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
```

---

## 2. Docker Compose (Three Commands)

```bash
cp .env.example .env            # optionally set NEKTAR_DOMAIN for Caddy Auto-TLS
docker compose -f compose.yaml --env-file .env up -d
docker compose -f compose.yaml logs -f agent
```

If `NEKTAR_HTTP_TOKEN` and `NEKTAR_OPERATOR_TOKEN` are left blank, the container automatically generates persistent mode-0600 tokens under `/data/http-token` and `/data/operator-token` on first boot and prints a 1-click `/pair?code=...` Dashboard URL in the logs.

Pass `--profile tls` with `NEKTAR_DOMAIN=agent.example.com` for automatic Let's Encrypt HTTPS via Caddy.

---

## 3. Kubernetes / k3s / EKS / GKE (Helm)

```bash
helm install my-agent ./helm \
  --set secrets.httpToken="$(openssl rand -hex 32)" \
  --set secrets.operatorToken="$(openssl rand -hex 32)"
```

For Mainnet:

```bash
helm install my-agent ./helm \
  --set network=main \
  --set mainnetConfirm=I_UNDERSTAND \
  --set secrets.httpToken="$(openssl rand -hex 32)" \
  --set secrets.operatorToken="$(openssl rand -hex 32)"
```

The chart runs `agent` + `restate` sidecar in one pod (loopback `127.0.0.1` between containers, separate PVCs for `/data` and `/restate-data`, non-root UID `10001`, readiness/liveness probes on `/v1/health`).

---

## 4. After It's Up

- **Health check:** `curl -fsS http://127.0.0.1:18420/v1/health`
- **Discovery card:** `curl -fsS http://127.0.0.1:18420/.well-known/nektar`
- **Dashboard:** open the `/pair?code=...` URL from `docker compose logs agent` (or mint a fresh one with `POST /v1/auth/pair-code` using `NEKTAR_OPERATOR_TOKEN`)
- **MCP (Streamable HTTP):** `http://127.0.0.1:18420/mcp` with `Authorization: Bearer $NEKTAR_HTTP_TOKEN`
- **Wallet status & funding:** `docker compose exec agent nektar wallet status` (on testnet, use the Dashboard 1-click faucet or `POST https://testnet.nektar.run/v1/faucet`)
- **Approve autonomous trading policy:** `docker compose exec agent nektar policy approve --preset both`
- **Run your own Hub:** see [../hub/README.md](../hub/README.md) or run `nektar hub up --auto-ip`
