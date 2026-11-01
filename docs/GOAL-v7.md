# GOAL-v7 — On-Chain Enforcement (Rúnar Covenants)

Three guarantees move from application code (bypassable by whoever holds the
key) into consensus rules enforced by the locking script — the differentiator
no off-chain stack can replicate.

Status: **V0 done; V1 unlocking SOLVED (opSig prefix + ABI-ordered args +
sha256(preimage) sig — checkPreimage/checkSig verified under real Spend
eval); remaining: in-script state slice via extractScriptCode (scriptCode
length ≠ locking-script length — csep-trimmed); V2/V3 contracts compiled,
adapters pending.** Order: V0 spike → V1 → V2 → V3, each with contract +
artifact + host adapter + env-gated wiring + unit tests + regtest e2e + docs +
commit/push; CI green per phase.

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

`packages/contracts/src/MamemFeeVault.runar.ts` — StatefulSmartContract.

Immutable params: `destPkh` (the only payable address — the anchor-funding
P2PKH wallet; anchors then consume plain P2PKH UTXOs through the existing
`Funder`, so MamemAnchor needs **no change**), `ownerPkh`, `perTxCap`,
`windowCap`, `windowBlocks`, `maxFee`.
Mutable state: `{spentInWindow, windowStart}`.

`spend(sig, pubKey, payAmount)`:
- `hash160(pubKey)==ownerPkh` + `checkSig` (operator-initiated only).
- Window roll via `extractLocktime`: `lt >= windowStart+windowBlocks` resets
  `{spentInWindow=payAmount, windowStart=lt}`, else `spentInWindow+=payAmount`;
  assert `spentInWindow <= windowCap`; assert `payAmount <= perTxCap`.
- Outputs: vault-continuation (remaining funds) + P2PKH payment to
  `destPkh` (the compiler change output IS the payment).
- Fee cap: `extractAmount(preimage) - continuationSats - payAmount <= maxFee`.

Host: `packages/chain/src/vault-funder.ts` — `VaultFunder implements Funder`
spends the vault to mint capped funding UTXOs. `SpendGuard` stays in front
(defense in depth). Env `MAMEM_FEE_VAULT`; testnet/main only.

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
wrong reveal) · regtest e2e deploy+spend · no mainnet/testnet broadcast
without explicit approval · docs (PROOFS / THREAT-MODEL / OPERATIONS /
test-spec rows).
