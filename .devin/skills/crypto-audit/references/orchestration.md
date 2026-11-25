# Orchestration — Multi-Agent Cryptographic Audits

Use this orchestration layout for whole-repo cryptographic audits or explicit
comprehensive audit requests. Default for small diffs or single packages is
inline mode (see `SKILL.md`).

---

## Roles & Isolation

- **Parent** — scopes the run, executes the automated pre-scanner
  (`scripts/crypto-scan.mjs` + available CLI tools), owns the merged
  Cryptographic Inventory, launches isolated agents, deduplicates candidates,
  and synthesizes final ranked remediation options.
- **Research / Explorer agents** (read-only) — run Stage 1 inventories, Stage 2
  domain audits, and Stage 3 independent refutation. Agents never edit target
  source and never verify their own findings.
- **General agent** (bounded local execution, optional) — used ONLY in Stage 3
  when a verifier needs to run a minimal read-only unit test or vector check to
  settle a disputed mathematical/cryptographic claim.

---

## Four-Stage Pipeline

### Stage 1 — Parallel Inventory + Automated Pre-Scan

Parent runs `node <skill-dir>/scripts/crypto-scan.mjs <target>` (plus any
installed `govulncheck`/`gosec`/`gitleaks`), while launching two parallel
read-only inventory agents:

1. **Go Crypto Inventory Agent** — scans `internal/` and `cmd/` for every hash,
   KDF, AEAD, ECDH, ECDSA/Schnorr signature, BRC-369/BRC-43 key op, Merkle
   tree, canonical serializer, RNG call, secret store, and auth token comparison.
2. **ZKP & Rúnar Covenant Inventory Agent** — scans `contracts/src/*.runar.ts`,
   `internal/contracts/`, and `vectors/` for every ZK verifier, bisection game,
   Schnorr-PoK, Poseidon2/BabyBear arithmetic op, HTLC/fair-exchange covenant,
   and artifact parity check.

Parent merges both outputs + scanner output into the canonical **Cryptographic
Inventory** (assigning stable IDs `inv-go-01`, `inv-zk-01`, …).

### Stage 2 — Parallel Domain Auditors

Launch three isolated read-only auditors against the merged inventory. Each
auditor receives the inventory slice, the exact reference file paths to load
(`rules.md`, `zkp-and-covenants.md`, `nektar.md`), and MUST return one JSON
object:

1. **ZKP & Covenant Auditor** — audits Groth16, optimistic bisection,
   Schnorr-PoK, BabyBear/Poseidon2, Rúnar Script arithmetic, co-spend pins, and
   fair-exchange soundness (`zk/*`, `script/*`, `crypto/fair-exchange-mismatch`).
2. **Encryption, Framing & Signatures Auditor** — audits ECDH/AEAD/KDF,
   BRC-369 vault & `BorgGroup` epochs, Merkle trees, canonical JSON/CBOR,
   domain separation, framing, and ECDSA signatures (`crypto/*`, `hash/*`,
   `sig/*`).
3. **Key Custody, Entropy & Side-Channel Auditor** — audits CSPRNG checks,
   key-at-rest storage/permissions, zeroization, curve-point validation, OAuth /
   PKCE / bearer token / BRC-105 constant-time hygiene (`key/*`, `ct/*`).

#### Domain Auditor JSON Contract

```json
{
  "auditor": "zkp-covenants | crypto-framing-sigs | keys-entropy-ct",
  "checked_inventory_ids": ["inv-zk-01", "inv-zk-02"],
  "candidates": [
    {
      "id": "cand-01",
      "rule": "zk/fiat-shamir-incomplete",
      "severity": "BLOCKER | HIGH | MED | LOW",
      "where": "contracts/src/NektarAutomatonZk.runar.ts:51",
      "invariant": "Challenge must bind every transition input authorized by the proof",
      "evidence": "quoted code lines",
      "attack": "concrete forgery, front-run, collision, or leak scenario",
      "wire_compat": "drop-in | wire-v2 | covenant-redeploy"
    }
  ],
  "cross_domain_notes": [
    { "inventory_id": "inv-go-04", "note": "handed off to peer auditor" }
  ]
}
```

### Stage 3 — Fresh Refuting Verifier

Every candidate finding goes to a **fresh read-only verifier agent** that did
NOT participate in Stage 2. Its sole mandate is to try to **disprove** each
candidate:

- Trace callers and consensus constraints to see if the invariant is already
  enforced upstream or on-chain.
- Verify the concrete attack actually works under the language/runtime/Script
  semantics (e.g., actual Bitcoin Script `OP_MOD` / `num2bin` rules, actual Go
  `go-sdk` EC point checks).
- Return strictly one verdict per candidate: `confirmed`, `rejected` (with
  disproving `file:line`), or `hypothesis` (if blocked on an external artifact
  like an uninspected verifier script binary).

### Stage 4 — Parent Remediation Synthesis

Only for `confirmed` findings (never for `rejected` ones), the parent loads
`references/go-tooling-and-libs.md` and produces:

1. **Smallest Effective Source Fix** (exact invariant, file/function seam,
   wire-compat tag, regression/Wycheproof/fuzz test).
2. **Pure-Go Library / Protocol Upgrade Option** (Go stdlib `crypto/hkdf`,
   `crypto/subtle`, `gnark`, `tink-go`/HPKE, `circl`, `memguard`, RFC 8785)
   with explicit trade-offs.
3. Final Markdown report following `SKILL.md` Step 6, including rejected
   candidate count and full coverage/tooling disclosure.

---

## Anti-Patterns

1. Auditing without first building the complete inventory — uninventoried
   primitives are blind spots.
2. Letting a domain auditor edit files or propose big library rewrites before
   refutation finishes.
3. Passing Stage 2 auditor prose directly into the final report without Stage 3
   refutation.
4. Dropping `cross_domain_notes` between auditors (e.g., `sale.go` <->
   `DisclosureSale.runar.ts` spans Go and Rúnar and MUST be evaluated across
   both sides together).
