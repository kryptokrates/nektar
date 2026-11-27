# infra — third-party stacks (not vendored)

The regtest stack and reference services are upstream repositories, cloned here at the pinned commits used to verify nektar:

```sh
git clone https://github.com/bsv-blockchain/teranode-quickstart.git infra/teranode-quickstart && git -C infra/teranode-quickstart checkout 508aeba
git clone https://github.com/bsv-blockchain/arcade.git            infra/arcade            && git -C infra/arcade checkout 8b48db6
git clone https://github.com/bsv-blockchain/wallet-services.git   infra/wallet-services   && git -C infra/wallet-services checkout 8db2fc6
```

Only `teranode-quickstart` is required for local regtest testing (Teranode v0.16.0 regtest). BSV Broadcaster (the `arcade` repo above) and wallet-services do not support regtest.

Credentials: set `rpc_user`/`rpc_pass` in `infra/teranode-quickstart/.env` (gitignored).

Required local override (not upstream): the default docker settings ship
`rpc_max_clients = 3` — keep-alive callers (hubs, agent trackers, parallel
test runs) saturate it and new RPC calls are refused while the healthcheck
stays green. After cloning, set `rpc_max_clients=128` in
`infra/teranode-quickstart/.env` (or add `rpc_max_clients:
${rpc_max_clients:-128}` to the rpc service's `environment:` in
`compose/docker-teranode.yml`). Symptom: RPC calls hang/close empty while
`docker logs rpc` shows `Max RPC clients exceeded`.

## Ops runbook — Nektar v0.1 (Pure-Go + Restate)

### Wallet & funding

- Embedded BRC-100 wallet state lives under `<NEKTAR_DATA_DIR>/wallet/`
  (`identity.json`, `wallet.db`). Inspect address & balance with
  `nektar wallet status`; run `nektar wallet sync` to refresh UTXOs.
- On testnet, fund via `POST https://testnet.nektar.run/v1/faucet` or the
  Dashboard faucet button. On mainnet, `NEKTAR_NETWORK=main` requires
  `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` (run `nektar check-mainnet` first).
- `NEKTAR_MAX_FEE_PER_TX` and `NEKTAR_MAX_SPEND_PER_DAY` are the SpendGuard
  fail-closed ceilings enforced before broadcast.

### Hub & agent restarts

- `<NEKTAR_DATA_DIR>/instance.lock` prevents two processes from sharing one
  state directory — stop the old process cleanly before restarting.
- Agent state lives in `<NEKTAR_DATA_DIR>/nektar.db` (SQLite FTS5 fragments,
  anchors, receipts, deals, obligations), `<NEKTAR_DATA_DIR>/wallet/`, and
  `<NEKTAR_DATA_DIR>/objects/` (CHIRP blobs). Durable deal workflows live in
  the co-located Restate v1.8.1 sidecar (`NEKTAR_RESTATE_URL`).
- Required env for a live agent: `NEKTAR_NETWORK=testnet|main`,
  `NEKTAR_MARKET_HOSTS=<csv>` (optional; defaults to built-in seed hubs), and
  `NEKTAR_DATA_DIR`. Autonomous trading requires an approved operator policy
  (`nektar policy approve --preset both`).
- Broadcaster & header overrides: `NEKTAR_HEADERS_SOURCE`, `NEKTAR_CHAIN_SPEC`,
  `NEKTAR_SETTLE_SPEC`, `NEKTAR_ARCADE_KEY`, `NEKTAR_WOC_KEY`. See
  `docs/ENVIRONMENT.md` and `docs/OPERATIONS.md` for full details.
