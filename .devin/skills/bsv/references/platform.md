# bsv platform reference — the service surface

Distilled from `bsvblockchain.com/docs/llms-full.txt` (BSV Broadcaster /
ARC API, overlays, wallet-toolbox internals, merkle service, node policy).
Cite these facts; re-verify endpoint URLs against the live docs before
shipping a pinned default.

## Endpoints

| Network | Route                                                                                           |
| ------- | ----------------------------------------------------------------------------------------------- |
| testnet | `https://arcade-v2-testnet-us-1.bsvblockchain.tech`                                             |
| main    | `https://arcade-us-1.bsvblockchain.tech`, `…-eu-1…`, `…-ap-1…`, `https://arcade.gorillapool.io` |

- The docs say plainly: _have a second route ready._ `broadcasterUrls`
  fans a submit across all of them; `broadcasterUrl` remains primary for
  `/events`, `/policy`, and read order.
- Chaintracks headers ride on the same hosts (`{base}/chaintracks/v2`).

## Submission (`POST /tx`)

- Body: **raw tx** or **EF** (Extended Format). EF is preferred — the
  broadcaster validates fee + scripts + ancestry locally without UTXO
  lookups, which is why `BsvBroadcaster.broadcast` sends `tx.toEF()`
  whenever every input carries `sourceTransaction`.
- **BEEF is refused here** — BEEF belongs to overlay `/submit`, not the
  broadcaster.
- Batch: `POST /v1/txs` for several txs in one call (headers shared).
- Headers that matter:
  - `X-CallbackUrl` — webhook target for status pushes
  - `X-CallbackToken` — tags submissions so only our statuses reach us
  - `X-FullStatusUpdates: true` — intermediate statuses, not just terminal
- Response `202` = accepted for delivery (a miner may not have it yet).
  Follow status to `SEEN_ON_NETWORK` / `MINED`.

## Status lifecycle

`RECEIVED → SENT → ACCEPTED → SEEN_ON_NETWORK → MINED → IMMUTABLE`

| Status                                            | Meaning                                              | Action                                                                    |
| ------------------------------------------------- | ---------------------------------------------------- | ------------------------------------------------------------------------- |
| `ACCEPTED` / `SENT`                               | validated, propagating                               | proceed on rung 3 only if policy allows                                   |
| `SEEN_ON_NETWORK`                                 | in a miner's subtree                                 | **zeroconf-safe for first-party money** — no competing tx                 |
| `MINED`                                           | included; response may carry `merklePath` (BUMP hex) | attach + verify vs own headers                                            |
| `IMMUTABLE`                                       | buried                                               | terminal                                                                  |
| `PENDING_RETRY` / `TX_MISSING_PARENT`             | parent not resolvable                                | fix ancestry, resubmit — it keeps retrying                                |
| `REJECTED` / `DOUBLE_SPEND_ATTEMPTED`             | refused; competingTxs listed on double-spend         | first-party death evidence only where the port declares it (`isRejected`) |
| `SEEN_IN_ORPHAN_MEMPOOL` / `MINED_IN_STALE_BLOCK` | fork casualties                                      | treat as unproven — re-anchor                                             |

- **Reorg**: a re-mined tx pushes a NEW `MINED` with a NEW BUMP —
  **replace the stored proof** until `IMMUTABLE`.
- `400` verdicts are final except code `476` (locktime) which retries.
- `503` = backpressure, nothing queued — retry the SAME bytes
  (resubmission is idempotent).

## Push channels (choose one)

1. **Webhook** — `X-CallbackUrl` + `X-CallbackToken` at submit time;
   broadcaster POSTs `{txid, txStatus, merklePath?, blockHeight?}` to it
   (arc-ingest shape). Needs a public URL — the hub path.
2. **SSE** — `GET {base}/events?callbackToken=<t>` streams the same
   payloads; `Last-Event-ID` resumes (ids are nanosecond-ordered).
   Outbound-only — the agent path (`statusStream` + `openHintStream`).

Both deliver `merklePath` on `MINED` — that is the pushed-BUMP rung; the
handler verifies the root against witnessed headers before `observe()`.

## Overlay submit (`POST /submit`)

- Body: **BEEF** + `x-topics` header (BRC-22). Returns a **STEAK**
  (per-topic admitted outputs).
- Lookups (`POST /lookup`, BRC-24) return BEEF — verify before using.
- GASP sync keeps hosts eventually consistent — our bstack hosts
  already run this plane; lanes deliver carrier BEEFs over it.

## Node policy, not consensus

Ancestor count/depth, min fee, max tx size, dust are **per-miner local
settings** — read live values from `GET {base}/policy`; never hardcode a
hop bound or a fee floor from memory. `feeRateSatPerKb` takes
`max(floor, /policy quote)` at `livePort` boot for exactly this reason.

## Merkle service `/watch`

Self-hosted service: register `{txid, callbackUrl}` → it POSTs the BUMP
when the tx mines. Useful only when running own infra and watching
counterparty txs we did not broadcast (our own txs already push via
webhook/SSE). Not currently deployed — see recipes F6 direction.

## Payment-evidence statement (BRC, ~docs §21000)

A canonical JSON a receiver can emit recording what it verified about an
**unmined** payment: anchored ancestors, longest/shortest unconfirmed
distance to an anchor, per-input script/amount/lock checks, observed
status. Nearly our `evidence: {spv, spends}` transition record
standardized — adopt its shape when the escrow-verification record next
changes, for interop.

## Wallet-toolbox UTXO lifecycle (mirrors `funds.ts`)

`transactions.status ∈ {unprocessed, unproven, proven}` + `proven_tx_reqs`
carrying to-be-anchored txs; change is spendable immediately;
`internalizeAction` adopts wallet-payment outputs; Monitor tasks are the
toolbox's own polls. Our `FundsBase` keeps the same two-tier shape —
`unproven` is spendable-first-party, `proven` is BUMP-anchored.

## Network naming (docs §networks)

`main`/`testnet`/`teratestnet` are distinct coin universes; packages
default to main — **pass the network in every call that takes one**
(code on main by mistake spends real BSV). Keys are net-agnostic; the
address prefix is the only visible tell.
