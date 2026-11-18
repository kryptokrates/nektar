# SPV + BEEF — the verification architecture, deep

The event-layer view of "is this transaction mined / proven / real": the
component map, the `spv/*` rules with event-tier severities, and the
repo seam index. **The doctrine itself (D-numbers, per-source demands)
lives in [`../../spv-strict/references/doctrine.md`](../../spv-strict/references/doctrine.md)
— read it there, it is the single source of truth.** Sources for both:
Wright's Substack essays (singulargrit.substack.com), the Bitcoin
whitepaper §8, the 1BSV/Lightweb papers (data-carrier, bstack, user
guide) + BRC-62/74 (BEEF/BUMP).

The five lines that matter for event work, compressed:

- SPV = headers + Merkle path + computation by the verifier — stateless
  proof; **depth** is the security parameter, not byte volume.
- Evidence travels with the transaction; verification is local to the
  relationship — never re-fetch what the sender already held.
- A txid alone proves nothing — `hash(bytes) = txid` first, always.
- SPV proves inclusion, not unspentness — unspentness is a
  watch/bookkeeping concern, never a proof concern.
- Applications take objects (BEEF) by topic + a header feed only — an
  app that pulls blocks has stepped into the proof service's layer.

## The perfect architecture — five components, no more

| Component               | Job                                                                                                      | The invariant                                                                                                                                                                                                                                                                                                                                                                                          |
| ----------------------- | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **Header service**      | Hold the longest-PoW 80 B header chain; answer `headerAtHeight`, `isValidRootForHeight`, `currentHeight` | _Self-authenticating_: headers check out via PoW — the source provides availability, not authority. Independent of whoever serves proofs/data. Ours: `ChainTracker` → `ChaintracksTracker` (primary), `WocTracker` (fallback), `TemporalSources` (median time, primary roots), `TeranodeRPCTracker` (regtest)                                                                                          |
| **Proof service**       | Build Merkle paths _from_ blocks                                                                         | The **only** component that ever touches a full block. Producer-side work — a client asking for a path gets bytes it can check, never a block it must scan. Ours: broadcaster `/tx/{txid}` BUMP (`merklePathMainnet`), WoC TSC `/proof/tsc`, regtest `/merkle_proof`; `merklePathFor`/`merklePathAtHeight`/`parseLegacyBlock` are the _local stand-in_ for when no proof service exists (regtest only) |
| **Evidence store**      | txid → {tx bytes, merkle path, BEEF ancestry}                                                            | Retain evidence forever-attachable to its tx; hand counterparties BEEF/EF — a txid is a pointer, never evidence. Ours: `TxStore` (`put`/`setProof`/`wire` = BEEF→EF→raw ladder, `read` = SPV-ingest), `runarchain.normalize` (recursively attaches parents _until a proof makes ancestry unnecessary_)                                                                                                 |
| **Status hints**        | Pushed statuses, index answers, broadcast verdicts                                                       | Wake calls — they shorten latency and fill `statusOf`, they never prove. Ours: `note()`, `statusStream`, `fetchTxStatus`, `arcTxStatus`                                                                                                                                                                                                                                                                |
| **Verification kernel** | The _only_ route to a 'mined'/'verified' claim                                                           | bytes → `dsha256(bytes)=txid` → BUMP leaf → `computeRoot` → `root == header[height].merkleRoot` on OUR header chain → `depth = tip−height+1` → policy. Ours: `proofLookup` (runtime.ts), `handleBroadcasterStatus` (http.ts — pushed BUMP verified before delivery), `TxStore.read` (counterparty wire SPV)                                                                                            |

The shape: headers flow in continuously from an independent source;
proofs arrive attached to transactions (push or pull); every terminal
verdict re-derives through the kernel. Blocks exist — the client simply
never meets one.

## What must never happen — `spv/*` rules

| Rule                     | The violation                                                                                                                                                                                                                                                                                                                                | Severity guide                |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------- |
| `spv/block-scan`         | App code fetches/scans full blocks to answer "is my tx mined". The answer arrives as BUMP + header check; block-walking is proof-service work. Contained today: `merklePathFor`/`merklePathAtHeight`/`parseLegacyBlock` + `ReserveFunder`'s early-coinbase scan — **regtest only**. Any spread to a live path or new call site is a finding. | MED; BLOCKER on mainnet paths |
| `spv/pathless-mined`     | A 'mined' event/status emitted without a merkle path in hand — e.g. an indexer's `blockheight > 0`. WoC may _hint_; only a BUMP anchored to our headers may _confirm_.                                                                                                                                                                       | MED                           |
| `spv/hint-as-verified`   | A status claim delivered on the verified path ('tx' emit, watch-drop, money decision). **Includes rejection verdicts** — no proof object exists for 'rejected'; only a double-spend is provable (via the _conflicting_ tx's own BUMP). Route through `note()`; terminal truth = double-spent's BUMP or 'abandoned'/TTL.                      | MED; BLOCKER on money paths   |
| `spv/txid-only`          | Trusting a txid or status without the bytes — you cannot verify `hash(bytes)=txid`, let alone inclusion.                                                                                                                                                                                                                                     | MED                           |
| `spv/circular-anchor`    | Verifying a path against a header fetched from the _same_ party that served the path — the source grading its own homework. Header anchor must be an independent chain (our `ChainTracker`), never the proof's own provider.                                                                                                                 | MED                           |
| `spv/beefless-handoff`   | Handing a counterparty raw tx/txid where BEEF or EF would travel self-verifying — the receiver then needs the network for what the sender already held. (`txWire` already ladders BEEF→EF→raw; findings = new handoff sites that skip it.)                                                                                                   | MINOR                         |
| `spv/indexer-dependency` | An indexer's answer treated as chain truth (blockheight, "known", presence) — indexers answer _what might be_; headers answer _what is_. Index lookups are discovery, not verification.                                                                                                                                                      | MED                           |
| `spv/world-state`        | Reconstructing global state (scan-all-txs, full-UTXO-set, block replay) to serve one relationship — the gatekeeper inversion SPV exists to prevent.                                                                                                                                                                                          | MED                           |
| `spv/header-by-block`    | Fetching a whole block (or `/block` JSON) where a header endpoint exists — wrong seam, wasted bytes. Tolerated fallback: `WocTracker` behind `ChaintracksTracker`.                                                                                                                                                                           | MINOR                         |
| `spv/depth-blind`        | 'mined' treated as binary at depth 0 where the stake demands a confirmation policy. Depth (`tip−height+1`) is the security parameter; the event can be terminal while the _policy_ still waits.                                                                                                                                              | ADVISORY                      |
| `spv/thin-ancestry`      | An unconfirmed tx accepted where a carried input chain does not terminate at a BUMP-anchored mined parent — an unverified hop inside the BEEF ancestry, or ancestor bytes absent and unchecked. Accept-side of zeroconf: valid only when every input chain reaches a proof or the issuer-class policy bound.                                 | MED; BLOCKER on money paths   |
| `spv/zeroconf-overreach` | Counterparty-issued unconfirmed ancestry accepted past the policy bound (ancestor count, unconfirmed sats) with no escalation toward depth — provenance verified, exclusivity not, and nothing priced the difference.                                                                                                                        | MED                           |
| `spv/zeroconf-unused`    | A step waits on a block for a tx whose FIRST-PARTY ancestry was carried, verified and within policy — we issued the ancestors; only we can double-spend them. The event-driven move: serve from the verified ancestry now, let `observe` attach the BUMP when it lands.                                                                      | MED                           |

## The audit checklist — "is it proven" must answer

Adapted from the data-carrier third-party checklist to our seams:

1. **Bytes in hand?** — `dsha256(tx) = txid`; a txid alone fails here.
2. **BUMP parsed + leaf located** at its claimed index.
3. **`computeRoot(path, txid)`** → candidate root.
4. **`isValidRootForHeight(root, height)`** against OUR header chain —
   an independent source from whoever served the proof.
5. **`depth = tip − height + 1`** → apply the caller's confirmation policy.
6. **Ancestry complete (BEEF)** — every input's source tx carried and
   itself proven or proven-by-parent; `normalize()`'s recursion is the
   model. For unconfirmed chains the recursion must reach a BUMP on
   every branch or stop at the issuer-class policy bound (first-party
   cap vs counterparty ancestor/value caps) — and counterparty chains
   carry a conflict watch as the escalation trip wire.
7. **Rejection/double-spend claims have no short path** — only the
   conflicting tx's own mined BUMP is evidence; a naked verdict stays
   in the hint channel (`note()` resolves waiters, keeps the floor).
8. **Absence is never proof** — silence keeps the watch (bounded by
   TTL/abandon, which emit named events rather than imply outcomes).

## Repo map — who plays which role

**Aligned ✅** — `ChainTracker` = header service (chaintracks primary);
`proofLookup` = the kernel (`computeRoot` + `isValidRootForHeight`);
`handleBroadcasterStatus` = pushed BUMP verified before delivery;
`TxStore` + `read`/`wire` = evidence store; `note()`/`observe()` =
hint/truth split; `TemporalSources` = anchored time (roots never mix
chains); `runarchain.normalize`/`beefOf`/`headAtomicBEEF` = BEEF
assembly; `toolbox.ts` services (`getMerklePath`/`getBeefForTxid`/
`getStatusForTxids`/`getRawTx`) = the wallet-services SPV shape —
depth-aware `getStatusForTxids` is the model for confirmation policy.

**Containment ⚠ (regtest proof-service stand-ins — correct inside
their boundary, drift outside it)** — `merklePathFor` (scans last-N
legacy blocks, _does_ self-verify `merkleRootOf(txids)` vs the header
before building a path), `merklePathAtHeight`, `parseLegacyBlock`,
`ReserveFunder`'s early-block scan. These are the proof service's job
running inside the client because regtest has no proof service; they
must never appear on a mainnet path.

## BRC / upstream index

- **BRC-62** — BEEF: tx + ancestors + BUMPs + known-txid markers in one
  versioned binary; the "evidence travels" packaging.
- **BRC-74** — BUMP: unified merkle-path format (`MerklePath` in the
  SDK; `tscMerklePath` for TSC proofs via `@lightwebinc/bcommon`).
- **AtomicBEEF** — BEEF pinned to a single subject txid (`toAtomicBEEF`)
  — the handoff shape for "here is THIS payment, fully proven".
- **EF** — extended format: tx + source outputs inline, no proofs; the
  intermediate wire when ancestry exists but paths don't yet.
- Lightwebinc reference impls: `retry-endpoint`, `teranode-bridge`,
  `overlay-bridge`, `bstore`, `beefcat` on github.com/lightwebinc.
  `beefcat/docs/verification.md` is the carrier-check reference
  (REFUSED-* codes) and self-documents its header-source limits —
  PoW-over-floor, no linkage — which are findings under this ruleset
  (witnessed sources + PoW + linkage is the bar here).

> The dedicated, strict audit for this material is the `spv-strict` skill
> (`.devin/skills/spv-strict`): same rules with no advisory tier, plus
> the latency half — block waits, lane stock, proofs travelling with
> funds.
