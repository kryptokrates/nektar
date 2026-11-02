# GOAL-v8 — Strict Re-Scan Closure: The 10 Remaining Divergences

Scope: a section-by-section re-scan of arXiv:2506.13246 against the
*code* (not the docs or test names) found ten real divergences on top
of the already-aligned core. Each was fixed — no contract changes were
needed, so no artifact churn and no new consensus surface.

Status: **DONE — all ten divergences closed in five phases, every fix
covered by a named negative test. mcp 18/18, reasoning 20/20,
anchor suite green.**

## Verified-aligned (unchanged — evidence, not claims)

- §3/§4 automaton + transition-ledger anchoring — `MamemAnchor` covenant
- §5 `K_i=(d,p,H(d‖p),E)` fragment model — `packages/core` Fragment
- §6 ECDH+HKDF — BRC-42 `wrapFor`/`unwrapFor`
- §7 `K_sym,i^l = HKDF(K_U,A ∥ t‖ψ‖ρ‖π‖l)` — `canonicalKeyID` field-exact
- §8 invertible `Diff`/`Apply` deltas — `revise()` + `materialize`
- §9 ontology + jurisdictional named graphs (TC-83, TC-93)
- §11 AORG vertices/edges/consistency (TC-42)
- §12 consensus time + weighted-median multi-chain — `TemporalSources`
- §13 `(π_j, ZKAccess(ℓ_q≥ℓ_j))` — STARK membership + level proofs
- §14 anchored policy derivation · §15 modality · §17 instancing + σ
- Recursive `R′` chain, META anchors, SPV/BEEF/BUMP, V1/V2/V3 covenants

## Phase A — receipt integrity (D1, D3, D9, D10)

- **D1 — §16 delegation map inside the signed digest.** `AnswerReceipt`
  gained `delegations`; `receiptDigest` serializes each entry, and
  `recall` binds `opts.delegations` into the terminal signature. A
  post-hoc add/strip/edit invalidates the receipt. TC-106.
- **D3 — receipts carry the Π artifact, not a flag.** `AnswerReceipt.zk`
  holds the wire-form `ClearancePresentation`; the digest binds its
  canonical JSON; `verifyReceiptZk` lets a third party re-verify the
  range proof + transcript scope — `zkaccess:'verified'` is now backed
  by evidence, not just asserted. TC-107.
- **D9 — signed sale offers expire.** `SaleOffer.expiresAt` is inside
  `offerDigest`; `verifyOfferSig` and `saleEscrow` refuse expired
  offers. TC-108.
- **D10 — escrow refund window sanity.** `saleEscrow` refuses
  `refundAfter ≤ currentHeight + margin` before funding. TC-108.

## Phase B — level + revocation propagation (D2, D8)

- **D2 — App E traversal constraint.** `remember(premiseRefs)` floors
  the stored level at `max(cited premise levels)` — derived content
  cannot leak into a lower-clearance read than its most-sensitive
  input. `revise()` inherits the base's level; the `remember` MCP tool
  exposes `premiseRefs`; `RememberResult.level` reports the effective
  level. TC-109.
- **D8 — anchored third-party revocation evidence.** `revocationProof(pub)`
  returns the `__revocation__` record's `{data, prov, E, leaf}` + SPV
  bundle (decrypting under agentSeed). `AuditBundle.revocations` carries
  it; `verifyAuditBundle` recomputes `computeLeaf(data,prov,E)` against
  the proven leaf before convicting — an inclusion proof for a
  *different* record is not evidence, and chain data alone convicts a
  revoked signer (no local `isRevoked` needed). TC-110.

## Phase C — anchor artifacts (D4, D5)

- **D4 — §19 `M_U` rotation ledger anchored.** `rotateShard` writes each
  `RotationCert` to `__identity__` and anchors it. `rotationChainProof`
  returns the genesis owner + every cert with inclusion evidence;
  `verifyAnchoredRotationChain` (reasoning) binds cert→leaf→anchor and
  replays `verifyCertChain`. A withheld cert can only make the chain
  look *older* — never forged. Latent bug fixed: certs are now indexed
  `0,1,2…` at emission (previously all `index:0`). TC-111.
- **D5 — Appendix D σ_n as a discrete artifact.**
  `extractAnchorAuthorship(spendTx, spentAnchorScript)` (contracts)
  decodes the spent anchor output, locates the owner pubKey push in the
  unlocking script, and verifies the preceding DER sig over
  `sha256(txPreimage)`. The preimage commits to the entire spend —
  including the continuation output carrying `{n+1, R, R′}` — so
  `Sign(preimage) ≡ Sign(R_n‖t_n)` semantically. σ_n is no longer just
  "somewhere in the input": it is an extractable, verifiable object.
  TC-112.

## Phase D — audit + model binding (D6, D7)

- **D6 — opt-in access audit trail.** `MAMEM_ACCESS_LOG=1` appends
  timestamped `{type:'access', event:'decrypt'|'traverse', shard, seq,
  caller}` records to `__access__` via `log.remember` (no mid-read
  anchor; best-effort). Off by default — a readable who-read-what log
  leaks the access pattern itself. `logAccess()` is public for callers
  that traverse outside memory (graph/reasoning). TC-113.
- **D7 — §16/§18 ℱ binding.** `AnswerReceipt.model:{id, params,
  weightsHash}` — operator-declared identity + invocation params +
  local-weights hash, merged with per-call `modelParams`, bound into
  the digest. An LLM is never *provably* deterministic; this is the
  strongest reproducible-ℱ binding the stack offers. TC-114.

## What changed shape (migration notes)

- `receiptDigest` gained `delegations`, `zk`, `model` fields — all
  empty-safe: receipts without them still verify.
- `SaleOffer` gained `expiresAt` — old unsigned/unsigned-expiry offers
  fail `verifyOfferSig` (digest mismatch), which is the intended
  behavior for a signature scheme change.
- `remember()` signature: `(shard, d, prov?, level?, opts?)` — new
  trailing `opts.premiseRefs`; existing calls unaffected.
- `AuditBundle.revocations` entries need `{data, prov, E, leaf}` —
  produced by `revocationProof`, not hand-built.
- Reasoning now runtime-depends on `@mamem/identity` (was dev-only).

## Honest deferrals that REMAIN (documented, not claimed)

- **Completeness/TEE** — inclusion proves *this* record was anchored,
  not that no shadow memory exists.
- **Truth ≠ inclusion** — a verified record proves it was written and
  anchored, not that the statement is true (`truthNote` stays).
- **Full OWL 2 DL** — RL/QL scope only.
- **On-chain ZK verification** — Groth16-in-Script / STARK verify in
  Script is out of covenant scope; proofs verify off-chain against
  anchored roots.
- **PoW header trust** — SPV verifies BUMP→header inclusion, not work;
  the header source is trusted.
- **V2 punitive slashing** — `checkDataSig` unavailable in runar.
- **Sale front-running/resale/wrapped-visibility** — documented
  boundaries of the escrow rail.
- **`ctx.t` = tenant, not literal paper timestamp** — changing
  `canonicalKeyID` would orphan every existing ciphertext; divergence
  by design.
- **`π_i` lazy** — an inclusion path cannot exist before the anchor.
- **Block-header `t_n`** — `issuedAt`/`time` are wall-clock; header
  time is a SPV-source detail.
- **LLM output not consensus-enforced** — receipts bind ℱ identity
  (D7), but what the model said is attested, not executed on-chain.
- **`__access__` completeness** — the log records decrypt/traverse
  events *when enabled*; a disabled log records nothing (by design),
  and an attacker with write access to the process could suppress
  writes — the trail is for the honest operator's auditability, not a
  tamper-proof monitor.
