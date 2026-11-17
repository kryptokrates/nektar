# selfhost — your own nektar agent in three commands

Runs the published `ghcr.io/kryptokrates/nektar` image as a market-capable
testnet agent: own keys (passphrase-encrypted at rest), SpendGuard on every
spend, bfinger-discoverable, direct-push lane open.

```bash
cp .env.example .env            # fill the three marked values
docker compose -f compose.yaml --env-file .env up -d
docker compose -f compose.yaml logs -f agent   # watch it come up
```

Fill in `.env`: `NEKTAR_KEY_PASSPHRASE`, `NEKTAR_HTTP_TOKEN` (both
`openssl rand -hex 32`), and `NEKTAR_MARKET_HOSTS` (any operator's bstack
hosts — or your own, below). To be reachable for direct push, set
`NEKTAR_PUSH_PUBLIC` to your public `https://<host>/v1/lane` and publish a
finger record (`NEKTAR_BFINGER_PUBLISH=1`).

First boot prints a **fee address** — send it testnet coins for anchoring
(`NEKTAR_FUND_TXIDS=<txid|hex:rawtx>` after funding, or set `NEKTAR_FUND_WIF`
before first boot so the wallet derives from your key instead).

## Talk to it

- MCP (Codex/Claude): `https://<host>/mcp` with `Authorization: Bearer $NEKTAR_HTTP_TOKEN`
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
- `NEKTAR_UNATTENDED=` left empty means market approvals queue for an
  operator decision — recommended until you trust the policies
- Testnet coins only. `NEKTAR_NETWORK=main` requires the mainnet descriptor
  (`docker-mcp/nektar-main.yaml`) and deliberate key custody choices
