# Automation debt — mainnet field report

Every item below was hit by hand on real mainnet (Nov 2025, heights ~974780–974820).
Ordered by time lost, not by code size.

**Status key:** `[done]` shipped in this pass · `[part]` partial · `[ ]` open

## F1 — Reserve drift vs chain truth  *(dominant cost)*

`reserve.json` is optimistic: spends mark inputs `spent` and append change
entries at *broadcast time*, never at *verdict time*. On mainnet this produced
four classes of damage:

- The broadcaster returned non-rejected 202s, then later marked the tx `REJECTED`
  (`UTXO_SPENT`, `parent rejected`) — dead txs stayed in the reserve as live
  entries; the next spend picked them and was orphaned. ~19 entries poisoned.
- `payInvoice` marks its input spent + adds change via `addOwned` — **and
  never broadcasts**. Each unbroadcast pay tx permanently consumed an entry.
- `takeReserve` picks the *first* unspent entry with no size check — it chose
  a 700-sat dust pair for a 10,600-sat bond → `negative fee -9900`, and a
  9,299-sat entry for a 20,600-sat pay → `insufficient`.
- Manual file repair is overwritten by the shutdown flush — the instance
  re-persists its in-memory spent set on SIGTERM, so edits must be made
  while stopped (discovered the hard way, twice).

**Fixes**

- `[done]` `fundingRefresh` tool: queries `address/{fee}/unspent` → revives
  outputs that are still live but spent-marked (dead unbroadcast txs),
  marks entries actually spent on-chain, drops entries whose parent tx
  the broadcaster reports REJECTED (and revives their inputs that are still live).
  On-demand call — a timed/triggered run is still open.
- `[done]` `takeReserve(minSats)`: smallest-sufficient-first (≥ need), never
  dust; `spendCheck` callback prunes chain-spent candidates.
  Confirmed-parent preference (store `height` at import) still open.
- `[part]` Broadcast verdict: covenant `spend()` throws on synchronous
  REFUSED, and `sendSats`/deploy check `br.status` before marking spent /
  advancing state. The post-accept async-REJECTED watch (~60 s poll of
  broadcaster `/tx/{txid}` + rollback) is still open — `fundingRefresh` is the
  manual recovery until it lands.
- `[done]` `sendSats` (below) broadcasts directly — the unbroadcast
  `payInvoice` path no longer needs to consume entries by hand. Making
  `payInvoice` itself not consume without airing is still open.
- `[done]` Bond covenant spends serialize on `bondBusy` — a manual
  `bondCommit` can no longer race the watchdog/poll into a double-spend
  (`5602caec` vs `ed7052c2` was exactly this). Rejected spends no longer
  advance `bondHead`.

## F2 — `sendSats` / self-funding

Funding agent→agent today: base58-decode the payee address, hand-build the
locking script, call `payInvoice` (which doesn't broadcast), decode BEEF,
re-encode, POST to WoC, then `fundingImport` on the payee — which 0-imports
until the tx indexes.

**Fixes**

- `[done]` `sendSats {address|scriptHex, amount, opReturn?}` — builds,
  signs, broadcasts, marks the input spent, tracks change. Returns txid.
- `[done]` `fundingRefresh` (F1) — on-demand reconcile + auto-import of new
  outputs at the fee address. A scheduled run is still open.
- `[ ]` `fundingImport` accepting BEEF/EF directly — not yet.

## F3 — Covenant arg math belongs inside the ops

`escrow`, `complete`, `refund`, `bondCommit`, `bondSeize`, `bondClose` all
carry a contract fee bound (`residual ≤ maxFee + N`). Callers must guess
`changeSats`; every path failed once with `unlocking scripts are invalid`
until the bound was reverse-engineered (refund needed changeSats ≥ 500).

**Fixes**

- `[done]` Auto-size: `complete` defaults `changeSats = utxo − price −
  min(maxFee − 2, 350)`; `refund` defaults `changeSats = utxo −
  min(maxFee, 350)`. Callers may still override explicitly.
- `[done]` `depositorPkh` defaults to the operator key's pkh on `escrow`,
  `complete`, and `refund` — the raw offer cfg works unmodified.
- `[ ]` `dryRun: true` on every covenant op — build + local-eval (the
  `Spend` interpreter already exists) and return `{valid, fee, residual,
  bound}` before airing.

## F4 — Parent-tx fetch at spend time

`bondCommit`/`bondClose`/`bondSeize` fetch the parent tx from WoC `/hex` at
spend time; a fresh deploy 404s for minutes → the commit dies.

**Fixes**

- `[done]` `txHex` cache persisted in `state.json`: every bond broadcast
  records its raw hex (bounded, last 64); `fetchParentTx` consults it
  before the network and survives restarts.
- `[done]` the instance transaction graph (`TxStore`) replaced the indexer
  hot path: parents for covenant spends, and the BEEF/EF a counterparty
  needs, come from the graph the instance already holds — `fetchParentTx`
  remains only as a last-resort fallback for transactions the instance was
  never handed. Deal messages (accept, delivered, recovery notes) carry the
  transaction itself; receivers run SPV against their own headers and keep
  the ancestry, so a fresh broadcast never needs an indexer to become usable.

## F5 — Chain-time waits as runtime tasks

MTP lag on refunds (~60–90 min), releaseHeight on close, seize deadline —
today each needed a hand-rolled `ssh`-polled python script armed manually.

**Fixes**

- `[ ]` In-service scheduler: `refund`, `bondClose`, `bondSeize` accept
  `{when: {height|time}}` — the runtime persists the pending op and fires
  it when the condition passes. Uses the **broadcaster callback** path already
  in `mainnet.ts` (`X-CallbackUrl` → `/v1/broadcaster-callback`) for block/tx
  events — push, not polling.
- `[ ]` A `tasks` tool to list/cancel pending scheduled ops.

## F6 — Service lifecycle

Repeated failure mode: stale `lock`/`instance.lock`, multiple `bin.js`
procs holding ports, shutdown flush overwriting repairs.

**Fixes**

- `tools/service-run.mjs` + installer (already in progress): single-owner
  enforce, lock cleanup, `restart` verb, readiness gate — `/well-known`
  answers 503 until reserve+state load, not silent 200s on a broken state.

## F7 — Observer discovery

`bondSeize(bond)` needs a hand-copied `{cfg, utxo}` — a real observer finds
the bond itself.

**Fixes**

- `[ ]` `bondScan {address|txid}` — match live `NektarCheckpointBond`
  outputs by script pattern (code-part prefix is fixed); returns
  `{cfg, utxo}` ready for `bondSeize`. Turns the switched-off-seller demo
  into a one-call op.

## F8 — Evidence bundle

Txids were logged truncated; statuses checked one-by-one.

**Fixes**

- `[ ]` `tools/e2e-mainnet.mjs` — runs the whole suite (escrow→complete→claim,
  refund, bond deploy→commit→close, alice-dark, S1 marketplace, delegation,
  attest/federation) with retry/indexing-wait logic inside, and emits
  `evidence.json`: `{txid, role, status, confs}` for every chain artifact.

## F9 — Error surface

`publishWant` → `"no rv accepted"` for a net-mismatch rejection;
`no funding UTXO` for a spent-marked entry; `Transaction inputs are
insufficient` for a dust pick.

**Fixes** — `[ ]` annotate with cause: `rv rejected: net mismatch
(rv=testnet, artifact=main)`; `no funding UTXO ≥ 20600 sats (3 live, all
spent-marked)`; `insufficient: picked 9299-sat entry for 20600-sat spend`.
(`bond op in flight` landed with the mutex; the rest are open.)

## F10 — Deterministic rebuild collision

`bondDeploy` retry rebuilt the identical tx (same input → same txid) and
reported it as a fresh deploy, hiding that nothing new aired.

**Fix** — `[ ]` detect `head.txid === lastDeployTxid` and return
`{txid, deduplicated: true}`.

---

## Ranked implementation order

| # | Item | Kills | Effort | Status |
|---|------|-------|--------|--------|
| 1 | `fundingRefresh` + smallest-sufficient `take` + bond mutex | F1 | M | done (async broadcast verdict loop open) |
| 2 | Auto-size covenant args + depositorPkh default + dryRun | F3 | S | done except dryRun |
| 3 | `sendSats` + `fundingRefresh` | F2 | S | done |
| 4 | parentHex in state + fetch fallback | F4 | S | done (broadcaster/peer fallback open) |
| 5 | Scheduled ops on broadcaster status callback (`when:`) + tasks tool | F5 | M | open |
| 6 | `e2e-mainnet` runner + evidence.json | F8 | M | open |
| 7 | bondScan | F7 | S | open |
| 8 | service-run/installer + readiness gate | F6 | M | in progress |
| 9 | Error-surface annotations | F9 | S | open |
| 10 | Deploy dedup | F10 | XS | open |

Items 1–4 remove every manual intervention hit today — the first four rows
are shipped (with the noted exceptions); 5–8 remove the hand-rolled
scaffolding; 9–10 are polish. Regressions live in
`packages/mcp/test/automation.test.ts`.

## Autonomous marketplace — approve a policy once, then publish wants / enable offers

The runtime does the trade; the operator approves the limits. Nothing here
spends outside an approved policy, and policies/exception approvals are decided
from the operator CLI only — the `automation` MCP/HTTP tool cannot approve.

```sh
# 1. operator, once: write limits, approve them (prints the policy hash)
cp .devin/automation.policy.example.json my-policy.json     # set agentPub, network, caps
pnpm automation policy approve my-policy.json

# 2. run the instance with the runtime on
NEKTAR_AUTOMATION=1 NEKTAR_PAID_DISCLOSURE=1 NEKTAR_MARKET_HOSTS=https://host-a,https://host-b \
  NEKTAR_HTTP_PORT=8080 node packages/mcp/dist/src/bin.js

# 3. that's it — buyer side: one call; seller side: one call
automation {op:"publishWant", policyHash, domains:["libfoo"], question:"…", maxSats:500}
automation {op:"enableSeller", policyHash}

# exceptions only
pnpm automation approvals          # pending human decisions (exact action + max exposure)
pnpm automation approve <id>       # or: deny <id>
pnpm automation ops | status <operationId> | budget <policyHash> | policy revoke <hash>
```

What runs unattended: seller matches a want against its sellable shards
(bounded AI job), prepares a buyer-bound offer, publishes the public header,
verifies the funded escrow on-chain, reveals and collects. Buyer collects
offers until its window closes, rejects invalid ones with reasons, scores the
rest (relevance 30 / provenance 25 / history 15 / price 20 / latency 10),
reserves budget, funds escrow, verifies the disclosure against the committed
leaf, and refunds itself at the deadline if the seller never completes. An
observer watches a bond descriptor and seizes at the on-chain deadline.

Where the human is: the policy (once); anything the policy does not cover
(price/fee/locked-capital above limits, domain outside policy, unresolved
payment state) pauses that one operation and waits in `approvals`. Revoking a
policy stops new work; existing escrows stay monitored and refundable.

Honest boundaries:

- `disclosure_verified` = the revealed bytes match the seller's commitment and
  the seller was paid. It is not a claim that the content is true or useful —
  usefulness is a separate `useful`/`not_useful` state set by the sandbox step.
- A valid completed sale is not refundable because the answer disappoints.
- Escrow/reveal acceptance is broadcast acceptance, not mined confirmation;
  evidence bundles label it `broadcast-accepted-not-proven` until a proof exists.
- Bond seizure demonstrates a deadline-conditioned spend of an unspent bond
  output — not that an answer was wrong, nor that a commit was mined in time.
- Purchased patches run only in an isolated container (no network, no
  secrets, hidden tests outside the writable checkout). No container runtime →
  `BLOCKED`, never a host run.
- A buyer that loses its automation database recovers escrowed purchases from
  an encrypted note in its own bbox inbox (kept until the deal is finished, at
  most 7 days) and can still refund or receive the disclosure; `delivered`
  envelopes stay unacknowledged until the purchase settles, so a crash between
  collecting the envelope and processing it does not lose the disclosure.
- Deal evidence bundles stored via the bstore object store are **redacted**:
  the question text and the disclosure tx (which would reveal the wrapped
  secret) are replaced by their SHA-256, and `verifyTradeEvidence` marks the
  checks that need them unavailable. The bundle is committed to the operation
  journal as a digest of its `{bucket, key, sha256, size}` ref
  (`evidence_stored`), and Nektar verifies the downloaded bytes' content
  hash — provider possession proofs, bounty races and gateway availability
  remain the gateway's and providers' business, never claims Nektar makes.
  A seller recovers quoted sales the same way — its own `sellerNote` rebuilds
  the sale under the original operation id. Policies, budgets and operations
  that had not reached escrow/quote are not recovered.
- Uncertain on-chain states reconcile automatically: the engine probes the
  transaction state (`mined`/`seen`/`rejected`/`unknown`, pushed early by
  broadcaster rejection callbacks) and either resumes the deal, frees the
  budget on a rejection, or retries a refused refund. A human is still needed
  for: `uncertain` after five refused refunds; an `unknown` transaction state
  that never resolves; and policy or budget loss.
- Transport is bstack carriers (wants, offers) and bbox envelopes (quote,
  accept, delivered), submitted over HTTPS to every configured overlay host
  and verified locally on read. 1BSV multicast delivery is not used yet. Read
  envelopes are acknowledged with a bbox receipt and leave the inbox;
  unacknowledged ones expire after 24h. Wants expire with their collect window
  and offers after 24h; hosts refuse artifacts without an expiry or with one
  more than 7 days ahead.
- Existing owner tools (`sale`, `sendSats`, `payInvoice`, bond ops, anchors)
  are not yet routed through the automation budget ledger; its caps bound what
  the automation runtime itself spends. SpendGuard still fronts every broadcast.
- A buyer process that dies between reserving budget and recording the escrow
  broadcast resumes as `uncertain`, then reconciles against the chain — it
  resumes the deal if the escrow is observable, frees the budget if it was
  rejected, and only asks the operator when the state never resolves.

Tests: `packages/mcp/test/{automation-scoring,automation-store,market-engine,automation-sandbox,market-bstack,market-faults}.test.ts`,
`packages/bstack/test/artifacts.test.ts`, and the integrated scenarios
(S1 competing sellers over two federated hosts, S2 offline operator + observer
seizure, S3 paid public disclosure read for free):

```sh
pnpm -r build && node --test --test-force-exit test/market-scenarios.test.mjs
```

### Ledger coverage and crash recovery (update)

All runtime broadcasts are now recorded in the automation ledger before their
outcome is known (bytes, parents, fee, leased inputs). `payInvoice` records an
`exported` signed transaction and no longer adds unbroadcast change back to
the reserve. `ReserveFunder` skips leased outpoints. Escrow funding is split
into build → persist → submit; a restart resubmits the same bytes instead of
escalating. A watcher observes late broadcaster rejections and triggers funding
reconciliation. Policy caps still bind only the automation runtime; manual
owner calls are recorded, not capped.

### Operator surfaces

- CLI: `pnpm automation approvals|approve|deny|ops|status|budget|policy ...`
- Dashboard `/dashboard` has an Automation card with pending approvals.
  Approving requires `NEKTAR_OPERATOR_TOKEN` (separate from the agent bearer)
  and is disabled when that is unset. The agent bearer cannot approve.
- `pnpm scenarios -- --profile mock --scenario all` runs S1/S2/S3 as real OS
  processes with evidence bundles; `pnpm scenarios:verify -- <dir>` re-checks
  the S1 bundle offline. Regtest/testnet profiles need their chain stacks.

### Real model evaluation

`packages/e2e/src/scenarios/model-trials.mjs` runs 20 predeclared matching
trials against a local OpenAI-compatible endpoint (Ollama). With
`qwen2.5:7b` the current result is **16/20 correct, 1 out-of-set error** —
below the 18/20 bar, so the scripted matcher remains the default for
deterministic runs. S1 also passes end to end with that model selected via
`NEKTAR_AI_BASE_URL=http://127.0.0.1:11434/v1 NEKTAR_AI_MODEL=qwen2.5:7b`.
The trial report is written to `artifacts/model-trials/report.json`.


### Verification status

- `pnpm -r build` and the package suites pass (chain 25, anchor 27, bstack 54,
  mcp 199 pass / 5 skipped).
- `pnpm scenarios -- --profile mock --scenario all` passes S1/S2/S3 as real OS
  processes; S1's sandbox step passes in a network-less container.
- `pnpm audit` reports one high (zod <4.6.6 via the MCP SDK; a major upgrade is
  needed) and one low (`@tootallnate/once` via node-gyp).
- **Regtest (fresh Teranode chain, reset + re-bootstrapped treasury):**
  `e2e-regtest` (anchors, overlay admission, header proofs, ZK membership,
  policy DAG, delegated instance, automaton, full-paper + v4 scenarios),
  covenant lane (CheckpointBond commit/close/seize incl. foreign seizer,
  DisclosureSale buy/refund), claim lifecycle (challenge → reveal → bisect ×2
  → proveStage → timeouts → anchored settle), fee vault, watcher fraud
  challenge, and the full MCP lane (anchor, STARK + replay/tamper negatives,
  clearance, restart persistence, claim deploy/settle, gated sale) all PASS.
- **Regtest, current transport (Teranode, two reference hosts, four production
  instances):** three sellers quoted 450/300/200 sats on different host
  assignments, the buyer took the 200-sat offer, escrow
  `8b598ef18ce3703c863dba576916a6fba87d6b26f1d359adcc1a45a6f8039582` (940
  bytes, 1 sat fee) and disclosure
  `f8073ab8a179551bb3d2f10ea1ca52ab490aac2dbaf8cc501acf50e51b17b13b` (1487
  bytes, 8 sats fee) verified 33 s after the want, over 16 blocks. Each agent
  paid 346 sats once for its four funding trees. Regtest coins, not money.
- **Testnet, current transport (public BSV testnet, BSV Broadcaster, two
  reference hosts on live testnet headers, four production instances):**
  `pnpm scenarios:real -- --net testnet` passes. Three sellers quoted
  450/300/200 sats, the buyer took the 200-sat offer, escrow
  `ac5d1275d7c0f626e1229c2e1cdc59914e350b0b571091dcbb0f8f02e8d497fb` (941
  bytes, 1 sat fee) and reveal + collect
  `660049bc6e7c59f9fa0bd6451d262f29639b2a94ab8724f1accdcc046afd8171` (1488
  bytes, 8 sats fee), both mined at height 1768056. The buyer verified the
  disclosure 155.8 s after the want — 120 s of that is the configured collect
  window, 34 s from escrow broadcast to verified — with no block mined in
  between. The seller's own state moved to `settled` only when its collect
  transaction was proven (about 9 minutes later, pushed over the status
  stream). Fees: 346 sats once per agent for four funding trees, 4 sats per
  seller for two anchors, 9 sats for the deal itself. The one block wait is
  before the deal: funding trees had to be proven once (1 to 19 minutes
  across runs). Limits of this run: the hosts are local http loopback, so
  bfinger records were refused (https only off regtest) and counterparties
  reached each other through shared configured hosts; inbox cleanup was
  therefore checked on each agent's own hosts only.
  Two defects surfaced only on this lane and are fixed: funding-tree proofs
  from the broadcaster flagged a neighbouring own transaction, which the bbox
  module refuses; and a seller never left `completion_submitted` because
  pushed proofs for market transactions were not remembered.
- **Testnet over https hosts, zero-conf (same host pair published at
  `https://nektar.run/hosts/{a,b}`):** with provisioned agents the whole run
  has no block in its path. bfinger records resolved for all four agents
  within 1 s (each record lists exactly that agent's hosts), the buyer
  verified the disclosure 67.9 s after the want (20 s collect window), escrow
  `b21cfd2aae53…`, disclosure `79a9fdc2d49f…`, receipts cleared on both
  hosts. Readers now accept an unmined identity token whose ancestry is
  proven, as the bfinger reader rules allow, and the publisher repairs a
  token whose fee parent was unmined when it was minted. What still needs one
  block, once per agent and never per deal: a new funding tree, because hosts
  admit carriers only from a proven tree. The runner keeps provisioned agents
  in `.state/testnet-agents-*` (`--fresh` provisions a new set). Open
  Latency follow-up: the 11 s/34 s hops were host-transport costs, not block
  waits — per-send retry sleeps, serial host fan-out, and receipts sent before
  the tick. With the transaction graph carrying BEEF/EF inside deal messages,
  bounded lookups, parallel lanes and post-tick receipts, the same run now
  measures ~1 s from escrow broadcast to seller verification and ~1.3 s from
  the seller's reveal to buyer verification — the whole deal in ~12 s over 0
  blocks (`hopMs` in the run's summary.json).
- **Testnet (Hetzner, real rendezvous + mailbox, two live instances):** this ran on the previous rendezvous + mailbox transport, which has since been removed from the automation runtime. The
  autonomous purchase loop ran end to end — signed want admitted to the feed,
  seller matched a level-0 record, signed a buyer-bound offer, buyer selected,
  funded and broadcast the DisclosureSale escrow
  `6282176ba7be816fab336d2b186fbeb53a383ffeb66b39f30ec4be9772dbf3a2`,
  seller revealed + collected in
  `630d31bc74e9fd6240c4c9cd411b8ce8f4b699e002be48ba5d4b95027ba9b951`
  (200 sats), buyer decrypted and verified the disclosure against the anchored
  commitment (record anchor `71136fbe…`, leaf `ad2ee57d…`). Single seller;
  the multi-seller competing-quotes run is still open.
  Two defects found and fixed on this lane: wants/offers now carry
  `tip`/`tipHeight` (strictNet rvs reject unbound artifacts with 400), and
  sellers running an `agentSeed` (encrypted-by-default) can now match wants —
  `marketCandidates` decrypts own records for matching while the sale still
  delivers only the fragment key.
- **Object plane:** the bstore stack (host/provider/gateway/MySQL, testnet) is
  deployed on the Hetzner box. The gateway identity needed a second UTXO for
  fee inputs — funded; the host lost its MySQL connection at boot and reports
  `ready=false` until restarted. No object-plane delivery demonstrated yet —
  at the time the runtime transport was HTTPS rendezvous + encrypted mailbox,
  since removed from the automation runtime.
- Reserve/provider fixes exercised live: `takeReserve` applies a 546-sat dust
  floor on bare calls, `reconcile` revives/prunes entries against chain truth
  (phantom entries pruned; spent marks no longer drop records),
  `NektarProvider.getUtxos` hands Rúnar a bounded candidate set instead of a
  single smallest pick.
