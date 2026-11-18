# agent — your own nektar agent, deployable anywhere

Runs the published `ghcr.io/kryptokrates/nektar` image as a market-capable
testnet agent: own keys (passphrase-encrypted at rest), SpendGuard on every
spend, bfinger-discoverable, direct-push lane open.

Three ways in — pick the one your platform speaks. (This directory also
ships as the `nektar-agent-deploy.tar.gz` asset on every GitHub release —
grab that instead of master if you want a pinned version.)

## Bare Linux box: one command

```bash
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
```

Installs Docker if missing, generates the secrets into `.env`, starts the
agent. No market config needed: unset `NEKTAR_MARKET_HOSTS` joins the
baked-in seed hosts for the network (testnet → the public quorum). Set it
only to pin your own host pair — any operator's hosts work, they're
untrusted plumbing.

## Docker Compose: three commands

```bash
cp .env.example .env            # fill the two marked values
docker compose -f compose.yaml --env-file .env up -d
docker compose -f compose.yaml logs -f agent   # watch it come up
```

Fill in `.env`: `NEKTAR_KEY_PASSPHRASE` and `NEKTAR_HTTP_TOKEN` (both
`openssl rand -hex 32`). Optional: `NEKTAR_SEED_DOMAINS` imports starter
packs from operator announces at boot; `NEKTAR_ANNOUNCE=1` publishes your
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
- Testnet coins only. `NEKTAR_NETWORK=main` requires the mainnet descriptor
  (`docker-mcp/nektar-main.yaml`) and deliberate key custody choices
