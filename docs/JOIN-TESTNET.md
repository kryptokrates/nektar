# Join testnet — run an independent nektar hub

We need operators who are **not us**. Everything the network does on one box
is a simulation; the gate that matters is a second team running `deploy/hub/`
on their own hardware, on their own keys, somewhere we don't control.

This is the whole ask: a Linux box, Docker, a public address, ~30 minutes,
zero real money (testnet sats only — we'll fund you if you ask).

## What you're agreeing to run

One compose file brings up a self-contained hub:

| Piece           | What it is                                                        | Trust              |
| --------------- | ----------------------------------------------------------------- | ------------------ |
| `agent`         | the nektar MCP server + automation (announces, seeds, deals)      | yours — your keys  |
| `host-a`/`host-b` | overlay hosts carrying market/envelope/finger/directory lanes   | untrusted plumbing |
| `watch-a`/`watch-b` | wake shims — peers long-poll these instead of polling hosts   | untrusted plumbing |
| `db` + `shim`   | mysql for host state; a header shim proxying testnet endpoints  | untrusted plumbing |

Hosts can censor or withhold — never forge. Everything they serve is
signature- and SPV-verified by the reader, so a misbehaving host is detected,
not trusted.

## Requirements

- Any always-on Linux box with Docker + docker compose (a $5 VPS is plenty —
  ~1 GB RAM, ~10 GB disk).
- A public address peers can reach: a domain with TLS (`hub.example.com`) or
  a bare `https://` reverse proxy in front of ports 18391/18392 (Caddy
  example in `deploy/hetzner/README.md`). Plain IP+port works on testnet if
  TLS is a blocker — say so and we'll note the exception.
- A testnet funding WIF — generate one with any BSV wallet or
  `openssl rand -hex 32`-shaped key; fund it from a testnet faucet or ask us
  for a grant (~500k sats covers weeks of operation: tree mints, carriers,
  deals).

## Up

```bash
git clone https://github.com/kryptokrates/nektar.git && cd nektar
cp deploy/hub/.env.example deploy/hub/.env
# generate the four secrets: openssl rand -hex 32  →  KEY_PASSPHRASE,
#   HTTP_TOKEN, OVERLAY_DB_PASSWORD, OVERLAY_ADMIN_TOKEN
# set NEKTAR_MARKET_HOSTS to your hosts' PUBLIC urls + the seed pair:
#   https://<you>/hosts/a,https://<you>/hosts/b,https://nektar.run/hosts/a,https://nektar.run/hosts/b
# set NEKTAR_FUND_WIF to your funded testnet WIF
docker compose -f deploy/hub/compose.yaml --env-file deploy/hub/.env up -d
docker compose -f deploy/hub/compose.yaml logs -f agent
```

Healthy boot looks like: `automation: preflight` green, then
`automation: announce` within the first upkeep pass, then quiet periodic
lane polls.

## Verify you joined

1. **Your card is on the dir lane.** Ask your own host pair:
   `curl -X POST https://<you>/hosts/a/lookup -H 'content-type: application/json' -d '{"service":"ls_nektar","query":{"announces":{"net":"testnet"}}}'`
   → your pubkey in the results (appears after the first announce — the
   keep-alive re-announces until the card's funding proof lands, ~one
   block). `POST /v1/tools/doctor` on the agent (bearer `NEKTAR_HTTP_TOKEN`)
   is the one-call health read for everything else.
2. **We union you.** Tell us your `https://<you>/hosts/{a,b}` — a fresh
   instance here reads the directory, unions your hosts, and we confirm the
   union in our logs. That is the gate: independent operators found by the
   protocol, not by configuration.
3. **(Optional) seed a pack.** Set `NEKTAR_SEED_DOMAINS=ops,teranode` — your
   agent imports every announced pack in those domains at boot; a second
   fresh instance can then import them *from you*.

## What we ask in return

- **Keep it up ~1 week.** The thing we're measuring is uptime-independence:
  your hosts + announce must be reachable when you're not thinking about us.
- **Report crashes, refused carriers, and anything in `docker compose logs`
  that looks wrong** — a paste is enough; don't debug it for us.
- Testnet sats only: nothing you run here can spend real money. If the agent
  asks for `NEKTAR_MAINNET_CONFIRM`, stop — that's a misconfiguration.

## If it breaks

- `EAI_AGAIN ... 'db'` on the hosts: mysql raced the restart — the
  `restart: unless-stopped` policy self-heals in ~30s; if it doesn't,
  `docker compose restart db && docker compose restart host-a host-b`.
- Ports refusing externally but `curl` works on the box: NAT/forwarding —
  your announce advertises the PUBLIC urls, so fix the proxy, not the .env.
- `no funding slot`/`automation: reserve:` errors: the funding WIF is empty
  or its outputs are dust — top it up; the agent adopts new outputs on the
  next reconcile.

## What running this proves (and doesn't)

It proves the wire under real independence: your clock, your NAT, your key
custody, your latency. Fresh instances finding your card and unioning your
hosts is the network's own bootstrap working with no registry. It does not
yet prove operator economics or mainnet safety — that's the soak and the
`docs/JOIN-MAINNET.md` gates, and your week of uptime is one of the inputs.
