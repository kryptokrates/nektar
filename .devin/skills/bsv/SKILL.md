---
name: bsv
description: >
  The operator's playbook for moving BSV coins fast and correctly —
  broadcasting, status handling, SPV/BEEF/BUMP evidence, zero-conf policy,
  funding stock, and wait-site design. Use when writing or reviewing coin-path
  code: a broadcast path, a status/proof consumer, a "why is this waiting"
  question, a UTXO/funding-stock change, an indexer/broadcaster integration,
  or any design that trades latency against chain truth. Grounded in the
  official BSV docs (bsvblockchain.com/docs, incl. their own build skill),
  the ARC/BRC wire shapes, and this repo's seams. For auditing truth claims
  use spv-strict; for the wider event plane use event-arch; for money-path
  invariants use code-review.
---

# bsv — move coins at network speed, never on faith

Two mandates, both absolute:

1. **Nothing is believed that we did not verify.** Bytes + Merkle path +
   headers we chose and checked. A txid, a status string, an index row is a
   hint that starts work — never a durable answer. (See `spv-strict`.)
2. **No step waits for a block that evidence would have made unnecessary.**
   Carried ancestry, pre-proven stock, pushed statuses, and `SEEN_ON_NETWORK`
   are all legal ways to proceed before `MINED` — at the right trust level.

The docs' own rule, verbatim, is the spine: _a payment is final at
`SEEN_ON_NETWORK` or later with no competing transaction; never wait for
confirmations or `MINED`._ Everything here serves that line without
violating mandate 1.

## The never-wait ladder

Apply in order. Each rung names what makes the wait unnecessary and the
trust it actually carries:

| Rung | Mechanism                                                                                      | What it proves                                   | Cost                                        |
| ---- | ---------------------------------------------------------------------------------------------- | ------------------------------------------------ | ------------------------------------------- |
| 0    | **Evidence travels with the tx** — BEEF/EF bytes + ancestry handed over at creation            | Receiver verifies without any fetch or block     | zero — just carry it                        |
| 1    | **Pre-proven stock** — funding trees minted & proven ahead of demand                           | A slot is already anchored when asked for        | none at use time                            |
| 2    | **Anchored stock** — unproven tree whose every input steps back to a BUMP anchor               | One-unconfirmed-hop carrier hosts admit now      | none — policy gate, `NEKTAR_ANCHORED_LANES` |
| 3    | **`SEEN_ON_NETWORK`** — first-party status of a tx we shaped, no competing spend               | Miners carry it; safe zeroconf for our own flows | one pushed event                            |
| 4    | **Pushed BUMP** — webhook/SSE `MINED` event → `deliverTxProof`, verified vs. witnessed headers | Mined, proven locally                            | the event RTT                               |
| 5    | **Settlement depth** — explicit `settleDepth`/n-blocks-on-top                                  | Finality as a security parameter                 | deliberate — never remove                   |

A poll is only ever the **ceiling** under rungs 0–4 (missed push, dead
endpoint, cold restart) — bounded, jittered, proof-gated. Never the
mechanism. (See `event-arch`.)

## The platform surface (full detail: [references/platform.md](references/platform.md))

- **Broadcast**: `POST {base}/tx`, raw or **EF** (EF preferred — the
  broadcaster validates fee/scripts without lookups). BEEF is refused —
  BEEF goes to **overlay `/submit`** instead. Batch: `POST /v1/txs`.
- **Statuses**: `SEEN_ON_NETWORK` is the zeroconf signal; `MINED` carries a
  BUMP; `IMMUTABLE` terminal. `PENDING_RETRY`/`TX_MISSING_PARENT` = fix the
  parent, keep retrying. `DOUBLE_SPEND_ATTEMPTED` lists `competingTxs`.
- **Push channels**: `X-CallbackUrl`+`X-CallbackToken` webhook (carries
  `merklePath`) or `GET /events?callbackToken=` SSE with `Last-Event-ID`
  resume. Both are implemented — `openHintStream`, `statusStream`,
  `handleBroadcasterStatus`.
- **Endpoints**: testnet `arcade-testnet-us-1.bsvblockchain.tech`; main
  `arcade-v2-us-1.bsvblockchain.tech` + `arcade-eu-1.bsvblockchain.tech`.
  `broadcasterUrls` fans a submit across all of them — first acceptance wins.
- **Reorg rule**: a re-mined tx pushes a **new BUMP** — replace the stored
  proof until `IMMUTABLE`. `unproveFrom` demotes dead-fork paths.
- **Ancestor limits are node policy, not consensus** — read them from
  `/policy`, never hardcode a hop count.
- **Toolbox UTXO lifecycle**: two statuses per tx (app `unproven →
completed`, Monitor `unmined → completed`); **unproven parents are
  spendable by default** — first-party zeroconf is the norm; the planner
  tiers `completed → unproven → sending`, which `FundsBase.take` mirrors
  (proven `merklePath` first). Outputs under `unprocessed` parents hide
  from `listOutputs`; `acceptDelayedBroadcast: false` is mandatory in our
  no-Monitor build.

## nektar seam map

| Concern               | Seam                                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------------------ |
| One write path        | `BsvBroadcaster` behind `SpendGuard` — compose in `livePort` only (check:arch)                               |
| Status answers        | `ChainPort.statusHint` → `probeTxStatus` (hint); `observe`/`TxFacts` (verdict)                               |
| Push statuses         | `openHintStream` SSE / `/v1/broadcaster-callback` webhook → `handleBroadcasterStatus` → `ChainEvents.note()` |
| Proof objects         | `proofLookup` → `merklePathMainnet`; push path `deliverTxProof` — both verified vs. `tracker`                |
| Funding stock         | `TreeLedger` (market/bbox/log/finger) — `anchoredTree` admissibility + `NEKTAR_ANCHORED_LANES`               |
| Coin book             | `funds.ts` — the one `take/spent/died/adopt` lifecycle; `utxoSpendStatus` burn oracle                        |
| Counterparty evidence | escrow wire carried in the deal note (`m.tx`); `deadAncestry` audit; seller rebroadcasts                     |

## Anti-patterns (find these on sight)

| ID  | Pattern                                                           | Why it fails                                        | Severity |
| --- | ----------------------------------------------------------------- | --------------------------------------------------- | -------- |
| B1  | Durable state transition on a status string (MINED/SEEN/REJECTED) | Statuses are claims — `spv/*` verdict order         | BLOCKER  |
| B2  | `sleep`/`setTimeout`/poll as the thing that makes progress        | Waits exist only as bounded ceilings                | BLOCKER  |
| B3  | Fetching a proof/tx the wire already carried                      | A fetch is a wait + a trust; carry the bytes        | HIGH     |
| B4  | Minting funding stock on demand                                   | A mint needs a block before it serves — stock ahead | HIGH     |
| B5  | Treating indexer "unspent"/"confirmed" as truth                   | Discovery ≠ truth; verify against own headers       | BLOCKER  |
| B6  | Single-endpoint broadcast on a multi-route net                    | A dead route is a stalled deal — fan out            | MED      |
| B7  | Hardcoded ancestor-depth/fee constants                            | Policy is per-node — ask `/policy`                  | MED      |
| B8  | Serial per-item index scans in a wait path                        | `discoverUtxos` workers — overlap the RTT           | MED      |
| B9  | Forgetting reorg proof replacement                                | A stored BUMP can die — replace until IMMUTABLE     | HIGH     |
| B10 | Trusting counterparty ancestry beyond your policy depth           | First-party vs. counterparty zeroconf differ        | HIGH     |
| B11 | Ordering spends without regard to parent-proof status             | Unproven parents drag ancestry into every spend     | MED      |

## Fast-path recipes

Concrete patterns with repo anchors: [references/recipes.md](references/recipes.md).

- **Broadcast**: build → sign → **EF submit** → attach evidence to the
  durable record → notify counterparty directly. `broadcast()` returns on
  first `ACCEPTED`; statuses arrive pushed.
- **Fund**: stock `lowWater` ahead of demand; refill during idle; prefer
  `anchoredTree` stock where the lane admits it.
- **Deal**: carry the escrow tx in the note; verify ancestry locally;
  rebroadcast in parallel with bookkeeping; disclose without a block;
  depth only for the money move.
- **Discover**: bounded-parallel index reads; verify every candidate's
  bytes hash to its claimed txid.
