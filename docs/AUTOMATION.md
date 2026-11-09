# Automation debt — mainnet field report

Every item below was hit by hand on real mainnet (Nov 2025, heights ~974780–974820).
Ordered by time lost, not by code size.

**Status key:** `[done]` shipped in this pass · `[part]` partial · `[ ]` open

## F1 — Reserve drift vs chain truth  *(dominant cost)*

`reserve.json` is optimistic: spends mark inputs `spent` and append change
entries at *broadcast time*, never at *verdict time*. On mainnet this produced
four classes of damage:

- ARC returned non-rejected 200s, then later marked the tx `REJECTED`
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
  ARC reports REJECTED (and revives their inputs that are still live).
  On-demand call — a timed/triggered run is still open.
- `[done]` `takeReserve(minSats)`: smallest-sufficient-first (≥ need), never
  dust; `spendCheck` callback prunes chain-spent candidates.
  Confirmed-parent preference (store `height` at import) still open.
- `[part]` Broadcast verdict: covenant `spend()` throws on synchronous
  REFUSED, and `sendSats`/deploy check `br.status` before marking spent /
  advancing state. The post-accept async-REJECTED watch (~60 s poll of
  ARC `/tx/{txid}` + rollback) is still open — `fundingRefresh` is the
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
- `[part]` `fetchParentTx` retries 404s 3× at 10 s (fresh txs index within
  ~30 s). ARC `rawTx` and peer fallbacks still open.

## F5 — Chain-time waits as runtime tasks

MTP lag on refunds (~60–90 min), releaseHeight on close, seize deadline —
today each needed a hand-rolled `ssh`-polled python script armed manually.

**Fixes**

- `[ ]` In-service scheduler: `refund`, `bondClose`, `bondSeize` accept
  `{when: {height|time}}` — the runtime persists the pending op and fires
  it when the condition passes. Uses the **ARC callback** path already in
  `mainnet.ts` (`X-CallbackUrl` → `/v1/arc-callback`) for block/tx events —
  push, not polling.
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
| 4 | parentHex in state + fetch fallback | F4 | S | done (ARC/peer fallback open) |
| 5 | Scheduled ops on ARC callback (`when:`) + tasks tool | F5 | M | open |
| 6 | `e2e-mainnet` runner + evidence.json | F8 | M | open |
| 7 | bondScan | F7 | S | open |
| 8 | service-run/installer + readiness gate | F6 | M | in progress |
| 9 | Error-surface annotations | F9 | S | open |
| 10 | Deploy dedup | F10 | XS | open |

Items 1–4 remove every manual intervention hit today — the first four rows
are shipped (with the noted exceptions); 5–8 remove the hand-rolled
scaffolding; 9–10 are polish. Regressions live in
`packages/mcp/test/automation.test.ts`.
