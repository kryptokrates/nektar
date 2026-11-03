"""V0 spike: generate a Groth16 verifier in Bitcoin Script via nChain zkscript
and evaluate lock+unlock locally with tx_engine.

Uses the *dummy* vk/proof approach from zkscript's own tests: alpha_beta is
constructed so the pairing equation holds by construction — this proves the
script-generation + script-semantics mechanics WITHOUT needing a real R1CS
prover (arkworks/snarkjs). A real circuit (membership/statement) is V1.

Run from the zkscript_package checkout:
    .venv312/Scripts/python <nektar>/tools/groth16-spike/spike_groth16.py

Artifacts land in <nektar>/tools/groth16-spike/out/ (gitignored — generated
scripts are Licensed Material under the nChain Research License).
"""
import json
import sys
from pathlib import Path

# --- make the zkscript checkout importable ---
ZKSCRIPT = Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else Path("../../zkscript").resolve()
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
OUT.mkdir(parents=True, exist_ok=True)

# --- minimal dummy CRS + proof (mirrors generate_random_tests minimal path) ---
g1, g2 = BLS12_381.g1_curve.get_generator(), BLS12_381.g2_curve.get_generator()
r = BLS12_381.scalar_field.get_modulus()

# dlog of the vk/proof points — ANY values work; the equation is made to hold.
alpha_ = 12790544464366708614940994215014625841059650806301063465359886625690547
beta_ = 35703396574432322187740701285564343485982931931825512168376677487234446722993
gamma_ = 39343400274038067941958661494186555637424382984868274330750039783384953010645
delta_ = 2254455857535174888113934810625770138551286599034257880685085874098503964843
A_ = 33976064719184601046772332296059783359114154888771311478941393763485422892419
B_ = 34384663481284015822004772645526885025674590588008966092270502606290692706039
C_ = 24893988411182277785498905442769971411187449562420914731983022451448972146513

alpha, beta, gamma, delta = g1.multiply(alpha_), g2.multiply(beta_), g2.multiply(gamma_), g2.multiply(delta_)
A, B, C = g1.multiply(A_), g2.multiply(B_), g1.multiply(C_)

dlog_gamma_abc = [15, 23, 11]           # minimal example: two public inputs
pub_statement = [1, 5, 4]
gamma_abc = [g1.multiply(d) for d in dlog_gamma_abc]

sum_gamma_abc = g1.multiply(0)
for i in range(len(gamma_abc)):
    sum_gamma_abc += gamma_abc[i].multiply(pub_statement[i])

# The trick: alpha_beta is DEFINED as the RHS pairing, so the check passes.
alpha_beta = BLS12_381.pairing([A, sum_gamma_abc, C], [B, -gamma, -delta])

vk = VerifyingKeyBls12381(alpha, beta, gamma, delta, gamma_abc)
proof = ProofBls12381(A, B, C)
cache_vk = vk.prepare()
prepared_vk = vk.prepare_for_zkscript(cache_vk)
prepared_proof = proof.prepare_for_zkscript(cache_vk, pub_statement[1:])

# --- emit unlock + lock ---
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
    alpha_beta=alpha_beta.to_list(),
    minus_gamma=prepared_vk.minus_gamma,
    minus_delta=prepared_vk.minus_delta,
    gamma_abc=prepared_vk.gamma_abc,
    gradients_pairings=[prepared_vk.gradients_minus_gamma, prepared_vk.gradients_minus_delta],
    has_precomputed_gradients=False,
)
lock = bls12_381.groth16_verifier(
    locking_key,
    modulo_threshold=1,
    extractable_inputs=1,
    max_multipliers=None,
    check_constant=True,
    clean_constant=True,
)

lock_bytes = lock.raw_serialize()
unlock_bytes = unlock.raw_serialize()
print(f"locking script:   {len(lock_bytes):>7} bytes")
print(f"unlocking script: {len(unlock_bytes):>7} bytes")

# --- evaluate in the tx_engine interpreter ---
ctx = Context(script=unlock + lock)
ok = ctx.evaluate()
print(f"tx_engine evaluate: {'PASS' if ok else 'FAIL'} "
      f"(stack={ctx.get_stack().size()}, altstack={ctx.get_altstack().size()})")

(OUT / "lock.hex").write_text(lock_bytes.hex())
(OUT / "unlock.hex").write_text(unlock_bytes.hex())
(OUT / "meta.json").write_text(json.dumps({
    "curve": "bls12_381",
    "pub_statement": pub_statement,
    "lock_bytes": len(lock_bytes),
    "unlock_bytes": len(unlock_bytes),
    "engine_ok": ok,
    "note": "dummy CRS/proof — equation holds by construction; proves script "
            "mechanics only. Generated artifacts are Licensed Material under "
            "the nChain Research License (non-commercial).",
}, indent=2))
print(f"artifacts -> {OUT}")
sys.exit(0 if ok else 1)
