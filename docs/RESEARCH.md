# Research & inspirations

What mamem took from each source, and how: **reused** (code or vectors), **adopted** (as a pattern), or **rejected** (with the reason). Patent and licence notes are technical observations, not legal advice (see `v2-legal-notes.md`).

## Primary source: Wright, *arXiv:2506.13246* (Merkle Automaton / immutable memory)

| § | Mechanism | Status in mamem | Where |
|---|---|---|---|
| §4 | Merkle Automaton (Q, Σ, δ, q0, F, O) | **implemented**. Light mode: anchored transition leaves. Full mode: the Rúnar `MamemAutomaton` enforces δ by a depth-4 Merkle path and q∈F by a second path. | `packages/automaton`, `contracts/src/MamemAutomaton.runar.ts` |
| §5 | K_i = (d, p, H(d‖p), E(d)); VerifyFragment | **implemented**: leaf → R_n → anchor tx → BUMP → headers | `core`, `verify` |
| §6–7 | ECDH + HKDF per-fragment keys, level lattice | **implemented**: BRC-42-style derivation, AES-256-GCM, disjoint level keyspaces | `crypto` |
| §8 | Append-only log, refinement not overwrite | **implemented** | `log`, `graph` |
| §11 | Append-only reasoning graph (AORG) | **implemented**: acyclic, justified, consistent, traceable, anchored, bi-temporal refinement | `graph` |
| §12 | Consensus time | **implemented** as *existence-by* the block time (never "created at") | `reasoning.timeBounds` |
| §13 | ZK membership, chained roots | **implemented**: Poseidon companion tree + Groth16, verified off-chain against the SPV-verified anchor | `zk` |
| §14 | Policy DAG (stratified Datalog) | **implemented**: L_pol, semi-naive evaluation with canonical provenance, layers/overrides, conflict localisation, Merkle-proven derivations | `datalog`, `graph/policy` |
| §15 | Failure modes: contradiction, revision | **implemented**: refinement + correction loop | `graph`, `reasoning.onRevision` |
| §16 | Audit/liability bundle | **implemented**, verified offline with headers and the verifier's own policy | `reasoning.verifyAuditBundle` |
| §17 | Instancing and delegation | **implemented** | `graph/instance` |
| §18 | Formal verification | **implemented**: TLA+ model of AORG invariants, TLC-checked, with a mutant that is caught. Cedar level isolation proved with SymCC. | `docs/formal`, `tools/cedar-symcc` |
| §19 | Identity, rotation, revocation | **implemented**: rotation certs, SMT revocation (cross-checked against the Rust crate), custody | `identity` |
| App. E | Access-policy-driven keys | **implemented**: Cedar ∧ DAG ∧ ZK clearance gate before decryption | `mcp/memory.recall` |
| — | ZK derivation attestation (POL-011) | **deferred**: needs a Datalog-in-circuit design | — |

## Craig Wright: *Singular Grit* essays (design pressure, not code)

- **Truth Is Expensive. Bullshit Scales.** Adopted as a rule throughout: every receipt says that inclusion ≠ truth, and the origin-falsehood test (SEC-007) proves that a recorded lie stays a lie.
- **The Graph Tax.** Adopted: agents call memory tools directly (MCP). There are no supervisor chains or agent graphs (AGT-010).
- **The Competent Verifier Problem.** Adopted: verifiers use *their own* authoritative policy DAG, pinned vkey and pinned clearance authorities, never the prover's copy (AGT-009). `verifyAuditBundle` refuses a policy path when it holds no DAG.
- **The Memory Hole Meets the Audit Trail.** Adopted: append-only storage, refinement instead of deletion, existence-by-time semantics, and crypto-shredding as the only erasure (LEG-006).
- The other technical essays reviewed (state machines in Script, UTXO-native design, SPV) back the head-UTXO and Rúnar-contract choices.

## Wright / nChain patents and Metanet

- **US10652014B2 (HD keys / common secrets).** Technical overlap with BRC-42 derivation is noted. Licence position: see `v2-legal-notes.md` (LEG-001).
- **WO 2022/100946 (Merkle Proof Entity) and WO 2025/119666 (selective verification).** **Rejected** for implementation: standard BEEF/BUMP proofs are used and the proof-sharding format is avoided (DIS-002 deferred).
- **Metanet.** **Adopted** as an export format: graph heads have a Metanet-compatible JSON head. No new tx type (AOR-011).
- **Bitcoin Script FSM writings (2019).** **Adopted**: transition table without nested IFs, using a committed table and a membership path (AUT-007).

## prof-faustus reference repositories

| Repo | Use |
|---|---|
| `anchorchain` | **Reused vectors**: Merkle (double-SHA-256, odd self-pairing) is vector-identical. **Adopted**: proof-entity two-tree SPV, Bulletproof ZKAccess, custody. Its custom chain layer was **rejected** in favour of the standard BSV stack. |
| `triple-entry-evidence-bsv` (TEA) | **Adopted**: disclosure envelopes, per-field commitments, the origin-falsehood simstudy. |
| `identity-attribution` (`idattr-smt`) | **Reused as oracle**: the TS SMT port matches the Rust crate byte for byte (`tools/smt-vectors`, run via Docker cargo). |
| `idattr-onchain` | Read for the on-chain anchoring pattern; not reused. |

## Transparency logs and verifiable databases

- **Google Trillian / Certificate Transparency / Tessera.** **Adopted**: identity-hash dedup, signed inclusion promise (SCT), consistency by chained roots. **Rejected** as a backend: the head-UTXO chain gives fork-freedom by consensus, so no gossip or monitor network is needed.
- **Sigstore Rekor.** Noted as an optional second witness (ANC-010, deferred).
- **immudb.** Its verifiable-KV ideas were compared; it is not used because the anchors are on-chain and SPV-verifiable.

## Agent memory systems

- **Graphiti / Zep.** **Adopted**: bi-temporal validity (valid_at/invalid_at) and verbatim episodes (AOR-008a/b).
- **Letta MemFS.** Compared for agent-facing memory ergonomics; MCP tools play this role.
- **OriginTrail DKG.** **Adopted**: the three-tier lifecycle draft → committed → anchored (ANC-011).
- **Kryptokrates / OpenWALDO answer receipts.** **Adopted**: receipts bound to the model identity, the correction feedback loop, and source availability (STO-004 is documented as an operator duty).
- **DVC.** Pattern noted for AI-BOM receipts (LEG-009, deferred).

## BSV stack and tooling

| Component | Use |
|---|---|
| `@bsv/sdk` 4.0.1 | Transactions, keys, BEEF/BUMP, SPV. One version for the whole graph (pnpm overrides). |
| `@bsv/overlay` / GASP | `tm_mamem`/`ls_mamem` topic manager and lookup; host-to-host sync of mined anchors. |
| Teranode v0.16.0 (regtest) | Live Chronicle script evaluation; propagation `:8833`, asset `block_legacy`, RPC headers. |
| Rúnar 0.4.6 | All contracts (`MamemAnchor`, `MamemAutomaton`). There is no hand-written script. Two miscompile patterns were found and avoided (see migration-log). |
| Rúnar EC ops (runar.build "Recursive contracts & ZK") | Evaluated for an on-chain Schnorr PoK gate. The compiled script was **883 KB, over the 100 KB rule**, so it was excluded from production. |
| sCrypt ZoKrates / Germouty Groth16-in-Script | Evaluated: on-chain Groth16 needs BN254 pairings, which Rúnar lacks, and the scripts are MB-scale. Verification therefore stays off-chain against the anchored root (ZKM-012 deferred). |
| circom2 / snarkjs / circomlibjs | Groth16 membership circuit, local multi-contributor ceremony. |
| Cedar (WASM) + `cedar-policy-symcc` 0.8 + cvc5 1.3.1 | Runtime authorization; **symbolic proof** of level isolation (POL-015). |
| TLA+ (TLC 2.19) | AGT-008 model checking via Docker. |

## Rejected or deferred approaches (summary)

- A full Trillian fork. **Rejected**: the UTXO chain subsumes its fork-freedom role.
- Per-memory on-chain transactions. **Rejected**: one Rúnar spend per batch of up to 65,536 leaves.
- Hand-written Bitcoin Script. **Rejected** (project rule): Rúnar only.
- On-chain Groth16 and the on-chain Schnorr gate. **Deferred** on script-size grounds; the measurements are recorded.
- Proof sharding (patented format). **Deferred** pending a licence position.
