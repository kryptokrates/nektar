# Demo — a paid, provable AI answer in five minutes

Live deployment: `http://167.233.174.91:8080` — `MAMEM_NETWORK=ttn`
(public BSV testnet: real transactions, test coins), `MAMEM_PAYMENTS=1`.

What the audience sees: an AI answers a question, pays the operator in
real sats to get it committed, and the receipt it returns is verifiable
by anyone — no login, no trust in us.

## The arc

1. **Discovery** — `GET /.well-known/x402` (public, no auth): the
   priced-tool catalog, `scheme: exact`, `network: bsv:teratestnet`.
   *"The service publishes what it charges before you ask."*

2. **Ask** — any MCP client calls `groundedAnswer`. Unpaid → the tool
   returns a **402 invoice** (x402 v2 `PaymentRequired`): amount,
   operator address, `mamempay:<invoiceId>` tag.
   *"HTTP 402 — reserved since 1997, finally doing its job."*

3. **Pay** — the client signs a normal BSV tx with a WIF key: one
   output pays the operator ≥ the price, one `OP_FALSE OP_RETURN`
   carries the invoice tag. `payclient.ts` shows the whole thing in
   ~40 lines — no wallet protocol, no account.
   *"The signature IS the authorization — no API key, no session."*

4. **Committed answer** — the service verifies the tx, broadcasts it
   through its chain broadcaster, and the receipt **commits the
   settlement txid**: the anchored "usage receipt" literally names what
   paid for it. Answer ends with `receipt: mamemr1.…` + `verify: <url>`.

5. **Verify on a phone** — open the verify URL (browsers get the human
   page; curl gets JSON). Green `paid` line, anchored fragments, the
   committed answer to compare against what the AI showed.
   *"Anyone can check it — the check needs no credentials."*

## Run it

```bash
# scripted, against the deployed box (needs the bearer + a payer utxo):
MAMEM_REMOTE_URL=http://167.233.174.91:8080 MAMEM_HTTP_TOKEN=<bearer> \
  node packages/e2e/src/e2e-pay-remote.mjs

# or live from an agent: point the MCP client at
#   http://167.233.174.91:8080/mcp  (bearer auth)
# and ask anything — the 402 → pay → receipt flow runs itself.
```

A worked example minted on 2026-11-03 (still verifiable):
`http://167.233.174.91:8080/v1/verify?r=mamemr1.MExLewq-WdZeUIWDltJ80AasW29_SzHVq6yIpB35zGI`

## Honest talking points (say these out loud)

- **Testnet, not mainnet.** Real consensus, real txs, test coins. The
  same code path runs on main — it's gated behind
  `MAMEM_MAINNET_CONFIRM` and spend caps until a funded pilot.
- **"Accepted" isn't "mined".** A fresh payment shows `RECEIVED` at
  the broadcaster and mines with the next block. Don't call broadcast
  "on-chain" — the verify page shows confirmations honestly.
- **Chained unconfirmed spends get parked.** Teranode returns
  `PENDING_RETRY` ("parent not yet accepted") when a payment spends a
  still-unconfirmed utxo; the reaper rebroadcasts and it resolves once
  ancestors confirm. Production payers should spend confirmed utxos —
  our e2e deliberately chained them and hit exactly this.
- **Commitment ≠ truth.** The page says it: the receipt proves the
  answer was committed unaltered and derives from anchored records —
  it can't prove the answer is *right*. Model identity is *declared*,
  not zkML-proven.
- **Everything else is checkable without us.** The receipt token is a
  hash of the committed record; `/v1/verify` re-derives it publicly.

## If the demo box is down

`systemctl status mamem-http` on the box, or `journalctl -u
mamem-http -n 50`. The data dir `/opt/mamem/.mamem-ttn` persists
records, anchors, keys and the reserve across restarts — a restart
loses nothing.
