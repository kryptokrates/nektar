# groth16-spike — V0

Proof-of-mechanics for on-chain Groth16 verification (nChain ePrint
2024/1498): the verifier lives **inside** the locking script, so the
proof is checked by BSV consensus — no watcher, no challenge window.

## What ran

`spike_groth16_real.py` loads the **real arkworks Groth16 proof** shipped
in `zkscript/examples/square_root` (circuit `w² = x mod p`, public
statement = 64, MNT4-753) and emits push-only scripts:

- locking script **489,234 B**, unlocking script **294,486 B** (all data
  pushes — required, see gotcha below)
- `tx_engine.Context` → PASS
- `packages/e2e/src/e2e-groth16-regtest.mjs` → deploy `56c2a4…` + spend
  `06853c…` **accepted by live Teranode regtest**, tampered unlock
  rejected, mined height 8485. A real SNARK ran under consensus.

Earlier mechanics pass (`spike_groth16.py`, BLS12-381, synthetic
vk/proof): 430,324 B lock / 22,629 B unlock, also deployed+spent green.

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
.venv312/Scripts/python <mamem>/tools/groth16-spike/spike_groth16_real.py <path>/zkscript
node <mamem>/packages/e2e/src/e2e-groth16-regtest.mjs   # needs live regtest + MAMEM_REGTEST_WIF
```

Artifacts land in `out/` (gitignored).

## License note

zkscript is under the **nChain Research License** (non-commercial;
generated scripts count as Licensed Material; patents not licensed).
Artifacts are generated at dev time and never committed. For production
the claim covenant treats the verifier as a pluggable co-spent artifact
— a self-built verifier could be substituted without architecture
changes.

## What's NOT proven here

- Mamem circuit — the square_root demo proof is real Groth16 from a real
  R1CS, but not OUR statement. A membership/statement circuit needs its
  own R1CS + trusted-setup ceremony (arkworks or snarkjs; MNT4-753
  elements are ~95 B each so unlocks are fat — BLS12-381 is leaner).
- Covenant wiring — a `MamemZkClaim` validity variant pinning the
  verifier UTXO as co-spent judge is the V1 design.
