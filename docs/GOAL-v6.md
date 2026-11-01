# GOAL-v6 — the five remaining paper items

How mamem reaches full coverage of arXiv:2506.13246, the way the paper describes each
item. Approved plan; every phase lands with tests + regtest e2e + docs + commit.

| Paper item | § | Approach |
|---|---|---|
| ZK derivation attestations | §14 | RISC Zero guest replays the Datalog derivation path; public (policyRoot, conclusionHash, queryId) |
| On-chain ZK verification | §13 | SP1 guest verifies the R0 ZK receipt → Rúnar SP1 FRI verifier on-chain (gate: script size) |
| Multi-chain consensus time | §12 | weighted-median aggregation f(T_C1…T_Cm) over pluggable header sources (BSV + BTC) |
| Full OWL 2 (tractable) | §9 | OWL 2 RL/QL axioms compiled to Datalog rules; anchored axioms feed §14 |
| Proof sharding | §13/DIS | subtree-of-K-leaf proofs; behind `MAMEM_PROOF_SHARDING` (default on) |

## 1. On-chain ZK verification (§13 — "on-chain if necessary")

π = ZKMerkleProve(h_j, R_n, P_j) "can be verified on-chain if necessary".

- Our ZK-STARK (RISC Zero) has no Script verifier; SP1 STARKs aren't zero-knowledge,
  so the private witness can never live in an SP1 guest.
- **Design:** an SP1 guest *verifies a RISC Zero succinct receipt* for
  (image ID, journal = R, size, queryId). The receipt is already ZK — leaf/index/path
  never exist in the guest. The outer non-ZK STARK then asserts "a valid ZK proof
  exists for these public values" — and Rúnar ships an SP1 FRI verifier for Script.
- **Gate S1.1:** compile the Rúnar SP1 FRI verifier, measure script bytes + cost on
  regtest. If beyond policy (≈500 KB script, 10 MB tx on GorillaPool) or unreasonable,
  the fallback is an OP_FALSE RETURN commitment sha256(receipt) — §13 allows off-chain.
- Deliverables: `tools/sp1-verify-guest/`, `packages/zk/src/stark/onchain.ts`,
  MCP tool `verifyMembershipOnChain`, optional MamemAnchor v4 `verifyZk`.
- Tests: fixed receipt → SP1 → on-chain verify; mutated journal / foreign image rejected.

## 2. ZK derivation attestations (§14)

h_i = H(serialise(v_i)); edges e_ij = (h_i, h_j, τ_ij, H(h_i‖h_j‖τ_ij)); a conclusion
v_k needs a derivation path π = {v₀…vₖ}, each step satisfying ⊢_R, anchored under the
policy DAG root — "zero-knowledge derivation attestations where required".

- Our Datalog evaluator already produces the entailment steps; commit them §14-style.
- New guest `tools/stark/methods/guest-datalog`: replays the path — checks each edge's
  rule application, recomputes h_i / edge hashes, closes at a root-committed node.
- Private: the path + fact contents. Public journal: (policyRoot, conclusionHash, queryId).
- Deliverables: canonical node/edge serialisation in `packages/datalog`,
  `proveDerivation`/`verifyDerivation`, MCP `zkDerivation`, combined Π with the
  membership + clearance proofs on the same issued queryId.
- Tests: rule-vectors; wrong rule / skipped step / wrong root rejected; e2e on regtest.

## 3. Multi-chain consensus time (§12 — conditional)

T_C*(i) = f(T_C1(i₁),…,T_Cm(iₘ)) — "in cases where multiple blockchains are consulted":
weighted median or quorum over external chains to resolve disputed clocks.

- `TemporalSource[]`: BSV (WoC) + optional BTC via the same WoC API shape
  (`/v1/btc/main`) — no new deps. `consensusTime` = weighted median; single source =
  today's behaviour, bit-identical.
- Config `MAMEM_TEMPORAL_SOURCES=bsv[,btc]`; regtest uses mock sources.
- Tests: skewed source → median wins; single-source parity; read-only BTC smoke.

## 4. OWL 2 coverage (§9 — "DL-Lite and OWL 2, which support tractable reasoning")

Full OWL 2 DL is undecidable — the paper needs the tractable profiles only.

- Compiler `packages/graph/src/owlrl.ts`: OWL 2 RL axioms → Datalog rules:
  subClassOf, subPropertyOf, domain, range, inverseOf, symmetric/transitive,
  propertyChain, sameAs/equivalence, negativeObjectAssertion rejection.
- Ontology axioms are committed nodes (anchored), so derivations remain
  "entailment only from anchored axioms" and feed the §14 guest unchanged.
- Tests: RL entailment subset; anchored/provable entailment; contradiction detection.

## 5. Proof sharding (US 12,730,827 B2 — granted 2026-09-08, nChain)

Selective proof of existence via a data reveal path over a hierarchical structure.

- **Licence (facts):** nChain/BSVA pledge patents "for free use on BSV only" under the
  Open BSV Licence (revocable; BSV + testnets). mamem is BSV-only by construction.
  Implemented behind `MAMEM_PROOF_SHARDING` (default on); NOTICE documents the pledge;
  flag = opt-out for cautious operators.
- **Design:** batch trees shard into K=64-leaf subtrees (leaf index >> 6); batch root
  commits shard roots. A subset proof carries {shardRoot_j, subpath_j} per touched
  shard — the patent's data-reveal-path structure, cheaper than per-leaf paths for
  multi-record disclosure; later feeds a shard-rooted variant of §13.
- Deliverables: L_ZKDis v2 codec, `proveSubset`/`verifySubset`, fuzz.
- Tests: subset {1,2,3 records} verifies vs anchored R; wrong shard root rejected;
  canonical re-encode; fuzz.

## Order & gates

1. S1.1 on-chain verifier spike (measured gate → decides §13 path)
2. S2 Datalog guest (unlocks §14, feeds S1)
3. S5 proof sharding
4. S4 OWL 2 RL
5. S3 temporal aggregation

Each: unit tests → fuzz → regtest e2e → RESEARCH/PROOFS/THREAT-MODEL/test-spec/
migration-log → commit → CI green.
