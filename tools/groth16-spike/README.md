# groth16-spike — V1

On-chain Groth16 verification (nChain ePrint 2024/1498): the verifier
lives **inside** the locking script, so the proof is checked by BSV
consensus — no watcher, no challenge window.

## What ran — V1: OUR circuit + validity claim (live regtest)

1. **`circuits/membership/`** — arkworks Groth16 over BLS12-381 proving
   **sha256d-Merkle membership** in a 16-leaf tree built with mamem's own
   `dsha256` fold. Statement: "leaf ∈ root `eda3ef76…` at index 5"; the
   root is baked as a **circuit constant** so the verifying key commits
   to the statement. Dev trusted setup + prove inside a Linux rust
   container (`gen-params.mjs` builds the tree/witnesses).
2. **`be2le.py`** — serialization bridge: arkworks `serialize_uncompressed`
   writes field elements **big-endian** while nChain's `elliptic_curves`
   deserialise expects **little-endian** (G2 points additionally swap
   Fq2 limb order; `gamma_abc` carries an 8B Vec length prefix).
   Converts `proof/` → `proof_le/`.
3. **`spike_groth16_membership.py`** — emits the verifier: **398,375 B
   lock / 22,225 B push-only unlock**. The lock is prefixed with a
   doc-push `<root> <index> OP_2DROP` so the bound statement is readable
   on-chain. `tx_engine.Context` → PASS.
4. **`e2e-groth16-regtest.mjs`** — live Teranode regtest: deploy
   `1d2a0a48…` + proof spend `a1d9ec35…` accepted, tampered unlock
   rejected, mined h8486.
5. **`e2e-grothclaim-regtest.mjs`** — the **validity claim**:
   `MamemGrothClaim.settleValid` pins the verifier UTXO as co-spent
   input1 (hashPrevouts, same pattern as `proveStage`). The claim's
   `ClaimVerdict` output can only exist if the proof verifies under
   consensus — claim-only spend and tampered-proof settle both rejected,
   valid settle `778a44dd…` emitted the verdict, mined h8489.

## Earlier passes

`spike_groth16_real.py` (MNT4-753 arkworks `square_root` — real proof,
not our statement): 489,234 B lock / 294,486 B push-only unlock, deploy
`56c2a4…` + spend `06853c…` accepted, h8485.
`spike_groth16.py` (BLS12-381, synthetic vk/proof): 430,324 B lock /
22,629 B unlock, deploy `8e3728…` + spend `0952dc…`.

## Gotcha that bit us

`script.py`'s default `Groth16UnlockingKey` emits an unlock containing
real opcodes → rejected by the node ("Only non-push operators allowed in
signatures" — scriptSig push-only is **consensus**, not policy, on BSV).
Fix: `has_precomputed_gradients=True` on the unlocking key (gradients
travel as data) — `spike_groth16_real.py` does this and self-checks the
unlock for push-only encoding before writing artifacts.

## Reproduce

```sh
git clone https://github.com/nchain-innovation/zkscript_package zkscript
cd zkscript && python -m venv .venv312   # Python 3.12 REQUIRED (3.11 fails)
.venv312/Scripts/pip install -r requirements.txt

# circuit (Linux — no MSVC linker on this Windows host):
node <mamem>/circuits/membership/gen-params.mjs            # tree + witnesses
docker run --rm -v <mamem>/circuits/membership:/w -w /w rust:1-slim \
  sh -c 'apt-get update -qq && apt-get install -yqq pkg-config libssl-dev >/dev/null && cargo run --release'

# artifacts + scripts:
.venv312/Scripts/python <mamem>/tools/groth16-spike/be2le.py <mamem>/circuits/membership
.venv312/Scripts/python <mamem>/tools/groth16-spike/spike_groth16_membership.py <path>/zkscript <mamem>

# live regtest (needs running Teranode stack + MAMEM_REGTEST_WIF):
node <mamem>/packages/e2e/src/e2e-groth16-regtest.mjs      # verifier deploy+spend
node <mamem>/packages/e2e/src/e2e-grothclaim-regtest.mjs   # validity claim settle
```

Artifacts land in `out/` and `circuits/membership/proof*/` (gitignored;
the `proof/` dir is the arkworks-native output, `proof_le/` the converted
one). The trusted setup is **dev single-party** — a production deployment
needs a documented ceremony.

## License note

zkscript is under the **nChain Research License** (non-commercial;
generated scripts count as Licensed Material; patents not licensed).
Artifacts are generated at dev time and never committed. For production
the claim covenant treats the verifier as a pluggable co-spent artifact
— a self-built verifier could be substituted without architecture
changes.

## What's NOT proven here

- **Trusted-setup ceremony** — today's vk/proof come from a dev
  single-party setup; toxic-waste assumptions are honest only with a
  documented multi-party ceremony.
- **Statement↔verifier semantic binding** — the claim's `journalRoot`
  mirrors the verifier's doc-push; the outpoint pin is consensus-bound
  but the doc-push↔journalRoot equality is checked off-chain at deploy.
- **Mainnet** — everything above is regtest; fee/policy numbers for a
  ~400KB lock on mainnet are their own exercise.
