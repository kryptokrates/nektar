# nektar for Codex — the complete manual

You're connecting your agent to a memory it **cannot silently rewrite**,
answers that arrive **pruned to what provable records support**, and a
receipt **anyone can check on their phone** — paid for per-call in real
sats, with no account and no subscription.

A live public instance runs at `https://nektar.run` on the BSV
public testnet (real transactions, test coins).

---

## What a blockchain-less AI gives you today

- **An answer.** Maybe a citation. Nothing you can _check_ — the
  citation asserts the source exists; it doesn't prove it.
- **Memory in the vendor's database.** Mutable, invisible, unportable.
  The vendor's "memory feature" can be edited, aged out, or deleted —
  you'd never know.
- **No record of what it said.** Yesterday's answer is gone; if the
  model contradicts itself, there is nothing to compare.
- **No proof for third parties.** A screenshot of a chat proves
  nothing — it's trivially fakeable.
- **No grounding check.** You cannot verify whether the answer came
  from _your_ documents or was confabulated from weights.
- **Account-based billing.** A subscription metered by the vendor, on
  the vendor's numbers, audited by nobody.
- **All-or-nothing sharing.** Showing someone one answer means
  exposing the conversation around it.

## What you get here instead

|                      | today                      | with nektar                                                                                                                  |
| -------------------- | -------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| **Memory**           | mutable, vendor-held       | append-only log; every batch anchors a Merkle root on-chain — edits break the proofs                                         |
| **Answers**          | asserted                   | `groundedAnswer` strips every claim it can't derive from verified fragments — the answer is _pruned_ to what records support |
| **Proof for others** | screenshots                | an anchored, signed receipt — a URL anyone opens, a QR anyone scans, no account needed                                       |
| **Billing**          | trust our metering         | per-call atomic payment; the receipt names the settlement txid — **the receipt log IS the invoice**, publicly auditable      |
| **Portability**      | dies with the subscription | shard keys are yours; records export and re-verify independently                                                             |
| **Disclosure**       | show everything or nothing | prove one claim exists in memory without revealing the rest                                                                  |
| **Trust**            | vendor says so             | receipts verify against a public ledger the service doesn't control                                                          |

The honest boundary, printed on every verify page: this proves the
answer was **committed and is derivable from sealed records** — never
that it's _true_. Model identity is _declared_, not zkML-proven.

---

## Pick your journey

|                                      | setup                          | what you can do                                                                                                |
| ------------------------------------ | ------------------------------ | -------------------------------------------------------------------------------------------------------------- |
| **A. Watch + verify**                | none                           | open a receipt link on your phone — see the whole verify surface                                               |
| **B. Free client on the hosted box** | bearer token from the operator | `remember`, `recall`, `recall`-verified proofs, `payQuote`, 30+ tools — everything except the three paid tools |
| **C. Paid client on the hosted box** | bearer + a funded testnet WIF  | the full arc: 402 invoice → your signed tx → receipt that names your txid                                      |
| **D. Your own instance**             | clone + build                  | everything, offline, on the simulated chain — your own keys, your own data dir                                 |

### A — Watch + verify (zero setup)

Open this on your phone:
`https://nektar.run/v1/verify?r=nektarr1.MExLewq-WdZeUIWDltJ80AasW29_SzHVq6yIpB35zGI`

That's a real paid-answer receipt minted on the live box: the committed
answer, the cited records, the `paid` check naming the settlement tx.
No login — verification is public by design.

### B — Free client on the hosted box

Ask me (the operator) for the **bearer token** — it gates `/mcp`, which
is the only thing that isn't public. In `~/.codex/config.toml`:

```toml
[mcp_servers.nektar]
url = "https://nektar.run/mcp"
bearer_token_env_var = "NEKTAR_HTTP_TOKEN"
```

```sh
setx NEKTAR_HTTP_TOKEN <token>     # Windows: persists to your user env
# or: export NEKTAR_HTTP_TOKEN=<token>  (per-shell)
```

Codex picks up `AGENTS.md` from the project root automatically — this
repo's copy already instructs recall-first / groundedAnswer /
receipt-footer behaviour. In another project, copy that block (it's at
the top of `AGENTS.md`).

Try it: `remember` a fact (`{"shard":"demo","data":"…"}`), wait ~a
minute for the testnet batch anchor, `recall` — the record returns
**verified**. That flag is the part no blockchain-less memory can fake.

### C — Paid client on the hosted box

Everything in B, plus the paid tools (`anchor` 100 sats,
`groundedAnswer`/`commitReceipt` 250 sats). You need a **testnet WIF
with sats** — ask me and I'll send test coins to an address you
generate (or hand you a pre-funded WIF for the demo).

The flow in-session:

1. Agent calls `groundedAnswer` → gets a **402 invoice**
   (`{paymentRequired}` with `accepts[].extra.invoiceId`,
   `lockingScriptHex`, `opReturn`, `amount`).
2. Sign the payment locally — one command, needs the repo built
   (`pnpm -r build`):

   ```sh
   node tools/pay.mjs '<paste the paymentRequired JSON>' \
     --wif <your WIF> --parent-tx-hex <funding tx hex> \
     --parent-vout <N> --parent-sats <N>
   ```

   It prints `{ "invoiceId": …, "transaction": "<BEEF hex>" }` — retry
   the same call with `payment:` set to that object. (Codex can run it
   as a shell step; the WIF never leaves your machine — the service
   only sees the signed tx.)

3. The service verifies the tx (pays the operator ≥ amount + carries
   the invoice tag), broadcasts it, and the receipt **commits your
   txid**. `verify:` link → green `paid` line, auditable on the public
   testnet.

One tx pays for exactly one call — invoice-tagged, replay-protected.

### D — Your own instance (offline, zero funds)

```sh
git clone https://github.com/kryptokrates/nektar && cd merkle-automaton
pnpm install && pnpm -r build
```

`~/.codex/config.toml` (HTTP mode — also gives you `/v1/verify`):

```toml
[mcp_servers.nektar]
url = "http://localhost:8080/mcp"
```

```sh
NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=mock node packages/mcp/dist/src/bin.js
```

Or pure stdio (no port, Codex spawns it):

```toml
[mcp_servers.nektar]
command = "node"
args = ["<abs path>/packages/mcp/dist/src/bin.js"]
env = { NEKTAR_NETWORK = "mock" }
```

`mock` is fully offline — the simulated chain auto-mines, receipts mint
and verify, anchors exist on the mock ledger (honest caveat: they
aren't on a public chain). `NEKTAR_NETWORK=regtest` upgrades to a local
Teranode; `ttn` to the public testnet.

## The receipt auto-check (optional, local runs)

Codex `notify` fires on each completed turn —
`integrations/codex/verify-notify.mjs` greps the last message for
`nektarr1.` tokens, verifies each against `/v1/verify`, and appends to
`codex-receipts.log`. It's fire-and-forget (it can flag failures, not
block the turn — honest boundary vs Claude Code's Stop hook). Setup:
`integrations/codex/README.md` + `config-snippet.toml`.

---

## The tools you'll actually use

| tool                 | what for                                                                                     |
| -------------------- | -------------------------------------------------------------------------------------------- |
| `remember`           | commit a fact to a shard — append-only, anchors on-chain                                     |
| `recall`             | read a record back — `unverifiable` until its anchor is proven                               |
| `groundedAnswer`     | answer claims, each citing a fragment; ungrounded claims stripped; returns the receipt token |
| `commitReceipt`      | commit an externally-produced answer receipt                                                 |
| `verifyReceipt`      | re-check a receipt token programmatically                                                    |
| `payQuote`           | get a 402 invoice for a paid tool without calling it                                         |
| `anchor`             | force the shard anchor now (paid — buys immediacy; batching is free)                         |
| `modelManifest`      | declare which model answered (declared, not proven)                                          |
| `checkpoint`/`proof` | shard Merkle root + a record's inclusion proof                                               |

Full list: `tools/list` over `/mcp`, or `GET /v1/tools` on the box.

## Honest boundaries — say these too

- **Testnet.** Real consensus, real txs, test coins. Mainnet is the
  same code path, gated (`NEKTAR_MAINNET_CONFIRM` + spend caps).
- **"Accepted" ≠ "mined".** A payment shows `RECEIVED` then mines in a
  block; chained unconfirmed spends get parked (`PENDING_RETRY`) —
  spend confirmed utxos.
- **Commitment ≠ truth.** The receipt proves committed + derivable.
- **Model is declared, not proven** — zkML/TEE is the honest roadmap,
  documented in the README.
- **Bearer protects writes**, not reads: `/v1/verify`, `/v1/health` and
  `/.well-known/x402` are public by design — that's what makes the
  receipts checkable by anyone.
