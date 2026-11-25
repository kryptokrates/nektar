# ZKP & Rúnar Covenant Audit Manual

Load this file whenever auditing zero-knowledge proofs, arithmetic circuits,
on-chain verifiers, Sigma/Schnorr protocols, algebraic hashes (Poseidon2 /
BabyBear), fair-exchange escrows, or `contracts/src/*.runar.ts`.

---

## 1. Groth16 & Pairing Verifiers (e.g., `NektarGrothClaim`)

Groth16 provides succinct constant-size proofs over pairing-friendly curves
(typically BN254 or BLS12-381), but soundness collapses if any boundary check is
dropped:

1. **Verifying-Key & Verifier-Script Pinning**
   - When settlement co-spends a Groth16 verifier UTXO (`hashPrevouts == hash256(ourOutpoint || verifierOutpoint)`), verify:
     - Who deploys the verifier UTXO, and how does the claim creator / consumer verify that `verifierOutpoint` actually locks the genuine compiled Groth16 verifier script (and NOT a trivial `OP_TRUE` script)?
     - Does the claim covenant enforce exact `len(verifierOutpoint) == 36` and exactly two inputs (no third input sneaking in extra funds or violating assumptions)?
2. **Statement / Public Input Binding (`doc-push == journalRoot`)**
   - If the verifier script uses a `doc-push` prefix (`<statement-root> <index> OP_2DROP`), check whether the claim covenant enforces `doc-push == journalRoot` on-chain or relies on off-chain consumer checks.
   - If off-chain only, treat any consumer path that trusts `ClaimVerdict` without cross-checking the verifier UTXO's `doc-push` against `journalRoot` as `zk/statement-binding-gap`.
   - Confirm every public input scalar satisfies $0 \le \text{pub}_i < r$ (scalar field order of BN254/BLS12-381); otherwise a prover can submit $\text{pub}_i + r$ (proof/public-input aliasing).
3. **Subgroup & Curve Membership**
   - In the verifier script (e.g., nChain-zkscript) and any Go verifier: check $\pi_A \in \mathbb{G}_1$, $\pi_C \in \mathbb{G}_1$, and $\pi_B \in \mathbb{G}_2$ (especially $\mathbb{G}_2$ prime-order subgroup check on BN254/BLS12-381).
4. **Trusted Setup & Gnark Commitment Extensions**
   - Verify `.zkey` / VK artifacts are NOT from a dev/single-party spike setup (`tools/groth16-spike`, `circuits/membership/proof*`) when targeting mainnet.
   - If using `ConsenSys/gnark` Groth16 with BSB22 Pedersen commitments, verify `gnark >= v0.11` (earlier versions had CVE-2024-45039 / CVE-2024-45040 soundness/ZK bugs) and ensure commitment public inputs match the on-chain verifier exactly.
5. **Proof Malleability & Settlement Replay**
   - Groth16 proofs can be rerandomized ($\pi'_A = \alpha \pi_A, \pi'_B = \alpha^{-1} \pi_B$). Never use `SHA256(proof)` as a nullifier or replay-protection key; bind replay protection to `(utxo_outpoint, queryId, nonce)`.

---

## 2. Optimistic ZK Bisection & STARK / BabyBear / Poseidon2 (`NektarZkClaim`, `Poseidon2Stage`, `Poseidon2Blocks`, `BabyBearProbe`)

### A. Bisection Game Soundness (`NektarZkClaim`)
1. **Leaf Stage Script Binding (`zk/unpinned-vk-or-stage`)**
   - At the bisection leaf (`bisectHi - bisectLo <= 1`), `proveStage` co-spends a stage UTXO.
   - **Critical check**: Does `proveStage` pin the stage UTXO's **script code hash** and **initial/final state commitments** (`midCommit` / bisected boundary hashes) on-chain? If `stageOutpoint` and `stageOutScript` are freely chosen by the prover without pinning the stage script template hash and matching the bisected `midCommit` at `bisectLo`/`bisectHi`, a malicious prover can co-spend a bogus UTXO and win any dispute!
2. **State Machine & Timeout Liveness**
   - Check every transition (`open -> challenge -> reveal -> bisect -> proveStage`, `claimTimeout`, `slashTimeout`):
     - Does `reveal` update `responseDeadline`? If `reveal` does NOT bump `responseDeadline`, a challenger can stall until `responseDeadline` passes or a prover's own deadline expires mid-turn!
     - Does `challenge` enforce the challenger actually posts a matching bond (`bondSats` increase), or can an attacker grief for 1 sat?
     - Can `bisect` produce an empty or non-shrinking interval if `stages == 0` or `stages == 1`?
     - Check `locktime` type confusion (block height `< 500_000_000` vs UNIX timestamp `>= 500_000_000`) and `nSequence != 0xffffffff` enforcement for `OP_CHECKLOCKTIMEVERIFY` / `extractLocktime`.
3. **Anchored Settlement Co-Spend (`settleAnchored` + `NektarAnchor.witnessSettle`)**
   - Verify both contracts enumerate the exact same output list and exact satoshi/script checks so neither party can divert `payoutSats` or mutate `anchor.R` / `anchor.ownerPkh`.

### B. BabyBear Field & Poseidon2 Arithmetic in Bitcoin Script
BabyBear prime: $p = 2^{31} - 2^{27} + 1 = 2013265921$.

1. **Signed `OP_MOD` Semantics (`script/signed-mod-neg`)**
   - Bitcoin Script integers are **signed** (sign-magnitude little-endian). `OP_MOD` returns a remainder with the **sign of the dividend** (e.g., `(-1) % 2013265921 == -1`, NOT `2013265920`).
   - Audit EVERY public method and stateful input (`x0..x23`, `s0..s23`, `e0..e7`) for explicit range assertions: `assert(x >= 0n && x < 2013265921n)`.
   - Without input range checks, a prover can pass negative numbers or values $\ge p$ (non-canonical representations) that still satisfy `s_i === e_i` or produce negative intermediate residues in `x^7 mod p`.
2. **Poseidon2 Parameter & Constant Integrity**
   - Cross-check all external/internal round constants (`0xfa20c37n`, diagonal matrix `0x409133f0n`, etc.), width $t = 24$, S-box degree $d = 7$ (`x^7` via `x^2 -> x^4 -> x^6 -> x^7`), and round counts ($R_F$, $R_P$) bit-for-bit against the exact target `risc0-zkp` version (`3.0.5`) using a differential Go/Rust test vector!
   - Check output coverage: notice if a method asserts only `s0..s7` (rate/digest) vs full state `s0..s23` — acceptable for final digest probe, **unsafe** for an intermediate bisection stage unless all 24 state cells are committed in the continuation UTXO.
3. **Stack & Script Limits**
   - Verify compiled script byte size and stack depth stay within BSV policy/consensus bounds, and verify that `readonly` vs mutable state serialization matches Rúnar's exact compiler version (`codeSeparatorIndex` shifts).

---

## 3. Schnorr-PoK / Sigma Protocols (`NektarAutomatonZk`)

When auditing on-chain or off-chain Schnorr Proofs of Knowledge ($s \cdot G = R + e \cdot P$):

1. **Strong Fiat-Shamir Transcript (`zk/fiat-shamir-incomplete`)**
   - Weak Fiat-Shamir (omitting public parameters or part of the bound message) allows existential forgery or front-running.
   - Check what is hashed into $e$:
     - Domain separator (unique to this protocol + contract version)
     - Commitment $R$ (canonical encoding)
     - Public key $P$ (`delegateP`)
     - Instance binding (contract genesis/shard/outpoint or `ownerPkh` so proofs cannot be replayed across two contracts sharing `delegateP`)
     - **Every** transition parameter the proof authorizes: step counter `n`, `from`, `sym`, `to`, AND `outHash`!
     - *Red flag*: if `e = sha256(R || P || n || to)` omits `sym` and `outHash`, a mempool observer can intercept `(R, s, e)` and front-run with a different `outHash` or different valid `sym` leading to the same `to`!
2. **Canonical Scalar & Point Validation (`key/missing-point-validation`, `zk/non-canonical-field-element`)**
   - Confirm `ecOnCurve(commitR)` and `ecOnCurve(delegateP)` ALSO reject the point at infinity (zero point) and non-canonical encodings.
   - Confirm $e$ and $s$ conversion between `ByteString` and Bitcoin Script `bigint` (`num2bin(e, 32n)`):
     - In Bitcoin Script, `num2bin(e, 32n)` uses **signed little-endian** with the MSB of byte 31 as the sign bit! If `sha256(...)` has bit 255 set (`>= 0x80` on byte 31), `num2bin(e, 32n)` represents a **negative** number unless masked/reduced or unsigned-decoded! Check how `ecMul(delegateP, e)` treats negative `e` or `e >= n` (secp256k1 order) and verify with cross-language test vectors (`vectors/curve-rust.json`).
   - Confirm $1 \le s < n$ and $e \not\equiv 0 \pmod n$.
3. **Prover Nonce Hygiene**
   - Off-chain prover generating $k$ for $R = k \cdot G$ MUST use RFC 6979 deterministic nonce or hedged CSPRNG (`k = H(priv || msg || rand)`). Reusing $k$ across two different challenges $(e_1, e_2)$ trivially leaks the secret key: $d = (s_1 - s_2)(e_1 - e_2)^{-1} \bmod n$.

---

## 4. Go `gnark` & R1CS / PLONK Circuits (When Authoring or Auditing Go Circuits)

If `gnark` (`github.com/consensys/gnark`) is introduced or audited:

1. **Compile-Time Go `if` vs Runtime Circuit `api.Select`**: Native Go `if` statements evaluate during `frontend.Compile`, never on runtime witnesses. Secret-dependent conditionals MUST use `api.Select` / `api.Lookup2`.
2. **Unconstrained Solver Hints (`solver.Hint`)**: Every output of a hint is an unconstrained prover witness until explicitly constrained with algebraic equations (e.g., quotient/remainder + range checks).
3. **Range Checks & Comparisons**: Finite-field `<`/`<=` requires bit decomposition (`api.ToBits` or `rangecheck.New(api)`). Always verify bit-length bounds prevent wrap-around modulo $r$.
4. **Division by Zero**: `api.Div(a, b)` requires guaranteeing $b \ne 0$ (or using `api.IsZero` when $b = 0$ is reachable).
5. **Public Input Ordering & Binding**: Verify `gnark:"public"` tags match the exact verifier input order and include replay protection (`chainId`, `contractId`, `nonce`).
6. **Soundness Testing**: Use `gnark/test.NewAssert(t)` with both `ProverSucceeded` (valid witnesses, boundary `0`, `max`) AND `ProverFailed` (mutated/forged witnesses), plus **mutation testing** (comment out one constraint -> tests MUST fail).

---

## 5. Fair Exchange & Verifiable Encryption (`DisclosureSale` + BRC-369)

In an atomic payment-for-disclosure escrow (`buy` reveals `wrapped` where `sha256(wrapped) === hWrapped`):

- **Hash-lock ≠ Verifiable Encryption (`crypto/fair-exchange-mismatch`)**:
  - Bitcoin Script `sha256(wrapped) === hWrapped` proves ONLY that `wrapped` is the SHA-256 preimage of `hWrapped`.
  - Unless the buyer can verify *before funding the escrow* that the preimage of `hWrapped` is the **same** key $k$ that opens `CiphertextHex` (and matches `KeyCommitment = keyed.Commitment(k)`), a malicious seller can commit `hWrapped = SHA256(junk)`, receive payment when revealing `junk`, and leave the buyer with undecryptable ciphertext.
  - Check whether the protocol enforces:
    1. A **ZK equivalence proof** (e.g., Groth16 / Sigma / `gnark` circuit proving $\exists k : \text{SHA256}(k) = h_{\text{wrapped}} \land \text{Commit}(k) = C_k$), OR
    2. **ECDSA/Schnorr adaptor signature** / BRC-369 point-lock where the witness scalar directly decrypts the ciphertext (replacing opaque `sha256` HTLC), OR
    3. An explicit **fraud-proof / slashable seller bond** on `ClaimVerdict` / `NektarCheckpointBond` that slashes the seller if revealed `k` fails `keyed.CheckOpened(k, commit)` or `OpenSegment`.
  - If none of (1)–(3) is enforced on-chain, document it explicitly as `crypto/fair-exchange-mismatch` and present all three remediation architectures.

---

## 6. Rúnar Bitcoin Script Structural Checks

Check every `contracts/src/*.runar.ts` file for:

1. **State Serialization Push-Length Bounds (`script/length-prefix-overflow`)**:
   - Pattern: `cat(num2bin(len(x), 1n), x)` fails silently when `len(x) > 75` (Bitcoin Script push opcodes) or `len(x) >= 128` (sign bit of 1-byte signed script number!).
   - Pattern: `sl < 253n ? num2bin(sl, 1n) : cat(num2bin(253n, 1n), num2bin(sl, 2n))` breaks if `sl >= 128 && sl < 253` (signed 1-byte `num2bin`!) or `sl >= 32768` (signed 2-byte `num2bin`!).
2. **Satoshi & Fee Accounting (`script/unbounded-fee-or-underflow`)**:
   - Check `changeSats >= 0n` (or `>= 1n`), `payoutSats >= 1n`, `verdictSats >= 1n`.
   - Check `extractAmount(txPreimage) - price - changeSats <= maxFee` — ensure `price` and `changeSats` cannot be negative to bypass the fee cap!
3. **Signature & Preimage Verification**:
   - Every stateless `SmartContract` using `txPreimage` MUST call `assert(checkPreimage(txPreimage))`.
   - Every P2PKH authorization MUST check BOTH `hash160(pubKey) === expectedPkh` AND `checkSig(sig, pubKey)`.
4. **Compiled Artifact Parity**:
   - Confirm `internal/contracts/artifacts/<Name>.json` matches `contracts/artifacts/<Name>.json` and the current `contracts/src/<Name>.runar.ts` source (no stale embedded bytecode).
