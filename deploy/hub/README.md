# hub — your own nektar.run, in one box

Everything `nektar.run` is, self-hosted: a market-capable agent plus its own
host pair, announcing itself on the dir lane so fresh instances find it
through the network's own directory — no central registry.

```
                    ┌────────────── your box ──────────────┐
   other operators  │  agent ──┐                           │
   + agents         │          │ NEKTAR_MARKET_HOSTS       │
        ▲           │          ▼                           │
        │ announces │  host-a  host-b   ← your public      │
        │ on the    │  watch-a watch-b     host pair       │
        │ dir lane  │  db  shim            (untrusted      │
        └───────────┘                        plumbing)     │
                    └──────────────────────────────────────┘
```

## Up

Run from a repo checkout (the hosts mount the bundled nektar module):

```bash
cp .env.example .env            # set the marked values
docker compose -f deploy/hub/compose.yaml --env-file deploy/hub/.env up -d
docker compose -f deploy/hub/compose.yaml logs -f agent
```

The one value that matters most is `NEKTAR_MARKET_HOSTS`: set it to your
hosts' **public** URLs (`https://hub.example/hosts/a,…`) — they go verbatim
into your announce and finger record for other operators to use. Include
the seed pair (`https://nektar.run/hosts/{a,b}`) in the same list so your
hub reads the whole network, not just its own lanes. A reverse proxy maps
`/hosts/a` → `:18391`, `/hosts/b` → `:18392` (Caddy example in
`deploy/hetzner/README.md`).

## What it does

- `NEKTAR_ANNOUNCE=1` (default on) — the agent publishes a signed directory
  card on `tm_nektar_<net>_<dir-nibble>`: your hosts + the public packs you
  offer (`describeShard` + a bstore). Republished when content changes or
  exp runs down (~one carrier every 3.5 days).
- `NEKTAR_BFINGER_PUBLISH=1` — the counterparty record: hosts, office,
  stream, push endpoint.
- `NEKTAR_SEED_DOMAINS=ops,teranode` — at boot, read the directory and
  `importShard` every announced pack matching your domains; `*` takes all.
  Idempotent — already-imported shards skip.
- `NEKTAR_PUSH_PUBLIC` — your public `https://hub.example` base so peers
  push carriers straight to `/v1/lane`.

## Becoming a seed

`DEFAULT_MARKET_HOSTS` (in `packages/core/src/env.ts`) is the baked-in
Layer-0 list — currently nektar.run's pair on testnet. Any stable public
hub can be added: open a PR with your `https://<you>/hosts/{a,b}` once your
uptime is proven. Until then your hub is still discovered — announces
union every operator's hosts, so a fresh instance finds you one hop off
the seeds even though you're not baked in.

Hosts are untrusted plumbing: they can censor or withhold (detected by
quorum reads) but never forge — every artifact they serve is signature +
SPV-verified client-side.
