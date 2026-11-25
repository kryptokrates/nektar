# Go Cryptographic Audit Tooling, External Skills & Pure-Go Libraries

Load this reference during **Step 1 (Automated Pre-Scan)** and **Step 5
(Remediation & Library Synthesis)**.

All library recommendations below respect `nektar`'s **single-binary,
zero-CGO (`CGO_ENABLED=0`)** constraint.

---

## 1. CLI Tools for Auditing Go & Covenant Code (Zero Source Changes)

Run whichever tools are available locally; report missing tools in the audit
report's Coverage footer.

| Tool | Install command | Run command | What it catches |
| --- | --- | --- | --- |
| **Bundled Pre-Scanner** (zero-dep Node) | *(included)* | `node <skill-dir>/scripts/crypto-scan.mjs .` | Inventory of Go crypto + Rúnar ZK/EC sites; unchecked `rand.Read`, raw SHA-256 KDF, delimiter hashing, `OP_MOD` without range guards, `VerifyFragmentStructural` misuse |
| **`govulncheck`** (Go official) | `go install golang.org/x/vuln/cmd/govulncheck@latest` | `govulncheck ./...` | Reachability-based CVE scan across Go stdlib `crypto/*`, `bsv-blockchain/go-sdk`, `bcommon`, `cedar-go`, `mangle-go` |
| **`gosec`** | `go install github.com/securego/gosec/v2/cmd/gosec@latest` | `gosec -fmt=json ./...` | AST/SSA rules for weak hashes (`G401`, `G501`), weak RNG (`G404`), TLS config (`G402`), hardcoded creds (`G101`), ignored errors (`G104`) |
| **`staticcheck`** | `go install honnef.co/go/tools/cmd/staticcheck@latest` | `staticcheck ./...` | Deprecated crypto APIs, subtle slice/error bugs, ineffective assignments |
| **`golangci-lint`** | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` | `golangci-lint run --enable=gosec,errcheck,errorlint,bidichk ./...` | Combined lint gate catching unchecked `rand.Read`, BiDi Trojan Source, error wrapping bugs |
| **`semgrep`** | `pip install semgrep` or `brew install semgrep` | `semgrep --config p/golang --config p/security-audit --json .` | Cross-language AST rules across Go and TypeScript (`contracts/src/*.runar.ts`) |
| **`gitleaks`** | `winget install Gitleaks.Gitleaks` (already installed in this env) | `gitleaks detect --config .gitleaks.toml --no-git` | Leaked WIFs, private keys, API/operator tokens, `.env` secrets |
| **Go Native Fuzzing** | *(built into Go 1.18+)* | `go test -fuzz=Fuzz -fuzztime=30s ./internal/...` | Crashes, round-trip canonicalization divergences, parser/decoder panics, Merkle & envelope edge cases |
| **Google Wycheproof** | `github.com/C2SP/wycheproof` / JSON vectors | Custom Go test table importing Wycheproof JSON | Known cryptographic attacks: invalid curve points, high-S/malformed DER ECDSA, AES-GCM nonce/tag edge cases, ECDH cofactor/infinity edge cases |

---

## 2. Installable External Community Skills

Before installing external skills, note that **Antigravity Marketplace**
currently has no dedicated cryptography/ZKP audit plugin. The strongest
open-source companion skills come from **Trail of Bits (`github.com/trailofbits/skills`, CC-BY-SA-4.0)**:

| External Skill (`trailofbits/skills`) | Best used for | Overlap with `crypto-audit` |
| --- | --- | --- |
| `constant-time-analysis` | Deep assembly/compiler-level timing side-channel review across Go & TypeScript (secret-dependent branches, `DIV`/`MOD`, cache lookups) | Complements our `ct/*` rules when auditing custom curve/field math |
| `sharp-edges` | Auditing cryptographic API ergonomics & footguns across public Go packages | Complements our `crypto/*` and `key/*` rules |
| `insecure-defaults` | Parallel fail-open configuration & fallback secret hunt | Complements `security-audit` & `key/*` |
| `differential-review` | Risk-first git diff / PR security review with blast-radius analysis | Useful for reviewing cryptographic PRs after fixes are written |

**Important gap**: `trailofbits/skills` has **no standalone `zero-knowledge` or
`cryptography` skill** and no knowledge of Rúnar Bitcoin Script covenants,
BabyBear/Poseidon2, BRC-369, or Nektar's wire formats. Use `crypto-audit` as
the primary cryptographic & ZKP auditor, and optionally install Trail of Bits'
`constant-time-analysis` / `sharp-edges` into `.agents/skills/` or
`.devin/skills/` when deep compiler-level constant-time inspection is needed.

---

## 3. Pure-Go (`CGO_ENABLED=0`) Remediation Library Matrix

Always prefer **Go standard library** first (zero supply-chain risk, audited
by Trail of Bits & FIPS 140-3 validated), then already-imported dependencies
(`golang.org/x/crypto`), then well-audited pure-Go libraries.

| Audit Finding Class | Preferred Pure-Go Solution | Package / Import | Wire-Compat Impact | Notes |
| --- | --- | --- | --- | --- |
| **Ad-hoc SHA-256 KDF** (`crypto/adhoc-kdf`) | RFC 5869 HKDF-SHA256 | `crypto/hkdf` *(Go 1.24+ stdlib)* | Version bump (`v: 2`) | Zero new deps. `hkdf.Key(sha256.New, sharedX, salt, info, 32)` with length-prefixed `info`. |
| **Variable-time secret/token comparison** (`ct/variable-time-secret-cmp`) | Constant-time compare / select | `crypto/subtle` *(stdlib)* | Drop-in | Use `subtle.ConstantTimeCompare(a, b) == 1`; hash bearer tokens with SHA-256 before map lookup. |
| **CSPRNG / Entropy** (`key/unchecked-rng`) | Checked stdlib CSPRNG | `crypto/rand` *(stdlib)* | Drop-in | Always check returned error (and never use `math/rand` for keys, nonces, salts, or tokens). |
| **Custom ECIES envelope** (`bbox_crypto.go`) | Standardized RFC 9180 HPKE or Tink AEAD | `github.com/cloudflare/circl/hpke` or `github.com/tink-crypto/tink-go/v2` | Version bump (`v: 2`) | Note: `circl/hpke` supports X25519/P-256/P-384/X25519MLKEM768; if secp256k1 identity keys are mandated by BSV BRC-42/43, keep secp256k1 ECDH + upgrade KDF to `crypto/hkdf` + length-prefixed AAD. |
| **AEAD without nonce-misuse fragility** | XChaCha20-Poly1305 (192-bit nonce) or AES-256-GCM-SIV | `golang.org/x/crypto/chacha20poly1305` *(already in `go.mod`!)* | Version bump | `chacha20poly1305.NewX(key)` eliminates 96-bit random-nonce birthday risk at high message volumes with zero new module additions. |
| **ZK Circuits, Groth16/PLONK, Soundness & Fair Exchange** (`zk/*`, `crypto/fair-exchange-mismatch`) | Go-native zk-SNARK & algebraic-hash framework | `github.com/consensys/gnark` + `github.com/consensys/gnark-crypto` | New proof rail / covenant upgrade | Pure Go. Provides Groth16/PLONK over BN254/BLS12-381, `gnark/test` soundness assertions, and reference Poseidon2/BabyBear/BN254 verification. Use `gnark >= v0.11` (post-BSB22 fixes). |
| **Post-Quantum / Advanced Primitives** (OPRF, threshold, hybrid KEM) | Cloudflare CIRCL | `github.com/cloudflare/circl` | New protocol version | Pure Go (with optional asm). Audited implementations of ML-KEM, X25519MLKEM768, blind signatures, OPRF. |
| **Secret Zeroization / Protected Memory** (`key/unzeroed-secret`, `key/plaintext-at-rest`) | Built-in `clear()` or `memguard` | `clear(b)` + `runtime.KeepAlive(b)` *(Go 1.21+)* or `github.com/awnumar/memguard` | Drop-in | Use `clear()` for ephemeral keys/shared secrets; use `memguard.LockedBuffer` (mlock + guard pages) for long-lived root WIFs and `BorgGroup.Epochs` keys. |
| **Cross-Language Canonical JSON** (`hash/non-canonical-json`) | Strict RFC 8785 JCS | `github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer` (or `bcommon/cbor`) | Version bump (if signatures change) | Prefer `bcommon/cbor` for new wire records; use RFC 8785 JCS where JSON interoperability with JS/Rust is required. |
| **FIPS 140-3 Strict Mode** (regulated deployments) | Native pure-Go FIPS module | `GOFIPS140=latest` + `crypto/fips140` *(Go 1.24+ stdlib)* | Build/runtime flag | Enforces minimum salt/key lengths and approved algorithms without CGO. |
