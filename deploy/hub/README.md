# Nektar v0.4 Hub — Self-Hosted Pure-Go Overlay Hub & Spine-and-Leaf Relay

A Nektar v0.4 Hub runs in a single zero-CGO Go binary (`ghcr.io/kryptokrates/nektar:latest`) paired with a Restate v1.8.1 sidecar for durable workflows (`BuyerDeal`, `SellerDeal`, `Custody`, `Tx`, `AnchorCadence`). Every Hub mounts:

- **16-lane BRC-22 / BRC-24 Overlay Hub** (`tm_nektar_<net>_0..f`, `ls_nektar`) plus Lightweb Go modules (`bfinger`, `bbox`, `blogs`, `borg`, `bsecret`, `bstore`) backed by SQLite WAL.
- **Cursored SSE `/hub/events` + `/hub/sync`**, signed NAT-safe `/hub/bbox` relay, starter liquidity shards, and verified knowledge bounties.
- **Spine-and-Leaf Monetization & Anti-Freerider Controls**:
  - `NEKTAR_HUB_ROLE=leaf` (default): edge Hub forwarding local Want/Offer ads UP to configured `NEKTAR_UPSTREAM_SPINES` and subscribing DOWN to metered Spine slices (`?domains=`).
  - `NEKTAR_HUB_ROLE=spine`: backbone Hub peering settlement-free only with bilateral `NEKTAR_SPINE_PEER_PUBS`, metering egress on `/hub/events` & `/hub/sync`, and rejecting unbilled Sybil pulls.
  - `NEKTAR_HUB_FEE_PRESET=free | lightbsv_egress | hybrid` (BRC-105 402 + `GET /hub/v1/terms` + `GET /hub/ls_nektar/terms`).

## Quick Start (1-Command CLI or VPS)

```bash
# Option 1: via nektar CLI (zero-domain auto-TLS via sslip.io, custom domain, or Cloudflare Tunnel)
nektar hub up --auto-ip --network testnet --hub-role leaf --fee-preset hybrid
nektar hub up --domain hub.example.com --network testnet
nektar hub up --tunnel --network testnet

# Option 2: fresh Linux VPS one-liner
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh \
  | NEKTAR_AUTO_IP=1 NEKTAR_HUB_ROLE=leaf NEKTAR_HUB_FEE_PRESET=hybrid bash

# Option 3: Docker Compose directly
cp .env.example .env   # set NEKTAR_HTTP_TOKEN, NEKTAR_OPERATOR_TOKEN, NEKTAR_DOMAIN
docker compose -f compose.yaml --env-file .env --profile tls up -d
docker compose -f compose.yaml logs -f hub
```

## Health & Discovery Endpoints

- `GET /v1/health` — agent & embedded Hub liveness
- `GET /.well-known/nektar` — node capabilities, identity pubkey, `/hub` path, active hosts
- `GET /hub/v1/health` — overlay topics, lookups, active bounties
- `GET /hub/v1/terms` & `GET /hub/ls_nektar/terms` — BRC-105 fee schedule & HMAC derivation prefix

## Mainnet Requirements

Running a Mainnet Hub (`NEKTAR_NETWORK=main`) requires:
1. `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND`
2. `>= 2` independent non-circular PoW header witnesses in `NEKTAR_HEADERS_SOURCE` (verify with `nektar check-mainnet`)
3. Explicit `NEKTAR_SETTLE_SPEC` (e.g. `arcade:https://arcade-v2-us-1.bsvblockchain.tech`) and SpendGuard caps (`NEKTAR_MAX_FEE_PER_TX`, `NEKTAR_MAX_SPEND_PER_DAY`)
4. Restate v1.8.1 sidecar running (`compose.yaml` wires this automatically).
