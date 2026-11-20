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

Fully self-contained — every service pulls a published image, nothing is
mounted from a checkout. `compose.yaml` + `.env.example` are the only files
you need:

```bash
cp .env.example .env            # set the marked values
docker compose -f compose.yaml --env-file .env up -d
docker compose -f compose.yaml logs -f agent
```

Or on kubernetes (k3s, minikube, EKS — the watch shim rides as a sidecar in
each host pod, not a supervised second process):

```bash
helm install hub deploy/hub/helm \
  --set marketHosts=https://hub.example/host-a,https://hub.example/host-b \
  --set secrets.keyPassphrase=… --set secrets.httpToken=… \
  --set secrets.overlayDbPassword=… --set secrets.overlayAdminToken=…
```

The one value that matters most is `NEKTAR_MARKET_HOSTS`: set it to your
hosts' **public** URLs (`https://hub.example/hosts/a,…`) — they go verbatim
into your announce and finger record for other operators to use. Include
the seed pair (`https://nektar.run/hosts/{a,b}`) in the same list so your
hub reads the whole network, not just its own lanes. A reverse proxy maps
`/hosts/a` → `:18391`, `/hosts/b` → `:18392` — a minimal Caddyfile is in
`docs/JOIN-TESTNET.md` (auto-TLS + path routing in ~5 lines).

## What it does

- `NEKTAR_ANNOUNCE=1` (default on) — the agent publishes a signed directory
  card on `tm_nektar_<net>_<dir-nibble>`: your hosts + the public packs you
  offer (`describeShard` + a bstore). Republished when content changes or
  exp runs down (~one carrier every 3.5 days).
- `NEKTAR_BFINGER_PUBLISH=1` — the counterparty record: hosts, office,
  stream, push endpoint.
- `NEKTAR_SEED_DOMAINS` — at boot, read the directory and `importShard`
  every announced pack matching your domains. Defaults to `*` (all);
  set a comma list to filter, or empty to keep a silo.
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

## Mainnet

`NEKTAR_NETWORK=main` is the one knob — it retargets the agent, the hosts'
served lanes (`NEKTAR_NETS`) and every `tm_nektar_main_*` topic together.
Two things must be true first:

- `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` — the runtime's real-sats ack gate.
- `SHIM_BROADCASTER` / `SHIM_INDEX` / `SHIM_CHAINTRACKS` — your own chain
  endpoints (self-hosted Teranode or an ARC-compatible service); the baked
  defaults are testnet and serve the wrong headers on main.
- Set all three SpendGuard caps (`NEKTAR_MAX_*`) — see `docs/OPERATIONS.md`
  "Going live" for sizing. There are no baked-in `main` seed hosts yet:
  until a second independent hub exists, main instances join via an
  explicit `NEKTAR_MARKET_HOSTS` — a main hub _is_ the first seed.
