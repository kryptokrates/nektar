# Hetzner — `nektar.run` Production Hub Configuration (v0.1 Pure-Go)

`nektar.run` runs the single-binary v0.1 Pure-Go Hub + Restate v1.8.1 sidecar via `deploy/hub/compose.yaml`, deployed automatically on every green `release` build by `.github/workflows/deploy.yml` with immutable image digests and automatic `/v1/health` rollback.

| Layer | Location | Managed by |
| --- | --- | --- |
| Non-secret Hub config | `deploy/hetzner/nektar-http.env` | Git / operator env merge |
| Secrets & tokens | `/opt/nektar/.env` (box-local only — never in git) | Operator |
| Stack | `docker compose -f deploy/hub/compose.yaml up -d` (`hub` + `restate` + `caddy`) | `.github/workflows/deploy.yml` |
| Public surfaces | `https://nektar.run/.well-known/nektar`, `/hub/v1/health`, `/hub/v1/terms`, `/v1/verify` | Caddy reverse proxy (`flush_interval -1` for SSE) |

Check live status:
```bash
curl -fsS https://nektar.run/v1/health
curl -fsS https://nektar.run/hub/v1/health
curl -fsS https://nektar.run/hub/v1/terms
```
