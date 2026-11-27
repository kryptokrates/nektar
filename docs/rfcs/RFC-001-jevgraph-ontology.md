# RFC-001: JevGraph Native Ontology, Bounded Extraction & Cross-Ontology Bridge Shards

- **Status:** Implemented (`v0.1` Pure-Go)
- **Authors:** Nektar Core
- **Requires:** `internal/core`, `internal/jev`, `internal/reasoning` (Google Mangle), `internal/memrepo`, `internal/bstack`
- **Canonical Code:** `internal/jevgraph/{ontology,blocking,pipeline,align}.go`, `internal/memory/memory_ontology.go`, `internal/memrepo/capsule_verify.go`, `internal/server/tools_ontology.go`

---

## 1. Motivation

Cross-agent memory trade breaks down if every agent invents ad-hoc internal schemas: a buyer cannot safely ingest purchased triples or reason over them without risking predicate hallucination or semantic drift. **RFC-001** standardizes:

1. **Content-addressed domain TBox ontologies** anchored as immutable Nektar Memory Repos (`ont-<domain>/ontology.yaml`).
2. **A deterministic 4-stage bounded extraction pipeline** pairing an optional self-hosted System-1 evaluator (`internal/jev`) with a fail-closed Google Mangle Datalog consistency gate.
3. **Verifiable edge commitments (`EdgesRoot`)** bound into L1 MPE selective-disclosure roots and offline `.nkt` capsule verification.
4. **Cross-Ontology Bridge Shards (`bridge-<src>-<dst>`)** so agents using different TBoxes translate facts via verifiable OWL 2 / RDFS / SKOS mappings without rewriting history.

---

## 2. Domain TBox Schema & Canonical `OntologyID`

A domain ontology is authored as YAML or JSON (`ParseOntologyAuto`) and committed to a Memory Repo named `ont-<domain>` at `ontology.yaml`.

### 2.1 Structural Limits (Fail-Closed)

| Limit | Constant | Value |
|---|---|---|
| Raw document size | `MaxOntologyBytes` | `1 MiB` (`1 << 20`) |
| Class declarations | `MaxOntologyClasses` | `256` |
| Relation declarations | `MaxOntologyRelations` | `256` |
| Inline gazetteer entries | `MaxOntologyEntities` | `4096` |

Class and relation parent hierarchies (`parent`) MUST be acyclic (`ErrOntologyCycle`).

### 2.2 Canonical `OntologyID` Invariance

To ensure independent agents sharing the same vocabulary produce identical 64-hex wire IDs while tuning local entity gazetteers and risk thresholds independently:

```text
OntologyID = hex(DSHA256(TagOntologyV1 || CanonicalJSON(TBox)))
```

- **Included in `TBox`:** `version`, `name`, `namespace`, `description`, sorted `classes` (`id`, `label`, `definition`, `parent`, sorted `disjointWith`), sorted `relations` (`id`, `label`, `domain`, `range`, `definition`, `parent`, `inverseOf`, `transitive`, `functional`, `symmetric`), and sorted `axioms`.
- **Excluded from `OntologyID`:** `entities` (local gazetteer aliases/RE2 patterns) and `thresholds` (local integer-bps acceptance/review cutoffs).

### 2.3 Deterministic Mangle Compilation

`Compile(ont)` compiles every TBox declaration into stratified Google Mangle Datalog rules covering:
- Class hierarchy (`rdfs:subClassOf` transitive closure) and relation hierarchy (`rdfs:subPropertyOf`).
- Domain & range type checks (`domain`, `range`).
- Disjointness conflict rules (`disjointWith`).
- Algebraic relation properties (`inverseOf`, `symmetric`, `transitive`, and `functional` single-valued cardinality conflicts).

---

## 3. Four-Stage Bounded Extraction Pipeline

Given fragment text `T` and a compiled `Ontology`:

1. **Stage 1 — $O(nk)$ Blocking (`blocking.go`):**
   - `SegmentSentences(T)` splits `T` into exact UTF-8 byte-offset spans `[byteStart, byteEnd)`.
   - `ExtractMentions(T, ont, hints)` performs case-folded Unicode word-boundary longest-match-first matching over canonical entity IDs, aliases, and bounded RE2 patterns.
   - `BlockCandidates` emits only typed entity pairs `(s, o)` within the sliding sentence window whose types have at least one valid relation in `allowedRels[subType][objType]`.
2. **Stage 2 — Closed-Set System-1 Classification (`internal/jev`):**
   - For each candidate pair, Jev evaluates a closed-set **Choice** over the allowed TBox relations (plus `none`) and a **Noul** affirmative-support check to catch negation/counterfactual modality traps.
   - All probabilities are quantized to integer basis points (`0..10000`): `confidenceBp`, `marginBp` (`top1 - top2`), and `supportBp`.
   - **Offline degradation:** If the Jev evaluator is unreachable, candidates degrade to `status = "review"` with reason `jev_offline` (or fail closed with `ErrEvaluatorRequired` when `Strict: true`).
3. **Stage 3 — Deterministic Mangle Gate (`ValidateEdgeAgainstOntology`):**
   - Every proposed triple is checked against the compiled Mangle TBox rules for domain/range validity, class disjointness, and functional cardinality conflicts. Violations are deterministically rejected (`status = "rejected"`).
4. **Stage 4 — Triage, Commitments & Gap Signals:**
   - Default acceptance thresholds: `acceptMinConfidenceBp = 7500`, `acceptMinMarginBp = 1500`, `acceptMinSupportBp = 7000`, `reviewMinConfidenceBp = 4500`.
   - Low-margin candidates emit an `OntologyGapSignal` for operator TBox refinement.
   - Accepted triples hash canonically under `TagTripleV1` and fold into a deterministic Merkle root `EdgesRoot` (`ComputeEdgesRoot`).

---

## 4. Storage, L1 MPE Binding & Privacy Boundaries

### 4.1 L1 MPE & Sidecar Commitments

- When a fragment is committed with an ontology, `ontologyId` and `edgesRoot` are bound into the fragment's L1 MPE field set (`ProveFragmentFields`). Fragments without an ontology keep their MPE root byte-identical to pre-RFC-001 commitments.
- For **explicitly `Public: true`** fragments, `graph/<path>.edges.json` is committed atomically in the same Git `TreeObject` and verified offline during `.nkt` capsule verification (`memrepo.VerifyCapsule` → `jevgraph.VerifySidecar` checks `sourceSha256`, exact UTF-8 byte spans, `TagTripleV1` hashes, and `EdgesRoot`).

### 4.2 Confidentiality Invariants (CLKA & Borg)

- **CLKA-encrypted** and **BRC-369 Borg-encrypted** fragments NEVER write plaintext edge sidecars into `mem_objects` or public `.nkt` capsules.
- **Borg-encrypted** fragments write zero plaintext rows to SQLite `knowledge_edges`.
- **Right-to-Forget (`ForgetFragment`):** Atomically shreds `knowledge_edges`, `fragment_graph_meta`, and `graph/<path>.edges.json` sidecar blobs while preserving the on-chain Merkle commitment.

---

## 5. Cross-Ontology Bridge Shards

Two ontologies `A` and `B` are aligned without rewriting historical fragments by committing a `BridgeManifest` into a `bridge-<src>-<dst>` Memory Repo:

- **Supported Predicates:**
  - Entailing: `owl:equivalentClass`, `rdfs:subClassOf`, `owl:equivalentProperty`, `rdfs:subPropertyOf`, `skos:exactMatch`.
  - Advisory-only (NOT compiled into hard Mangle entailment): `skos:closeMatch`.
- **Canonical `BridgeID`:**
  ```text
  BridgeID = hex(DSHA256(TagOntologyV1 || CanonicalJSON({v:1, sourceOntologyId, targetOntologyId, mappingHashes})))
  ```
- **Mangle Consistency:** `AlignOntologies` rejects bridge mappings that induce disjointness contradictions across `A ∪ B`.

---

## 6. Proof-Carrying Grounding & Market Interoperability

- **`GroundedAnswer` / `VerifyReceipt`:** Feeds quote-verified accepted `knowledge_edges` alongside committed `ont-*` TBox rules and `bridge-*` rules into Google Mangle, attaching `RuleRef`s to `nektarr1.*` receipts so offline verifiers independently re-check derivation.
- **Market Wire (`nkt1`):** `WantAd.OntologyID`, `OfferAd.{OntologyID, EdgesRoot}`, `PackRef.{OntologyID, TargetOntologyID}`, and `StandingOrder.{OntologyID, AllowBridge}` enable exact or bridge-aligned autonomous matching (see [MARKET-WIRE.md](../MARKET-WIRE.md) and [RFC-002](RFC-002-nmu1-commodity-standard.md)).

---

## 7. Honest Boundaries (Normative)

- `EdgesRoot` and sidecar verification prove **verbatim span citation, canonical `TagTripleV1` hashing, deterministic Mangle TBox consistency, and on-chain commitment** — **never** real-world factual truth or extraction completeness.
- Self-hosted System-1 Jev model identity, `confidenceBp`, `marginBp`, and `supportBp` are **declared/advisory provenance only**, not zkML-proven. Every extraction and bridge artifact MUST carry its advisory note (`AdvisoryExtractionNote` / `AdvisoryBridgeNote`).
