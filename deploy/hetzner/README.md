# hetzner — the nektar.run instance configuration

`nektar.run` runs `nektar-http` under systemd from the `/opt/nektar`
checkout. `.github/workflows/deploy.yml` deploys every master push and every
`v*` tag: fetch → reset --hard → install → build → restart → health gates.

## What's configured where

| Layer                            | File                                                                                                                                                                       | Managed by                                                                                |
| -------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| Non-secret market/network config | `deploy/hetzner/nektar-http.env` (this repo)                                                                                                                               | deploy.yml applies each `KEY=value` to `/etc/nektar-http.env` — edit + push = reconfigure |
| Secrets + box-local plumbing     | `/etc/nektar-http.env` on the box (HTTP_TOKEN, KEY_PASSPHRASE, FUND_TXIDS, DATA_DIR, HTTP_PORT/HOST, VERIFY_BASE…)                                                         | box-local only — never in git                                                             |
| Services                         | `nektar-http`, `nektar-watch`, `nektar-prover` systemd units                                                                                                               | box + deploy.yml                                                                          |
| Edge                             | Caddy `nektar.run` → `:8080` for `/v1`, `/.well-known`, `/mcp`, `/hosts/a` → `:18291`, `/hosts/b` → `:18292`, plus `/install.sh` & `/install.ps1` → `/opt/nektar/install/` | box                                                                                       |
| Market hosts (the quorum)        | `docker compose -p nektar-bstack-testnet -f deploy/bstack-sandbox/compose.testnet.yaml` on the box                                                                         | this repo — the pair agents publish/read through                                          |

## What the instance is right now

A **testnet market agent** — a quorum participant:

- `NEKTAR_AUTOMATION=1` + `NEKTAR_UNATTENDED=1` — runs want→collect→escrow→
  settle autonomously under SpendGuard caps (`MAX_SPEND_SATS_PER_DAY`,
  `MAX_FEE_SATS_PER_TX`)
- Publishes a bfinger record (`NEKTAR_BFINGER_PUBLISH=1`) and accepts peer
  push at `https://nektar.run/v1/lane`
- Reads/writes through its own public host pair `https://nektar.run/hosts/{a,b}`
  — which also serves as quorum hosts for any other operator's agent

Check it live: `curl https://nektar.run/.well-known/nektar` — the `market`
field reports `automation`, `hosts` and `push` as actually configured.

## Notes

- The bstack pair needs `packages/bstack/bundle` — deploy.yml builds it
  (`pnpm --filter @nektar/bstack bundle`) so the compose mount stays fresh
- The box's wallet/identity lives in `NEKTAR_DATA_DIR` (`/opt/nektar/.mamem-ttn`,
  legacy name) — it IS the agent; back it up
- Market hosts are untrusted plumbing by design: everything they serve is
  signed + SPV-verified client-side
