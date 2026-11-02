# The precise version — sealing, receipts, ZK, claim layer

Moved from the README; everything stated exactly.

## Sealing (writes)

- Records are appended to a local encrypted log (`BatchLog`); a batch's
  SHA-256 Merkle root `R` is committed in one spend of the **`MamemAnchor`
  Rúnar covenant**. The anchor UTXO must spend its own previous head —
  a single non-forking UTXO chain per shard. One anchor tx per batch,
  independent of batch size.
- Anchors carry `n` (sequence) and `size`; the head is pinned by covenant
  introspection (`OP_PUSH_TX` + `hashPrevouts`/`hashOutputs`), not by the
  backend being honest.

## Receipts (reads)

A recall returns the payload plus: Merkle path leaf→`R`, the anchor UTXO
chain, and an SPV check against real block headers. Access is gated by
*all* of: identity, ZK clearance (Bulletproof: level ≥ threshold without
revealing the level), Cedar policy, policy-DAG (Datalog) authorization.
Derived content inherits the max clearance of its premises.

## Zero knowledge

`@mamem/zk` wraps a RISC Zero STARK (BabyBear field, Poseidon2 sponge) that
proves *leaf ∈ R* plus a committed `queryId` — verifying a ~224 KB receipt
takes milliseconds, off-chain, via a wasm-bindgen Rust verifier
(`verifyZkm2`). The guest program is pinned by `image-id.json` and built
reproducibly inside the Docker image (build fails on mismatch).

## The claim layer (optimistic on-chain verification)

| Contract | Role |
|---|---|
| `MamemZkClaim` | 18-field state covenant: `status, receiptHash, journalRoot, journalSize, queryId, bondSats, deadline, responseDeadline, challengerPkh, bisectLo/Hi, midCommit, proverPkh, maxFee, window, stages, verdictCode, anchorOutpoint`. Methods: `challenge → reveal → bisect → proveStage → settle/claimTimeout/slashTimeout` (8 public). |
| `Poseidon2Stage` | the on-chain judge: ONE risc0-zkp 3.0.5 poseidon2 round (24-cell state, x⁷ sbox, M_ext/M_int) executed as a stateful covenant step — a dispute resolves to a single verified round. |
| `ClaimVerdict` | minted by `settle`; `authorize` enforces payment to `payeePkh` in the same tx. |
| `DisclosureSale` | `buyGated` pins `hash256(prevouts)` to the exact [sale, verdict] pair — the buy is consensus-impossible without spending the verdict atomically. |
| `MamemAnchor.witnessSettle` | co-spent input of `settleAnchored`; re-emits its own continuation (anchor survives) + the identical 4-output list the claim enumerates → `journalRoot == anchor.R` is consensus-enforced. |

The bisection game: `stages` committed stage-hashes; `reveal`/`bisect` halve
the disputed range each round; the terminal step replays one Poseidon2 round
on-chain. Challenger bond + prover bond; loser forfeits.

## The watcher

`packages/watcher` — a daemon (also `docker compose up -d watcher`) that
polls a file-backed claims registry shared with the MCP server via a
volume. Per open claim it: pulls the proof bundle, re-verifies the STARK in
an **isolated worker process** (a wasm panic can't kill the daemon; a crash
is logged as *inconclusive*, never auto-challenged — inconclusive is not
fraud), cross-checks `journalRoot/journalSize/queryId` against the claim's
committed fields, self-funds its challenger bond, broadcasts `challenge`,
and marks outpoints `spent` so it never re-broadcasts a dead claim.

## What's verified vs. honest gaps

- ✅ Live on Teranode regtest, real transactions: anchor chain, STARK
  membership + replay/tamper rejection, clearance gates, restart
  persistence, claim deploy/status/settle, verdict mint, gated sale,
  watcher fraud-detection → challenge accepted, anchored settle
  (`journalRoot == anchor.R`), continuing anchor. `pnpm e2e` covers the
  offline equivalents.
- ✅ Interpreter-level: full claim lifecycle incl. bisection + stage proof
  (16/16 evals).
- ⚠️ Watcher tracks known registry outpoints only — following covenant
  continuations past the first spend needs a tx-index (documented).
- ⚠️ Stage 2 (full-receipt on-chain validity, ~$40–150/use) deliberately
  skipped — the optimistic system is complete without it.
- ⚠️ No security audit. Regtest economics measured; mainnet pilot pending.

## Design rules

- Contracts are **Rúnar only** — no hand-written Bitcoin Script anywhere
  (shared libs go through `runar-sdk` `buildUnlockingScript`/`buildP2PKHScript`).
- Secrets live in gitignored `.env` / `.state/`. Regtest keys only.
- Measured numbers only in docs — see [BENCHMARKS.md](../BENCHMARKS.md).
