# research briefs — the autonomous economy + WALDO plug-play

## the autonomy stack — what nektar owns

| layer | what | state |
|---|---|---|
| memory | verifiable provenance-chained storage | done — shards, `hdp`, anchors |
| reasoning | claims derived only from verified fragments | done — `groundedAnswer` |
| identity | attributable keys, per-domain reputation | done — BRC-42 + reputation cards |
| discovery | declare expertise by content, find peers | done — `rv.nektar.run` |
| pricing | signed quotes/offers, clearing prices | done |
| settlement | verify-then-pay escrow, claim | done — public ttn |
| **attestation** | who/what produced the claim | **built** — delegation grants, model manifests, `callProof` slot |

## Wright requirement map

- "the agent has permission; who pays for the mistake" — `delegate`/`revoke` + `peerCall` enforcement: bounded permission as a checkable object
- "truth is expensive, bullshit scales" — the market: priced, signed, escrow-settled memory
- Merkle-automaton memory — anchored append-only shards (done)

## attestation for self-hosted AI

Viable on consumer hardware: signed call-logs, model manifests, delegation chains — the full liability story with zero exotic hardware. Out of reach today: ZK inference (~70-95× GPU, FlashProve-class) and universal TEE. `callProof` is the slot for when provers get cheap.

## WALDO plug-play

WALDO = Go CLI for auditable training data (Git index, Parquet BOMs, provenance records) — "attributable facts, not compliance." A blockchain-less tool proves *what* the data is; it can't do **when** (immutable anchor), **who paid** (priced provenance), **who's liable** (delegation receipts), or **settlement** (escrow for paid access) — that's nektar's territory:

`waldo export` → `attest{modelId, weightsHash: bomHash}` → catalog → `offer` → escrow → claim → receipt.

WALDO stays the verifier of what data is; nektar becomes its market + liability + settlement layer. Gaps: Go codebase, unstable interfaces, dataset-access escrow variant not built — a connector, not a merge.
