# GOAL-v7 — On-Chain Enforcement (Rúnar Covenants) + Verification Hardening

Scope: (A) close the read-path verification gaps so an agent's memory is
checked as described — revocation, epoch-window authorship, provenance
re-verify, standalone auditor; (B) move three guarantees from application
code (bypassable by whoever holds the key) into consensus rules enforced by
the locking script — the differentiator no off-chain stack can replicate;
(C) document the honest deferrals that application-level verification cannot
close.

Status: **DONE — Phase A (read-path hardening) committed; all three
covenants implemented in Rúnar, green under real script eval AND live
Teranode regtest.** V1 `MamemFeeVault` (stateful, opSig-prefix unlocking,
host-computed window transition asserted on-chain) — funded, spent,
continuation spent, over-cap refused (TX_INVALID) on regtest. V2
`MamemCheckpointBond` (stateful {n,lastRoot}, uniform-frame pad args +
auto-change slot) — commit×2, early close refused, release-height close
accepted on regtest. V3 `DisclosureSale` (stateless terminal, explicit
txPreimage, preimage-last ABI order, fully-inlined out() asserts —
runar 0.4.6 ANF dead-codes the second out() in nested `cat(a,b)` and leaks
locals otherwise) — buy reveals wrapped + pays seller, wrong wrapped refused,
early refund refused, deadline refund accepted on regtest. E2E:
`packages/e2e/src/e2e-covenants-regtest.ts` (12 checks, PASS).

## Phase A — Read-path verification hardening (done)

Four gaps that kept the paper's verification from being fully enforced on the
read path — all closed this session:

- **Revocation consulted** — `recall` now withholds a record whose
  `authorPub` is revoked (`'revoked key'`), via `authorGuard`.
- **Epoch-window authorship enforced** — `verifyAuthorship` runs when the
  record's author is on the shard's own owner cert chain (genesis `ownerPub`
  + `rotationCerts`). A rotated-out or forged-epoch own write is withheld.
  Foreign authors carry no chain — their originator σ still binds them; the
  epoch claim is simply not checkable, so no false rejection.
- **Provenance re-verified** — post-decrypt, `verifyProvenance(authorPub,
  pt, p)` re-checks the originator σ over the plaintext (it was bound at
  ingest; now also checked on the way out).
- **Audit-side revocation** — `verifyAuditBundle` takes `opts.isRevoked`
  and fails revoked signer-promise / delegator keys.
- **Standalone auditor** — `tools/verify-bundle.mjs` verifies a ProofBundle
  or AuditBundle against a header chain (`--headers` file or `--rpc`)
  without the agent runtime — the operationalisation hook for third-party
  auditors.

Files: `packages/mcp/src/memory.ts`, `packages/reasoning/src/index.ts`,
`packages/mcp/test/memory.test.ts`, `tools/verify-bundle.mjs`. Tests: 13/13
mcp green incl. the revoked-author withhold case.

## Feasibility baseline (measured, not assumed)

- runar-lang 0.4.6 TS tier provides: `txPreimage`, `extractOutputHash`,
  `extractLocktime`, `extractAmount`, `addOutput` (self-continuation only),
  `buildP2PKH`, `checkSig`/`checkMultiSig`, `sha256`/`hash160`/`num2bin`.
- Compiler mechanics (verified in `runar-compiler` pass 04): every state-
  mutating public method is given implicit params `_changePKH`,
  `_changeAmount` (and `_newAmount` for single-output continuation) plus a
  trailing P2PKH change output; `txPreimage` is injected into every public
  method. Non-mutating methods do NOT constrain outputs.
- **Missing:** `checkDataSig` (arbitrary-message ECDSA) — not in runar-lang
  0.4.6. Consequence: V2 ships *prevention* (an unforkable on-chain
  checkpoint chain) rather than *punitive slashing* of off-chain signed
  evidence. Upstream request filed/documented; script-level secp256k1 is
  possible but ~hundreds of KB and stays deferred.

## V0 — Compiler spike (findings, all verified in shipped code)

1. `_changePKH`/`_changeAmount`/`_newAmount` are **not body-visible** —
   the auto-change output is unbound ⇒ V1 bounds `leftover+change` instead
   of constraining the destination (see V1).
2. Stateless `SmartContract` **does** accept a declared `SigHashPreimage`
   param + `checkPreimage` — V3 uses it (preimage-last ABI order; the
   `OP_5 OP_ROLL` preamble pulls the opSig from a fixed frame depth).
3. `extractLocktime` returns `nLocktime`; spendable on regtest with
   input `sequence < 0xffffffff` (0xfffffffe used throughout).
4. Sizes: V1 ~0.5 KB, V2 ~0.6 KB, V3 ~0.35 KB — far under limits.
5. ANF caveats hit during bring-up: (a) conditional state mutation
   miscompiles → host passes transition witnesses (V1); (b) nested
   `cat(out0, out1)` dead-codes the second build and named locals leak
   onto the stack → terminal outs are fully inlined into the assert (V3);
   (c) multi-method terminal methods get the uniform injected frame —
   pad args + auto-change output required even when the ABI omits them (V2).

## V1 — MamemFeeVault (spend caps as consensus)

`packages/contracts/src/MamemFeeVault.runar.ts` — **idiomatic
`StatefulSmartContract`** (as shipped; the earlier stateless+`codeSha`-pin
sketch was abandoned — the stateful path verifies under real script eval).

Props: `destPkh` (the only payable address), `ownerPkh`, `perTxCap`,
`windowCap`, `windowBlocks`, `maxFee` + mutable `{spentInWindow, windowStart}`
(state after OP_RETURN, serialized via SDK `serializeState`).

`spend(sig, pubKey, payAmount, contSats, newSpent, newStart, _changePKH,
_changeAmount, txPreimage)`:
- `checkPreimage` + `hash160(pubKey)==ownerPkh` + `checkSig` (operator only).
- The window roll is **computed on-chain but expressed host-side**: runar
  0.4.6's ANF pass miscompiles conditional state mutation, so the host
  passes `newSpent`/`newStart`; the contract asserts the transition
  unconditionally (`lt >= windowStart+windowBlocks ⇒ newSpent==payAmount ∧
  newStart==lt`, else `newSpent==spentInWindow+payAmount ∧ newStart==windowStart`)
  — the transition rule itself is still consensus-checked.
- `newSpent <= windowCap`, `payAmount <= perTxCap`.
- `addOutput` continuation (new state) + `addRawOutput` payment to `destPkh`
  + SDK auto-change; `extractAmount(txPreimage) - contSats - payAmount <=
  maxFee + 1` bounds leftover+change — the unreachable-change leak is
  closed by bounding, since `_changePKH` isn't body-visible (V0 finding).

Host: `packages/anchor/src/vault.ts` — `FeeVault`/`VaultFunder` (SDK
`computeOpPushTxWithCodeSep` + `buildStatefulPrefix(opSig, true)` — the
`_changePKH` method frame needs the code-part witness push).
`SpendGuard` stays in front (defense in depth). Env `MAMEM_FEE_VAULT`.
Regtest-validated in `e2e-vault-regtest.ts` (fund → spend → continuation
spend accepted; over-cap refused TX_INVALID).

## V2 — MamemCheckpointBond (unforkable checkpoint chain)

`packages/contracts/src/MamemCheckpointBond.runar.ts` — StatefulSmartContract.

Immutable params: `operatorPkh`, `bondReleaseHeight`, `minBond`.
Mutable state: `{n, lastRoot}` — the checkpoint chain lives IN the script:
`commit(sig, pubKey, newRoot, _newAmount, _changePKH, _changeAmount,
txPreimage)` continues `{n+1, newRoot}` only — conflicting on-chain
checkpoints are impossible by construction (stronger than detection).
`close(sig, pubKey, payoutSats, _changePKH, _changeAmount, txPreimage)`
returns the bond to `operatorPkh` once `extractLocktime >=
bondReleaseHeight`, fee-bounded. Both carry the uniform injected frame +
1-sat auto-change output (V0 finding c).

Documented limit: punitive slashing needs `checkDataSig` — deferred (see
baseline). Closes VER-007's on-chain side.
Host: `packages/anchor/src/bond.ts`; regtest-validated (commit×2,
early-close refused TX_INVALID, release close accepted).

## V3 — DisclosureSale (atomic paid disclosure)

`packages/contracts/src/DisclosureSale.runar.ts` — SmartContract.

Immutable params: `sellerPkh`, `price`, `hWrapped` (sha256),
`refundAfter`, `maxFee`.

`buy(wrapped, buyerPkh, changeSats, txPreimage)`: `sha256(wrapped) ==
hWrapped` — the spend itself reveals the payload on-chain, bound to the
committed hash. Outputs: `price` → `sellerPkh`, `changeSats` → `buyerPkh`,
enforced via `hash256(outs) === extractOutputHash`.
`refund(sig, pubKey, changeSats, txPreimage)`: `hash160(pubKey) ==
sellerPkh` + `checkSig` + `extractLocktime >= refundAfter` +
`extractAmount - changeSats <= maxFee`.

`wrapped` is the existing `DisclosureEnvelope.wrapped` (BRC-42 ciphertext to
the buyer) — on-chain exposure is safe; or plaintext `{field,salt,path}` for
paid *public* disclosure.

Host: `packages/anchor/src/sale.ts` (`DisclosureSaleHost`) —
`buildBuy`/`buildRefund` via SDK `computeOpPushTxWithCodeSep` +
`buildStatefulPrefix` + `buildUnlockingScript` (no hand-assembled script).
Regtest-validated in `e2e-covenants-regtest.ts`. MCP tools behind
`MAMEM_PAID_DISCLOSURE` remain a follow-up (adapter is ready).

## Gates per phase

compile artifact < standard script limits · contract unit tests incl.
negatives (wrong dest, over-cap, non-owner, early close, underpayment,
wrong reveal, forged codePrefix) · regtest e2e deploy+spend · no
mainnet/testnet broadcast without explicit approval · docs (PROOFS /
THREAT-MODEL / OPERATIONS / test-spec rows).

## Phase C — honest deferrals (documented, not claimed)

What application-level verification cannot close:

- **Header trust** — SPV verifies BUMP→header inclusion but not PoW/work;
  the header source (RPC / headers file) is trusted. Recorded in
  THREAT-MODEL.
- **No anchored revocation root** — revocation is operator-local
  (in-memory / shard); a third-party verifier cannot chain-verify it.
  Follow-up candidate: revocation root inside a covenant state (V4-style).
- **Completeness gap** — inclusion proves *this* record was anchored, not
  that no shadow memory exists; closing that needs TEE / model-execution
  attestation (S6.11 deferred).
- **Truth vs. inclusion** — a verified record proves it was written and
  anchored, not that the statement is true; a lie, once written, is
  preserved and marked.

Verification-distinction matrix to be kept current in `docs/PROOFS.md`:
data-integrity · provenance · inclusion/finality · authorization ·
confidentiality · derivation · model-id — vs. *factual truth*.
