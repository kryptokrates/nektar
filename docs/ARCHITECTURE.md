# mamem architecture

mamem is a BSV-native implementation of the Merkle Automaton (arXiv:2506.13246). It is encrypted, provenance-bearing agent memory whose history is append-only and anchored. State transitions are enforced by Rúnar UTXO contracts. Everything a verifier needs ends in the BSV header chain (GEN-003).

## 1. Layers

```mermaid
flowchart TB
  subgraph L8["L8 Agent interface"]
    MCP["MCP stdio server<br/>remember · recall · prove · anchor · challenge · authorize<br/>proveMembership · verifyMembership · rotate · revoke · disclose · automaton"]
  end
  subgraph L7["L7 Reasoning"]
    RS["grounding · receipts · correction loop · audit bundle · consensus time"]
  end
  subgraph L6["L6 Policy"]
    CED["Cedar (WASM) + SymCC proof"]
    DAG["Policy DAG (stratified Datalog)"]
  end
  subgraph L5["L5 Graph"]
    AORG["AORG · refinement · instancing/delegation"]
  end
  subgraph L4["L4 Identity & ZK"]
    ID["bindings · challenges · rotation certs · SMT revocation · custody"]
    ZKA["ZK clearance (Bulletproofs)"]
    ZKM["ZK membership (Groth16 / Poseidon)"]
  end
  subgraph L3["L3 Crypto"]
    CR["per-fragment HKDF keys · AES-256-GCM · level keyspaces · disclosure wraps"]
  end
  subgraph L2["L2 Log & verify"]
    LOG["BatchLog (append-only, promises)"]
    VER["verifyFragment (leaf→R→anchor→BUMP→headers)"]
  end
  subgraph L1["L1 Anchoring (Rúnar contracts)"]
    ANC["MamemAnchor — one head UTXO per shard"]
    AUT["MamemAutomaton — δ enforced on-chain"]
  end
  subgraph L0["L0 BSV"]
    TN["Teranode (Chronicle) · headers · BEEF/BUMP · overlay (tm_mamem / ls_mamem, GASP)"]
  end
  MCP --> RS --> DAG --> AORG
  MCP --> CED
  MCP --> ZKA & ZKM & ID
  AORG --> LOG
  CR --> LOG
  LOG --> ANC --> TN
  AUT --> TN
  VER --> TN
  ZKM --> VER
```

## 2. Package graph

```mermaid
flowchart LR
  core --> chain
  core --> log
  core --> contracts
  contracts --> anchor
  chain --> anchor
  log --> anchor
  identity --> anchor
  anchor --> overlay
  contracts --> verify
  chain --> verify
  core --> crypto
  core --> identity
  crypto --> zk
  contracts --> zk
  identity --> zk
  datalog --> aorg["graph"]
  identity --> aorg
  contracts --> automaton
  anchor --> automaton
  anchor & overlay & verify & crypto & identity & zk & aorg & datalog & policy & automaton --> mcp
  mcp & aorg & verify & datalog --> reasoning
  mcp & reasoning --> e2e
  mcp & zk & automaton & identity --> fuzz
```

## 3. Write path: remember → batch → anchor

```mermaid
sequenceDiagram
  autonumber
  participant A as Agent
  participant M as MamemMemory
  participant L as BatchLog
  participant C as AnchorChain (Rúnar)
  participant T as Teranode
  A->>M: remember(shard, d, provenance, level)
  M->>M: hdp = H(d‖p) · E = AES-GCM(k_frag(level), d)
  M->>L: append(fragment) — dedup by hdp
  L-->>A: signed inclusion promise (seq, leaf)
  Note over L: batch closes (count policy, ≤ 65,536 leaves)
  M->>M: R = Merkle(leaves) · poseidonRoot = Poseidon(leafCommit)
  M->>C: anchor(size, R, poseidonRoot)
  C->>C: fresh contract from decoded head · sign append
  C->>T: tx spends head UTXO → new head (n+1, R' = H(tag‖R'‖R‖size))
  T-->>C: accepted (script-evaluated) · later mined → BUMP
```

## 4. Read path: recall with identity, ZK clearance, Cedar and DAG

```mermaid
sequenceDiagram
  autonumber
  participant B as Reader B
  participant M as MamemMemory
  participant Z as ZK clearance verifier
  participant P as Cedar + Policy DAG
  participant V as verifyFragment
  B->>M: challenge()
  M-->>B: nonce (issued here, 5 min)
  B->>M: recall(shard, seq, identity{binding, sig(nonce)}, zk{commitment, proof, transcript})
  M->>M: verifiedCaller: binding.pubkey = pubkey · nonce issued & fresh · sig ok
  M->>Z: authority ∈ trusted · holder = caller · transcript scope = shard/seq · not replayed · range proof
  M->>P: Cedar(principal = hash160(pk), level = PROVEN level) ∧ DAG derives permit ∧ no conflict
  M->>V: proof bundle → anchor → BUMP → headers
  alt every gate passes
    M->>M: decrypt with the level key (only now)
    M-->>B: plaintext + receipt (zk, cedar, dag root, derivation hash, truth note)
  else any gate fails
    M-->>B: no plaintext, no ciphertext · specific reason
  end
```

## 5. Anchor contract state machine (`MamemAnchor`)

```mermaid
stateDiagram-v2
  [*] --> Head0: deploy (genesis, n=0, owner pk0)
  Head0 --> HeadN: append(sig_owner, R, size, poseidonRoot) — n+1, R' chained
  HeadN --> HeadN: append — spends the head UTXO (no fork possible)
  HeadN --> HeadN: rotate(sig_owner, newOwnerPkh) — n, R, R' unchanged
  note right of HeadN
    shard id = hash160(genesisOwner), stable across rotations
    wrong signer / n / R' / tag → script fails (Teranode rejects)
  end note
```

## 6. Automaton contract (`MamemAutomaton`)

```mermaid
stateDiagram-v2
  [*] --> Running: deploy(q0, deltaRoot, finalRoot)
  Running --> Running: step(sig, from = q, sym, to, outHash, δ-path) — (from, sym, to) ∈ δ
  Running --> Halted: halt(sig, F-path) — q ∈ F
  Halted --> [*]
  note right of Running
    δ and F are Merkle-committed (depth 4, 16 entries each)
    no nested IFs over the table (AUT-007) · 1,007-byte locking script
    halt continues state (runar 0.4.6 terminal-method caveat)
  end note
```

## 7. Identity, rotation and revocation

```mermaid
flowchart LR
  pk0["owner pk⁰ (genesis)"] -- "rotation cert C¹ = Sign_pk⁰(H(pk⁰‖pk¹‖t))" --> pk1["pk¹"]
  pk1 -- "C²" --> pk2["pk²"]
  pk1 -. "MamemAnchor.rotate (on-chain)" .-> head["anchor head ownerPkh"]
  pk0 -- "derive_key(pk⁰)" --> smt["revocation SMT (2²⁵⁶ keys)"]
  smt -- "root anchored" --> chain[("BSV")]
  cert["cert log"] -- "anchored" --> chain
  verifier["verifier"] -- "inclusion / non-inclusion proof vs anchored root" --> smt
```

## 8. ZK: membership and clearance over one query

```mermaid
flowchart TB
  q["ZkQuery {nonce, verifierId}"] --> qid["queryId = H(tag‖nonce‖verifierId) mod r"]
  subgraph holder["holder (secrets: blinding, level, opening)"]
    mem["Groth16: leaf = Poseidon(hi, lo, blinding) ∈ Poseidon root<br/>nullifier = Poseidon(blinding, queryId)"]
    clr["Bulletproof: level − required ≥ 0 on C = ℓG + rH<br/>(C signed by a trusted authority, bound to holder pk)"]
  end
  qid --> mem & clr
  mem --> v1["verifyMembershipAnchored: expected shard · canonical chain · SPV · vkey pinned · nullifier unused"]
  clr --> v2["verifyClearance: trusted authority · holder = verified caller · transcript scope · single use"]
  v1 & v2 --> comb["verifyCombined: same nonce, same queryId"]
```

## 9. Overlay synchronisation (GASP)

```mermaid
sequenceDiagram
  participant H1 as overlay host 1 (tm_mamem)
  participant H2 as overlay host 2 (empty)
  H2->>H1: GASP initial request (since)
  H1-->>H2: UTXO list (shard heads)
  loop each head
    H2->>H1: request node (anchor tx + BUMP + topical ancestors)
    H1-->>H2: graph node
    H2->>H2: tm_mamem admits only a valid successor of the known head<br/>(forged genesis / non-head spend rejected)
  end
  Note over H2: converges to host 1 heads, including rotations
```

## 10. Reasoning, correction and audit

```mermaid
flowchart LR
  ans["model answer (claims + citations)"] --> g["ground(): quote or Datalog entailment from VERIFIED fragments"]
  g --> rcp["receipt = H(answer‖refs‖check‖model id‖time‖grounding hash) → anchored"]
  rcp --> idx["citation index"]
  rev["source revised (new record)"] --> idx
  idx --> corr["onRevision: re-ground → withdrawn claims → correction anchored"]
  rcp --> bundle["audit bundle: output → fragments → signers → policy path → anchors"]
  bundle --> off["verifyAuditBundle(headers, OUR policy DAG) — offline"]
```

## 11. Data model

```mermaid
classDiagram
  class Fragment { d: bytes (plaintext, dropped when encrypted) p: Provenance hdp = H(d‖p) E: AES-GCM ciphertext }
  class Provenance { source time signature modelId }
  class LogRecord { shard seq leaf hdp level batchIndex leafIndex anchorTxid }
  class AnchorState { genesisOwner n size R Rp poseidonRoot tagRprime ownerPkh }
  class ProofBundle { shard n size leaf leafIndex path anchor chainTail anchorBEEF }
  class Vertex { kind phi sigma t context h }
  class Edge { from to rule e = H(from‖to‖rule) }
  class Receipt { answer fragmentRefs checkResult modelId timestamp groundingReportHash truthNote }
  class AuditBundle { receipt receiptHash fragments signers policy }
  Fragment --> Provenance
  LogRecord --> Fragment
  LogRecord --> AnchorState : batchIndex → n
  ProofBundle --> AnchorState
  Vertex --> Edge
  AuditBundle --> Receipt
  AuditBundle --> ProofBundle
```

## 12. Deployment

```mermaid
flowchart LR
  subgraph host["operator host"]
    subgraph mamem["mamem (docker compose)"]
      mcp["mamem-mcp (stdio)"]
      demo["demo client (health/smoke)"]
    end
    subgraph tn["Teranode regtest (pnpm stack:up)"]
      prop["propagation :8833"]
      asset["asset API :8090 (block_legacy)"]
      rpc["RPC :9292 (headers)"]
      pg[("postgres")] 
      ae[("aerospike")]
      kf[("kafka")]
    end
    st[(".state/ treasury, reserve — gitignored")]
  end
  client["Claude Desktop / local Qwen (OpenAI-compatible)"] -- MCP --> mcp
  mcp -- broadcast --> prop
  mcp -- BUMP --> asset
  mcp -- headers --> rpc
  mcp --- st
```

## 13. Checkpoints, promise monitor, finality and the HTTP surface

```mermaid
flowchart LR
  w["remember (signed provenance verified)"] --> pr["inclusion promise (seq, leaf, sig)"]
  w --> b["batch close: leaves · age · bytes · cost"] --> a["anchor (Rúnar spend)"]
  a --> cp["signed checkpoint (logId, size, n, R, R', time, txid)"]
  pr --> mon["PromiseMonitor (max merge delay)"]
  cp --> mon
  mon --> met["/v1/metrics → ops/alerts.yml"]
  a --> fin["anchorStatus: pending → mined(k) → final(depth)"]
  fin --> rd["recall(requireFinal)"]
  cp --> api["/v1/checkpoint/{shard}"]
  a --> api2["/v1/proof/{shard}/{seq}"]
  sec[("securityStore: challenges · transcripts · nullifiers · revocations")] --- rd
```

## 14. Test and release pipeline

```mermaid
flowchart LR
  b["pnpm -r build"] --> d["pnpm check:deps"] --> t["pnpm -r test (unit · contract · integration · fuzz)"] --> r["pnpm reproduce (vectors, vkey)"] --> e["pnpm e2e (offline, full paper)"] --> rg["MAMEM_NETWORK=regtest pnpm e2e ×3"]
  t -.-> f["pnpm formal (TLC)"]
  t -.-> s["pnpm cedar:symcc (SymCC + cvc5)"]
  t -.-> v["pnpm smt:vectors (Rust oracle)"]
  rg --> img["docker build (clean-checkout gate)"]
```

## Key decisions (ADR summary)

| ADR | Decision |
|---|---|
| 001 | TypeScript on `@bsv/sdk`; Rust references used as vector oracles (SMT run via Docker cargo) |
| 002 | Double-SHA-256 Merkle, odd self-pairing, leaf count committed |
| 003 | Trillian is a design reference only; fork-freedom comes from the head UTXO |
| 011 | Standard BSV stack (Teranode, overlay, BEEF/BUMP) replaces custom chain code |
| 012 | One anchor UTXO per shard; every successor spends it (Rúnar `MamemAnchor`) |
| 013 | Standard BEEF/BUMP proofs (no patented proof-entity formats) |
| 014 | BRC-42-style key derivation, level as KDF namespace |
| 015 | ~~ZK membership over a Poseidon companion tree whose root rides in the anchor~~ — **superseded by ADR-020** |
| 016 | Horizontal sharding; shards never coordinate |
| 017 | Automaton δ and F Merkle-committed in the Rúnar state; every method continues state |
| 018 | One `@bsv/sdk` version across the dependency graph (pnpm overrides) |
| 019 | Verifier-issued challenges; clearance holder-bound and authority-pinned; the Cedar level comes only from a verified ZK proof |
| 020 | ZK membership as Wright §13 specifies: **one** double-SHA-256 tree (the anchored R_n), proven with a transparent **ZK-STARK** (RISC Zero 3.0.3 succinct receipt; Fiat–Shamir, FRI, no trusted setup), verified off-chain (WASM) against the SPV-verified anchor with a pinned program ID and a verifier-issued single-use queryId. See below. |

### ADR-020 — hash-based STARK over the anchored SHA-256 tree

**Context.** Wright §13 / App. C define π = ZKMerkleProve(h_j, R_n, P_j) over the tree anchored on-chain. H is "a collision-resistant hash, e.g., SHA-256". Proofs are non-interactive via Fiat–Shamir. Only "public parameters for H, ZKProve, ZKAccess" are needed, with no central authority. ADR-015 departed from this in two ways:
- a second (Poseidon) tree, whose equality with R_n was never proven;
- a Groth16 trusted setup.

**Options evaluated (measured or documented, Nov 2026).**

| Option | Finding |
|---|---|
| SP1 (Plonky3 STARK) — the BSV-stack choice (Rúnar ships an SP1 FRI verifier; BSVM verifies SP1 STARKs on-chain) | **Rejected:** Succinct's security model states "individual STARK proofs in SP1 do not currently satisfy the zero-knowledge property". Only the Groth16/PLONK wrapper is ZK, which reintroduces a trusted setup. A non-ZK proof would leak the leaf, breaking §13. |
| RISC Zero succinct receipt | **Chosen:** documented as a zero-knowledge STARK (DEEP-ALI + FRI, trace noise added; "hides all program inputs"). Composite receipts leak the trace length, so only succinct receipts are accepted. |
| Noir/UltraHonk | Not chosen: universal setup (KZG), so not hash-only. |
| Bulletproofs over SHA-256 | Transparent, but impractical prover cost at depth 16. |

**Measured on this machine** (CPU only, prover in Docker; spike `tools/stark/`):
- **Program:** reproduces `core/merkle.ts` for all 561 leaf positions of sizes 1..33, and refuses index ≥ size, a short path, a broken odd self-pair and a foreign leaf at the odd position.
- **Real Teranode anchor** `662e9ae9…7a77`:
  - prove **34.9 s**, succinct receipt **224 KB**;
  - WASM verify in native Node **37–83 ms**;
  - the proven root equals the anchored R and the size equals the anchored size.
- **WASM verifier rejects:** a tampered receipt, a swapped header root or queryId, and a foreign program ID.

**On-chain verification:** not available for RISC Zero receipts in Rúnar, which only has an SP1 FRI verifier. Verification stays off-chain against the SPV-verified anchor, which §13 permits ("executable on-chain if necessary").

**Consequences:**
- the Poseidon tree, the Groth16 circuits and ceremony, and the anchor's `poseidonRoot` are removed (MamemAnchor v3, fresh regtest chains);
- the nullifier is replaced by a verifier-issued single-use queryId;
- proofs are larger (224 KB vs 389 B, all off-chain);
- proving is slower (≈35–50 s per proof on CPU);
- trust moves from a ceremony to the RISC Zero implementation plus the pinned program ID.
