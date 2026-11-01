# v2 legal notes — patent adjacency (facts only, no legal conclusions)

Patents checked (nChain Licensing AG, claims per Google Patents):

| Patent | Title | Status | What it claims (claim-text summary) |
|---|---|---|---|
| **US10652014B2** | Determining a common secret for the secure exchange of information and hierarchical, deterministic cryptographic keys | Active, exp. 2037-05-04 | Deriving a common secret at two nodes where each node computes a message M = SHA-256 over a shared message; second-generation child private keys = (master + M) / H(master) style; deterministic hierarchy of keys where descendants can be derived only with the corresponding first-generation key and the message. This is the BRC-42 key-derivation construction. |
| **WO 2022/100946** | Merkle proof entity | Ceased | Methods of providing/obtaining proof that data of a blockchain transaction exists on-chain: constructing a Merkle path/proof entity for a target transaction and verifying it against a block header. This is the standard BUMP/merkle-proof space. |
| **WO 2025/119666** | Selective verification / proof sharding | Pending (per filing) | Partitioning a block's transactions into "shards"/subsets, computing per-shard Merkle subtrees/proofs so a verifier can prove inclusion for a subset of transactions without holding all block transactions. This is the "proof sharding" format for selective verification. |

## Feature-by-feature assessment (facts only)

| v2 feature | US10652014B2 | WO 2022/100946 | WO 2025/119666 | Note |
|---|---|---|---|---|
| BRC-42/HD key derivation (already in v1 + KEY-013/014) | **touches** | does not touch | does not touch | Directly the claimed construction. v1 already uses it via the BSV SDK's `deriveChild`/BRC-42 helpers. |
| Rotation certificates C(i)=Sign(H(pk(i-1)‖pk(i)‖t_i)) | does not touch | does not touch | does not touch | Plain message signatures; no shared-secret derivation, no Merkle structure, no tx-subset proofs. |
| Proof-sharding selective disclosure (DIS) | does not touch | unclear | **touches** | If implemented as tx-subset/proof partitioning per the WO filing. Marked uncleared → per GOAL-v2 we implement selective disclosure WITHOUT the patented sharding format: field-level commitments + disclosure envelopes only. |
| Field-commitment disclosure envelopes (DIS fallback) | does not touch | unclear | does not touch | Envelope = one field + named verifier + expiry + signature; no shard-partitioned proofs. Chosen implementation path. |
| SMT revocation ledger (IDN) | does not touch | unclear | does not touch | Sparse Merkle tree non-inclusion proofs are a generic structure; anchored like other roots, not tx-subset proofs. |
| Bulletproof ZKAccess (ZKA) | does not touch | does not touch | does not touch | Pedersen + range proofs over secp256k1; unrelated to all three. |
| Cedar policy engine (POL) | does not touch | does not touch | does not touch | Off-chain authorization decisions + receipts. |
| BUMP/BEEF + verifyFragment (v1, already shipped) | does not touch | **touches** | does not touch | Standard merkle-path-to-header verification; WO filing marked Ceased. |

## Decision recorded
Selective disclosure is implemented via **field-level commitments + disclosure envelopes only** (no tx-subset proof sharding), per the GOAL-v2 stop-gate instruction. If proof sharding is later cleared, add it behind a feature flag.
