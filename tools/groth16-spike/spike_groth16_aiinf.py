"""V2 — ai_inference variant: load the arkworks-generated Groth16 proof
nChain ships in zkscript examples/ai_inference/proof/ and emit the BLS12-381
in-script verifier (lock) + a PUSH-ONLY unlocking script (proof + gradients
as data — consensus requires 'Only non-push operators in signatures').

The circuit proves a quantized 784→16→10 MLP forward pass; its single public
input is the field element  poseidon(input ‖ argmax_output ‖ poseidon(model)).
The statement lives INSIDE the unlock (pushed by the spender) — the spend tx
therefore binds the exact (input, output, model) triple on-chain, which is
what nektar's modelAttestation cites. nChain's Poseidon parameters are toy
constants — see packages/zk/src/poseidon-toy.ts which re-derives the
statement off-chain byte-exactly.

Run from zkscript checkout:
    .venv312/Scripts/python <nektar>/tools/groth16-spike/spike_groth16_aiinf.py <path>/zkscript
"""
import json
import sys
from pathlib import Path

ZKSCRIPT = Path(sys.argv[1]).resolve()
sys.path.insert(0, str(ZKSCRIPT))

from elliptic_curves.instantiations.bls12_381.bls12_381 import (
    BLS12_381,
    ProofBls12381,
    VerifyingKeyBls12381,
)
from tx_engine import Context

from src.zkscript.groth16.bls12_381.bls12_381 import bls12_381
from src.zkscript.script_types.locking_keys.groth16 import Groth16LockingKey
from src.zkscript.script_types.unlocking_keys.groth16 import Groth16UnlockingKey

OUT = Path(__file__).resolve().parent / "out"
EXAMPLES = ZKSCRIPT / "examples" / "ai_inference" / "proof"

proof = ProofBls12381.deserialise(json.load(open(EXAMPLES / "proof.json"))["proof"])
vk = VerifyingKeyBls12381.deserialise(json.load(open(EXAMPLES / "verifying_key.json"))["verifying_key"])

# public_inputs.json: [u64 LE count][n × Fr (32B LE)] — one element for this circuit
raw = bytes(json.load(open(EXAMPLES / "public_inputs.json"))["public_inputs"])
n = int.from_bytes(raw[:8], "little")
flen = (BLS12_381.get_order_scalar_field().bit_length() + 8) // 8
pub = [1]
for i in range(n):
    e = BLS12_381.scalar_field.deserialise(raw[8 + i * flen:8 + (i + 1) * flen])
    pub.extend(e.to_list() if isinstance(e.to_list(), list) else [e.to_list()])
print(f"public inputs: {pub}  (circuit: quantized MLP argmax; statement = poseidon(input||out||poseidon(model)))")

cache_vk = vk.prepare()
prepared_vk = vk.prepare_for_zkscript(cache_vk)
prepared_proof = proof.prepare_for_zkscript(cache_vk, pub[1:])

unlocking_key = Groth16UnlockingKey.from_data(
    groth16_model=bls12_381,
    pub=prepared_proof.public_statements,
    A=prepared_proof.a,
    B=prepared_proof.b,
    C=prepared_proof.c,
    gradients_pairings=[
        prepared_proof.gradients_b,
        prepared_proof.gradients_minus_gamma,
        prepared_proof.gradients_minus_delta,
    ],
    gradients_multiplications=prepared_proof.gradients_multiplications,
    max_multipliers=None,
    gradients_additions=prepared_proof.gradients_additions,
    inverse_miller_output=prepared_proof.inverse_miller_loop,
    gradient_gamma_abc_zero=prepared_proof.gradient_gamma_abc_zero,
    has_precomputed_gradients=True,
)
unlock = unlocking_key.to_unlocking_script(bls12_381, True, 1)

locking_key = Groth16LockingKey(
    alpha_beta=prepared_vk.alpha_beta,
    minus_gamma=prepared_vk.minus_gamma,
    minus_delta=prepared_vk.minus_delta,
    gamma_abc=prepared_vk.gamma_abc,
    gradients_pairings=[prepared_vk.gradients_minus_gamma, prepared_vk.gradients_minus_delta],
    has_precomputed_gradients=False,
)
lock = bls12_381.groth16_verifier(
    locking_key, modulo_threshold=200 * 8, extractable_inputs=1,
    max_multipliers=None, check_constant=True, clean_constant=True,
)

ctx = Context(script=unlock + lock)
ok = ctx.evaluate()
print("engine evaluation:", "PASS" if ok else "FAIL")
assert ok, "Context evaluation failed"

OUT.mkdir(exist_ok=True)
# raw_serialize(): bare opcode stream — serialize() prepends a compact-size
# length and breaks LockingScript.fromHex / push-only signature checks
(OUT / "lock-aiinf.hex").write_text(lock.raw_serialize().hex())
(OUT / "unlock-aiinf.hex").write_text(unlock.raw_serialize().hex())

meta = {
    "curve": "bls12_381",
    "circuit": "ai_inference (quantized 784x16x10 MLP argmax)",
    "proof": "arkworks-generated, shipped in zkscript examples/ai_inference",
    "public_statement_fr": "0x%064x" % int.from_bytes(raw[8:8 + flen], "little"),
    "lock_bytes": len(lock.raw_serialize()),
    "unlock_bytes": len(unlock.raw_serialize()),
    "push_only": True,
    "engine_ok": True,
    "license": "nChain Research License — generated scripts are non-commercial",
    "setup": "dev trusted setup (nChain example params) — regtest-grade only",
}
(OUT / "meta-aiinf.json").write_text(json.dumps(meta, indent=2))
print(json.dumps(meta, indent=2))
