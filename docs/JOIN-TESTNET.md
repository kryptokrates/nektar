# Join testnet — run an independent Nektar v0.1 Hub

We need operators who are **not us**. Everything the network does on one box
is a simulation; the gate that matters is a second team running a Nektar v0.1 Hub
on their own hardware, on their own keys, somewhere we don't control.

This is the whole ask: any Linux box (even a $4 / 512 MB RAM VPS), a public address, ~2 minutes,
zero real money (testnet sats only — free faucet built in).

## What you're agreeing to run (v0.1 Pure-Go Single Binary)

In v0.1, the entire Hub — agent, 16-lane Overlay Hub (`tm_nektar_<net>_0..f` + `ls_nektar`),
Lightweb modules (`bfinger`, `bbox`, `blogs`, `borg`, `bsecret`, `bstore`), native SSE `/events`,
liquidity/bounty engine, faucet, and Svelte 5 Dashboard — runs inside **one static Go binary**
(`nektar serve -hub -listen :18420`), with optional Caddy Auto-TLS in front:

| Piece   | What it is                                                               | Trust              |
| ------- | ------------------------------------------------------------------------ | ------------------ |
| `hub`   | single v0.1 Go binary (`nektar serve -listen :18420 -hub`, SQLite FTS5) | yours — your keys  |
| `caddy` | optional Let's Encrypt HTTPS reverse proxy (or `cloudflared` tunnel)     | untrusted plumbing |

Hubs can censor or withhold — never forge. Everything they serve is
signature- and SPV-verified by the reader, so a misbehaving Hub is detected,
not trusted.

## Up in 1 Command (Built-in Caddy Auto-TLS & Auto-Faucet)

```bash
# Option 1 — Zero-domain instant HTTPS on any fresh VPS (<public-ip>.sslip.io):
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_AUTO_IP=1 bash

# Option 2 — With your own domain (Caddy provisions Let's Encrypt automatically):
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | \
  NEKTAR_DOMAIN=hub.example.com bash

# Option 3 — Behind a home router / NAT (zero open ports via Cloudflare Tunnel):
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | \
  NEKTAR_TUNNEL=1 bash

# Option 4 — Already installed the nektar CLI? Graduate in one command:
nektar hub up --auto-ip   # or: nektar hub up --domain <you>  |  --tunnel

# Option 5 — Zero-Docker native Go binary on any Linux box:
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh
nektar serve -hub -listen :18420
```

**1-Click Cloud & Home-Server Templates** also ship in `deploy/hub/templates/`:

- **Hetzner / DigitalOcean / Vultr / AWS:** paste [cloud-init.yaml](../deploy/hub/templates/cloud-init.yaml) into User Data (~45s to live HTTPS hub).
- **Coolify / Dokploy:** import [coolify.compose.yaml](../deploy/hub/templates/coolify.compose.yaml) or [dokploy.compose.yaml](../deploy/hub/templates/dokploy.compose.yaml).
- **Fly.io / Render / Railway:** [fly.toml](../deploy/hub/templates/fly.toml), [render.yaml](../deploy/hub/templates/render.yaml), [railway.json](../deploy/hub/templates/railway.json).
- **Umbrel / CasaOS:** [umbrel/](../deploy/hub/templates/umbrel/umbrel-app.yml) and [casaos.yaml](../deploy/hub/templates/casaos.yaml).
- **Kubernetes / Helm:** `helm install nektar-hub deploy/hub/helm --set publicUrl=https://<you>`.

## Funding on Testnet

No key to generate manually — the Hub creates its own persistent BRC-100 embedded
wallet on first boot under `/data/wallet/`. Self-serve testnet sats:

```bash
curl -X POST https://testnet.nektar.run/v1/faucet \
  -H 'content-type: application/json' \
  -d '{"address":"<your funding address from nektar status>"}'
```

## Verify you joined

1. **Health & Hub status:** `curl -fsS https://<you>/v1/health` and `curl -fsS https://<you>/hub/v1/health`
2. **Your card is on the dir lane:**
   ```bash
   curl -X POST https://<you>/hub/lookup \
     -H 'content-type: application/json' \
     -d '{"service":"ls_nektar","query":{"kind":"announce","net":"testnet"}}'
   ```
3. **Connect your AI clients:**
   ```bash
   claude mcp add --transport http nektar https://<you>/mcp \
     --header "Authorization: Bearer <NEKTAR_HTTP_TOKEN>"
   ```
   Or inspect `/v1/openapi.json` and `/v1/metrics` (Prometheus).

## Honest Boundaries

Verification proves **recorded, unaltered, and derivable** — never that a claim is _true_.
Testnet sats have no monetary value; mainnet (`NEKTAR_NETWORK=main`) refuses to boot
without `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` and enforces `SpendGuard` caps fail-closed.
