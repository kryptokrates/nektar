# nektar-bstack-sandbox

Local regtest sandbox for the @nektar/bstack host module: a devchain (no real
coin, nothing mined is money), MySQL, and one reference overlay host running
ONLY the bundled nektar module on the 16 `tm_nektar_regtest_*` lanes +
`ls_nektar`, the bbox module on `tm_bbox_nektar_abcdefghij` + `ls_bbox`, the
blogs module on `tm_log_nektar_abcdefghij` + `ls_log` (journal stream
`nektar_abcdefghij`, stream policy `open`, 90-day carrier retention), and the
bfinger module on `tm_finger` + `ls_finger` (identity records).

## Bring up

    pnpm --filter @nektar/bstack bundle          # writes packages/bstack/bundle/nektar-module.js
    docker compose -f deploy/bstack-sandbox/compose.yaml up -d

The host mounts `packages/bstack/bundle/` read-only; after rebundling run
`docker compose -f deploy/bstack-sandbox/compose.yaml restart host`.

### Blogs module bundle

The blogs host module bundle is a gitignored build artifact like
`packages/bstack/bundle/nektar-module.js` (and the bbox module the image
ships). Build it from a sibling checkout of the blogs repo at v0.6.3 and copy
it into place:

    pnpm --dir ../blogs/host bundle                     # writes ../blogs/host/bundle/blogs-module.js
    mkdir -p deploy/bstack-sandbox/modules/blogs
    cp ../blogs/host/bundle/blogs-module.js deploy/bstack-sandbox/modules/blogs/

`deploy/bstack-sandbox/modules/blogs/` is mounted read-only at
`/app/modules/blogs`; `docker compose ... restart host` picks up a new bundle.
`BLOGS_STATE_DIR` and `BLOGS_KEEP_DAYS` must both be set (or neither) — the
compose file sets 90 days under `/var/lib/bbox/blogs`.

### Bfinger module bundle

Same convention, from a sibling checkout of the bfinger repo at v0.8.4:

    pnpm --dir ../bfinger/host bundle                    # writes ../bfinger/host/bundle/index.js
    mkdir -p deploy/bstack-sandbox/modules/finger
    cp ../bfinger/host/bundle/index.js deploy/bstack-sandbox/modules/finger/finger-module.js

`deploy/bstack-sandbox/modules/finger/` is mounted read-only at
`/app/modules/finger`. The module needs no extra env — the engine hands it
storage, headers and metrics; `tm_finger` and `ls_finger` come from the
bundle alone.

## Run the demo (mint tree → submit want → read → sweep → gone)

    CHAIN_URL=http://127.0.0.1:18344 HOST_URL=http://127.0.0.1:18091 \
      node packages/bstack/demo/live-sandbox.mjs

The blogs counterpart (log tree → 3 batches → ls_log read → mined anchor):

    CHAIN_URL=http://127.0.0.1:18344 HOST_URL=http://127.0.0.1:18091 \
      node packages/bstack/demo/live-log.mjs

And the bfinger one (finger tree → publish create → mine → resolve →
update → mine → resolve the newer seq):

    CHAIN_URL=http://127.0.0.1:18344 HOST_URL=http://127.0.0.1:18091 \
      node packages/bstack/demo/live-finger.mjs

Ports: devchain 127.0.0.1:18344, host 127.0.0.1:18091 (no public bind).
Tear down: `docker compose -f deploy/bstack-sandbox/compose.yaml down -v`.
Never point NEKTAR_* or the demo at mainnet/testnet — devchain only.
