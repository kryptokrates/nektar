// BabyBearProbe — GOAL-v9 Stage 0a spike: measure the real Script cost of
// BabyBear field arithmetic (p = 2^31 − 2^27 + 1 = 2013265921) on BSV.
// Post-Genesis/Chronicle numbers are arbitrary-precision, and OP_MOD is a
// native opcode — mulmod needs no pseudo-Mersenne fold, just mul + mod.
import { SmartContract, assert } from 'runar-lang'

export class BabyBearProbe extends SmartContract {
  constructor () { super() }

  /** (a*b) mod p — the FRI fold's inner op. Asserts caller-supplied result. */
  public mulmod (a: bigint, b: bigint, expected: bigint) {
    assert((a * b) % 2013265921n === expected)
  }

  /** x^7 mod p — the Poseidon2-BabyBear S-box (4 chained mulmods). */
  public sbox7 (x: bigint, expected: bigint) {
    const x2 = (x * x) % 2013265921n
    const x4 = (x2 * x2) % 2013265921n
    const x6 = (x4 * x2) % 2013265921n
    const x7 = (x6 * x) % 2013265921n
    assert(x7 === expected)
  }

  /** one MDS-style row: sum of 16 field products — the Poseidon2 diffusion
   *  inner loop for one state element (t=16 BabyBear params). */
  public mdsRow (a0: bigint, a1: bigint, a2: bigint, a3: bigint, a4: bigint, a5: bigint, a6: bigint, a7: bigint, a8: bigint, a9: bigint, a10: bigint, a11: bigint, a12: bigint, a13: bigint, a14: bigint, a15: bigint, expected: bigint) {
    // diagonal weights of the circulant BabyBear MDS (small constants → add)
    const acc = a0 * 2n + a1 + a2 + a3 + a4 * 2n + a5 + a6 + a7 + a8 * 2n + a9 + a10 + a11 + a12 * 2n + a13 + a14 + a15
    assert(acc % 2013265921n === expected)
  }
}
