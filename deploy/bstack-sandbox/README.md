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

## beefcat — the reference implementation as a conformance client

`deploy/bstack-sandbox/beefcat/` builds `lightwebinc/beefcat` v0.1.1 from
pinned source — Lightweb's second implementation of the same carrier
pattern our TS code vendored. Its verifier accepts a generic header source
of exactly the shape our shim serves (`GET /v1/tip` → `{height}`,
`GET /v1/root/<h>` → `{merkleRoot}`, 404 = unknown), so pointing it at the
shim makes an independent implementation a client of our witnessed anchor:
any drift in the shim's contract shows up as their verifier failing, not
ours.

Opt-in `conformance` profile on both sandbox composes:

    # regtest (needs the Teranode stack + NEKTAR_RPC_* exported):
    docker compose -f deploy/bstack-sandbox/compose.regtest.yaml \
      --profile conformance run --rm beefcat

    # testnet:
    docker compose -f deploy/bstack-sandbox/compose.testnet.yaml \
      --profile conformance run --rm beefcat

The smoke phases:

1. mint a carrier stream on a **devchain inside the container** (nothing
   real) and verify it against devchain's own headers — proves the tool;
2. verify the same stream **against our shim** — must refuse: the devchain
   parent's BUMP is foreign to our chain. An exit 0 here means the shim
   lied or the verifier skipped its anchor check; both fail loudly;
3. (regtest only, `BEEFCAT_NODE_RPC`/`BEEFCAT_ASSET` wired) mint real
   postage on our Teranode and verify through the shim — a second
   implementation's carrier workload against our node + anchor. If the
   node's `generatetoaddress` is unavailable it reports a skip, not a
   failure.

Operator shell for real flows (persistent identity/pool on the
`beefcat-home` volume):

    docker compose -f ... --profile conformance run --rm --entrypoint sh beefcat
    beefcat init -network test -headers http://shim:8080   # prints the fund address
    beefcat status                                        # sources + postage

The repo-side counterpart is `packages/bstack/tools/beefcat-probe.mjs`,
which runs OUR seams on THEIR stream bytes (same vectors, both directions).
