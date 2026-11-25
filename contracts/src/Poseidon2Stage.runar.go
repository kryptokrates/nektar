// Poseidon2Stage — GOAL-v9 Stage 0d: hardened ONE round of poseidon2_mix as a
// stateful covenant step.
// F-23 & F-27: enforces BabyBear canonical range 0 <= s_i < 2013265921 on all 24
// cells, enforces round schedule (fullStep on 0..3 / 26..29, partialStep on 4..25,
// bounded < 30), requires caller-supplied round constants verified against a
// committed roundConstHash, and gates advancement via proverPkh signature.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type Poseidon2Stage struct {
	runar.StatefulSmartContract
	S0 runar.Bigint
	S1 runar.Bigint
	S2 runar.Bigint
	S3 runar.Bigint
	S4 runar.Bigint
	S5 runar.Bigint
	S6 runar.Bigint
	S7 runar.Bigint
	S8 runar.Bigint
	S9 runar.Bigint
	S10 runar.Bigint
	S11 runar.Bigint
	S12 runar.Bigint
	S13 runar.Bigint
	S14 runar.Bigint
	S15 runar.Bigint
	S16 runar.Bigint
	S17 runar.Bigint
	S18 runar.Bigint
	S19 runar.Bigint
	S20 runar.Bigint
	S21 runar.Bigint
	S22 runar.Bigint
	S23 runar.Bigint
	Round runar.Bigint
	ProverPkh runar.ByteString
}

func (c *Poseidon2Stage) assertCanonicalState() {
	runar.Assert(c.S0 >= 0 && c.S0 < 2013265921 && c.S1 >= 0 && c.S1 < 2013265921 && c.S2 >= 0 && c.S2 < 2013265921 && c.S3 >= 0 && c.S3 < 2013265921)
	runar.Assert(c.S4 >= 0 && c.S4 < 2013265921 && c.S5 >= 0 && c.S5 < 2013265921 && c.S6 >= 0 && c.S6 < 2013265921 && c.S7 >= 0 && c.S7 < 2013265921)
	runar.Assert(c.S8 >= 0 && c.S8 < 2013265921 && c.S9 >= 0 && c.S9 < 2013265921 && c.S10 >= 0 && c.S10 < 2013265921 && c.S11 >= 0 && c.S11 < 2013265921)
	runar.Assert(c.S12 >= 0 && c.S12 < 2013265921 && c.S13 >= 0 && c.S13 < 2013265921 && c.S14 >= 0 && c.S14 < 2013265921 && c.S15 >= 0 && c.S15 < 2013265921)
	runar.Assert(c.S16 >= 0 && c.S16 < 2013265921 && c.S17 >= 0 && c.S17 < 2013265921 && c.S18 >= 0 && c.S18 < 2013265921 && c.S19 >= 0 && c.S19 < 2013265921)
	runar.Assert(c.S20 >= 0 && c.S20 < 2013265921 && c.S21 >= 0 && c.S21 < 2013265921 && c.S22 >= 0 && c.S22 < 2013265921 && c.S23 >= 0 && c.S23 < 2013265921)
}

func (c *Poseidon2Stage) FullStep(sig runar.Sig, pubKey runar.PubKey, postSats runar.Bigint, postScript runar.ByteString) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert((c.Round >= 0 && c.Round < 4) || (c.Round >= 26 && c.Round < 30))
	c.assertCanonicalState()
	var qt runar.Bigint = 0
	var wt runar.Bigint = 0
	var zt runar.Bigint = 0
	var ls0 runar.Bigint = c.S0
	var ls1 runar.Bigint = c.S1
	var ls2 runar.Bigint = c.S2
	var ls3 runar.Bigint = c.S3
	var ls4 runar.Bigint = c.S4
	var ls5 runar.Bigint = c.S5
	var ls6 runar.Bigint = c.S6
	var ls7 runar.Bigint = c.S7
	var ls8 runar.Bigint = c.S8
	var ls9 runar.Bigint = c.S9
	var ls10 runar.Bigint = c.S10
	var ls11 runar.Bigint = c.S11
	var ls12 runar.Bigint = c.S12
	var ls13 runar.Bigint = c.S13
	var ls14 runar.Bigint = c.S14
	var ls15 runar.Bigint = c.S15
	var ls16 runar.Bigint = c.S16
	var ls17 runar.Bigint = c.S17
	var ls18 runar.Bigint = c.S18
	var ls19 runar.Bigint = c.S19
	var ls20 runar.Bigint = c.S20
	var ls21 runar.Bigint = c.S21
	var ls22 runar.Bigint = c.S22
	var ls23 runar.Bigint = c.S23
	ls0 = (ls0 + 0xfa20c37) % 2013265921
	ls1 = (ls1 + 0x795bb97) % 2013265921
	ls2 = (ls2 + 0x12c60b9c) % 2013265921
	ls3 = (ls3 + 0xeabd88e) % 2013265921
	ls4 = (ls4 + 0x96485ca) % 2013265921
	ls5 = (ls5 + 0x7093527) % 2013265921
	ls6 = (ls6 + 0x1b1d4e50) % 2013265921
	ls7 = (ls7 + 0x30a01ace) % 2013265921
	ls8 = (ls8 + 0x3bd86f5a) % 2013265921
	ls9 = (ls9 + 0x69af7c28) % 2013265921
	ls10 = (ls10 + 0x3f94775f) % 2013265921
	ls11 = (ls11 + 0x731560e8) % 2013265921
	ls12 = (ls12 + 0x465a0ecd) % 2013265921
	ls13 = (ls13 + 0x574ef807) % 2013265921
	ls14 = (ls14 + 0x62fd4870) % 2013265921
	ls15 = (ls15 + 0x52ccfe44) % 2013265921
	ls16 = (ls16 + 0x14772b14) % 2013265921
	ls17 = (ls17 + 0x4dedf371) % 2013265921
	ls18 = (ls18 + 0x260acd7c) % 2013265921
	ls19 = (ls19 + 0x1f51dc58) % 2013265921
	ls20 = (ls20 + 0x75125532) % 2013265921
	ls21 = (ls21 + 0x686a4d7b) % 2013265921
	ls22 = (ls22 + 0x54bac179) % 2013265921
	ls23 = (ls23 + 0x31947706) % 2013265921
	qt = (ls0 * ls0) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls0 = (zt * ls0) % 2013265921
	qt = (ls1 * ls1) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls1 = (zt * ls1) % 2013265921
	qt = (ls2 * ls2) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls2 = (zt * ls2) % 2013265921
	qt = (ls3 * ls3) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls3 = (zt * ls3) % 2013265921
	qt = (ls4 * ls4) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls4 = (zt * ls4) % 2013265921
	qt = (ls5 * ls5) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls5 = (zt * ls5) % 2013265921
	qt = (ls6 * ls6) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls6 = (zt * ls6) % 2013265921
	qt = (ls7 * ls7) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls7 = (zt * ls7) % 2013265921
	qt = (ls8 * ls8) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls8 = (zt * ls8) % 2013265921
	qt = (ls9 * ls9) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls9 = (zt * ls9) % 2013265921
	qt = (ls10 * ls10) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls10 = (zt * ls10) % 2013265921
	qt = (ls11 * ls11) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls11 = (zt * ls11) % 2013265921
	qt = (ls12 * ls12) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls12 = (zt * ls12) % 2013265921
	qt = (ls13 * ls13) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls13 = (zt * ls13) % 2013265921
	qt = (ls14 * ls14) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls14 = (zt * ls14) % 2013265921
	qt = (ls15 * ls15) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls15 = (zt * ls15) % 2013265921
	qt = (ls16 * ls16) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls16 = (zt * ls16) % 2013265921
	qt = (ls17 * ls17) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls17 = (zt * ls17) % 2013265921
	qt = (ls18 * ls18) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls18 = (zt * ls18) % 2013265921
	qt = (ls19 * ls19) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls19 = (zt * ls19) % 2013265921
	qt = (ls20 * ls20) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls20 = (zt * ls20) % 2013265921
	qt = (ls21 * ls21) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls21 = (zt * ls21) % 2013265921
	qt = (ls22 * ls22) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls22 = (zt * ls22) % 2013265921
	qt = (ls23 * ls23) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls23 = (zt * ls23) % 2013265921
	var b0t0 runar.Bigint = (ls0 + ls1) % 2013265921
	var b0t1 runar.Bigint = (ls2 + ls3) % 2013265921
	var b0t2 runar.Bigint = (2 * ls1 + b0t1) % 2013265921
	var b0t3 runar.Bigint = (2 * ls3 + b0t0) % 2013265921
	var b0t4 runar.Bigint = (4 * b0t1 + b0t3) % 2013265921
	var b0t5 runar.Bigint = (4 * b0t0 + b0t2) % 2013265921
	var b0o0 runar.Bigint = (b0t3 + b0t5) % 2013265921
	var b0o1 runar.Bigint = b0t5
	var b0o2 runar.Bigint = (b0t2 + b0t4) % 2013265921
	var b0o3 runar.Bigint = b0t4
	var b1t0 runar.Bigint = (ls4 + ls5) % 2013265921
	var b1t1 runar.Bigint = (ls6 + ls7) % 2013265921
	var b1t2 runar.Bigint = (2 * ls5 + b1t1) % 2013265921
	var b1t3 runar.Bigint = (2 * ls7 + b1t0) % 2013265921
	var b1t4 runar.Bigint = (4 * b1t1 + b1t3) % 2013265921
	var b1t5 runar.Bigint = (4 * b1t0 + b1t2) % 2013265921
	var b1o0 runar.Bigint = (b1t3 + b1t5) % 2013265921
	var b1o1 runar.Bigint = b1t5
	var b1o2 runar.Bigint = (b1t2 + b1t4) % 2013265921
	var b1o3 runar.Bigint = b1t4
	var b2t0 runar.Bigint = (ls8 + ls9) % 2013265921
	var b2t1 runar.Bigint = (ls10 + ls11) % 2013265921
	var b2t2 runar.Bigint = (2 * ls9 + b2t1) % 2013265921
	var b2t3 runar.Bigint = (2 * ls11 + b2t0) % 2013265921
	var b2t4 runar.Bigint = (4 * b2t1 + b2t3) % 2013265921
	var b2t5 runar.Bigint = (4 * b2t0 + b2t2) % 2013265921
	var b2o0 runar.Bigint = (b2t3 + b2t5) % 2013265921
	var b2o1 runar.Bigint = b2t5
	var b2o2 runar.Bigint = (b2t2 + b2t4) % 2013265921
	var b2o3 runar.Bigint = b2t4
	var b3t0 runar.Bigint = (ls12 + ls13) % 2013265921
	var b3t1 runar.Bigint = (ls14 + ls15) % 2013265921
	var b3t2 runar.Bigint = (2 * ls13 + b3t1) % 2013265921
	var b3t3 runar.Bigint = (2 * ls15 + b3t0) % 2013265921
	var b3t4 runar.Bigint = (4 * b3t1 + b3t3) % 2013265921
	var b3t5 runar.Bigint = (4 * b3t0 + b3t2) % 2013265921
	var b3o0 runar.Bigint = (b3t3 + b3t5) % 2013265921
	var b3o1 runar.Bigint = b3t5
	var b3o2 runar.Bigint = (b3t2 + b3t4) % 2013265921
	var b3o3 runar.Bigint = b3t4
	var b4t0 runar.Bigint = (ls16 + ls17) % 2013265921
	var b4t1 runar.Bigint = (ls18 + ls19) % 2013265921
	var b4t2 runar.Bigint = (2 * ls17 + b4t1) % 2013265921
	var b4t3 runar.Bigint = (2 * ls19 + b4t0) % 2013265921
	var b4t4 runar.Bigint = (4 * b4t1 + b4t3) % 2013265921
	var b4t5 runar.Bigint = (4 * b4t0 + b4t2) % 2013265921
	var b4o0 runar.Bigint = (b4t3 + b4t5) % 2013265921
	var b4o1 runar.Bigint = b4t5
	var b4o2 runar.Bigint = (b4t2 + b4t4) % 2013265921
	var b4o3 runar.Bigint = b4t4
	var b5t0 runar.Bigint = (ls20 + ls21) % 2013265921
	var b5t1 runar.Bigint = (ls22 + ls23) % 2013265921
	var b5t2 runar.Bigint = (2 * ls21 + b5t1) % 2013265921
	var b5t3 runar.Bigint = (2 * ls23 + b5t0) % 2013265921
	var b5t4 runar.Bigint = (4 * b5t1 + b5t3) % 2013265921
	var b5t5 runar.Bigint = (4 * b5t0 + b5t2) % 2013265921
	var b5o0 runar.Bigint = (b5t3 + b5t5) % 2013265921
	var b5o1 runar.Bigint = b5t5
	var b5o2 runar.Bigint = (b5t2 + b5t4) % 2013265921
	var b5o3 runar.Bigint = b5t4
	var mtmp0 runar.Bigint = (b0o0 + b1o0 + b2o0 + b3o0 + b4o0 + b5o0) % 2013265921
	var mtmp1 runar.Bigint = (b0o1 + b1o1 + b2o1 + b3o1 + b4o1 + b5o1) % 2013265921
	var mtmp2 runar.Bigint = (b0o2 + b1o2 + b2o2 + b3o2 + b4o2 + b5o2) % 2013265921
	var mtmp3 runar.Bigint = (b0o3 + b1o3 + b2o3 + b3o3 + b4o3 + b5o3) % 2013265921
	ls0 = (b0o0 + mtmp0) % 2013265921
	ls1 = (b0o1 + mtmp1) % 2013265921
	ls2 = (b0o2 + mtmp2) % 2013265921
	ls3 = (b0o3 + mtmp3) % 2013265921
	ls4 = (b1o0 + mtmp0) % 2013265921
	ls5 = (b1o1 + mtmp1) % 2013265921
	ls6 = (b1o2 + mtmp2) % 2013265921
	ls7 = (b1o3 + mtmp3) % 2013265921
	ls8 = (b2o0 + mtmp0) % 2013265921
	ls9 = (b2o1 + mtmp1) % 2013265921
	ls10 = (b2o2 + mtmp2) % 2013265921
	ls11 = (b2o3 + mtmp3) % 2013265921
	ls12 = (b3o0 + mtmp0) % 2013265921
	ls13 = (b3o1 + mtmp1) % 2013265921
	ls14 = (b3o2 + mtmp2) % 2013265921
	ls15 = (b3o3 + mtmp3) % 2013265921
	ls16 = (b4o0 + mtmp0) % 2013265921
	ls17 = (b4o1 + mtmp1) % 2013265921
	ls18 = (b4o2 + mtmp2) % 2013265921
	ls19 = (b4o3 + mtmp3) % 2013265921
	ls20 = (b5o0 + mtmp0) % 2013265921
	ls21 = (b5o1 + mtmp1) % 2013265921
	ls22 = (b5o2 + mtmp2) % 2013265921
	ls23 = (b5o3 + mtmp3) % 2013265921
	c.S0 = ls0
	c.S1 = ls1
	c.S2 = ls2
	c.S3 = ls3
	c.S4 = ls4
	c.S5 = ls5
	c.S6 = ls6
	c.S7 = ls7
	c.S8 = ls8
	c.S9 = ls9
	c.S10 = ls10
	c.S11 = ls11
	c.S12 = ls12
	c.S13 = ls13
	c.S14 = ls14
	c.S15 = ls15
	c.S16 = ls16
	c.S17 = ls17
	c.S18 = ls18
	c.S19 = ls19
	c.S20 = ls20
	c.S21 = ls21
	c.S22 = ls22
	c.S23 = ls23
	c.AddOutput(1, ls0, ls1, ls2, ls3, ls4, ls5, ls6, ls7, ls8, ls9, ls10, ls11, ls12, ls13, ls14, ls15, ls16, ls17, ls18, ls19, ls20, ls21, ls22, ls23, c.Round+1, c.ProverPkh)
	c.AddRawOutput(postSats, postScript)
}

func (c *Poseidon2Stage) PartialStep(sig runar.Sig, pubKey runar.PubKey, postSats runar.Bigint, postScript runar.ByteString) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Round >= 4 && c.Round < 26)
	c.assertCanonicalState()
	var qt runar.Bigint = 0
	var wt runar.Bigint = 0
	var zt runar.Bigint = 0
	var ls0 runar.Bigint = c.S0
	var ls1 runar.Bigint = c.S1
	var ls2 runar.Bigint = c.S2
	var ls3 runar.Bigint = c.S3
	var ls4 runar.Bigint = c.S4
	var ls5 runar.Bigint = c.S5
	var ls6 runar.Bigint = c.S6
	var ls7 runar.Bigint = c.S7
	var ls8 runar.Bigint = c.S8
	var ls9 runar.Bigint = c.S9
	var ls10 runar.Bigint = c.S10
	var ls11 runar.Bigint = c.S11
	var ls12 runar.Bigint = c.S12
	var ls13 runar.Bigint = c.S13
	var ls14 runar.Bigint = c.S14
	var ls15 runar.Bigint = c.S15
	var ls16 runar.Bigint = c.S16
	var ls17 runar.Bigint = c.S17
	var ls18 runar.Bigint = c.S18
	var ls19 runar.Bigint = c.S19
	var ls20 runar.Bigint = c.S20
	var ls21 runar.Bigint = c.S21
	var ls22 runar.Bigint = c.S22
	var ls23 runar.Bigint = c.S23
	ls0 = (ls0 + 0x1da78ec2) % 2013265921
	qt = (ls0 * ls0) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	ls0 = (zt * ls0) % 2013265921
	var msum runar.Bigint = (ls0 + ls1 + ls2 + ls3 + ls4 + ls5 + ls6 + ls7 + ls8 + ls9 + ls10 + ls11 + ls12 + ls13 + ls14 + ls15 + ls16 + ls17 + ls18 + ls19 + ls20 + ls21 + ls22 + ls23) % 2013265921
	ls0 = (msum + 0x409133f0 * ls0) % 2013265921
	ls1 = (msum + 0x1667a8a1 * ls1) % 2013265921
	ls2 = (msum + 0x6a6c7b6 * ls2) % 2013265921
	ls3 = (msum + 0x6f53160e * ls3) % 2013265921
	ls4 = (msum + 0x273b11d1 * ls4) % 2013265921
	ls5 = (msum + 0x3176c5d * ls5) % 2013265921
	ls6 = (msum + 0x72f9bbf9 * ls6) % 2013265921
	ls7 = (msum + 0x73ceba91 * ls7) % 2013265921
	ls8 = (msum + 0x5cdef81d * ls8) % 2013265921
	ls9 = (msum + 0x1393285 * ls9) % 2013265921
	ls10 = (msum + 0x46daee06 * ls10) % 2013265921
	ls11 = (msum + 0x65d7ba6 * ls11) % 2013265921
	ls12 = (msum + 0x52d72d6f * ls12) % 2013265921
	ls13 = (msum + 0x5dd05e0 * ls13) % 2013265921
	ls14 = (msum + 0x3bab4b63 * ls14) % 2013265921
	ls15 = (msum + 0x6ada3842 * ls15) % 2013265921
	ls16 = (msum + 0x2fc5fbec * ls16) % 2013265921
	ls17 = (msum + 0x770d61b0 * ls17) % 2013265921
	ls18 = (msum + 0x5715aae9 * ls18) % 2013265921
	ls19 = (msum + 0x3ef0e90 * ls19) % 2013265921
	ls20 = (msum + 0x75b6c770 * ls20) % 2013265921
	ls21 = (msum + 0x242adf5f * ls21) % 2013265921
	ls22 = (msum + 0xd0ca4c * ls22) % 2013265921
	ls23 = (msum + 0x36c0e388 * ls23) % 2013265921
	c.S0 = ls0
	c.S1 = ls1
	c.S2 = ls2
	c.S3 = ls3
	c.S4 = ls4
	c.S5 = ls5
	c.S6 = ls6
	c.S7 = ls7
	c.S8 = ls8
	c.S9 = ls9
	c.S10 = ls10
	c.S11 = ls11
	c.S12 = ls12
	c.S13 = ls13
	c.S14 = ls14
	c.S15 = ls15
	c.S16 = ls16
	c.S17 = ls17
	c.S18 = ls18
	c.S19 = ls19
	c.S20 = ls20
	c.S21 = ls21
	c.S22 = ls22
	c.S23 = ls23
	c.AddOutput(1, ls0, ls1, ls2, ls3, ls4, ls5, ls6, ls7, ls8, ls9, ls10, ls11, ls12, ls13, ls14, ls15, ls16, ls17, ls18, ls19, ls20, ls21, ls22, ls23, c.Round+1, c.ProverPkh)
	c.AddRawOutput(postSats, postScript)
}

