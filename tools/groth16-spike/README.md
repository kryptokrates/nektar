# groth16-spike — V0

Proof-of-mechanics for on-chain Groth16 verification (nChain ePrint
2024/1498): the verifier lives **inside** the locking script, so the
proof is checked by BSV consensus — no watcher, no challenge window.

## What ran

`spike_groth16.py` drives `nchain-innovation/zkscript` to emit a
BLS12-381 Groth16 verifier (dummy CRS + proof — the pairing equation is
made to hold by construction; this proves *script mechanics*, not a real
circuit):

- locking script **430,324 B**, unlocking script **22,629 B**
- `tx_engine.Context` → PASS
- `packages/e2e/src/e2e-groth16-regtest.mjs` → deploy + spend **accepted
  by live Teranode regtest**, tampered unlock rejected

## Reproduce

```sh
git clone https://github.com/nchain-innovation/zkscript_package zkscript
cd zkscript && python -m venv .venv312   # Python 3.12 REQUIRED (3.11 fails)
.venv312/Scripts/pip install -r requirements.txt
.venv312/Scripts/python <mamem>/tools/groth16-spike/spike_groth16.py <path>/zkscript
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

- Real circuit — the demo vk/proof is synthetic. A membership or
  statement circuit needs an R1CS prover (arkworks/snarkjs) + a
  trusted-setup ceremony per circuit.
- Covenant wiring — a `MamemZkClaim` validity variant pinning the
  verifier UTXO as co-spent judge is the V1 design.
