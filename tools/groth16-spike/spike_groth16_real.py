"""V0.1 — REAL proof variant: load the arkworks-generated square_root proof
(w^2 = x mod p, params root=8, square=64) shipped in zkscript examples and
emit a PUSH-ONLY unlocking script (gradients precomputed as data — required:
'Only non-push operators allowed in signatures' is a consensus rule on BSV).

Run from zkscript checkout:
    .venv312/Scripts/python <mamem>/tools/groth16-spike/spike_groth16_real.py <path>/zkscript
"""
import json
import sys
from pathlib import Path

ZKSCRIPT = Path(sys.argv[1]).resolve()
sys.path.insert(0, str(ZKSCRIPT))

from elliptic_curves.instantiations.mnt4_753.mnt4_753 import (
    MNT4_753,
    ProofMnt4753,
    VerifyingKeyMnt4753,
)
from tx_engine import Context

from src.zkscript.groth16.mnt4_753.mnt4_753 import mnt4_753
from src.zkscript.script_types.locking_keys.groth16 import Groth16LockingKey
from src.zkscript.script_types.unlocking_keys.groth16 import Groth16UnlockingKey

OUT = Path(__file__).resolve().parent / "out"
EXAMPLES = ZKSCRIPT / "examples" / "square_root" / "proof"

proof = ProofMnt4753.deserialise(json.load(open(EXAMPLES / "proof.json"))["proof"])
vk = VerifyingKeyMnt4753.deserialise(json.load(open(EXAMPLES / "verifying_key.json"))["verifying_key"])

# public_inputs.json: serialized field elements; first u64 little-endian count
raw = bytes(json.load(open(EXAMPLES / "public_inputs.json"))["public_inputs"])
n = int.from_bytes(raw[:8], "little")
flen = (MNT4_753.get_order_scalar_field().bit_length() + 8) // 8
pub = [1]
for i in range(n):
    e = MNT4_753.scalar_field.deserialise(raw[8 + i * flen:8 + (i + 1) * flen])
    pub.extend(e.to_list() if isinstance(e.to_list(), list) else [e.to_list()])
print(f"public inputs: {pub}  (circuit: w^2 = x mod p; statement = square)")

cache_vk = vk.prepare()
prepared_vk = vk.prepare_for_zkscript(cache_vk)
prepared_proof = proof.prepare_for_zkscript(cache_vk, pub[1:])

# PUSH-ONLY unlock: has_precomputed_gradients=True puts all pairing gradients
# in the unlock as DATA pushes; the lock then skips recomputing them.
unlocking_key = Groth16UnlockingKey.from_data(
    groth16_model=mnt4_753,
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
unlock = unlocking_key.to_unlocking_script(mnt4_753, True, 1)

locking_key = Groth16LockingKey(
    alpha_beta=prepared_vk.alpha_beta,
    minus_gamma=prepared_vk.minus_gamma,
    minus_delta=prepared_vk.minus_delta,
    gamma_abc=prepared_vk.gamma_abc,
    gradients_pairings=[prepared_vk.gradients_minus_gamma, prepared_vk.gradients_minus_delta],
    has_precomputed_gradients=False,
)
lock = mnt4_753.groth16_verifier(
    locking_key, modulo_threshold=200 * 8, extractable_inputs=1,
    max_multipliers=None, check_constant=True, clean_constant=True,
)

lb, ub = lock.raw_serialize(), unlock.raw_serialize()
print(f"locking:   {len(lb):>7} bytes")
print(f"unlocking: {len(ub):>7} bytes")

# push-only sanity on the unlock: every element must be a data push
i, pushes_only = 0, True
while i < len(ub):
    op = ub[i]
    if op == 0: i += 1; continue
    if op <= 75: i += 1 + op
    elif op == 76: i += 3 + ub[i + 1]
    elif op == 77: i += 4 + int.from_bytes(ub[i + 1:i + 3], 'little')
    elif op == 78: i += 6 + int.from_bytes(ub[i + 1:i + 5], 'little')
    else:
        pushes_only = False
        print(f"  non-push opcode 0x{op:02x} at offset {i}")
        break
print(f"unlock push-only: {pushes_only}")

ctx = Context(script=unlock + lock)
ok = ctx.evaluate()
print(f"tx_engine evaluate (REAL proof): {'PASS' if ok else 'FAIL'}")

(OUT / "lock.hex").write_text(lb.hex())
(OUT / "unlock.hex").write_text(ub.hex())
(OUT / "meta-real.json").write_text(json.dumps({
    "curve": "mnt4_753", "circuit": "square_root (w^2=x mod p)",
    "proof": "arkworks-generated, shipped in zkscript examples/square_root",
    "lock_bytes": len(lb), "unlock_bytes": len(ub),
    "push_only": pushes_only, "engine_ok": ok,
}, indent=2))
sys.exit(0 if (ok and pushes_only) else 1)
