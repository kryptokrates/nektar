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
- **Gate S1.1 result (measured in icellan/runar, commit 513aa3d):** the SP1 FRI
  verifier is Go-tier-only by Rúnar project policy and validated on BSV regtest:
  PoC locking script 248.6 KB / spend 3.6 KB; production 6.32 MB / spend 285 KB,
  both deployed + spent under Chronicle defaults. Fee at 100 sat/kB ≈ 25k sats
  (PoC) to ~648k sats (production) one-time per verifier UTXO. **Decision:** the
  full SP1-wrap pipeline needs runar-go + the SP1 toolchain — schedule as its own
  phase; ship the cheap §13 path first (below).
- Deliverables (two stages):
  - **S1a (cheap, now):** OP_FALSE RETURN output carrying `sha256(receipt)` in the
    same tx as an attestation spend — the STARK's existence + binding is anchored
    on-chain; off-chain WASM verify stays authoritative. `attestMembership`
    MCP tool + `onchain.ts`.
  - **S1b (full, deferred phase):** `tools/sp1-verify-guest/` (risc0 receipt
    verify inside SP1), runar-go FRI verifier contract, `verifyMembershipOnChain`
    on an SP1 STARK — the real on-chain path, once the Go tier is integrated.
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

1. S1.1 on-chain verifier spike (measured gate → decides §13 path) — **done; S1a/S1b split**
2. S2 Datalog guest (unlocks §14, feeds S1)
3. S5 proof sharding
4. S4 OWL 2 RL
5. S3 temporal aggregation
6. S6 small paper gaps (below)
7. S7 `recall` verification fix (below)

Each: unit tests → fuzz → regtest e2e → RESEARCH/PROOFS/THREAT-MODEL/test-spec/
migration-log → commit → CI green.

## 6. S6 — remaining paper-level gaps (small, batched)

Paper coverage that the traceability matrix shows as ◐/⏸, ordered cheap→hard:

| # | Item (spec id) | Paper § | Work |
|---|---|---|---|
| S6.1 | `recall` verifies only encrypted records (self-found bug) | VER | Full SPV verify for plaintext items too, or mark receipt `checkResult` honestly. **Fix immediately.** |
| S6.2 | ANC-006 Metanet flag | App. D | Add the `0x4D455441` ("META") flag byte to the anchor schema (versioned, v3 decodes both). |
| S6.3 | GEN-007 per-structure algorithm ID | §6 | Add a `algorithm` byte to L_* codec headers (v3 codecs), defaulting to current hashes/suites. |
| S6.4 | KEY-010 privilege-scoped triple hash H(s‖p‖o‖l) | §7/App. A | Encode level+owner+shard in the KDF context explicitly (one tagged hash) — evaluate vs current derivation; implement if compatible. |
| S6.5 | DIS-005 / COM-008 chunked files | App. A/C | Optional: record-type `chunked` — split payloads >64 KB into content-addressed fragments, each a leaf; manifest record references the chunk leaves. Only if needed. |
| S6.6 | ANC-009/010 witness redundancy | App. D | Optional: mirror anchor roots into a second output (OP_RETURN sidecar) so one anchor chain isn't a single point of loss. Rekor-style witness stays deferred. |
| S6.7 | LOG-010..013 | App. D | Deferred: pre-ordered mode, quota protocol, Trillian adapter — schema variants, not core. |
| S6.8 | ZKA-005 ZK set-membership variant | §13 | Optional §13 variant; defer unless needed. |
| S6.9 | ZKM-013 Plonk path | §13 | Paper lists Plonk as an option, not a requirement; defer. |
| S6.10 | AUT-010 fuzzy automaton | §15 extension | Defer — extension, not core. |
| S6.11 | KEY-016/017 TEE/HSM + PQ keys | hardening | Defer — post-MVP hardening options. |

S6.1–S6.4 implement now; S6.5–S6.11 stay documented as deliberate deferrals with reasons.

## 7. Ops/engineering gaps (not paper items — tracked, not in GOAL-v6 scope)

- **SEC-002/004/005/006:** constant-time audit, external security audit + pen test
  (required before real mainnet), signed releases.
- **CHN-001** own PoW/difficulty validation of headers; **CHN-009** multi-endpoint
  broadcast failover; **VER-007** gossip/split-view monitor.
- **STO-001/002/004:** content-addressed proofstore, tested backup/restore, source mirroring.
- **API-001/002:** gRPC, Go/Python client SDKs. **OPS-004..006:** public monitor, metering.
- **TST-007:** node-outage chaos tests.

## 8. Definition of done (v6)

- S1a, S2, S3, S4, S5 implemented, unit+fuzz+regtest-tested, documented.
- S6.1–S6.4 fixed; S6.5–S6.11 marked deferred-with-reason in the test spec.
- `pnpm reproduce`, `pnpm e2e`, regtest e2e (3×), `e2e:mcp` all green on the final tree.
- Test-spec matrix updated: every remaining ◐/⏸ has either an implementation or a documented deferral.
