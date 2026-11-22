# agent — your own nektar agent, deployable anywhere

Runs the published `ghcr.io/kryptokrates/nektar` image as a market-capable
testnet agent: own keys (passphrase-encrypted at rest), SpendGuard on every
spend, bfinger-discoverable, direct-push lane open.

Three ways in — pick the one your platform speaks. (This directory ships
inside the `nektar-deploy.tar.gz` asset on every GitHub release — `agent/`
here, `hub/` beside it if you want to run market hosts too. Grab the
release tarball instead of master if you want a pinned version.)

## What lives where (mainnet)

You host **one thing**; the rest is shared or public infrastructure you
verify, never trust:

| Piece                | What it is                                     | Hosted where                                     | Released as                                                                             |
| -------------------- | ---------------------------------------------- | ------------------------------------------------ | --------------------------------------------------------------------------------------- |
| **Your agent**       | identity, treasury, journals, HTTP/MCP surface | **your box** (VPS/laptop)                        | `ghcr.io/kryptokrates/nektar` image · `nektar-deploy.tar.gz` (`agent/`)                 |
| **Executor**         | Restate sidecar — required                     | same box/pod as the agent                        | `docker.restate.dev/restatedev/restate` (upstream)                                      |
| **Agent state**      | keys, sqlite, lane stock                       | the `agent-data` volume/PVC                      | — (your backups)                                                                        |
| **Market hosts**     | overlay envelope transport                     | shared/public hosts, or your own                 | `ghcr.io/kryptokrates/nektar-{bbox-host,shim,hub-db}` · `nektar-deploy.tar.gz` (`hub/`) |
| **Chain truth**      | indexers + broadcasters                        | public internet (WoC, banana, GorillaPool, TAAL) | — (SPV-verified, never trusted)                                                         |
| **Restate journals** | durable executor state                         | the `restate-data` volume/PVC                    | —                                                                                       |

Mainnet has **no baked seed hosts** (deliberately — a single-operator seed
list is the centralization the design avoids): set `NEKTAR_MARKET_HOSTS` to
a public pair, or run your own (`hub/` sits beside `agent/` in the same
`nektar-deploy.tar.gz`). Without it the agent still does
memory/anchors/receipts — it just has no market.

## Bare Linux box: one command

```bash
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
```

Installs Docker if missing, generates the secrets into `.env`, starts the
agent. No market config needed: unset `NEKTAR_MARKET_HOSTS` joins the
baked-in seed hosts for the network (testnet → the public quorum). Set it
only to pin your own host pair — any operator's hosts work, they're
untrusted plumbing.

Mainnet one-liner (real BSV — the confirm is required, the hosts are too):

```bash
NEKTAR_NETWORK=main NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND \
NEKTAR_MARKET_HOSTS=http://host-a:8080,http://host-b:8080 \
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
```

## Docker Compose: three commands

```bash
cp .env.example .env            # fill the two marked values
docker compose -f compose.yaml --env-file .env up -d
docker compose -f compose.yaml logs -f agent   # watch it come up
```

Fill in `.env`: `NEKTAR_KEY_PASSPHRASE` and `NEKTAR_HTTP_TOKEN` (both
`openssl rand -hex 32`). `NEKTAR_SEED_DOMAINS` defaults to `*` — imports
every announced pack at boot (set empty to keep a silo); `NEKTAR_ANNOUNCE=1` publishes your
directory card so others can find you.

## Kubernetes / k3s / EKS / GKE: Helm

```bash
helm install my-agent ./helm \
  --set secrets.keyPassphrase=$(openssl rand -hex 32) \
  --set secrets.httpToken=$(openssl rand -hex 32) \
  --set-string 'marketHosts=http://host-a:8080\,http://host-b:8080'
```

(`marketHosts` is comma-separated — escape each comma or use `-f` with a
values file; helm splits unescaped commas into separate keys.)

The chart runs agent + Restate sidecar **in one pod** — loopback URIs are
correct there (shared network namespace), which retires the whole
container-DNS trap class. Each keeps its own PVC (`data`, `restate-data`).
Ships hardened: startup/liveness/readiness probes (`/healthz` public by
design, `/health` on the sidecar's admin port), non-root agent
(uid 1000 + fsGroup so it can write its claims), no SA token mount,
resource requests/limits — verified on k3s with the pod reaching 2/2
behind real readiness gates.

`helm/values.yaml` exposes everything — image tag, persistence
(`StatefulSet` + PVC for `/data`), `secrets.existingSecret` for your own
Secret, ingress + `pushPublic` for direct push, SpendGuard caps, tuning.

## After it's up

- To be reachable for direct push, set `NEKTAR_PUSH_PUBLIC` to your public
  `https://<host>/v1/lane` (Helm: `pushPublic` + `ingress.enabled=true`) and
  publish a finger record (`NEKTAR_BFINGER_PUBLISH=1` / `bfingerPublish`)
- First boot prints a **fee address** — send it testnet coins for anchoring
  (`NEKTAR_FUND_TXIDS=<txid|hex:rawtx>` after funding, or `NEKTAR_FUND_WIF`
  before first boot so the wallet derives from your key instead)
- MCP (Codex/Claude): `https://<host>/mcp` with
  `Authorization: Bearer $NEKTAR_HTTP_TOKEN`
- Tools: `remember`, `recall`, `prove`, `groundedAnswer`, `want`, `sell`,
  `deals`, `fund` — the market engine runs autonomously once funded
- Public surfaces (no bearer, by design): `/v1/lane`, `/v1/pack/{shard}`,
  `/.well-known/nektar`, `/v1/verify`

## Run your own market hosts too (the decentralized part)

Hosts are untrusted plumbing — everything they serve is signed and
SPV-verified client-side, so the more independent operators the better.
This repo ships the same pair the public testnet runs:

```bash
docker compose -p my-bstack -f deploy/bstack-sandbox/compose.testnet.yaml up -d
```

Then point `NEKTAR_MARKET_HOSTS` (and your finger record) at them. Agents
union the record's hosts with their own, detect withholding, and repair
missed legs — a host can censor, never forge, and never take the network
with it.

## Alternative: the MCP toolkit descriptor

`docker-mcp/nektar.ghcr.yaml` is the same image packaged for toolkit-style
deployments (what `nektar.run` runs) — use whichever your platform speaks.

## Notes

- `/data` holds identity, wallet and journals — back it up; it _is_ the agent
  (compose: the `agent-data` volume; Helm: the PVC)
- `NEKTAR_UNATTENDED=` / `unattended=false` means market approvals queue for
  an operator decision — recommended until you trust the policies
- `NEKTAR_NETWORK=main` works from this same compose/chart — it needs
  `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND`, a `NEKTAR_MARKET_HOSTS` pair (no
  baked seeds on main), and deliberate key custody. The toolkit-style
  alternative is `docker-mcp/nektar-main.yaml`
