// Poseidon2Blocks — GOAL-v9 Stage 0c: per-component measurements of the
// risc0-zkp 3.0.5 poseidon2_mix building blocks. Generated file.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type Poseidon2Blocks struct {
	runar.SmartContract
}

func (c *Poseidon2Blocks) MExt(x0 runar.Bigint, x1 runar.Bigint, x2 runar.Bigint, x3 runar.Bigint, x4 runar.Bigint, x5 runar.Bigint, x6 runar.Bigint, x7 runar.Bigint, x8 runar.Bigint, x9 runar.Bigint, x10 runar.Bigint, x11 runar.Bigint, x12 runar.Bigint, x13 runar.Bigint, x14 runar.Bigint, x15 runar.Bigint, x16 runar.Bigint, x17 runar.Bigint, x18 runar.Bigint, x19 runar.Bigint, x20 runar.Bigint, x21 runar.Bigint, x22 runar.Bigint, x23 runar.Bigint, e0 runar.Bigint, e1 runar.Bigint, e2 runar.Bigint, e3 runar.Bigint, e4 runar.Bigint, e5 runar.Bigint, e6 runar.Bigint, e7 runar.Bigint) {
	runar.Assert(x0 >= 0 && x0 < 2013265921 && x1 >= 0 && x1 < 2013265921 && x2 >= 0 && x2 < 2013265921 && x3 >= 0 && x3 < 2013265921)
	runar.Assert(x4 >= 0 && x4 < 2013265921 && x5 >= 0 && x5 < 2013265921 && x6 >= 0 && x6 < 2013265921 && x7 >= 0 && x7 < 2013265921)
	runar.Assert(x8 >= 0 && x8 < 2013265921 && x9 >= 0 && x9 < 2013265921 && x10 >= 0 && x10 < 2013265921 && x11 >= 0 && x11 < 2013265921)
	runar.Assert(x12 >= 0 && x12 < 2013265921 && x13 >= 0 && x13 < 2013265921 && x14 >= 0 && x14 < 2013265921 && x15 >= 0 && x15 < 2013265921)
	runar.Assert(x16 >= 0 && x16 < 2013265921 && x17 >= 0 && x17 < 2013265921 && x18 >= 0 && x18 < 2013265921 && x19 >= 0 && x19 < 2013265921)
	runar.Assert(x20 >= 0 && x20 < 2013265921 && x21 >= 0 && x21 < 2013265921 && x22 >= 0 && x22 < 2013265921 && x23 >= 0 && x23 < 2013265921)
	runar.Assert(e0 >= 0 && e0 < 2013265921 && e1 >= 0 && e1 < 2013265921 && e2 >= 0 && e2 < 2013265921 && e3 >= 0 && e3 < 2013265921)
	runar.Assert(e4 >= 0 && e4 < 2013265921 && e5 >= 0 && e5 < 2013265921 && e6 >= 0 && e6 < 2013265921 && e7 >= 0 && e7 < 2013265921)
	var s0 runar.Bigint = x0
	var s1 runar.Bigint = x1
	var s2 runar.Bigint = x2
	var s3 runar.Bigint = x3
	var s4 runar.Bigint = x4
	var s5 runar.Bigint = x5
	var s6 runar.Bigint = x6
	var s7 runar.Bigint = x7
	var s8 runar.Bigint = x8
	var s9 runar.Bigint = x9
	var s10 runar.Bigint = x10
	var s11 runar.Bigint = x11
	var s12 runar.Bigint = x12
	var s13 runar.Bigint = x13
	var s14 runar.Bigint = x14
	var s15 runar.Bigint = x15
	var s16 runar.Bigint = x16
	var s17 runar.Bigint = x17
	var s18 runar.Bigint = x18
	var s19 runar.Bigint = x19
	var s20 runar.Bigint = x20
	var s21 runar.Bigint = x21
	var s22 runar.Bigint = x22
	var s23 runar.Bigint = x23
	var b0t0 runar.Bigint = (s0 + s1) % 2013265921
	var b0t1 runar.Bigint = (s2 + s3) % 2013265921
	var b0t2 runar.Bigint = (2 * s1 + b0t1) % 2013265921
	var b0t3 runar.Bigint = (2 * s3 + b0t0) % 2013265921
	var b0t4 runar.Bigint = (4 * b0t1 + b0t3) % 2013265921
	var b0t5 runar.Bigint = (4 * b0t0 + b0t2) % 2013265921
	var b0o0 runar.Bigint = (b0t3 + b0t5) % 2013265921
	var b0o1 runar.Bigint = b0t5
	var b0o2 runar.Bigint = (b0t2 + b0t4) % 2013265921
	var b0o3 runar.Bigint = b0t4
	var b1t0 runar.Bigint = (s4 + s5) % 2013265921
	var b1t1 runar.Bigint = (s6 + s7) % 2013265921
	var b1t2 runar.Bigint = (2 * s5 + b1t1) % 2013265921
	var b1t3 runar.Bigint = (2 * s7 + b1t0) % 2013265921
	var b1t4 runar.Bigint = (4 * b1t1 + b1t3) % 2013265921
	var b1t5 runar.Bigint = (4 * b1t0 + b1t2) % 2013265921
	var b1o0 runar.Bigint = (b1t3 + b1t5) % 2013265921
	var b1o1 runar.Bigint = b1t5
	var b1o2 runar.Bigint = (b1t2 + b1t4) % 2013265921
	var b1o3 runar.Bigint = b1t4
	var b2t0 runar.Bigint = (s8 + s9) % 2013265921
	var b2t1 runar.Bigint = (s10 + s11) % 2013265921
	var b2t2 runar.Bigint = (2 * s9 + b2t1) % 2013265921
	var b2t3 runar.Bigint = (2 * s11 + b2t0) % 2013265921
	var b2t4 runar.Bigint = (4 * b2t1 + b2t3) % 2013265921
	var b2t5 runar.Bigint = (4 * b2t0 + b2t2) % 2013265921
	var b2o0 runar.Bigint = (b2t3 + b2t5) % 2013265921
	var b2o1 runar.Bigint = b2t5
	var b2o2 runar.Bigint = (b2t2 + b2t4) % 2013265921
	var b2o3 runar.Bigint = b2t4
	var b3t0 runar.Bigint = (s12 + s13) % 2013265921
	var b3t1 runar.Bigint = (s14 + s15) % 2013265921
	var b3t2 runar.Bigint = (2 * s13 + b3t1) % 2013265921
	var b3t3 runar.Bigint = (2 * s15 + b3t0) % 2013265921
	var b3t4 runar.Bigint = (4 * b3t1 + b3t3) % 2013265921
	var b3t5 runar.Bigint = (4 * b3t0 + b3t2) % 2013265921
	var b3o0 runar.Bigint = (b3t3 + b3t5) % 2013265921
	var b3o1 runar.Bigint = b3t5
	var b3o2 runar.Bigint = (b3t2 + b3t4) % 2013265921
	var b3o3 runar.Bigint = b3t4
	var b4t0 runar.Bigint = (s16 + s17) % 2013265921
	var b4t1 runar.Bigint = (s18 + s19) % 2013265921
	var b4t2 runar.Bigint = (2 * s17 + b4t1) % 2013265921
	var b4t3 runar.Bigint = (2 * s19 + b4t0) % 2013265921
	var b4t4 runar.Bigint = (4 * b4t1 + b4t3) % 2013265921
	var b4t5 runar.Bigint = (4 * b4t0 + b4t2) % 2013265921
	var b4o0 runar.Bigint = (b4t3 + b4t5) % 2013265921
	var b4o1 runar.Bigint = b4t5
	var b4o2 runar.Bigint = (b4t2 + b4t4) % 2013265921
	var b4o3 runar.Bigint = b4t4
	var b5t0 runar.Bigint = (s20 + s21) % 2013265921
	var b5t1 runar.Bigint = (s22 + s23) % 2013265921
	var b5t2 runar.Bigint = (2 * s21 + b5t1) % 2013265921
	var b5t3 runar.Bigint = (2 * s23 + b5t0) % 2013265921
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
	s0 = (b0o0 + mtmp0) % 2013265921
	s1 = (b0o1 + mtmp1) % 2013265921
	s2 = (b0o2 + mtmp2) % 2013265921
	s3 = (b0o3 + mtmp3) % 2013265921
	s4 = (b1o0 + mtmp0) % 2013265921
	s5 = (b1o1 + mtmp1) % 2013265921
	s6 = (b1o2 + mtmp2) % 2013265921
	s7 = (b1o3 + mtmp3) % 2013265921
	s8 = (b2o0 + mtmp0) % 2013265921
	s9 = (b2o1 + mtmp1) % 2013265921
	s10 = (b2o2 + mtmp2) % 2013265921
	s11 = (b2o3 + mtmp3) % 2013265921
	s12 = (b3o0 + mtmp0) % 2013265921
	s13 = (b3o1 + mtmp1) % 2013265921
	s14 = (b3o2 + mtmp2) % 2013265921
	s15 = (b3o3 + mtmp3) % 2013265921
	s16 = (b4o0 + mtmp0) % 2013265921
	s17 = (b4o1 + mtmp1) % 2013265921
	s18 = (b4o2 + mtmp2) % 2013265921
	s19 = (b4o3 + mtmp3) % 2013265921
	s20 = (b5o0 + mtmp0) % 2013265921
	s21 = (b5o1 + mtmp1) % 2013265921
	s22 = (b5o2 + mtmp2) % 2013265921
	s23 = (b5o3 + mtmp3) % 2013265921
	runar.Assert(s0 == e0 && s1 == e1 && s2 == e2 && s3 == e3 && s4 == e4 && s5 == e5 && s6 == e6 && s7 == e7)
}

func (c *Poseidon2Blocks) MInt(x0 runar.Bigint, x1 runar.Bigint, x2 runar.Bigint, x3 runar.Bigint, x4 runar.Bigint, x5 runar.Bigint, x6 runar.Bigint, x7 runar.Bigint, x8 runar.Bigint, x9 runar.Bigint, x10 runar.Bigint, x11 runar.Bigint, x12 runar.Bigint, x13 runar.Bigint, x14 runar.Bigint, x15 runar.Bigint, x16 runar.Bigint, x17 runar.Bigint, x18 runar.Bigint, x19 runar.Bigint, x20 runar.Bigint, x21 runar.Bigint, x22 runar.Bigint, x23 runar.Bigint, e0 runar.Bigint, e1 runar.Bigint, e2 runar.Bigint, e3 runar.Bigint, e4 runar.Bigint, e5 runar.Bigint, e6 runar.Bigint, e7 runar.Bigint) {
	runar.Assert(x0 >= 0 && x0 < 2013265921 && x1 >= 0 && x1 < 2013265921 && x2 >= 0 && x2 < 2013265921 && x3 >= 0 && x3 < 2013265921)
	runar.Assert(x4 >= 0 && x4 < 2013265921 && x5 >= 0 && x5 < 2013265921 && x6 >= 0 && x6 < 2013265921 && x7 >= 0 && x7 < 2013265921)
	runar.Assert(x8 >= 0 && x8 < 2013265921 && x9 >= 0 && x9 < 2013265921 && x10 >= 0 && x10 < 2013265921 && x11 >= 0 && x11 < 2013265921)
	runar.Assert(x12 >= 0 && x12 < 2013265921 && x13 >= 0 && x13 < 2013265921 && x14 >= 0 && x14 < 2013265921 && x15 >= 0 && x15 < 2013265921)
	runar.Assert(x16 >= 0 && x16 < 2013265921 && x17 >= 0 && x17 < 2013265921 && x18 >= 0 && x18 < 2013265921 && x19 >= 0 && x19 < 2013265921)
	runar.Assert(x20 >= 0 && x20 < 2013265921 && x21 >= 0 && x21 < 2013265921 && x22 >= 0 && x22 < 2013265921 && x23 >= 0 && x23 < 2013265921)
	runar.Assert(e0 >= 0 && e0 < 2013265921 && e1 >= 0 && e1 < 2013265921 && e2 >= 0 && e2 < 2013265921 && e3 >= 0 && e3 < 2013265921)
	runar.Assert(e4 >= 0 && e4 < 2013265921 && e5 >= 0 && e5 < 2013265921 && e6 >= 0 && e6 < 2013265921 && e7 >= 0 && e7 < 2013265921)
	var s0 runar.Bigint = x0
	var s1 runar.Bigint = x1
	var s2 runar.Bigint = x2
	var s3 runar.Bigint = x3
	var s4 runar.Bigint = x4
	var s5 runar.Bigint = x5
	var s6 runar.Bigint = x6
	var s7 runar.Bigint = x7
	var s8 runar.Bigint = x8
	var s9 runar.Bigint = x9
	var s10 runar.Bigint = x10
	var s11 runar.Bigint = x11
	var s12 runar.Bigint = x12
	var s13 runar.Bigint = x13
	var s14 runar.Bigint = x14
	var s15 runar.Bigint = x15
	var s16 runar.Bigint = x16
	var s17 runar.Bigint = x17
	var s18 runar.Bigint = x18
	var s19 runar.Bigint = x19
	var s20 runar.Bigint = x20
	var s21 runar.Bigint = x21
	var s22 runar.Bigint = x22
	var s23 runar.Bigint = x23
	var msum runar.Bigint = (s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7 + s8 + s9 + s10 + s11 + s12 + s13 + s14 + s15 + s16 + s17 + s18 + s19 + s20 + s21 + s22 + s23) % 2013265921
	s0 = (msum + 0x409133f0 * s0) % 2013265921
	s1 = (msum + 0x1667a8a1 * s1) % 2013265921
	s2 = (msum + 0x6a6c7b6 * s2) % 2013265921
	s3 = (msum + 0x6f53160e * s3) % 2013265921
	s4 = (msum + 0x273b11d1 * s4) % 2013265921
	s5 = (msum + 0x3176c5d * s5) % 2013265921
	s6 = (msum + 0x72f9bbf9 * s6) % 2013265921
	s7 = (msum + 0x73ceba91 * s7) % 2013265921
	s8 = (msum + 0x5cdef81d * s8) % 2013265921
	s9 = (msum + 0x1393285 * s9) % 2013265921
	s10 = (msum + 0x46daee06 * s10) % 2013265921
	s11 = (msum + 0x65d7ba6 * s11) % 2013265921
	s12 = (msum + 0x52d72d6f * s12) % 2013265921
	s13 = (msum + 0x5dd05e0 * s13) % 2013265921
	s14 = (msum + 0x3bab4b63 * s14) % 2013265921
	s15 = (msum + 0x6ada3842 * s15) % 2013265921
	s16 = (msum + 0x2fc5fbec * s16) % 2013265921
	s17 = (msum + 0x770d61b0 * s17) % 2013265921
	s18 = (msum + 0x5715aae9 * s18) % 2013265921
	s19 = (msum + 0x3ef0e90 * s19) % 2013265921
	s20 = (msum + 0x75b6c770 * s20) % 2013265921
	s21 = (msum + 0x242adf5f * s21) % 2013265921
	s22 = (msum + 0xd0ca4c * s22) % 2013265921
	s23 = (msum + 0x36c0e388 * s23) % 2013265921
	runar.Assert(s0 == e0 && s1 == e1 && s2 == e2 && s3 == e3 && s4 == e4 && s5 == e5 && s6 == e6 && s7 == e7)
}

func (c *Poseidon2Blocks) SboxesFull(x0 runar.Bigint, x1 runar.Bigint, x2 runar.Bigint, x3 runar.Bigint, x4 runar.Bigint, x5 runar.Bigint, x6 runar.Bigint, x7 runar.Bigint, x8 runar.Bigint, x9 runar.Bigint, x10 runar.Bigint, x11 runar.Bigint, x12 runar.Bigint, x13 runar.Bigint, x14 runar.Bigint, x15 runar.Bigint, x16 runar.Bigint, x17 runar.Bigint, x18 runar.Bigint, x19 runar.Bigint, x20 runar.Bigint, x21 runar.Bigint, x22 runar.Bigint, x23 runar.Bigint, e0 runar.Bigint, e1 runar.Bigint, e2 runar.Bigint, e3 runar.Bigint, e4 runar.Bigint, e5 runar.Bigint, e6 runar.Bigint, e7 runar.Bigint) {
	var qt runar.Bigint = 0
	var wt runar.Bigint = 0
	var zt runar.Bigint = 0
	runar.Assert(x0 >= 0 && x0 < 2013265921 && x1 >= 0 && x1 < 2013265921 && x2 >= 0 && x2 < 2013265921 && x3 >= 0 && x3 < 2013265921)
	runar.Assert(x4 >= 0 && x4 < 2013265921 && x5 >= 0 && x5 < 2013265921 && x6 >= 0 && x6 < 2013265921 && x7 >= 0 && x7 < 2013265921)
	runar.Assert(x8 >= 0 && x8 < 2013265921 && x9 >= 0 && x9 < 2013265921 && x10 >= 0 && x10 < 2013265921 && x11 >= 0 && x11 < 2013265921)
	runar.Assert(x12 >= 0 && x12 < 2013265921 && x13 >= 0 && x13 < 2013265921 && x14 >= 0 && x14 < 2013265921 && x15 >= 0 && x15 < 2013265921)
	runar.Assert(x16 >= 0 && x16 < 2013265921 && x17 >= 0 && x17 < 2013265921 && x18 >= 0 && x18 < 2013265921 && x19 >= 0 && x19 < 2013265921)
	runar.Assert(x20 >= 0 && x20 < 2013265921 && x21 >= 0 && x21 < 2013265921 && x22 >= 0 && x22 < 2013265921 && x23 >= 0 && x23 < 2013265921)
	runar.Assert(e0 >= 0 && e0 < 2013265921 && e1 >= 0 && e1 < 2013265921 && e2 >= 0 && e2 < 2013265921 && e3 >= 0 && e3 < 2013265921)
	runar.Assert(e4 >= 0 && e4 < 2013265921 && e5 >= 0 && e5 < 2013265921 && e6 >= 0 && e6 < 2013265921 && e7 >= 0 && e7 < 2013265921)
	var s0 runar.Bigint = x0
	var s1 runar.Bigint = x1
	var s2 runar.Bigint = x2
	var s3 runar.Bigint = x3
	var s4 runar.Bigint = x4
	var s5 runar.Bigint = x5
	var s6 runar.Bigint = x6
	var s7 runar.Bigint = x7
	var s8 runar.Bigint = x8
	var s9 runar.Bigint = x9
	var s10 runar.Bigint = x10
	var s11 runar.Bigint = x11
	var s12 runar.Bigint = x12
	var s13 runar.Bigint = x13
	var s14 runar.Bigint = x14
	var s15 runar.Bigint = x15
	var s16 runar.Bigint = x16
	var s17 runar.Bigint = x17
	var s18 runar.Bigint = x18
	var s19 runar.Bigint = x19
	var s20 runar.Bigint = x20
	var s21 runar.Bigint = x21
	var s22 runar.Bigint = x22
	var s23 runar.Bigint = x23
	qt = (s0 * s0) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s0 = (zt * s0) % 2013265921
	qt = (s1 * s1) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s1 = (zt * s1) % 2013265921
	qt = (s2 * s2) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s2 = (zt * s2) % 2013265921
	qt = (s3 * s3) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s3 = (zt * s3) % 2013265921
	qt = (s4 * s4) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s4 = (zt * s4) % 2013265921
	qt = (s5 * s5) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s5 = (zt * s5) % 2013265921
	qt = (s6 * s6) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s6 = (zt * s6) % 2013265921
	qt = (s7 * s7) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s7 = (zt * s7) % 2013265921
	qt = (s8 * s8) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s8 = (zt * s8) % 2013265921
	qt = (s9 * s9) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s9 = (zt * s9) % 2013265921
	qt = (s10 * s10) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s10 = (zt * s10) % 2013265921
	qt = (s11 * s11) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s11 = (zt * s11) % 2013265921
	qt = (s12 * s12) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s12 = (zt * s12) % 2013265921
	qt = (s13 * s13) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s13 = (zt * s13) % 2013265921
	qt = (s14 * s14) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s14 = (zt * s14) % 2013265921
	qt = (s15 * s15) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s15 = (zt * s15) % 2013265921
	qt = (s16 * s16) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s16 = (zt * s16) % 2013265921
	qt = (s17 * s17) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s17 = (zt * s17) % 2013265921
	qt = (s18 * s18) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s18 = (zt * s18) % 2013265921
	qt = (s19 * s19) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s19 = (zt * s19) % 2013265921
	qt = (s20 * s20) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s20 = (zt * s20) % 2013265921
	qt = (s21 * s21) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s21 = (zt * s21) % 2013265921
	qt = (s22 * s22) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s22 = (zt * s22) % 2013265921
	qt = (s23 * s23) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s23 = (zt * s23) % 2013265921
	runar.Assert(s0 == e0 && s1 == e1 && s2 == e2 && s3 == e3 && s4 == e4 && s5 == e5 && s6 == e6 && s7 == e7)
}

func (c *Poseidon2Blocks) FullRound(x0 runar.Bigint, x1 runar.Bigint, x2 runar.Bigint, x3 runar.Bigint, x4 runar.Bigint, x5 runar.Bigint, x6 runar.Bigint, x7 runar.Bigint, x8 runar.Bigint, x9 runar.Bigint, x10 runar.Bigint, x11 runar.Bigint, x12 runar.Bigint, x13 runar.Bigint, x14 runar.Bigint, x15 runar.Bigint, x16 runar.Bigint, x17 runar.Bigint, x18 runar.Bigint, x19 runar.Bigint, x20 runar.Bigint, x21 runar.Bigint, x22 runar.Bigint, x23 runar.Bigint, e0 runar.Bigint, e1 runar.Bigint, e2 runar.Bigint, e3 runar.Bigint, e4 runar.Bigint, e5 runar.Bigint, e6 runar.Bigint, e7 runar.Bigint) {
	var qt runar.Bigint = 0
	var wt runar.Bigint = 0
	var zt runar.Bigint = 0
	runar.Assert(x0 >= 0 && x0 < 2013265921 && x1 >= 0 && x1 < 2013265921 && x2 >= 0 && x2 < 2013265921 && x3 >= 0 && x3 < 2013265921)
	runar.Assert(x4 >= 0 && x4 < 2013265921 && x5 >= 0 && x5 < 2013265921 && x6 >= 0 && x6 < 2013265921 && x7 >= 0 && x7 < 2013265921)
	runar.Assert(x8 >= 0 && x8 < 2013265921 && x9 >= 0 && x9 < 2013265921 && x10 >= 0 && x10 < 2013265921 && x11 >= 0 && x11 < 2013265921)
	runar.Assert(x12 >= 0 && x12 < 2013265921 && x13 >= 0 && x13 < 2013265921 && x14 >= 0 && x14 < 2013265921 && x15 >= 0 && x15 < 2013265921)
	runar.Assert(x16 >= 0 && x16 < 2013265921 && x17 >= 0 && x17 < 2013265921 && x18 >= 0 && x18 < 2013265921 && x19 >= 0 && x19 < 2013265921)
	runar.Assert(x20 >= 0 && x20 < 2013265921 && x21 >= 0 && x21 < 2013265921 && x22 >= 0 && x22 < 2013265921 && x23 >= 0 && x23 < 2013265921)
	runar.Assert(e0 >= 0 && e0 < 2013265921 && e1 >= 0 && e1 < 2013265921 && e2 >= 0 && e2 < 2013265921 && e3 >= 0 && e3 < 2013265921)
	runar.Assert(e4 >= 0 && e4 < 2013265921 && e5 >= 0 && e5 < 2013265921 && e6 >= 0 && e6 < 2013265921 && e7 >= 0 && e7 < 2013265921)
	var s0 runar.Bigint = x0
	var s1 runar.Bigint = x1
	var s2 runar.Bigint = x2
	var s3 runar.Bigint = x3
	var s4 runar.Bigint = x4
	var s5 runar.Bigint = x5
	var s6 runar.Bigint = x6
	var s7 runar.Bigint = x7
	var s8 runar.Bigint = x8
	var s9 runar.Bigint = x9
	var s10 runar.Bigint = x10
	var s11 runar.Bigint = x11
	var s12 runar.Bigint = x12
	var s13 runar.Bigint = x13
	var s14 runar.Bigint = x14
	var s15 runar.Bigint = x15
	var s16 runar.Bigint = x16
	var s17 runar.Bigint = x17
	var s18 runar.Bigint = x18
	var s19 runar.Bigint = x19
	var s20 runar.Bigint = x20
	var s21 runar.Bigint = x21
	var s22 runar.Bigint = x22
	var s23 runar.Bigint = x23
	s0 = (s0 + 0xfa20c37) % 2013265921
	s1 = (s1 + 0x795bb97) % 2013265921
	s2 = (s2 + 0x12c60b9c) % 2013265921
	s3 = (s3 + 0xeabd88e) % 2013265921
	s4 = (s4 + 0x96485ca) % 2013265921
	s5 = (s5 + 0x7093527) % 2013265921
	s6 = (s6 + 0x1b1d4e50) % 2013265921
	s7 = (s7 + 0x30a01ace) % 2013265921
	s8 = (s8 + 0x3bd86f5a) % 2013265921
	s9 = (s9 + 0x69af7c28) % 2013265921
	s10 = (s10 + 0x3f94775f) % 2013265921
	s11 = (s11 + 0x731560e8) % 2013265921
	s12 = (s12 + 0x465a0ecd) % 2013265921
	s13 = (s13 + 0x574ef807) % 2013265921
	s14 = (s14 + 0x62fd4870) % 2013265921
	s15 = (s15 + 0x52ccfe44) % 2013265921
	s16 = (s16 + 0x14772b14) % 2013265921
	s17 = (s17 + 0x4dedf371) % 2013265921
	s18 = (s18 + 0x260acd7c) % 2013265921
	s19 = (s19 + 0x1f51dc58) % 2013265921
	s20 = (s20 + 0x75125532) % 2013265921
	s21 = (s21 + 0x686a4d7b) % 2013265921
	s22 = (s22 + 0x54bac179) % 2013265921
	s23 = (s23 + 0x31947706) % 2013265921
	qt = (s0 * s0) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s0 = (zt * s0) % 2013265921
	qt = (s1 * s1) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s1 = (zt * s1) % 2013265921
	qt = (s2 * s2) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s2 = (zt * s2) % 2013265921
	qt = (s3 * s3) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s3 = (zt * s3) % 2013265921
	qt = (s4 * s4) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s4 = (zt * s4) % 2013265921
	qt = (s5 * s5) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s5 = (zt * s5) % 2013265921
	qt = (s6 * s6) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s6 = (zt * s6) % 2013265921
	qt = (s7 * s7) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s7 = (zt * s7) % 2013265921
	qt = (s8 * s8) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s8 = (zt * s8) % 2013265921
	qt = (s9 * s9) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s9 = (zt * s9) % 2013265921
	qt = (s10 * s10) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s10 = (zt * s10) % 2013265921
	qt = (s11 * s11) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s11 = (zt * s11) % 2013265921
	qt = (s12 * s12) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s12 = (zt * s12) % 2013265921
	qt = (s13 * s13) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s13 = (zt * s13) % 2013265921
	qt = (s14 * s14) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s14 = (zt * s14) % 2013265921
	qt = (s15 * s15) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s15 = (zt * s15) % 2013265921
	qt = (s16 * s16) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s16 = (zt * s16) % 2013265921
	qt = (s17 * s17) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s17 = (zt * s17) % 2013265921
	qt = (s18 * s18) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s18 = (zt * s18) % 2013265921
	qt = (s19 * s19) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s19 = (zt * s19) % 2013265921
	qt = (s20 * s20) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s20 = (zt * s20) % 2013265921
	qt = (s21 * s21) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s21 = (zt * s21) % 2013265921
	qt = (s22 * s22) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s22 = (zt * s22) % 2013265921
	qt = (s23 * s23) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s23 = (zt * s23) % 2013265921
	var b0t0 runar.Bigint = (s0 + s1) % 2013265921
	var b0t1 runar.Bigint = (s2 + s3) % 2013265921
	var b0t2 runar.Bigint = (2 * s1 + b0t1) % 2013265921
	var b0t3 runar.Bigint = (2 * s3 + b0t0) % 2013265921
	var b0t4 runar.Bigint = (4 * b0t1 + b0t3) % 2013265921
	var b0t5 runar.Bigint = (4 * b0t0 + b0t2) % 2013265921
	var b0o0 runar.Bigint = (b0t3 + b0t5) % 2013265921
	var b0o1 runar.Bigint = b0t5
	var b0o2 runar.Bigint = (b0t2 + b0t4) % 2013265921
	var b0o3 runar.Bigint = b0t4
	var b1t0 runar.Bigint = (s4 + s5) % 2013265921
	var b1t1 runar.Bigint = (s6 + s7) % 2013265921
	var b1t2 runar.Bigint = (2 * s5 + b1t1) % 2013265921
	var b1t3 runar.Bigint = (2 * s7 + b1t0) % 2013265921
	var b1t4 runar.Bigint = (4 * b1t1 + b1t3) % 2013265921
	var b1t5 runar.Bigint = (4 * b1t0 + b1t2) % 2013265921
	var b1o0 runar.Bigint = (b1t3 + b1t5) % 2013265921
	var b1o1 runar.Bigint = b1t5
	var b1o2 runar.Bigint = (b1t2 + b1t4) % 2013265921
	var b1o3 runar.Bigint = b1t4
	var b2t0 runar.Bigint = (s8 + s9) % 2013265921
	var b2t1 runar.Bigint = (s10 + s11) % 2013265921
	var b2t2 runar.Bigint = (2 * s9 + b2t1) % 2013265921
	var b2t3 runar.Bigint = (2 * s11 + b2t0) % 2013265921
	var b2t4 runar.Bigint = (4 * b2t1 + b2t3) % 2013265921
	var b2t5 runar.Bigint = (4 * b2t0 + b2t2) % 2013265921
	var b2o0 runar.Bigint = (b2t3 + b2t5) % 2013265921
	var b2o1 runar.Bigint = b2t5
	var b2o2 runar.Bigint = (b2t2 + b2t4) % 2013265921
	var b2o3 runar.Bigint = b2t4
	var b3t0 runar.Bigint = (s12 + s13) % 2013265921
	var b3t1 runar.Bigint = (s14 + s15) % 2013265921
	var b3t2 runar.Bigint = (2 * s13 + b3t1) % 2013265921
	var b3t3 runar.Bigint = (2 * s15 + b3t0) % 2013265921
	var b3t4 runar.Bigint = (4 * b3t1 + b3t3) % 2013265921
	var b3t5 runar.Bigint = (4 * b3t0 + b3t2) % 2013265921
	var b3o0 runar.Bigint = (b3t3 + b3t5) % 2013265921
	var b3o1 runar.Bigint = b3t5
	var b3o2 runar.Bigint = (b3t2 + b3t4) % 2013265921
	var b3o3 runar.Bigint = b3t4
	var b4t0 runar.Bigint = (s16 + s17) % 2013265921
	var b4t1 runar.Bigint = (s18 + s19) % 2013265921
	var b4t2 runar.Bigint = (2 * s17 + b4t1) % 2013265921
	var b4t3 runar.Bigint = (2 * s19 + b4t0) % 2013265921
	var b4t4 runar.Bigint = (4 * b4t1 + b4t3) % 2013265921
	var b4t5 runar.Bigint = (4 * b4t0 + b4t2) % 2013265921
	var b4o0 runar.Bigint = (b4t3 + b4t5) % 2013265921
	var b4o1 runar.Bigint = b4t5
	var b4o2 runar.Bigint = (b4t2 + b4t4) % 2013265921
	var b4o3 runar.Bigint = b4t4
	var b5t0 runar.Bigint = (s20 + s21) % 2013265921
	var b5t1 runar.Bigint = (s22 + s23) % 2013265921
	var b5t2 runar.Bigint = (2 * s21 + b5t1) % 2013265921
	var b5t3 runar.Bigint = (2 * s23 + b5t0) % 2013265921
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
	s0 = (b0o0 + mtmp0) % 2013265921
	s1 = (b0o1 + mtmp1) % 2013265921
	s2 = (b0o2 + mtmp2) % 2013265921
	s3 = (b0o3 + mtmp3) % 2013265921
	s4 = (b1o0 + mtmp0) % 2013265921
	s5 = (b1o1 + mtmp1) % 2013265921
	s6 = (b1o2 + mtmp2) % 2013265921
	s7 = (b1o3 + mtmp3) % 2013265921
	s8 = (b2o0 + mtmp0) % 2013265921
	s9 = (b2o1 + mtmp1) % 2013265921
	s10 = (b2o2 + mtmp2) % 2013265921
	s11 = (b2o3 + mtmp3) % 2013265921
	s12 = (b3o0 + mtmp0) % 2013265921
	s13 = (b3o1 + mtmp1) % 2013265921
	s14 = (b3o2 + mtmp2) % 2013265921
	s15 = (b3o3 + mtmp3) % 2013265921
	s16 = (b4o0 + mtmp0) % 2013265921
	s17 = (b4o1 + mtmp1) % 2013265921
	s18 = (b4o2 + mtmp2) % 2013265921
	s19 = (b4o3 + mtmp3) % 2013265921
	s20 = (b5o0 + mtmp0) % 2013265921
	s21 = (b5o1 + mtmp1) % 2013265921
	s22 = (b5o2 + mtmp2) % 2013265921
	s23 = (b5o3 + mtmp3) % 2013265921
	runar.Assert(s0 == e0 && s1 == e1 && s2 == e2 && s3 == e3 && s4 == e4 && s5 == e5 && s6 == e6 && s7 == e7)
}

func (c *Poseidon2Blocks) PartialRound(x0 runar.Bigint, x1 runar.Bigint, x2 runar.Bigint, x3 runar.Bigint, x4 runar.Bigint, x5 runar.Bigint, x6 runar.Bigint, x7 runar.Bigint, x8 runar.Bigint, x9 runar.Bigint, x10 runar.Bigint, x11 runar.Bigint, x12 runar.Bigint, x13 runar.Bigint, x14 runar.Bigint, x15 runar.Bigint, x16 runar.Bigint, x17 runar.Bigint, x18 runar.Bigint, x19 runar.Bigint, x20 runar.Bigint, x21 runar.Bigint, x22 runar.Bigint, x23 runar.Bigint, e0 runar.Bigint, e1 runar.Bigint, e2 runar.Bigint, e3 runar.Bigint, e4 runar.Bigint, e5 runar.Bigint, e6 runar.Bigint, e7 runar.Bigint) {
	var qt runar.Bigint = 0
	var wt runar.Bigint = 0
	var zt runar.Bigint = 0
	runar.Assert(x0 >= 0 && x0 < 2013265921 && x1 >= 0 && x1 < 2013265921 && x2 >= 0 && x2 < 2013265921 && x3 >= 0 && x3 < 2013265921)
	runar.Assert(x4 >= 0 && x4 < 2013265921 && x5 >= 0 && x5 < 2013265921 && x6 >= 0 && x6 < 2013265921 && x7 >= 0 && x7 < 2013265921)
	runar.Assert(x8 >= 0 && x8 < 2013265921 && x9 >= 0 && x9 < 2013265921 && x10 >= 0 && x10 < 2013265921 && x11 >= 0 && x11 < 2013265921)
	runar.Assert(x12 >= 0 && x12 < 2013265921 && x13 >= 0 && x13 < 2013265921 && x14 >= 0 && x14 < 2013265921 && x15 >= 0 && x15 < 2013265921)
	runar.Assert(x16 >= 0 && x16 < 2013265921 && x17 >= 0 && x17 < 2013265921 && x18 >= 0 && x18 < 2013265921 && x19 >= 0 && x19 < 2013265921)
	runar.Assert(x20 >= 0 && x20 < 2013265921 && x21 >= 0 && x21 < 2013265921 && x22 >= 0 && x22 < 2013265921 && x23 >= 0 && x23 < 2013265921)
	runar.Assert(e0 >= 0 && e0 < 2013265921 && e1 >= 0 && e1 < 2013265921 && e2 >= 0 && e2 < 2013265921 && e3 >= 0 && e3 < 2013265921)
	runar.Assert(e4 >= 0 && e4 < 2013265921 && e5 >= 0 && e5 < 2013265921 && e6 >= 0 && e6 < 2013265921 && e7 >= 0 && e7 < 2013265921)
	var s0 runar.Bigint = x0
	var s1 runar.Bigint = x1
	var s2 runar.Bigint = x2
	var s3 runar.Bigint = x3
	var s4 runar.Bigint = x4
	var s5 runar.Bigint = x5
	var s6 runar.Bigint = x6
	var s7 runar.Bigint = x7
	var s8 runar.Bigint = x8
	var s9 runar.Bigint = x9
	var s10 runar.Bigint = x10
	var s11 runar.Bigint = x11
	var s12 runar.Bigint = x12
	var s13 runar.Bigint = x13
	var s14 runar.Bigint = x14
	var s15 runar.Bigint = x15
	var s16 runar.Bigint = x16
	var s17 runar.Bigint = x17
	var s18 runar.Bigint = x18
	var s19 runar.Bigint = x19
	var s20 runar.Bigint = x20
	var s21 runar.Bigint = x21
	var s22 runar.Bigint = x22
	var s23 runar.Bigint = x23
	s0 = (s0 + 0x1da78ec2) % 2013265921
	qt = (s0 * s0) % 2013265921
	wt = (qt * qt) % 2013265921
	zt = (wt * qt) % 2013265921
	s0 = (zt * s0) % 2013265921
	var msum runar.Bigint = (s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7 + s8 + s9 + s10 + s11 + s12 + s13 + s14 + s15 + s16 + s17 + s18 + s19 + s20 + s21 + s22 + s23) % 2013265921
	s0 = (msum + 0x409133f0 * s0) % 2013265921
	s1 = (msum + 0x1667a8a1 * s1) % 2013265921
	s2 = (msum + 0x6a6c7b6 * s2) % 2013265921
	s3 = (msum + 0x6f53160e * s3) % 2013265921
	s4 = (msum + 0x273b11d1 * s4) % 2013265921
	s5 = (msum + 0x3176c5d * s5) % 2013265921
	s6 = (msum + 0x72f9bbf9 * s6) % 2013265921
	s7 = (msum + 0x73ceba91 * s7) % 2013265921
	s8 = (msum + 0x5cdef81d * s8) % 2013265921
	s9 = (msum + 0x1393285 * s9) % 2013265921
	s10 = (msum + 0x46daee06 * s10) % 2013265921
	s11 = (msum + 0x65d7ba6 * s11) % 2013265921
	s12 = (msum + 0x52d72d6f * s12) % 2013265921
	s13 = (msum + 0x5dd05e0 * s13) % 2013265921
	s14 = (msum + 0x3bab4b63 * s14) % 2013265921
	s15 = (msum + 0x6ada3842 * s15) % 2013265921
	s16 = (msum + 0x2fc5fbec * s16) % 2013265921
	s17 = (msum + 0x770d61b0 * s17) % 2013265921
	s18 = (msum + 0x5715aae9 * s18) % 2013265921
	s19 = (msum + 0x3ef0e90 * s19) % 2013265921
	s20 = (msum + 0x75b6c770 * s20) % 2013265921
	s21 = (msum + 0x242adf5f * s21) % 2013265921
	s22 = (msum + 0xd0ca4c * s22) % 2013265921
	s23 = (msum + 0x36c0e388 * s23) % 2013265921
	runar.Assert(s0 == e0 && s1 == e1 && s2 == e2 && s3 == e3 && s4 == e4 && s5 == e5 && s6 == e6 && s7 == e7)
}

