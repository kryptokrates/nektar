# SPV + BEEF — the verification architecture, deep

The reference for auditing any code that answers "is this transaction
mined / proven / real." Sources: Wright's Substack essays
(singulargrit.substack.com) — the formal SPV treatment, the gatekeeper
essay, the mechanism-of-honour essay — the Bitcoin whitepaper §8, and
the 1BSV/Lightweb papers (data-carrier, bstack, user guide) + BRC-62/74
(BEEF/BUMP).

## The doctrine, stated precisely

**What SPV is (whitepaper §8 + "Safe Low Bandwidth SPV: A Formal
Treatment"):** a block-header chain meeting proof-of-work criteria plus
a valid Merkle path _probabilistically verifies_ a transaction's
inclusion in the longest-PoW chain. The client need not verify scripts,
hold blocks, or retain global state — the model is _stateless proof_.
Security parameter is **depth** (confirmations), not byte volume:
inclusion at depth N is safe under honest-majority mining; proofs stay
logarithmic while throughput grows by orders of magnitude.

**Evidence travels with the transaction ("The Protocol No Gatekeeper
Wants"):** the recipient needs _the transaction being received, the
relevant signatures and conditions, an inclusion proof, and a reliable
chain of PoW headers_. Verification is **local to the relationship** —
Bob never asks the network what Alice already has. MF-SPV = hierarchical
Merkle commitments + sender-held proofs; the sender is responsible for
handing over the evidence.

**TXID alone proves nothing ("A Mechanism of Honour"):** providing only
the TXID is insufficient — the verifier cannot confirm `hash(bytes) ==
txid` without the full transaction data. A user maintains _their own
transaction and its Merkle path to the block header_ — "traced,
verified, and confirmed — not by trust, but by computation."

**SPV proves inclusion, not currency (1BSV data-carrier):** three things
suffice — the bytes, a short Merkle path, the chain's block headers
(80 B each, ~4.2 MB/year, fits on a phone). SPV _cannot_ say whether an
output is still unspent — unspentness is a watch/interest concern, not
a proof concern. Verifiable ≠ mined: a tx riding an unmineable carrier
that spends a mined parent is still provable.

**The layered need-to-have (1BSV user guide):** miners take the
settlement layer (tx stream + subtrees + block data); service providers
take txs + subtrees + the header feed; **applications take objects
(BEEF) by topic + a block-header feed only**. An app that pulls blocks
has stepped into a layer it was never meant to see.

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
   model.
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
  `overlay-bridge`, `bstore` on github.com/lightwebinc.
