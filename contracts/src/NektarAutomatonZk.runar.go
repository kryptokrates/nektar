// NektarAutomatonZk — AUT-005 + hardened Schnorr-PoK delegation gate.
// F-22: Strong Fiat-Shamir challenge binds domain tag, R, delegateP, contract instance
//       (ownerPkh, deltaRoot), AND every transition parameter (n, from, sym, to, outHash).
// F-26: Unsigned scalar decoding via bin2num(cat(challenge, 0x00)) % ecN and strict
//       1 <= e < ecN, 1 <= s < ecN canonicality bounds.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarAutomatonZk struct {
	runar.StatefulSmartContract
	Q         runar.Bigint
	DeltaRoot runar.Sha256Digest
	OwnerPkh  runar.ByteString
	N         runar.Bigint
	LastOut   runar.ByteString
	FinalRoot runar.Sha256Digest
	Halted    runar.Bigint
	DelegateP runar.Point
}

func (c *NektarAutomatonZk) fold(h runar.ByteString, sib runar.ByteString, dir runar.Bigint) runar.ByteString {
	if dir == 1 {
		return runar.Hash256(runar.Cat(sib, h))
	}
	return runar.Hash256(runar.Cat(h, sib))
}

func (c *NektarAutomatonZk) StepZk(commitR runar.Point, s runar.Bigint,
	from runar.Bigint, sym runar.Bigint, to runar.Bigint, outHash runar.ByteString,
	sib0 runar.ByteString, sib1 runar.ByteString, sib2 runar.ByteString, sib3 runar.ByteString,
	dir runar.Bigint) {
	runar.Assert(c.Halted == 0)
	runar.Assert(runar.Len(outHash) == 32)
	runar.Assert(from == c.Q)
	runar.Assert(runar.EcOnCurve(commitR))
	runar.Assert(runar.EcOnCurve(c.DelegateP))
	// secp256k1 group order N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141 (LE + 0x00 sign byte)
	ecN := runar.Bin2Num(runar.HexToBytes("414136d08c5ed2bf3ba048afe6dcaebafeffffffffffffffffffffffffffffff00"))
	runar.Assert(s >= 1 && s < ecN)
	domain := runar.HexToBytes("6e656b7461723a6175747a6b3a7632") // "nektar:autzk:v2"
	ctxBytes := runar.Cat(runar.Cat(domain, runar.Cat(commitR, c.DelegateP)), runar.Cat(c.OwnerPkh, c.DeltaRoot))
	stepBytes := runar.Cat(runar.Cat(runar.Num2Bin(c.N, 8), runar.Num2Bin(from, 8)), runar.Cat(runar.Num2Bin(sym, 8), runar.Cat(runar.Num2Bin(to, 8), outHash)))
	challenge := runar.Sha256(runar.Cat(ctxBytes, stepBytes))
	e := runar.Bin2Num(runar.Cat(challenge, runar.HexToBytes("00"))) % ecN
	runar.Assert(e >= 1 && e < ecN)
	runar.Assert(runar.EcMulGen(s) == runar.EcAdd(commitR, runar.EcMul(c.DelegateP, e)))
	h0 := runar.Cat(runar.Num2Bin(from, 8), runar.Cat(runar.Num2Bin(sym, 8), runar.Num2Bin(to, 8)))
	h1 := c.fold(h0, sib0, dir%2)
	h2 := c.fold(h1, sib1, (dir/2)%2)
	h3 := c.fold(h2, sib2, (dir/4)%2)
	h4 := c.fold(h3, sib3, (dir/8)%2)
	runar.Assert(h4 == c.DeltaRoot)
	c.AddOutput(1, to, c.DeltaRoot, c.OwnerPkh, c.N+1, outHash, c.FinalRoot, 0, c.DelegateP)
}

func (c *NektarAutomatonZk) Step(sig runar.Sig, pubKey runar.PubKey,
	from runar.Bigint, sym runar.Bigint, to runar.Bigint, outHash runar.ByteString,
	sib0 runar.ByteString, sib1 runar.ByteString, sib2 runar.ByteString, sib3 runar.ByteString,
	dir runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Halted == 0)
	runar.Assert(runar.Len(outHash) == 32)
	runar.Assert(from == c.Q)
	h0 := runar.Cat(runar.Num2Bin(from, 8), runar.Cat(runar.Num2Bin(sym, 8), runar.Num2Bin(to, 8)))
	h1 := c.fold(h0, sib0, dir%2)
	h2 := c.fold(h1, sib1, (dir/2)%2)
	h3 := c.fold(h2, sib2, (dir/4)%2)
	h4 := c.fold(h3, sib3, (dir/8)%2)
	runar.Assert(h4 == c.DeltaRoot)
	c.AddOutput(1, to, c.DeltaRoot, c.OwnerPkh, c.N+1, outHash, c.FinalRoot, 0, c.DelegateP)
}

func (c *NektarAutomatonZk) Halt(sig runar.Sig, pubKey runar.PubKey,
	sib0 runar.ByteString, sib1 runar.ByteString, sib2 runar.ByteString, sib3 runar.ByteString, dir runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Halted == 0)
	f0 := runar.Num2Bin(c.Q, 8)
	f1 := c.fold(f0, sib0, dir%2)
	f2 := c.fold(f1, sib1, (dir/2)%2)
	f3 := c.fold(f2, sib2, (dir/4)%2)
	f4 := c.fold(f3, sib3, (dir/8)%2)
	runar.Assert(f4 == c.FinalRoot)
	c.AddOutput(1, c.Q, c.DeltaRoot, c.OwnerPkh, c.N+1, c.LastOut, c.FinalRoot, 1, c.DelegateP)
}
