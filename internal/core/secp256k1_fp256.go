package core

import (
	"encoding/binary"
	"math/bits"

	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Pure-Go 4x64-bit secp256k1 field arithmetic over p = 2^256 - 2^32 - 977 (C = 0x1000003D1)
// using math/bits.Mul64/Add64/Sub64 intrinsics (single-instruction MULQ/ADCQ/SBBQ on amd64 & arm64,
// zero CGO, used strictly for public-data ECDSA signature verification).

const (
	secpP0 uint64 = 0xFFFFFFFEFFFFFC2F
	secpP1 uint64 = 0xFFFFFFFFFFFFFFFF
	secpP2 uint64 = 0xFFFFFFFFFFFFFFFF
	secpP3 uint64 = 0xFFFFFFFFFFFFFFFF
	secpC  uint64 = 0x00000001000003D1 // 2^256 - p

	secpN0 uint64 = 0xBFD25E8CD0364141
	secpN1 uint64 = 0xBAAEDCE6AF48A03B
	secpN2 uint64 = 0xFFFFFFFFFFFFFFFE
	secpN3 uint64 = 0xFFFFFFFFFFFFFFFF
)

// fp256 represents a canonical field element in [0, p-1] as 4 little-endian uint64 limbs.
type fp256 [4]uint64

func (f *fp256) isZero() bool {
	return (f[0] | f[1] | f[2] | f[3]) == 0
}

func (f *fp256) setOne() {
	f[0] = 1
	f[1] = 0
	f[2] = 0
	f[3] = 0
}

func (f *fp256) setBytes32(b *[32]byte) {
	f[3] = binary.BigEndian.Uint64(b[0:8])
	f[2] = binary.BigEndian.Uint64(b[8:16])
	f[1] = binary.BigEndian.Uint64(b[16:24])
	f[0] = binary.BigEndian.Uint64(b[24:32])
	// Reduce if >= p.
	t0, bw := bits.Sub64(f[0], secpP0, 0)
	t1, bw := bits.Sub64(f[1], secpP1, bw)
	t2, bw := bits.Sub64(f[2], secpP2, bw)
	t3, bw := bits.Sub64(f[3], secpP3, bw)
	if bw == 0 {
		f[0], f[1], f[2], f[3] = t0, t1, t2, t3
	}
}

func (f *fp256) putBytes32(b *[32]byte) {
	binary.BigEndian.PutUint64(b[0:8], f[3])
	binary.BigEndian.PutUint64(b[8:16], f[2])
	binary.BigEndian.PutUint64(b[16:24], f[1])
	binary.BigEndian.PutUint64(b[24:32], f[0])
}

func fp256FromFieldVal(fv *dsecp.FieldVal) fp256 {
	var b [32]byte
	cp := *fv
	cp.Normalize()
	cp.PutBytes(&b)
	var out fp256
	out.setBytes32(&b)
	return out
}

// ctMaskNZ returns ^uint64(0) if v != 0, else 0 (constant-time, branchless).
func ctMaskNZ(v uint64) uint64 {
	return uint64(int64(v|-v) >> 63)
}

func fpAdd(a, b *fp256) fp256 {
	r0, c := bits.Add64(a[0], b[0], 0)
	r1, c := bits.Add64(a[1], b[1], c)
	r2, c := bits.Add64(a[2], b[2], c)
	r3, c := bits.Add64(a[3], b[3], c)

	t0, bw := bits.Sub64(r0, secpP0, 0)
	t1, bw := bits.Sub64(r1, secpP1, bw)
	t2, bw := bits.Sub64(r2, secpP2, bw)
	t3, bw := bits.Sub64(r3, secpP3, bw)
	// Use subtracted value iff c != 0 || bw == 0 (branchless constant-time).
	useSub := ctMaskNZ(c | (bw ^ 1))
	keep := ^useSub
	return fp256{
		(r0 & keep) | (t0 & useSub),
		(r1 & keep) | (t1 & useSub),
		(r2 & keep) | (t2 & useSub),
		(r3 & keep) | (t3 & useSub),
	}
}

func fpSub(a, b *fp256) fp256 {
	r0, bw := bits.Sub64(a[0], b[0], 0)
	r1, bw := bits.Sub64(a[1], b[1], bw)
	r2, bw := bits.Sub64(a[2], b[2], bw)
	r3, bw := bits.Sub64(a[3], b[3], bw)
	// Add p iff bw != 0 (branchless constant-time).
	mask := ctMaskNZ(bw)
	var c uint64
	r0, c = bits.Add64(r0, secpP0&mask, 0)
	r1, c = bits.Add64(r1, secpP1&mask, c)
	r2, c = bits.Add64(r2, secpP2&mask, c)
	r3, _ = bits.Add64(r3, secpP3&mask, c)
	return fp256{r0, r1, r2, r3}
}

func fpDouble(a *fp256) fp256 {
	return fpAdd(a, a)
}

func fpMul(a, b *fp256) fp256 {
	a0, a1, a2, a3 := a[0], a[1], a[2], a[3]
	b0, b1, b2, b3 := b[0], b[1], b[2], b[3]

	// Row 0: a0 * b[0..3]
	hi, t0 := bits.Mul64(a0, b0)
	h, l := bits.Mul64(a0, b1)
	t1, c := bits.Add64(l, hi, 0)
	hi = h + c
	h, l = bits.Mul64(a0, b2)
	t2, c := bits.Add64(l, hi, 0)
	hi = h + c
	h, l = bits.Mul64(a0, b3)
	t3, c := bits.Add64(l, hi, 0)
	t4 := h + c

	// Row 1: a1 * b[0..3]
	h, l = bits.Mul64(a1, b0)
	t1, c = bits.Add64(t1, l, 0)
	hi = h + c

	h, l = bits.Mul64(a1, b1)
	var c2 uint64
	t2, c = bits.Add64(t2, l, 0)
	t2, c2 = bits.Add64(t2, hi, 0)
	hi = h + c + c2

	h, l = bits.Mul64(a1, b2)
	t3, c = bits.Add64(t3, l, 0)
	t3, c2 = bits.Add64(t3, hi, 0)
	hi = h + c + c2

	h, l = bits.Mul64(a1, b3)
	t4, c = bits.Add64(t4, l, 0)
	t4, c2 = bits.Add64(t4, hi, 0)
	t5 := h + c + c2

	// Row 2: a2 * b[0..3]
	h, l = bits.Mul64(a2, b0)
	t2, c = bits.Add64(t2, l, 0)
	hi = h + c

	h, l = bits.Mul64(a2, b1)
	t3, c = bits.Add64(t3, l, 0)
	t3, c2 = bits.Add64(t3, hi, 0)
	hi = h + c + c2

	h, l = bits.Mul64(a2, b2)
	t4, c = bits.Add64(t4, l, 0)
	t4, c2 = bits.Add64(t4, hi, 0)
	hi = h + c + c2

	h, l = bits.Mul64(a2, b3)
	t5, c = bits.Add64(t5, l, 0)
	t5, c2 = bits.Add64(t5, hi, 0)
	t6 := h + c + c2

	// Row 3: a3 * b[0..3]
	h, l = bits.Mul64(a3, b0)
	t3, c = bits.Add64(t3, l, 0)
	hi = h + c

	h, l = bits.Mul64(a3, b1)
	t4, c = bits.Add64(t4, l, 0)
	t4, c2 = bits.Add64(t4, hi, 0)
	hi = h + c + c2

	h, l = bits.Mul64(a3, b2)
	t5, c = bits.Add64(t5, l, 0)
	t5, c2 = bits.Add64(t5, hi, 0)
	hi = h + c + c2

	h, l = bits.Mul64(a3, b3)
	t6, c = bits.Add64(t6, l, 0)
	t6, c2 = bits.Add64(t6, hi, 0)
	t7 := h + c + c2

	// Inlined 512 -> 256 reduction via 2^256 == secpC (mod p).
	h0, l0 := bits.Mul64(t4, secpC)
	h1, l1 := bits.Mul64(t5, secpC)
	h2, l2 := bits.Mul64(t6, secpC)
	h3, l3 := bits.Mul64(t7, secpC)

	var c1, c3 uint64
	r0, c := bits.Add64(t0, l0, 0)
	r1, c1 := bits.Add64(t1, l1, c)
	r1, c2 = bits.Add64(r1, h0, 0)
	c = c1 + c2

	r2, c1 := bits.Add64(t2, l2, 0)
	r2, c2 = bits.Add64(r2, h1, 0)
	r2, c3 = bits.Add64(r2, c, 0)
	c = c1 + c2 + c3

	r3, c1 := bits.Add64(t3, l3, 0)
	r3, c2 = bits.Add64(r3, h2, 0)
	r3, c3 = bits.Add64(r3, c, 0)
	c4 := h3 + c1 + c2 + c3

	rhi, rlo := bits.Mul64(c4, secpC)
	r0, c = bits.Add64(r0, rlo, 0)
	r1, c = bits.Add64(r1, rhi, c)
	r2, c = bits.Add64(r2, 0, c)
	r3, c = bits.Add64(r3, 0, c)

	cmask := ctMaskNZ(c)
	r0, c = bits.Add64(r0, secpC&cmask, 0)
	r1, c = bits.Add64(r1, 0, c)
	r2, c = bits.Add64(r2, 0, c)
	r3, _ = bits.Add64(r3, 0, c)

	s0, bw := bits.Sub64(r0, secpP0, 0)
	s1, bw := bits.Sub64(r1, secpP1, bw)
	s2, bw := bits.Sub64(r2, secpP2, bw)
	s3, bw := bits.Sub64(r3, secpP3, bw)
	useSub := ctMaskNZ(bw ^ 1)
	keep := ^useSub
	return fp256{
		(r0 & keep) | (s0 & useSub),
		(r1 & keep) | (s1 & useSub),
		(r2 & keep) | (s2 & useSub),
		(r3 & keep) | (s3 & useSub),
	}
}

func fpSqr(a *fp256) fp256 {
	a0, a1, a2, a3 := a[0], a[1], a[2], a[3]

	// Off-diagonal products (i < j): 6 Mul64 instead of 12.
	hi, t1 := bits.Mul64(a0, a1)
	h, l := bits.Mul64(a0, a2)
	t2, c := bits.Add64(l, hi, 0)
	hi = h + c
	h, l = bits.Mul64(a0, a3)
	t3, c := bits.Add64(l, hi, 0)
	t4 := h + c

	var c2 uint64
	h, l = bits.Mul64(a1, a2)
	t3, c = bits.Add64(t3, l, 0)
	hi = h + c
	h, l = bits.Mul64(a1, a3)
	t4, c = bits.Add64(t4, l, 0)
	t4, c2 = bits.Add64(t4, hi, 0)
	t5 := h + c + c2

	h, l = bits.Mul64(a2, a3)
	t5, c = bits.Add64(t5, l, 0)
	t6 := h + c

	// Double off-diagonal sum (t1..t6 << 1).
	t7 := t6 >> 63
	t6 = (t6 << 1) | (t5 >> 63)
	t5 = (t5 << 1) | (t4 >> 63)
	t4 = (t4 << 1) | (t3 >> 63)
	t3 = (t3 << 1) | (t2 >> 63)
	t2 = (t2 << 1) | (t1 >> 63)
	t1 = t1 << 1

	// Add 4 diagonal squares a[i]*a[i].
	d0h, t0 := bits.Mul64(a0, a0)
	t1, c = bits.Add64(t1, d0h, 0)

	d1h, d1l := bits.Mul64(a1, a1)
	t2, c = bits.Add64(t2, d1l, c)
	t3, c = bits.Add64(t3, d1h, c)

	d2h, d2l := bits.Mul64(a2, a2)
	t4, c = bits.Add64(t4, d2l, c)
	t5, c = bits.Add64(t5, d2h, c)

	d3h, d3l := bits.Mul64(a3, a3)
	t6, c = bits.Add64(t6, d3l, c)
	t7, _ = bits.Add64(t7, d3h, c)

	// Inlined 512 -> 256 reduction via 2^256 == secpC (mod p).
	h0, l0 := bits.Mul64(t4, secpC)
	h1, l1 := bits.Mul64(t5, secpC)
	h2, l2 := bits.Mul64(t6, secpC)
	h3, l3 := bits.Mul64(t7, secpC)

	var c1, c3 uint64
	r0, c := bits.Add64(t0, l0, 0)
	r1, c1 := bits.Add64(t1, l1, c)
	r1, c2 = bits.Add64(r1, h0, 0)
	c = c1 + c2

	r2, c1 := bits.Add64(t2, l2, 0)
	r2, c2 = bits.Add64(r2, h1, 0)
	r2, c3 = bits.Add64(r2, c, 0)
	c = c1 + c2 + c3

	r3, c1 := bits.Add64(t3, l3, 0)
	r3, c2 = bits.Add64(r3, h2, 0)
	r3, c3 = bits.Add64(r3, c, 0)
	c4 := h3 + c1 + c2 + c3

	rhi, rlo := bits.Mul64(c4, secpC)
	r0, c = bits.Add64(r0, rlo, 0)
	r1, c = bits.Add64(r1, rhi, c)
	r2, c = bits.Add64(r2, 0, c)
	r3, c = bits.Add64(r3, 0, c)

	cmask := ctMaskNZ(c)
	r0, c = bits.Add64(r0, secpC&cmask, 0)
	r1, c = bits.Add64(r1, 0, c)
	r2, c = bits.Add64(r2, 0, c)
	r3, _ = bits.Add64(r3, 0, c)

	s0, bw := bits.Sub64(r0, secpP0, 0)
	s1, bw := bits.Sub64(r1, secpP1, bw)
	s2, bw := bits.Sub64(r2, secpP2, bw)
	s3, bw := bits.Sub64(r3, secpP3, bw)
	useSub := ctMaskNZ(bw ^ 1)
	keep := ^useSub
	return fp256{
		(r0 & keep) | (s0 & useSub),
		(r1 & keep) | (s1 & useSub),
		(r2 & keep) | (s2 & useSub),
		(r3 & keep) | (s3 & useSub),
	}
}

// fpAffPoint is a compact 64-byte (1 cache line!) normalized affine point on secp256k1.
type fpAffPoint struct {
	X, Y fp256
}

// fpJacPoint is a 96-byte Jacobian point (x = X/Z^2, y = Y/Z^3) on secp256k1.
type fpJacPoint struct {
	X, Y, Z fp256
}

func (p *fpJacPoint) setInfinity() {
	p.X = fp256{}
	p.Y = fp256{}
	p.Z = fp256{}
}

// jacDouble computes p = 2*p on y^2 = x^3 + 7 (a=0): 4 fpSqr + 3 fpMul.
func (p *fpJacPoint) double() {
	if p.Z.isZero() || p.Y.isZero() {
		p.setInfinity()
		return
	}
	a := fpSqr(&p.X)      // X1^2
	b := fpSqr(&p.Y)      // Y1^2
	c := fpSqr(&b)        // Y1^4
	xb := fpMul(&p.X, &b) // X1*Y1^2
	d := fpDouble(&xb)
	d = fpDouble(&d) // 4*X1*Y1^2
	e := fpDouble(&a)
	e = fpAdd(&e, &a) // 3*X1^2
	f := fpSqr(&e)    // 9*X1^4

	d2 := fpDouble(&d)
	x3 := fpSub(&f, &d2)

	c2 := fpDouble(&c)
	c4 := fpDouble(&c2)
	c8 := fpDouble(&c4)
	dMinusX3 := fpSub(&d, &x3)
	y3 := fpMul(&e, &dMinusX3)
	y3 = fpSub(&y3, &c8)

	yz := fpMul(&p.Y, &p.Z)
	z3 := fpDouble(&yz)

	p.X = x3
	p.Y = y3
	p.Z = z3
}

// jacAddMixed computes p = p + q where q is affine (Z2 = 1): 8 fpMul + 3 fpSqr.
func (p *fpJacPoint) addMixed(q *fpAffPoint) {
	if p.Z.isZero() {
		p.X = q.X
		p.Y = q.Y
		p.Z.setOne()
		return
	}
	z1z1 := fpSqr(&p.Z)
	u2 := fpMul(&q.X, &z1z1)
	z1cubed := fpMul(&p.Z, &z1z1)
	s2 := fpMul(&q.Y, &z1cubed)

	h := fpSub(&u2, &p.X)
	r := fpSub(&s2, &p.Y)
	if h.isZero() {
		if r.isZero() {
			p.double()
			return
		}
		p.setInfinity()
		return
	}
	hh := fpSqr(&h)
	hhh := fpMul(&h, &hh)
	v := fpMul(&p.X, &hh)

	rr := fpSqr(&r)
	v2 := fpDouble(&v)
	x3 := fpSub(&rr, &hhh)
	x3 = fpSub(&x3, &v2)

	vMinusX3 := fpSub(&v, &x3)
	t1 := fpMul(&r, &vMinusX3)
	t2 := fpMul(&p.Y, &hhh)
	y3 := fpSub(&t1, &t2)

	z3 := fpMul(&p.Z, &h)

	p.X = x3
	p.Y = y3
	p.Z = z3
}

// fpCombTable is a 65.2 KiB 4-Comb (w=8, d=8) precomputed affine table for secp256k1 point Q.
type fpCombTable struct {
	pts [4][255]fpAffPoint
}

var secp256k1GenComb *fpCombTable

// scalarBaseMultComb computes k * G using the 65 KiB 4-Comb (w=8, d=8) table (~22us).
func scalarBaseMultComb(k *dsecp.ModNScalar) fpJacPoint {
	var b [32]byte
	k.PutBytes(&b)
	u0 := binary.BigEndian.Uint32(b[28:32])
	u1 := binary.BigEndian.Uint32(b[24:28])
	u2 := binary.BigEndian.Uint32(b[20:24])
	u3 := binary.BigEndian.Uint32(b[16:20])
	u4 := binary.BigEndian.Uint32(b[12:16])
	u5 := binary.BigEndian.Uint32(b[8:12])
	u6 := binary.BigEndian.Uint32(b[4:8])
	u7 := binary.BigEndian.Uint32(b[0:4])

	gTbl := secp256k1GenComb
	var R fpJacPoint
	for c := uint(7); ; c-- {
		if !R.Z.isZero() {
			R.double()
		}
		for s := uint(0); s < 4; s++ {
			sh := s*8 + c
			m := ((u0 >> sh) & 1) |
				(((u1 >> sh) & 1) << 1) |
				(((u2 >> sh) & 1) << 2) |
				(((u3 >> sh) & 1) << 3) |
				(((u4 >> sh) & 1) << 4) |
				(((u5 >> sh) & 1) << 5) |
				(((u6 >> sh) & 1) << 6) |
				(((u7 >> sh) & 1) << 7)
			if m != 0 {
				R.addMixed(&gTbl.pts[s][m-1])
			}
		}
		if c == 0 {
			break
		}
	}
	return R
}

// u256 helpers for Stein's binary modular inverse mod secp256k1 order n (~2us, ~20x faster than dcrd ModNScalar.InverseNonConst).
func u256IsOne(a *[4]uint64) bool {
	return a[0] == 1 && (a[1]|a[2]|a[3]) == 0
}

func u256GTE(a, b *[4]uint64) bool {
	if a[3] != b[3] {
		return a[3] > b[3]
	}
	if a[2] != b[2] {
		return a[2] > b[2]
	}
	if a[1] != b[1] {
		return a[1] > b[1]
	}
	return a[0] >= b[0]
}

func u256Sub(a, b *[4]uint64) {
	var bw uint64
	a[0], bw = bits.Sub64(a[0], b[0], 0)
	a[1], bw = bits.Sub64(a[1], b[1], bw)
	a[2], bw = bits.Sub64(a[2], b[2], bw)
	a[3], _ = bits.Sub64(a[3], b[3], bw)
}

func u256SubModN(a, b *[4]uint64) {
	var bw uint64
	a[0], bw = bits.Sub64(a[0], b[0], 0)
	a[1], bw = bits.Sub64(a[1], b[1], bw)
	a[2], bw = bits.Sub64(a[2], b[2], bw)
	a[3], bw = bits.Sub64(a[3], b[3], bw)
	if bw != 0 {
		var c uint64
		a[0], c = bits.Add64(a[0], secpN0, 0)
		a[1], c = bits.Add64(a[1], secpN1, c)
		a[2], c = bits.Add64(a[2], secpN2, c)
		a[3], _ = bits.Add64(a[3], secpN3, c)
	}
}

func u256Rsh1(a *[4]uint64) {
	a[0] = (a[0] >> 1) | (a[1] << 63)
	a[1] = (a[1] >> 1) | (a[2] << 63)
	a[2] = (a[2] >> 1) | (a[3] << 63)
	a[3] >>= 1
}

func u256HalveModN(a *[4]uint64) {
	if (a[0] & 1) == 0 {
		u256Rsh1(a)
		return
	}
	var c uint64
	a[0], c = bits.Add64(a[0], secpN0, 0)
	a[1], c = bits.Add64(a[1], secpN1, c)
	a[2], c = bits.Add64(a[2], secpN2, c)
	a[3], c = bits.Add64(a[3], secpN3, c)
	a[0] = (a[0] >> 1) | (a[1] << 63)
	a[1] = (a[1] >> 1) | (a[2] << 63)
	a[2] = (a[2] >> 1) | (a[3] << 63)
	a[3] = (a[3] >> 1) | (c << 63)
}

// modNInvFast computes s^-1 mod n for non-zero s using 4x64-bit Stein's binary modular inverse (~2us).
func modNInvFast(s *dsecp.ModNScalar, out *dsecp.ModNScalar) {
	var b [32]byte
	s.PutBytes(&b)
	u := [4]uint64{
		binary.BigEndian.Uint64(b[24:32]),
		binary.BigEndian.Uint64(b[16:24]),
		binary.BigEndian.Uint64(b[8:16]),
		binary.BigEndian.Uint64(b[0:8]),
	}
	v := [4]uint64{secpN0, secpN1, secpN2, secpN3}
	x1 := [4]uint64{1, 0, 0, 0}
	x2 := [4]uint64{0, 0, 0, 0}

	for !u256IsOne(&u) && !u256IsOne(&v) {
		for (u[0] & 1) == 0 {
			u256Rsh1(&u)
			u256HalveModN(&x1)
		}
		for (v[0] & 1) == 0 {
			u256Rsh1(&v)
			u256HalveModN(&x2)
		}
		if u256GTE(&u, &v) {
			u256Sub(&u, &v)
			u256SubModN(&x1, &x2)
		} else {
			u256Sub(&v, &u)
			u256SubModN(&x2, &x1)
		}
	}
	res := &x2
	if u256IsOne(&u) {
		res = &x1
	}
	binary.BigEndian.PutUint64(b[0:8], res[3])
	binary.BigEndian.PutUint64(b[8:16], res[2])
	binary.BigEndian.PutUint64(b[16:24], res[1])
	binary.BigEndian.PutUint64(b[24:32], res[0])
	out.SetBytes(&b)
}

// fpInvFast computes x^-1 mod p using decred's constant-time SafeGCD FieldVal.Inverse (~3.5us vs 16us Fermat).
func fpInvFast(x *fp256) fp256 {
	var b [32]byte
	x.putBytes32(&b)
	var fv dsecp.FieldVal
	fv.SetBytes(&b)
	fv.Inverse()
	return fp256FromFieldVal(&fv)
}

func (p *fpJacPoint) toAffine() (fp256, fp256) {
	zInv := fpInvFast(&p.Z)
	zInv2 := fpSqr(&zInv)
	zInv3 := fpMul(&zInv2, &zInv)
	return fpMul(&p.X, &zInv2), fpMul(&p.Y, &zInv3)
}

func fpCompressedPub(x, y *fp256) [33]byte {
	var out [33]byte
	out[0] = byte(0x02 | (y[0] & 1))
	var xb [32]byte
	x.putBytes32(&xb)
	copy(out[1:], xb[:])
	return out
}

func buildFPCombTable(pub *dsecp.PublicKey) *fpCombTable {
	t := &fpCombTable{}
	var bases [4][8]dsecp.JacobianPoint
	var cur dsecp.JacobianPoint
	pub.AsJacobian(&cur)
	for idx := 0; idx < 32; idx++ {
		r := idx >> 2
		s := idx & 3
		bases[s][r] = cur
		if idx < 31 {
			for k := 0; k < 8; k++ {
				dsecp.DoubleNonConst(&cur, &cur)
			}
		}
	}

	const total = 4 * 255
	var jacs [total]dsecp.JacobianPoint
	for s := 0; s < 4; s++ {
		off := s * 255
		for mask := 1; mask < 256; mask++ {
			lowBit := bits.TrailingZeros8(uint8(mask))
			prev := mask ^ (1 << lowBit)
			if prev == 0 {
				jacs[off+mask-1] = bases[s][lowBit]
			} else {
				dsecp.AddNonConst(&jacs[off+prev-1], &bases[s][lowBit], &jacs[off+mask-1])
			}
		}
	}

	// Single Montgomery batch inversion via dcrd FieldVal -> convert once to compact 64-byte fpAffPoint!
	var prods [total]dsecp.FieldVal
	var acc dsecp.FieldVal
	acc.SetInt(1)
	for idx := 0; idx < total; idx++ {
		acc.Mul(&jacs[idx].Z)
		prods[idx] = acc
	}
	inv := acc
	inv.Inverse()
	var zInv, zInv2, zInv3, ax, ay dsecp.FieldVal
	for idx := total - 1; idx >= 0; idx-- {
		p := &jacs[idx]
		if idx > 0 {
			zInv.Mul2(&inv, &prods[idx-1])
			inv.Mul(&p.Z)
		} else {
			zInv = inv
		}
		zInv2.SquareVal(&zInv)
		zInv3.Mul2(&zInv2, &zInv)
		ax.Mul2(&p.X, &zInv2).Normalize()
		ay.Mul2(&p.Y, &zInv3).Normalize()
		dst := &t.pts[idx/255][idx%255]
		dst.X = fp256FromFieldVal(&ax)
		dst.Y = fp256FromFieldVal(&ay)
	}
	return t
}

func initGenCombTable() {
	var one dsecp.ModNScalar
	one.SetInt(1)
	var gJac dsecp.JacobianPoint
	dsecp.ScalarBaseMultNonConst(&one, &gJac)
	gJac.ToAffine()
	gPub := dsecp.NewPublicKey(&gJac.X, &gJac.Y)
	secp256k1GenComb = buildFPCombTable(gPub)
}

// verifyInterleavedComb computes R = u1*G + u2*Q in ONE single 8-iteration interleaved 4-Comb loop
// (<=7 doublings + <=64 mixed additions in 4x64-bit math/bits fp256) and checks R.X == r*Z^2 (mod p).
func verifyInterleavedComb(qTbl *fpCombTable, u1, u2, r *dsecp.ModNScalar) bool {
	var b1, b2 [32]byte
	u1.PutBytes(&b1)
	u2.PutBytes(&b2)

	a0 := binary.BigEndian.Uint32(b1[28:32])
	a1 := binary.BigEndian.Uint32(b1[24:28])
	a2 := binary.BigEndian.Uint32(b1[20:24])
	a3 := binary.BigEndian.Uint32(b1[16:20])
	a4 := binary.BigEndian.Uint32(b1[12:16])
	a5 := binary.BigEndian.Uint32(b1[8:12])
	a6 := binary.BigEndian.Uint32(b1[4:8])
	a7 := binary.BigEndian.Uint32(b1[0:4])

	q0 := binary.BigEndian.Uint32(b2[28:32])
	q1 := binary.BigEndian.Uint32(b2[24:28])
	q2 := binary.BigEndian.Uint32(b2[20:24])
	q3 := binary.BigEndian.Uint32(b2[16:20])
	q4 := binary.BigEndian.Uint32(b2[12:16])
	q5 := binary.BigEndian.Uint32(b2[8:12])
	q6 := binary.BigEndian.Uint32(b2[4:8])
	q7 := binary.BigEndian.Uint32(b2[0:4])

	gTbl := secp256k1GenComb
	var R fpJacPoint
	for c := uint(7); ; c-- {
		if !R.Z.isZero() {
			R.double()
		}
		for s := uint(0); s < 4; s++ {
			sh := s*8 + c
			m1 := ((a0 >> sh) & 1) |
				(((a1 >> sh) & 1) << 1) |
				(((a2 >> sh) & 1) << 2) |
				(((a3 >> sh) & 1) << 3) |
				(((a4 >> sh) & 1) << 4) |
				(((a5 >> sh) & 1) << 5) |
				(((a6 >> sh) & 1) << 6) |
				(((a7 >> sh) & 1) << 7)
			if m1 != 0 {
				R.addMixed(&gTbl.pts[s][m1-1])
			}
			m2 := ((q0 >> sh) & 1) |
				(((q1 >> sh) & 1) << 1) |
				(((q2 >> sh) & 1) << 2) |
				(((q3 >> sh) & 1) << 3) |
				(((q4 >> sh) & 1) << 4) |
				(((q5 >> sh) & 1) << 5) |
				(((q6 >> sh) & 1) << 6) |
				(((q7 >> sh) & 1) << 7)
			if m2 != 0 {
				R.addMixed(&qTbl.pts[s][m2-1])
			}
		}
		if c == 0 {
			break
		}
	}
	if R.Z.isZero() {
		return false
	}

	var rBytes [32]byte
	r.PutBytes(&rBytes)
	var rFp fp256
	rFp.setBytes32(&rBytes)
	z2 := fpSqr(&R.Z)
	rhs := fpMul(&rFp, &z2)
	if R.X == rhs {
		return true
	}
	// Edge case: r < p - n (0x0000000000000000_0000000000000001_4551231950b75fc4_402da1722fc9baee)
	// => check R.X == (r + n) * Z^2 (mod p).
	if rFp[3] == 0 && (rFp[2] == 0 || (rFp[2] == 1 && (rFp[1] < 0x4551231950b75fc4 || (rFp[1] == 0x4551231950b75fc4 && rFp[0] < 0x402da1722fc9baee)))) {
		orderFp := fp256{secpN0, secpN1, secpN2, secpN3}
		rPlusN := fpAdd(&rFp, &orderFp)
		rhs2 := fpMul(&rPlusN, &z2)
		if R.X == rhs2 {
			return true
		}
	}
	return false
}
