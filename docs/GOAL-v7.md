# GOAL-v7 — On-Chain Enforcement (Rúnar Covenants) + Verification Hardening

Scope: (A) close the read-path verification gaps so an agent's memory is
checked as described — revocation, epoch-window authorship, provenance
re-verify, standalone auditor; (B) move three guarantees from application
code (bypassable by whoever holds the key) into consensus rules enforced by
the locking script — the differentiator no off-chain stack can replicate;
(C) document the honest deferrals that application-level verification cannot
close.

Status: **Phase A done (implemented + tested, this session); B in flight —
V0 done; V1 unlocking SOLVED (opSig prefix + ABI-ordered args +
sha256(preimage) sig — checkPreimage/checkSig verified under real Spend
eval); codeSha-state pattern added (scriptCode is csep-trimmed → 48B state =
codeSha‖spent‖windowStart + codePrefix arg pins the code); remaining: the
`hash256(outs) === extractOutputHash` assert (bisected — checkPreimage/
checkSig/caps/state all pass; out() serialization byte-diff left); V2/V3
contracts compiled, adapters pending.** Order: A → B (V1 → V2 → V3) → C.

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

## V0 — Compiler spike (gate)

Verify before the contracts:

1. Whether method bodies may reference the implicit `_changePKH`,
   `_changeAmount`, `_newAmount` params (decides V1's payment mechanism:
   compiler change output = the payment).
2. Whether a stateless `SmartContract` accepts a declared
   `SigHashPreimage` param + `checkPreimage` (decides V3's mechanism;
   fallback: stateful with dummy state).
3. `nLocktime`/`extractLocktime` semantics on regtest (sequence
   < 0xffffffff on the spending input).
4. Compile-size sanity: each contract well under the standard script limit.

Deliverable: `spikes/v7/` mini-contract compiled and (if mechanics hold)
deployed+spent on regtest.

## V1 — MamemFeeVault (spend caps as consensus)

`packages/contracts/src/MamemFeeVault.runar.ts` — **stateless `SmartContract`
with an explicit `txPreimage` + manual output construction** (not
StatefulSmartContract: the compiler's `addOutput` machinery emits an
auto-change output whose destination the contract cannot read — a leak for
strict enforcement).

Immutable params: `destPkh` (the only payable address — the anchor-funding
P2PKH wallet; anchors then consume plain P2PKH UTXOs through the existing
`Funder`, so MamemAnchor needs **no change**), `ownerPkh`, `perTxCap`,
`windowCap`, `windowBlocks`, `maxFee`.

State (48 B suffix after `OP_RETURN`): `codeSha(32) ‖ spentInWindow(8) ‖
windowStart(8)`. `extractScriptCode` returns a **csep-trimmed** scriptCode
(`P ‖ 0x6a ‖ state` where `P` = code after the last executed separator), so
the contract cannot rebuild the full locking script from the suffix alone —
it takes `codePrefix` as a method arg and pins the code via
`sha256(codePrefix ‖ P) === codeSha` (codeSha is stored *in* the state, so
there is no hash-in-own-code fixpoint).

`spend(txPreimage, sig, pubKey, codePrefix, payAmount, contSats, changeSats)`:
- `checkPreimage` + `hash160(pubKey)==ownerPkh` + `checkSig` (operator only).
- Window roll via `extractLocktime`: `lt >= windowStart+windowBlocks` resets
  `{spent=payAmount, windowStart=lt}`, else `spent+=payAmount`;
  assert `spent <= windowCap`; assert `payAmount <= perTxCap`.
- Outputs built by hand: continuation (rebuilt code + new state) + payment
  to `destPkh` + change to `ownerPkh`; `hash256(outs) ===
  extractOutputHash(preimage)` makes it consensus.
- Fee cap: `extractAmount - contSats - payAmount - changeSats <= maxFee`.

Host: `packages/anchor/src/vault.ts` — `FeeVault`/`VaultFunder` spends the
vault to mint capped funding UTXOs (SDK `computeOpPushTx` + `buildStatefulPrefix`
for the preimage-sig prefix). `SpendGuard` stays in front (defense in depth).
Env `MAMEM_FEE_VAULT`; testnet/main only.

## V2 — MamemCheckpointBond (unforkable checkpoint chain)

`packages/contracts/src/MamemCheckpointBond.runar.ts` — StatefulSmartContract.

Immutable params: `operatorPkh`, `bondReleaseHeight`, `minBond`.
Mutable state: `{n, lastRoot}` — the checkpoint chain lives IN the script:
`commit(sig, pubKey, newRoot)` continues `{n+1, newRoot}` only — conflicting
on-chain checkpoints are impossible by construction (stronger than detection).
`close(sig, pubKey)` returns the bond to `operatorPkh` once
`extractLocktime >= bondReleaseHeight`.

Documented limit: punitive slashing needs `checkDataSig` — deferred (see
baseline). Closes VER-007's on-chain side.

## V3 — DisclosureSale (atomic paid disclosure)

`packages/contracts/src/DisclosureSale.runar.ts` — SmartContract.

Immutable params: `sellerPkh`, `price`, `hWrapped` (sha256), 
`refundAfterHeight`, optional `buyerPkh`.

`buy(wrapped, ... )`: `sha256(wrapped) == hWrapped` — the spend itself
reveals the payload on-chain, bound to the committed hash. Outputs:
`>= price` → `sellerPkh`; change → buyer. If `buyerPkh` set, the funding
input's key must hash to it (`checkSig`). `refund(sig)`: seller + locktime
past `refundAfterHeight`.

`wrapped` is the existing `DisclosureEnvelope.wrapped` (BRC-42 ciphertext to
the buyer) — on-chain exposure is safe; or plaintext `{field,salt,path}` for
paid *public* disclosure.

Host: `packages/identity/src/sale.ts` — `offerDisclosure` /
`buyDisclosure` / `claimDisclosure` (fetch wrapped from the tx, decrypt,
verify envelope end-to-end). MCP tools behind `MAMEM_PAID_DISCLOSURE`.

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
