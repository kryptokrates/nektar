# nektar-bstack-sandbox

Local regtest sandbox for the @nektar/bstack host module: a devchain (no real
coin, nothing mined is money), MySQL, and one reference overlay host running
ONLY the bundled nektar module on the 16 `tm_nektar_regtest_*` lanes +
`ls_nektar`, beside the bbox module the image ships.

## Bring up

    pnpm --filter @nektar/bstack bundle          # writes packages/bstack/bundle/nektar-module.js
    docker compose -f deploy/bstack-sandbox/compose.yaml up -d

The host mounts `packages/bstack/bundle/` read-only; after rebundling run
`docker compose -f deploy/bstack-sandbox/compose.yaml restart host`.

## Run the demo (mint tree → submit want → read → sweep → gone)

    CHAIN_URL=http://127.0.0.1:18344 HOST_URL=http://127.0.0.1:18091 \
      node packages/bstack/demo/live-sandbox.mjs

Ports: devchain 127.0.0.1:18344, host 127.0.0.1:18091 (no public bind).
Tear down: `docker compose -f deploy/bstack-sandbox/compose.yaml down -v`.
Never point NEKTAR_* or the demo at mainnet/testnet — devchain only.
