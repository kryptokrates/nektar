"""V1 — OUR circuit: sha256d-Merkle membership (16-leaf), root BAKED as a
circuit constant so the verifying key commits to the statement. Loads the
arkworks proof from circuits/membership/proof_le/ (see be2le.py — arkworks
serialises field elements big-endian while elliptic_curves expects
little-endian; run be2le.py first) and emits a BLS12-381
verifier locking script + push-only unlocking script.

The lock is prepended with `<root> <index> OP_2DROP` — the bound statement
is publicly readable in the locking script itself (consumers of a claim or
verdict can fetch the verifier tx and check which statement it binds).

Run from zkscript checkout:
    .venv312/Scripts/python <nektar>/tools/groth16-spike/spike_groth16_membership.py <path>/zkscript <nektar>
"""
import json
import sys
from pathlib import Path

ZKSCRIPT = Path(sys.argv[1]).resolve()
NEKTAR = Path(sys.argv[2]).resolve() if len(sys.argv) > 2 else Path("../..").resolve()
sys.path.insert(0, str(ZKSCRIPT))

from elliptic_curves.instantiations.bls12_381.bls12_381 import (
    BLS12_381,
    ProofBls12381,
    VerifyingKeyBls12381,
)
from tx_engine import Context, Script

from src.zkscript.groth16.bls12_381.bls12_381 import bls12_381
from src.zkscript.script_types.locking_keys.groth16 import Groth16LockingKey
from src.zkscript.script_types.unlocking_keys.groth16 import Groth16UnlockingKey
from src.zkscript.util.utility_scripts import nums_to_script

OUT = Path(__file__).resolve().parent / "out"
CIRCUIT = NEKTAR / "circuits" / "membership"

proof = ProofBls12381.deserialise(json.load(open(CIRCUIT / "proof_le/proof.json"))["proof"])
vk = VerifyingKeyBls12381.deserialise(json.load(open(CIRCUIT / "proof_le/verifying_key.json"))["verifying_key"])
params = json.load(open(CIRCUIT / "parameters.json"))
root_bytes = bytes.fromhex(params["root"])
index = int(params["index"])
pub = [index]  # our single public input
print(f"statement: membership in root {params['root'][:16]}.. at index {index} (16-leaf sha256d tree)")

cache_vk = vk.prepare()
prepared_vk = vk.prepare_for_zkscript(cache_vk)
prepared_proof = proof.prepare_for_zkscript(cache_vk, pub)

# PUSH-ONLY unlock (consensus: scriptSig must be pushes only)
unlocking_key = Groth16UnlockingKey.from_data(
    groth16_model=bls12_381,
    pub=prepared_proof.public_statements,
    A=prepared_proof.a, B=prepared_proof.b, C=prepared_proof.c,
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
body = bls12_381.groth16_verifier(
    locking_key, modulo_threshold=200 * 8, extractable_inputs=1,
    max_multipliers=None, check_constant=True, clean_constant=True,
)

# Statement doc-push: `<root> <index> OP_2DROP` + verifier body — readable
# in the lock, bound by the txid, zero semantic effect on the stack.
lock = (Script.parse_string(f"0x{root_bytes.hex()}")
        + nums_to_script([index])
        + Script.parse_string("OP_2DROP")
        + body)
lb, ub = lock.raw_serialize(), unlock.raw_serialize()
print(f"locking:   {len(lb):>7} bytes")
print(f"unlocking: {len(ub):>7} bytes")

# push-only scan
i, pushes_only = 0, True
while i < len(ub):
    op = ub[i]
    if op == 0: i += 1; continue
    if op <= 75: i += 1 + op
    elif op == 76: i += 2 + ub[i + 1]
    elif op == 77: i += 3 + int.from_bytes(ub[i + 1:i + 3], 'little')
    elif op == 78: i += 5 + int.from_bytes(ub[i + 1:i + 5], 'little')
    elif 0x51 <= op <= 0x60: i += 1; continue
    else:
        pushes_only = False
        print(f"  non-push opcode 0x{op:02x} at {i}")
        break
print(f"unlock push-only: {pushes_only}")

ctx = Context(script=unlock + lock)
ok = ctx.evaluate()
print(f"tx_engine evaluate (membership proof): {'PASS' if ok else 'FAIL'}")

(OUT / "lock.hex").write_text(lb.hex())
(OUT / "unlock.hex").write_text(ub.hex())
(OUT / "meta-membership.json").write_text(json.dumps({
    "curve": "bls12_381",
    "circuit": "nektar sha256d membership, depth 4 (16-leaf), root baked as constant",
    "root": params["root"], "index": index,
    "lock_bytes": len(lb), "unlock_bytes": len(ub),
    "push_only": pushes_only, "engine_ok": ok,
}, indent=2))
sys.exit(0 if (ok and pushes_only) else 1)
