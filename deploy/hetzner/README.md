# Hetzner — `nektar.run` Production Hub Configuration (v0.1 Pure-Go)

`nektar.run` runs the single-binary v0.1 Pure-Go Hub + Restate v1.8.1 sidecar with immutable image digests and automatic `/v1/health` rollback.

| Layer | Location | Managed by |
| --- | --- | --- |
| Testnet Hub config (active CI deploy) | `deploy/hetzner/nektar-http.env` | Git / `.github/workflows/deploy.yml` |
| Testnet secrets & tokens | `/opt/nektar/.env` (box-local only — never in git) | Operator |
| **Mainnet Spine Hub config (side-by-side)** | `deploy/hetzner/nektar-mainnet.env` + `deploy/hetzner/compose.mainnet.yaml` | Git / operator cutover ([MAINNET-CUTOVER.md](MAINNET-CUTOVER.md)) |
| **Mainnet secrets & confirmation** | `/opt/nektar/.env.mainnet` (`chmod 600` — never in git) | Operator |
| Caddy side-by-side routing | `deploy/hetzner/Caddyfile.mainnet` (`nektar.run` -> `:18422` Mainnet, `testnet.nektar.run` -> `:18420` Testnet) | Operator (`flush_interval -1` for SSE) |
| Public surfaces | `https://nektar.run/.well-known/nektar`, `/hub/v1/health`, `/hub/v1/terms`, `/v1/verify` | Caddy reverse proxy |

Check live status:
```bash
curl -fsS https://nektar.run/v1/health
curl -fsS https://nektar.run/hub/v1/health
curl -fsS https://nektar.run/hub/v1/terms
```
