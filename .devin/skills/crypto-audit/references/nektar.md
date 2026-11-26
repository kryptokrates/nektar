# Nektar v0.1 Cryptographic Surface & Calibration

Load this reference when running `crypto-audit` on `github.com/kryptokrates/nektar`.
It maps every cryptographic package, covenant, test vector, and project-specific
invariant so audits target real trust boundaries instead of generic checklists.

---

## 1. Cryptographic Surface Map

### A. Rúnar BSV Covenants & ZK Contracts (`contracts/src/*.runar.go` + `contracts/cmd/compile` + `internal/contracts/artifacts/*.json`)

| File | Cryptographic Role | Primary Rules to Walk |
| --- | --- | --- |
| `contracts/src/NektarGrothClaim.runar.go` | Groth16 validity-rail settlement (co-spends nChain-zkscript Groth16 verifier UTXO via `hashPrevouts`, emits `ClaimVerdict`) | `zk/unpinned-vk-or-stage`, `zk/statement-binding-gap`, `script/length-prefix-overflow`, `script/unbounded-fee-or-underflow` |
| `contracts/src/NektarZkClaim.runar.go` | Optimistic ZK claim + bisection game (`Settle`, `SettleAnchored`, `Challenge`, `Reveal`, `Bisect`, `ProveStage`, timeouts) | `zk/unpinned-vk-or-stage`, `script/unpinned-cospend`, `script/length-prefix-overflow`, `script/locktime-sequence-bypass`, bisection liveness & deadline updates |
| `contracts/src/NektarAutomatonZk.runar.go` | Stateful automaton with secp256k1 Schnorr-PoK delegation (`StepZk`), plus Merkle `fold` | `zk/fiat-shamir-incomplete`, `zk/non-canonical-field-element`, `key/missing-point-validation`, `hash/merkle-second-preimage` |
| `contracts/src/Poseidon2Stage.runar.go` | Stateful 24-cell BabyBear Poseidon2 round transition covenant for bisection leaf | `script/signed-mod-neg`, `zk/non-canonical-field-element`, constant parity vs `risc0-zkp 3.0.5` |
| `contracts/src/Poseidon2Blocks.runar.go`, `Poseidon2Probe.runar.go`, `BabyBearProbe.runar.go` | BabyBear ($p = 2013265921$) & Poseidon2 building-block probes | `script/signed-mod-neg`, `zk/non-canonical-field-element` |
| `contracts/src/DisclosureSale.runar.go` | Atomic payment-for-disclosure escrow (`Buy`, `BuyGated`, `Refund`) | `crypto/fair-exchange-mismatch`, `script/length-prefix-overflow` *(custom varint)*, `script/locktime-sequence-bypass`, `script/unbounded-fee-or-underflow` |
| `contracts/src/NektarAnchor.runar.go` | Stateful anchor covenant (`Append`, `Rotate`, `WitnessSettle`) | `script/unpinned-cospend`, `hash/missing-domain-tag` (`TagRprime`), `script/length-prefix-overflow` |
| `contracts/src/ClaimVerdict.runar.go` | Settled verdict consumer gate (`Authorize`) | `script/unpinned-cospend`, `script/unbounded-fee-or-underflow` |
| `contracts/src/NektarAutomaton.runar.go`, `NektarCheckpointBond.runar.go`, `NektarFeeVault.runar.go` | Core automaton, checkpoint bond, fee vault covenants | `script/unpinned-cospend`, `script/locktime-sequence-bypass`, `script/unbounded-fee-or-underflow`, auth checks |

### B. Pure-Go Cryptographic Packages (`internal/*`)

| File | Cryptographic Role | Primary Rules to Walk |
| --- | --- | --- |
| `internal/core/hash.go` | `SHA256`, `DSHA256`, `Hash160`, domain tags (`TagLeaf`, `TagRPrime`, `TagAnchorTag`, etc.), `EqualBytes`, `ChainRp` | `hash/missing-domain-tag`, `TagRPrime` vs `TagAnchorTag` consistency, `ct/variable-time-secret-cmp` (verify `EqualBytes` is never used on secrets) |
| `internal/core/merkle.go` | Odd-self-pairing double-SHA-256 Merkle tree (`BuildTree`, `ComputeMerkleProof`, `ReconstructRoot`, `VerifyProof`) | `hash/merkle-second-preimage` (CVE-2012-2459 odd-leaf duplication, height/size enforcement) |
| `internal/core/schema.go` | Fragment & Provenance binary framing (`Field`, `ProvenanceDigest`, `ComputeHDP`, `ComputeLeaf`) | `hash/unframed-concat` (compare `ProvenanceDigest` using `Field(data)` vs `ComputeHDP` raw `data`), integer overflow in `takeField` (`uint32` -> `int` on 32-bit/signed wrap) |
| `internal/core/canonical.go` | `CanonicalJSON`, `HashOf` | `hash/non-canonical-json` (HTML escaping, float/number canonicality vs RFC 8785) |
| `internal/bstack/bbox_crypto.go` | E2E Bbox sealing (`SealBboxBody`, `OpenBboxBody`, `deriveBboxAEADKey`, `BboxAAD`) | `crypto/adhoc-kdf`, `crypto/aad-ambiguity`, `key/missing-point-validation`, `key/unzeroed-secret` |
| `internal/bstack/vault.go` | BRC-369 `bcommon/keyed` segment sealing + `BorgGroup` epoch keys & revocation | `crypto/epoch-secrecy-gap` (forward secrecy on revoke, backward access on `AddMember`, key-at-rest wrapping in `GroupPersistence`), `crypto/nonce-reuse-or-short-salt` (4-byte `SaltLen`) |
| `internal/bstack/wire.go` | Canonical CBOR `nkt1` records + `SignArtifact`/`VerifyArtifact` (ECDSA secp256k1 DER) | `sig/cross-type-replay` (domain separation across `WantAd`/`OfferAd`/`AnnounceAd`), `sig/malleable-der-or-high-s` |
| `internal/bstack/chirp.go`, `module.go` | BRC-167 CHIRP content-addressed packs & 16-lane overlay validation | `hash/missing-domain-tag`, integrity verification before storage/serve |
| `internal/contracts/anchor.go` | Embedded Rúnar artifact loader, `EncodeAnchorLockingScript`/`DecodeNektarAnchor`, offline SPV `VerifyFragment` & `VerifyFragmentStructural` | Artifact parity (`contracts/artifacts/` vs `internal/contracts/artifacts/`), `VerifyFragmentStructural` never reachable on live networks, `TagRPrime` vs `TagAnchorTag` |
| `internal/contracts/sale.go` | `CreateKeyedSaleOffer`, `VerifySaleOffer`, `ClaimKeyedSale`, `OfferDigest` | `crypto/fair-exchange-mismatch` (`hWrapped` vs `KeyCommitment`), `hash/unframed-concat` in `OfferDigest`, `key/unzeroed-secret` (`contentKey`), `sig/malleable-der-or-high-s` |
| `internal/reasoning/grounding.go` | `Receipt`, `ReceiptHash`, `ReceiptToken`, §16 `MakeDelegation`/`VerifyDelegations`, `AORGraph` `vertexHash` & edge hashes | `hash/unframed-concat` (`MakeDelegation`), `hash/delimiter-collision` (`vertexHash`, edge hash), `sig/unsigned-commitment` (`Receipt`) |
| `internal/policy/cedar.go` | Cedar policy `Canonicalize` hash, `MakeReceipt`, `ProveLevelIsolation` | `hash/delimiter-collision` (`DecisionReceipt.Request`), `sig/unsigned-commitment`, bounded isolation probe completeness |
| `internal/wallet/identity.go`, `engine.go`, `desktop_bridge.go` | Root key / WIF `identity.json`, BRC-43 key derivation, embedded vs client wallet custody | `key/plaintext-at-rest`, `key/unzeroed-secret`, Windows ACLs, loopback client boundary |
| `internal/server/auth_tokens.go`, `oauth.go`, `http.go`, `security_helpers.go` | Auto-generated HTTP/operator tokens, OAuth 2.1 + PKCE S256, CSRF tokens, BRC-105 / HTTP 402 paywall | `key/unchecked-rng` (`_, _ = rand.Read(...)`), `ct/variable-time-secret-cmp` (bearer token map lookup & header comparison), BRC-105 payment claim verification |

### C. Cross-Language Test Vectors (`vectors/`)

- `vectors/core.json` — canonical hashing, framing, Merkle vectors
- `vectors/curve-rust.json` — secp256k1 / Schnorr / curve vectors
- `vectors/smt-rust.json` — legacy/reference SMT vectors

Whenever suggesting any change touching hashing, framing, curve math, or
serialization, check whether `vectors/*.json` or embedded Rúnar artifacts must
be regenerated/versioned.

---

## 2. Nektar-Specific Calibration Rules

1. **Honesty Rule**: Never treat `TruthNote` or declared `ModelIdentity` as
   broken cryptography — Nektar explicitly documents that proofs establish
   *recorded + unaltered + derivable* (not truth of content or zkML). Flag a
   defect ONLY if a code path omits `TruthNote`, fakes a receipt footer, or
   misrepresents a declared field as cryptographically proven.
2. **Zero-CGO Constraint**: Every recommended Go library MUST build with
   `CGO_ENABLED=0`. Reject suggestions requiring CGO/OpenSSL/libsecp256k1 C
   bindings unless explicitly requested.
3. **Wire & On-Chain Compatibility Classification**: Tag every suggested fix as:
   - `[drop-in]` — no wire/on-chain format change (e.g., checking `rand.Read`
     errors, adding `subtle.ConstantTimeCompare`, zeroing buffers, adding input
     validation or `VerifyFragmentStructural` guards).
   - `[wire-v2]` — requires bumping envelope/protocol version (e.g., migrating
     `BboxSealedBody` from `v: 1` raw-SHA256 KDF to `v: 2` `crypto/hkdf`, or
     framing `MakeDelegation` under `nektar:delegation:v2`).
   - `[covenant-redeploy]` — requires recompiling Rúnar `.runar.go` (`cd contracts && CGO_ENABLED=0 go run ./cmd/compile -prototypes`) ->
     `contracts/artifacts/*.json` -> `internal/contracts/artifacts/*.json` and
     keeping backward-compatible decoders for existing on-chain UTXOs.
4. **SPV Boundary Split**: Do not duplicate `spv-strict` (header PoW, BEEF
   ancestry, funding-tree stock, block waits). Cross-reference `spv-strict` if
   an SPV header-trust bug is observed en route; keep `crypto-audit` focused on
   cryptographic soundness, ZK proofs, encryption, framing, keys, and Script
   arithmetic.
