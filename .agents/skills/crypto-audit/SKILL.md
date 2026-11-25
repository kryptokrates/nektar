---
name: crypto-audit
description: >
  Use when auditing, reviewing, or designing cryptographic or zero-knowledge
  proof (ZKP) components — Groth16/STARK/SNARK verifiers, R1CS/PLONK/gnark
  circuits, BabyBear/Poseidon2 arithmetic, Schnorr-PoK & Fiat-Shamir
  transcripts, Rúnar Bitcoin Script covenants, ECDH/ECIES/AEAD/HPKE
  encryption, KDFs, BRC-369/BRC-42/BRC-43 key wrapping and epoch rotation,
  fair-exchange/verifiable encryption, hashing/Merkle/canonical framing,
  ECDSA/Schnorr signatures, entropy/key custody, and constant-time hygiene.
  Strictly audit-first: inspect, verify, and rank findings with concrete
  evidence before suggesting smallest source fixes and pure-Go library
  upgrades. Never modifies target source during an audit.
---

# crypto-audit — audit first, harden second

Cryptographic bugs fail silently: a missing range check, an incomplete
Fiat-Shamir transcript, an unframed hash concatenation, or an unpinned verifier
outpoint compiles cleanly and passes happy-path tests while breaking soundness,
confidentiality, or fair exchange.

This skill enforces a strict **two-stage discipline**:

1. **Audit first (read-only).** Build a complete cryptographic inventory, run
   automated pre-scans and available Go/security tools, walk every primitive
   against domain invariants, and independently refute/verify every candidate.
   **Never edit target source code during an audit.**
2. **Suggest solutions second.** Only for verified findings, present two-tier
   remediation options: (a) the **smallest effective source fix** + regression /
   Wycheproof / fuzz test, and (b) **pure-Go (`CGO_ENABLED=0`) library or
   protocol upgrades** (Go stdlib `crypto/hkdf`, `crypto/subtle`, `gnark`,
   `tink-go`/HPKE, `circl`, `memguard`, RFC 8785 JCS) with wire-compatibility
   trade-offs clearly marked.

## Operating modes

- **Inline mode (default)** — for diffs, single packages, single Rúnar
  contracts, or focused cryptographic design reviews. Run steps 1–6 in one
  structured pass, including an explicit self-refutation check on every
  candidate finding.
- **Swarm / multi-agent mode** — for whole-repo cryptographic audits or
  explicit full/comprehensive audit requests. Follow
  [references/orchestration.md](references/orchestration.md): parallel Go and
  Rúnar/ZK inventory agents, parallel domain auditors, a fresh refuting
  verifier that challenges every candidate, and parent-led remediation
  synthesis.

## Six-step audit procedure

### 1. Scope & automated pre-scan

Resolve scope (`--files`, `--pkg`, `--contracts`, `--last [n]`, or whole repo:
`internal/`, `contracts/src/`, `vectors/`).

Run the bundled zero-dependency pre-scanner first (read-only):

```bash
node <skill-dir>/scripts/crypto-scan.mjs <target-dir>
```

Check `availableTools` in its JSON output and run whichever local Go/security
tools are installed (see [references/go-tooling-and-libs.md](references/go-tooling-and-libs.md)):
`govulncheck ./...`, `gosec ./...`, `staticcheck ./...`, `semgrep`, `gitleaks`.
Report any unavailable tools in the Coverage footer rather than skipping
silently.

### 2. Cryptographic inventory

Merge pre-scanner output with source inspection into a single **Cryptographic
Inventory** covering every in-scope primitive across all five domains:

1. **ZKP & Verifiable Computation** — Groth16 / STARK / SNARK verifiers,
   bisection games, Schnorr-PoK / Sigma protocols, R1CS/PLONK/gnark circuits,
   algebraic hashes (Poseidon2 over BabyBear).
2. **Encryption, KDF & Key Wrapping** — ECDH/ECIES, AEAD (AES-GCM,
   ChaCha20-Poly1305), KDFs, AAD framing, BRC-369 keyed segments, group epoch
   rotation, fair-exchange / verifiable encryption.
3. **Hashing, Framing, Merkle & Signatures** — domain tags, length-prefixed
   framing vs delimiter concatenation, Merkle trees (second-preimage &
   odd-leaf duplication), canonical JSON/CBOR, ECDSA DER canonicality/low-S,
   signed vs unsigned commitments/receipts.
4. **Key Custody, Entropy & Constant-Time Hygiene** — CSPRNG error checks,
   root/WIF/epoch key storage & permissions, secret zeroization, public-key /
   curve-point validation, `crypto/subtle` constant-time comparisons, timing
   side-channels.
5. **Rúnar / Bitcoin Script Covenant Soundness** — signed `OP_MOD` semantics,
   field-element range checks, `num2bin` sign/length-byte truncation,
   `hashPrevouts`/`hashOutputs` co-spend binding, fee/underflow checks, replay
   and front-running.

### 3. Domain rule walk

Load reference manuals progressively as needed for the in-scope surface:

- Always load [references/rules.md](references/rules.md) for normative
  `zk/*`, `crypto/*`, `hash/*`, `sig/*`, `key/*`, `ct/*`, and `script/*` IDs.
- Load [references/zkp-and-covenants.md](references/zkp-and-covenants.md) when
  scope includes ZKP circuits, Groth16/STARK verifiers, Schnorr-PoK, Poseidon2,
  BabyBear, fair exchange, or any `contracts/src/*.runar.ts`.
- Load [references/nektar.md](references/nektar.md) when auditing the `nektar`
  repository (baseline surface map, wire-compatibility rules, honesty rules).

### 4. Independent refutation & verification

Before any finding enters the report, attempt to **disprove** it:

- Is the input actually bounded, authenticated, or canonicalized by an
  upstream caller or consensus rule?
- Does the claimed collision, forgery, timing leak, or under-constrained
  witness actually cross a real trust boundary?
- Can a minimal, concrete counterexample (input bytes, witness values,
  interleaving, or bounded test) demonstrate the break?

Drop disproved candidates and record the rejected count in the footer. A
finding without exact `file:line` evidence (or scanner/test output) is a
`hypothesis`, never `confirmed`.

### 5. Remediation & library synthesis (solutions second)

Load [references/go-tooling-and-libs.md](references/go-tooling-and-libs.md)
only **after** findings are verified. For each confirmed finding, supply:

1. **Smallest effective source fix** — exact invariant to enforce, narrowest
   patch seam, wire/on-chain compatibility impact (`drop-in` vs `version-bump`
   vs `redeploy-covenant`), and the concrete regression / Wycheproof / fuzz
   test that fails before the fix and passes after.
2. **Library / protocol upgrade option** (when applicable) — e.g., replacing an
   ad-hoc KDF with Go stdlib `crypto/hkdf`, replacing raw ECIES with RFC 9180
   HPKE / `tink-go`, adding `gnark` soundness checks or verifiable-encryption
   proofs, or protecting long-lived secrets with `memguard` / `clear()`.

### 6. Structured report

Emit the audit report in this exact shape:

```markdown
## crypto-audit — <scope>

**Verdict: SOUND | HARDEN | BROKEN**
(BROKEN = any BLOCKER; HARDEN = no BLOCKER, ≥1 HIGH/MED; SOUND = no BLOCKER/HIGH/MED)

### 1. Cryptographic Inventory
| # | Domain | Component (file:line) | Primitive / Curve / Field | Domain Tag / Framing | Verification Boundary |

### 2. Verified Findings
| # | Sev | Rule | Where (file:line) | Violated Invariant | Concrete Break / Attack | Status |

#### Finding Details
For each finding:
- **Invariant**: what must hold mathematically or cryptographically
- **Evidence**: exact `file:line` + quoted code or command output
- **Attack / Failure Mode**: concrete forgery, leak, ambiguity, or bypass
- **Smallest Effective Fix**: narrow patch seam + wire-compat tag + regression/fuzz test
- **Library / Upgrade Option**: stdlib / pure-Go library or protocol upgrade (if applicable)

### 3. Ranked Remediation Roadmap
1. **Immediate (BLOCKER / HIGH, drop-in safe)**
2. **Protocol / Wire-Versioned Upgrades**
3. **Tooling & Continuous Verification** (CI scanners, fuzz targets, Wycheproof vectors, external skills)

### 4. Coverage, Tooling & Refutation Footer
- Scope & files inspected
- Tools executed vs unavailable
- Candidates raised / confirmed / rejected / hypotheses
- Explicit out-of-scope boundaries (never claim unproven coverage)
```

## Severity rubric

- **BLOCKER** — breaks soundness (forged ZK proof, unpinned verifier/stage,
  missing signature/transcript binding allowing arbitrary state transition),
  recovers private keys or plaintext across a trust boundary, allows theft in
  atomic fair exchange, or causes silent zero-entropy key generation.
- **HIGH** — defeats an explicit cryptographic control under realistic
  conditions: hash/concatenation ambiguity enabling signature or commitment
  forgery, nonce reuse or weak KDF without domain separation, unauthenticated
  ciphertext tampering, revoked member decrypting post-revocation data, or
  exploitable timing side-channel on secret comparison.
- **MED** — real cryptographic weakness with narrower preconditions or defense
  in depth gap: high-S DER malleability where txid/hash stability matters,
  backward epoch exposure on member add, plaintext secret lingering in memory
  without zeroization, non-RFC-8785 JSON canonicalization drift, or missing
  curve-point / range checks partially mitigated upstream.
- **LOW** — cryptographic hygiene defect with low direct exploitability (e.g.,
  missing explicit zeroing on ephemeral buffers, undocumented domain-tag alias,
  or missing Wycheproof/fuzz vector coverage on a critical primitive).

## Sibling skills — load, don't re-derive

| Question / surface | Sibling skill |
| --- | --- |
| Header PoW, BEEF/BUMP SPV proof truth, deal-path block waits | `spv-strict` |
| Non-crypto web/HTTP/IPC/OS/supply-chain vulnerability audit | `security-audit` |
| Resource lifecycles, recovery tables, Restate durability | `state-audit` |
| BSV transaction building, UTXO/coin path, broadcast mechanics | `bsv` |
| Test-file purity and fixture invariants | `test-boundaries` |

## Anti-patterns

1. **Fixing before auditing** — never edit target files during an audit run.
2. **Checklist dumping** — never report "use library X" without proving a
   violated invariant or concrete gap in the actual code.
3. **Confusing honesty with cryptography** — in Nektar, proofs establish
   *recorded + unaltered + derivable*, never ground truth of memory content or
   zkML model identity.
4. **Proposing CGO dependencies** — Nektar ships as a single `CGO_ENABLED=0`
   Go binary; never recommend CGO-only crypto libraries without marking the
   constraint violation.
5. **Skipping refutation** — an audit that never challenges or rejects weak
   leads produces noise that obscures real BLOCKERs.
