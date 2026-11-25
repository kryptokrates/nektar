# Cryptographic Audit Rules (`zk/*`, `crypto/*`, `hash/*`, `sig/*`, `key/*`, `ct/*`, `script/*`)

Normative finding IDs for `crypto-audit`. Every finding in a report MUST cite
one rule ID below (or justify a new domain-scoped ID if an issue is genuinely
novel). Never emit a finding without concrete `file:line` evidence and a
realistic break.

---

## 1. Zero-Knowledge Proofs (`zk/*`)

| Rule ID | Invariant | Failure mode | Typical Sev | Fix seam |
| --- | --- | --- | --- | --- |
| `zk/under-constrained-witness` | Every witness wire/signal is bound by polynomial/arithmetic constraints, not assignment alone | Malicious prover supplies forged witness satisfying the circuit for a false statement | BLOCKER | Bind all hints/assignments with explicit constraints (`===`, `api.AssertIsEqual`, range checks); add mutation test |
| `zk/fiat-shamir-incomplete` | Fiat-Shamir challenge hashes **all** public parameters, commitments, statement fields, and context/domain tags | Prover replays, malleates, or front-runs a valid proof across steps, outputs, or contract instances (weak FS) | BLOCKER | Hash fixed-length/length-prefixed domain tag + public params + $R$ + $P$ + full bound statement + contract/UTXO context |
| `zk/unpinned-vk-or-stage` | Verifier/stage script hash or verifying key is consensus-pinned on-chain | Prover co-spends a dummy/permissive UTXO whose script trivially returns `OP_TRUE` | BLOCKER | Pin exact script template hash / VK hash / outpoint derived from committed genesis state under consensus |
| `zk/non-canonical-field-element` | Every field/scalar input satisfies $0 \le x < p$ (or $1 \le s < n$ for EC scalars) | Aliasing ($x + kp \equiv x \pmod p$), negative residue under signed `OP_MOD`, or zero-scalar trivial proof | BLOCKER / HIGH | Assert canonical bounds before arithmetic; reject $0$ / $\ge n$ / $\ge p$ |
| `zk/statement-binding-gap` | Public statement verified inside proof equals the statement committed in settlement output under consensus | Off-chain check omitted by downstream consumer; valid proof settles a different claim | BLOCKER / HIGH | Bind statement root inside locking script / `hashOutputs` or enforce mandatory verifier extraction |
| `zk/malleable-proof-or-replay` | Proof or settlement cannot be replayed across nonces, queries, chains, or contracts | Replay attack settles duplicate payouts or advances state twice | HIGH | Include unique domain + chain/contract ID + monotonic counter/nonce in public inputs |
| `zk/trusted-setup-unverified` | Groth16 / KZG ceremony parameters, `.zkey`, and on-chain VK match auditable ceremony transcript | Toxic waste or dev ceremony shipped to production allows arbitrary proof forgery | BLOCKER | Enforce release-time VK hash check against published ceremony transcript; refuse dev setup on mainnet |

---

## 2. Encryption, KDF & Key Wrapping (`crypto/*`)

| Rule ID | Invariant | Failure mode | Typical Sev | Fix seam |
| --- | --- | --- | --- | --- |
| `crypto/adhoc-kdf` | Key derivation from ECDH shared secret or master secret uses an extract-then-expand KDF (HKDF-SHA256, RFC 5869) with explicit salt & structured `info` | Raw `SHA256(tag \|\| secret \|\| varlen...)` lacks formal PRF extraction, risks length-extension/framing ambiguity | HIGH / MED | Migrate to Go stdlib `crypto/hkdf.Key(sha256.New, sharedX, salt, info, 32)` with versioned wire tag |
| `crypto/nonce-reuse-or-short-salt` | AEAD nonces are unique per key (≥96-bit random or monotonic counter under safe volume bound); salts resist collision under expected volume | AES-GCM nonce reuse recovers GHASH key and XOR of plaintexts; 32-bit salt collides around $2^{16}$ items under same key | BLOCKER / HIGH | Never reuse `(key, nonce)`; ensure fresh per-object key or ≥96-bit random/counter nonce; document/bound segment count for short salts |
| `crypto/unauthenticated-ciphertext` | Every decrypted payload is authenticated (AEAD tag or Encrypt-then-MAC) before parsing or acting on plaintext | Chosen-ciphertext / padding-oracle / silent plaintext corruption | BLOCKER / HIGH | Use AES-256-GCM, ChaCha20-Poly1305, or audited `bcommon/keyed` AEAD; reject unauthenticated envelopes |
| `crypto/fair-exchange-mismatch` | Atomic payment-for-key escrow proves the revealed key opens the committed ciphertext (not merely a hash preimage) | Malicious seller commits `hWrapped = SHA256(garbage)` ≠ `keyCommitment`, claims buyer funds on-chain while buyer gets undecryptable blob | BLOCKER | Require verifiable encryption / adaptor signature / ZK equivalence proof binding `hWrapped` to `keyCommitment` (or explicit dispute/slash bond) |
| `crypto/aad-ambiguity` | AAD binds all envelope metadata using unambiguous length-prefixed or canonical encoding | Delimiter-joined AAD (`a\|b\|c`) collides if any field can contain `\|`, allowing metadata splicing across deals/offices | HIGH / MED | Length-prefix every AAD field (`u32le(len) \|\| field`) or encode via canonical CBOR |
| `crypto/epoch-secrecy-gap` | Revoked members cannot read future epochs (forward secrecy); newly added members' backward access policy is explicit and enforced; epoch keys are encrypted at rest | Revoked member accesses new epoch, new member reads prior secrets unintentionally, or DB dump leaks all historical epoch keys | HIGH / MED | Rotate epoch on revoke (and optionally on add if backward secrecy is required); wrap persisted epoch keys under root/KMS KEK |

---

## 3. Hashing, Framing, Merkle & Signatures (`hash/*`, `sig/*`)

| Rule ID | Invariant | Failure mode | Typical Sev | Fix seam |
| --- | --- | --- | --- | --- |
| `hash/unframed-concat` | Concatenating ≥2 variable-length fields inside a hash or signed digest length-prefixes every variable field | `H(tag \|\| "a" \|\| "bc") == H(tag \|\| "ab" \|\| "c")` enables signature/commitment forgery | HIGH | Frame each variable field with `core.Field(b)` (`u32le(len) \|\| b`) or canonical CBOR |
| `hash/missing-domain-tag` | Every distinct protocol object/digest uses a unique, non-empty domain-separation tag | Cross-protocol / cross-type digest collision (e.g., signing an `OfferAd` replayed as `WantAd` or delegation) | HIGH / MED | Prefix every hash/signature preimage with a unique versioned domain tag |
| `hash/delimiter-collision` | Structured hash inputs never rely on unescaped string delimiters (`\|`, `,`, `:`) over user-controlled strings | Crafted string containing delimiter impersonates another vertex/request/edge | HIGH / MED | Replace `fmt.Sprintf("%s\|%s")` with length-prefixed framing or canonical JSON/CBOR |
| `hash/merkle-second-preimage` | Merkle leaf vs internal node hashes are domain-separated or tree height + exact `size` are strictly enforced against odd-leaf duplication (CVE-2012-2459) | Attacker forges inclusion of an internal 64-byte node as a leaf, or crafts a duplicate-tail tree with the same root | HIGH / MED | Enforce `len(leaf) == 32` + `len(path) == HeightForLeafCount(size)` + `index < size` (and reject duplicate-sibling last-leaf malleation) |
| `hash/non-canonical-json` | Cross-language signed JSON uses strict RFC 8785 (JCS) without HTML escaping (`<`, `>`, `&`) or float ambiguity | Go `json.Marshal` escapes `<>&` to `\u003c` while JS/Rust does not, breaking cross-language signature verification or allowing duplicate canonical forms | MED | Use `SetEscapeHTML(false)` + strict RFC 8785 JCS (or canonical CBOR) for signed payloads |
| `sig/malleable-der-or-high-s` | ECDSA signatures enforce strict DER + low-S canonicality ($s \le n/2$) where signature bytes are hashed or indexed | Third party flips $s \to n - s$ producing a second valid signature with a different hash/txid | MED | Enforce low-S on verify or hash only canonical `(r, low-s)` / non-signature fields |
| `sig/cross-type-replay` | Signatures bind artifact type, network (`mainnet`/`testnet`/`regtest`), and expiry/nonce | Testnet signature replayed on mainnet, or one message type accepted as another | HIGH | Include explicit type tag + network + expiry/nonce inside signed preimage |
| `sig/unsigned-commitment` | Security-critical receipts or decision artifacts claiming third-party verifiability are cryptographically signed or anchored | Caller forges arbitrary `DecisionReceipt` or unanchored `Receipt` | HIGH / MED | Sign receipts with node identity key (domain-tagged) or require on-chain anchor proof |

---

## 4. Keys, Entropy & Constant-Time Hygiene (`key/*`, `ct/*`)

| Rule ID | Invariant | Failure mode | Typical Sev | Fix seam |
| --- | --- | --- | --- | --- |
| `key/unchecked-rng` | Every `crypto/rand.Read` (or key-gen) call checks `err != nil` and never falls back to `math/rand` | Silent all-zero key/nonce/CSRF/OAuth token on CSPRNG failure; predictable tokens with `math/rand` | BLOCKER / HIGH | Always check `if _, err := rand.Read(buf); err != nil { return err }` (or Go 1.24+ `rand.Read` contract audit); forbid `math/rand` in crypto/auth |
| `key/plaintext-at-rest` | Long-lived root keys, WIFs, and epoch keys have strict OS permissions (including Windows ACL awareness) and optional encryption at rest | Local user/backup/log leak compromises identity and all derived keys | HIGH / MED | Enforce atomic `0600` (POSIX) + ACL hardening (Windows); offer passphrase/OS-keychain KEK wrapping |
| `key/unzeroed-secret` | Ephemeral private keys, ECDH shared secrets, raw AEAD keys, and BRC-369 scalars are wiped after use | Heap dump / core dump / swap leaks secret key material long after operation ends | MED / LOW | Call `clear(buf)` + `runtime.KeepAlive(buf)` via `defer`, or isolate long-lived keys in `memguard.LockedBuffer` |
| `key/missing-point-validation` | External elliptic-curve public keys are validated as compressed canonical points on-curve, non-infinity, and in prime-order subgroup | Invalid-curve attack or point-at-infinity yields predictable shared secret `0` | BLOCKER / HIGH | Parse strictly (`ec.ParsePubKey`), check compressed length (33B), verify `IsOnCurve` & non-zero scalar/shared point |
| `ct/variable-time-secret-cmp` | Comparisons involving tokens, MACs, auth secrets, nonces, or secret-derived tags use `crypto/subtle.ConstantTimeCompare` | Timing side-channel allows byte-by-byte forgery or token oracle (including Go map lookup timing on raw bearer tokens) | HIGH / MED | Use `subtle.ConstantTimeCompare` (or HMAC/SHA-256 hash tokens before map lookup) |
| `ct/secret-dependent-branch` | Custom scalar arithmetic, key parsing, or secret comparisons avoid secret-dependent branches, table lookups, or variable-time division/modulo | Cache/branch timing leaks secret scalar bits | HIGH / MED | Delegate EC/AEAD math to constant-time Go stdlib / audited libraries; apply `crypto/subtle` selection |

---

## 5. Rúnar / Bitcoin Script Covenants (`script/*`)

| Rule ID | Invariant | Failure mode | Typical Sev | Fix seam |
| --- | --- | --- | --- | --- |
| `script/signed-mod-neg` | Field arithmetic over prime $p$ using Bitcoin Script `OP_MOD` guarantees non-negative canonical inputs ($0 \le x < p$) | In Bitcoin Script, `(-a) % p` returns a **negative** number (not $p - a$), corrupting modular arithmetic or allowing alternative witness encodings | BLOCKER / HIGH | Assert `x >= 0n && x < p` on every caller-supplied field input and normalize subtractions `(a - b + p) % p` |
| `script/length-prefix-overflow` | Manual state/output serialization (`num2bin(len(x), 1n)`, custom varints) checks length bounds before encoding | `len(x) >= 128` sets the sign bit in signed Bitcoin Script `num2bin(..., 1n)` or overflows 1-byte pushdata (`> 75` / `> 255`), producing unspendable/corrupted output scripts | HIGH | Enforce `assert(len(x) <= 75n)` (or proper `OP_PUSHDATA1`/`varint` encoding for `>= 76` / `>= 253` / `>= 65536`) |
| `script/unpinned-cospend` | Multi-input covenant co-spends pin BOTH the counterparty outpoint/script AND shared output enumeration | Attacker substitutes a fake input1 or drains sats via unexpected extra inputs/outputs | BLOCKER | Pin `hashPrevouts` to exact expected outpoint(s) + verify counterparty script/code hash + enforce `hashOutputs` |
| `script/unbounded-fee-or-underflow` | Satoshis accounting asserts non-negative outputs (`>= 1n` / dust), no negative `changeSats`, and bounded `maxFee` | Negative `changeSats` fails or wraps `num2bin(..., 8n)`; unbounded fee lets miner/snipers burn deposit; subsidy math leaks bond sats | HIGH | Assert `payoutSats >= 1n`, `changeSats >= 0n`, and strict exact-input minus output `<= maxFee` |
