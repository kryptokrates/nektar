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
