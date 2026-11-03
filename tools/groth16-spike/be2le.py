"""be2le — convert arkworks big-endian proof/vk/public_inputs JSON artifacts
into the little-endian byte layout nChain elliptic_curves.deserialise expects.

Formats (verified empirically):
  G1 point (96B): two 48B fields, each BE→LE.
  G2 point (192B): two 96B Fq2 coords; each coord's limbs swap order AND
                   each 48B limb is BE→LE.
  Fr (32B): BE→LE (Vec<Fr> = 8B count + elements).

Usage: be2le.py <circuit_dir>   (reads proof/*.json, writes proof_le/*.json)
"""
import json
import sys
from pathlib import Path

D = Path(sys.argv[1]) / "proof"
OUT = Path(sys.argv[1]) / "proof_le"
OUT.mkdir(exist_ok=True)

def rev(b): return list(b[::-1])
def g1(b):  # 96B: x(48B LE) + y(48B LE)
    return rev(b[:48]) + rev(b[48:96])
def g2(b):  # 192B: x=[c1,c0](LE limbs) + y=[c1,c0]  (ark writes [c0,c1] BE)
    x = rev(b[48:96]) + rev(b[:48])
    y = rev(b[144:192]) + rev(b[96:144])
    return x + y

proof = json.load(open(D / "proof.json"))["proof"]
vk = json.load(open(D / "verifying_key.json"))["verifying_key"]
pis = json.load(open(D / "public_inputs.json"))["public_inputs"]

# proof: A(G1 96B) B(G2 192B) C(G1 96B) = 384B
proof_le = g1(proof[:96]) + g2(proof[96:288]) + g1(proof[288:384])
# vk: alpha(G1) beta(G2) gamma(G2) delta(G2) then gamma_abc k*G1
alpha = g1(vk[:96]); beta = g2(vk[96:288]); gamma = g2(vk[288:480]); delta = g2(vk[480:672])
# gamma_abc is Vec<G1>: 8B count prefix + k*96B elements
rest = vk[672:]
n_gabc = int.from_bytes(bytes(rest[:8]), "little")
gabc = rest[:8]
for i in range(n_gabc):
    s = 8 + i * 96
    gabc += g1(rest[s:s + 96])
vk_le = alpha + beta + gamma + delta + gabc
# public_inputs: u64 count + k*32B Fr (BE→LE each)
n = int.from_bytes(bytes(pis[:8]), "little")
pi_le = list(pis[:8])
for i in range(n):
    pi_le += rev(pis[8 + i * 32:8 + (i + 1) * 32])

json.dump({"proof": proof_le}, open(OUT / "proof.json", "w"))
json.dump({"verifying_key": list(vk_le)}, open(OUT / "verifying_key.json", "w"))
json.dump({"public_inputs": pi_le}, open(OUT / "public_inputs.json", "w"))
print(f"converted: proof {len(proof_le)}B vk {len(vk_le)}B pi {len(pi_le)}B -> {OUT}")
