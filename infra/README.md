# infra — third-party stacks (not vendored)

The regtest stack and reference services are upstream repositories, cloned here at the pinned commits used to verify nektar:

```sh
git clone https://github.com/bsv-blockchain/teranode-quickstart.git infra/teranode-quickstart && git -C infra/teranode-quickstart checkout 508aeba
git clone https://github.com/bsv-blockchain/arcade.git            infra/arcade            && git -C infra/arcade checkout 8b48db6
git clone https://github.com/bsv-blockchain/wallet-services.git   infra/wallet-services   && git -C infra/wallet-services checkout 8db2fc6
```

Only `teranode-quickstart` is required (`pnpm stack:up` / `stack:down`; Teranode v0.16.0 regtest). BSV Broadcaster (the `arcade` repo above) and wallet-services were evaluated and don't support regtest (see docs/RESEARCH.md).

Credentials: set `rpc_user`/`rpc_pass` in `infra/teranode-quickstart/.env` and `NEKTAR_WIF` in `nektar/.env` — both gitignored.

Required local override (not upstream): the docker.m settings ship
`rpc_max_clients = 3` — keep-alive callers (bstack hosts, agent trackers,
parallel test files) saturate it and new RPC calls are refused while the
healthcheck stays green. After cloning, set `rpc_max_clients=128` in
`infra/teranode-quickstart/.env` (or add `rpc_max_clients:
${rpc_max_clients:-128}` to the rpc service's `environment:` in
`compose/docker-teranode.yml`). Symptom: RPC calls hang/close empty while
`docker logs rpc` shows `Max RPC clients exceeded`.

## Ops runbook — testnet rig

### Repointing the treasury / re-funding agents

- `NEKTAR_FUND_WIF` is the funding key; `NEKTAR_FUND_TXIDS=<txid>` or
  `hex:<rawtx>` registers pre-built funding outputs at boot (the raw hex is
  required so the agent can prove the output without an indexer). Change the
  WIF and every subsequent reserve/topup tx pays from the new key.
- Agent reserve state lives at `<dataDir>/reserve.json`: `entries` are the
  owned UTXOs, `spent` the outpoint list consumed so far. `entries - spent` is
  the spendable set; `no funding UTXO` in stderr means that set can't cover
  the operation (check dust-level entries — ~2-sat outputs are unusable).
- `NEKTAR_MAX_SPEND_SATS_PER_DAY` + `NEKTAR_MIN_BROADCAST_INTERVAL_MS` are the
  SpendGuard ceilings — automated spends refuse pre-broadcast when exceeded.

### Restarting bstack hosts

- Testnet hosts are docker compose services (`nektar-bstack-testnet-host-*`).
  Host state is its own sqlite volume — a restart is safe; agents re-poll on
  their 1 s deal tick and treat a down host as `unreachable` (never a fault).
- `ls_*` lookups are fan-out reads: a carrier on host A but not host B is
  reported `withheld` until reconciliation — noise, not an error. Agents only
  need ONE reachable host to make progress.

### Restarting an agent process

- `<dataDir>/instance.lock` prevents a second process on the same state —
  delete it only after the old process is fully dead (Windows: `Stop-Process`
  on the port's `OwningProcess`).
- Required env for a live agent: `NEKTAR_NETWORK=testnet`,
  `NEKTAR_AUTOMATION=1` (the engine is opt-in — without it the HTTP API
  answers but no deal processing happens), `NEKTAR_KEY_PASSPHRASE`
  (unlocks the scrypt keystore at `<dataDir>/keys.json`),
  `NEKTAR_MARKET_HOSTS=<csv>` and `NEKTAR_DATA_DIR`.
- `automation.sqlite` holds operations/inbox/outbox/budgets;
  `lanes.sqlite` holds bstack trees/carriers/journal. Operations recover from
  self-notes + counterparty messages; lane state does NOT rebuild cleanly —
  back up `lanes.sqlite` before any state surgery (stale tree-slot data makes
  carriers fail host-side script evaluation).
- Broadcaster overrides: `NEKTAR_BROADCASTER_URL` (arcade endpoint) and
  `NEKTAR_WOC_URL`. With the broadcaster down, WoC carries broadcasts; with
  both down, spend tools fail closed at the boundary — no ops, no
  reservations.
