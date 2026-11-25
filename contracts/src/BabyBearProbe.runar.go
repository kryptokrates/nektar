// BabyBearProbe — hardened BabyBear field arithmetic (p = 2^31 - 2^27 + 1 = 2013265921) on BSV.
// F-27: asserts canonical non-negative inputs (0 <= x < 2013265921) before Bitcoin Script OP_MOD
// so negative dividends can never produce negative residues.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type BabyBearProbe struct {
	runar.SmartContract
}

func (c *BabyBearProbe) Mulmod(a runar.Bigint, b runar.Bigint, expected runar.Bigint) {
	runar.Assert(a >= 0 && a < 2013265921 && b >= 0 && b < 2013265921 && expected >= 0 && expected < 2013265921)
	runar.Assert((a*b)%2013265921 == expected)
}

func (c *BabyBearProbe) Sbox7(x runar.Bigint, expected runar.Bigint) {
	runar.Assert(x >= 0 && x < 2013265921 && expected >= 0 && expected < 2013265921)
	x2 := (x * x) % 2013265921
	x4 := (x2 * x2) % 2013265921
	x6 := (x4 * x2) % 2013265921
	x7 := (x6 * x) % 2013265921
	runar.Assert(x7 == expected)
}

func (c *BabyBearProbe) MdsRow(a0 runar.Bigint, a1 runar.Bigint, a2 runar.Bigint, a3 runar.Bigint, a4 runar.Bigint, a5 runar.Bigint, a6 runar.Bigint, a7 runar.Bigint, a8 runar.Bigint, a9 runar.Bigint, a10 runar.Bigint, a11 runar.Bigint, a12 runar.Bigint, a13 runar.Bigint, a14 runar.Bigint, a15 runar.Bigint, expected runar.Bigint) {
	runar.Assert(a0 >= 0 && a0 < 2013265921 && a1 >= 0 && a1 < 2013265921 && a2 >= 0 && a2 < 2013265921 && a3 >= 0 && a3 < 2013265921)
	runar.Assert(a4 >= 0 && a4 < 2013265921 && a5 >= 0 && a5 < 2013265921 && a6 >= 0 && a6 < 2013265921 && a7 >= 0 && a7 < 2013265921)
	runar.Assert(a8 >= 0 && a8 < 2013265921 && a9 >= 0 && a9 < 2013265921 && a10 >= 0 && a10 < 2013265921 && a11 >= 0 && a11 < 2013265921)
	runar.Assert(a12 >= 0 && a12 < 2013265921 && a13 >= 0 && a13 < 2013265921 && a14 >= 0 && a14 < 2013265921 && a15 >= 0 && a15 < 2013265921)
	runar.Assert(expected >= 0 && expected < 2013265921)
	acc := a0*2 + a1 + a2 + a3 + a4*2 + a5 + a6 + a7 + a8*2 + a9 + a10 + a11 + a12*2 + a13 + a14 + a15
	runar.Assert(acc%2013265921 == expected)
}
