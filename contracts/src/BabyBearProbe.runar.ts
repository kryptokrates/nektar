// BabyBearProbe — hardened BabyBear field arithmetic (p = 2^31 − 2^27 + 1 = 2013265921) on BSV.
// F-27: asserts canonical non-negative inputs (0 <= x < 2013265921) before Bitcoin Script OP_MOD
// so negative dividends can never produce negative residues.
import { SmartContract, assert } from 'runar-lang'

export class BabyBearProbe extends SmartContract {
  constructor () { super() }

  public mulmod (a: bigint, b: bigint, expected: bigint) {
    assert(a >= 0n && a < 2013265921n && b >= 0n && b < 2013265921n && expected >= 0n && expected < 2013265921n)
    assert((a * b) % 2013265921n === expected)
  }

  public sbox7 (x: bigint, expected: bigint) {
    assert(x >= 0n && x < 2013265921n && expected >= 0n && expected < 2013265921n)
    const x2 = (x * x) % 2013265921n
    const x4 = (x2 * x2) % 2013265921n
    const x6 = (x4 * x2) % 2013265921n
    const x7 = (x6 * x) % 2013265921n
    assert(x7 === expected)
  }

  public mdsRow (a0: bigint, a1: bigint, a2: bigint, a3: bigint, a4: bigint, a5: bigint, a6: bigint, a7: bigint, a8: bigint, a9: bigint, a10: bigint, a11: bigint, a12: bigint, a13: bigint, a14: bigint, a15: bigint, expected: bigint) {
    assert(a0 >= 0n && a0 < 2013265921n && a1 >= 0n && a1 < 2013265921n && a2 >= 0n && a2 < 2013265921n && a3 >= 0n && a3 < 2013265921n)
    assert(a4 >= 0n && a4 < 2013265921n && a5 >= 0n && a5 < 2013265921n && a6 >= 0n && a6 < 2013265921n && a7 >= 0n && a7 < 2013265921n)
    assert(a8 >= 0n && a8 < 2013265921n && a9 >= 0n && a9 < 2013265921n && a10 >= 0n && a10 < 2013265921n && a11 >= 0n && a11 < 2013265921n)
    assert(a12 >= 0n && a12 < 2013265921n && a13 >= 0n && a13 < 2013265921n && a14 >= 0n && a14 < 2013265921n && a15 >= 0n && a15 < 2013265921n)
    assert(expected >= 0n && expected < 2013265921n)
    const acc = a0 * 2n + a1 + a2 + a3 + a4 * 2n + a5 + a6 + a7 + a8 * 2n + a9 + a10 + a11 + a12 * 2n + a13 + a14 + a15
    assert(acc % 2013265921n === expected)
  }
}
