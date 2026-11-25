package contracts

import (
	"testing"

	runar "github.com/icellan/runar/packages/runar-go"
)

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected runar.Assert panic, but succeeded", name)
		}
	}()
	fn()
}

func TestBabyBearProbe(t *testing.T) {
	c := &BabyBearProbe{}
	c.Mulmod(7, 11, 77)
	c.Sbox7(2, 128)
	c.MdsRow(1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2)

	// F-27: negative or >= p inputs must panic before OP_MOD.
	assertPanics(t, "negative a", func() { c.Mulmod(-1, 5, 0) })
	assertPanics(t, "a >= p", func() { c.Mulmod(2013265921, 1, 0) })
	assertPanics(t, "negative sbox", func() { c.Sbox7(-3, 0) })
}

func TestClaimVerdictAuthorize(t *testing.T) {
	sig := runar.SignTestMessage(runar.Alice.PrivKey)
	pub := runar.Alice.PubKey
	pkh := runar.Hash160(pub)

	c := &ClaimVerdict{
		ReceiptHash: runar.Sha256("receipt"),
		JournalRoot: runar.Sha256("journal"),
		QueryId:     runar.ByteString("q1"),
		PayeePkh:    pkh,
	}
	c.Authorize(sig, pub, 500)
	if len(c.Outputs()) != 1 {
		t.Fatalf("expected 1 output, got %d", len(c.Outputs()))
	}

	// F-21: unauthorized caller (Bob) must be rejected.
	bobSig := runar.SignTestMessage(runar.Bob.PrivKey)
	assertPanics(t, "bob unauthorized", func() {
		c.Authorize(bobSig, runar.Bob.PubKey, 1)
	})
}

func TestDisclosureSaleBRC369AndGating(t *testing.T) {
	sellerPKH := runar.Hash160(runar.Alice.PubKey)
	buyerPKH := runar.Hash160(runar.Bob.PubKey)
	wrapped := runar.Sha256("brc369-content-key-32-bytes")
	domain := runar.HexToBytes("6d6574616e6574206b6579656420636f6e74656e7420636f6d6d69746d656e74207631")
	hWrapped := runar.Sha256(runar.Cat(domain, wrapped))

	c := &DisclosureSale{
		SellerPkh:       sellerPKH,
		Price:           1000,
		HWrapped:        hWrapped,
		RefundAfter:     100,
		MaxFee:          500,
		DepositorPkh:    buyerPKH,
		VerdictOutpoint: "",
	}
	expectedOuts := runar.Cat(
		c.out(1000, c.p2pkh(sellerPKH)),
		c.out(8600, c.p2pkh(buyerPKH)),
	)
	preimage := runar.SigHashPreimage(runar.Cat(runar.Hash256(expectedOuts), runar.ByteString(make([]byte, 8))))
	c.Buy(wrapped, 8600, preimage)

	// F-05: wrong BRC-369 content key must fail.
	assertPanics(t, "wrong key", func() {
		c.Buy(runar.Sha256("wrong-key"), 8600, preimage)
	})

	// F-06: Buy() must fail when VerdictOutpoint is set; BuyGated() must succeed.
	verdictOut := runar.ByteString(make([]byte, 36))
	cGated := &DisclosureSale{
		SellerPkh:       sellerPKH,
		Price:           1000,
		HWrapped:        hWrapped,
		RefundAfter:     100,
		MaxFee:          500,
		DepositorPkh:    buyerPKH,
		VerdictOutpoint: verdictOut,
	}
	assertPanics(t, "buy bypass on gated sale", func() {
		cGated.Buy(wrapped, 8600, preimage)
	})
	prevouts := runar.Cat(runar.ExtractOutpoint(preimage), verdictOut)
	cGated.BuyGated(wrapped, 8600, prevouts, preimage)
}

func TestNektarAnchorMethods(t *testing.T) {
	sig := runar.SignTestMessage(runar.Alice.PrivKey)
	pub := runar.Alice.PubKey
	pkh := runar.Hash160(pub)
	r0 := runar.Sha256("r0")
	rp0 := runar.Sha256("rp0")

	c := &NektarAnchor{
		MetaFlag:     runar.HexToBytes("4d455441"),
		GenesisOwner: pub,
		N:            1,
		Size:         10,
		R:            r0,
		Rp:           rp0,
		TagRprime:    runar.ByteString("nektar:rprime:v1"),
		OwnerPkh:     pkh,
	}
	c.Append(sig, pub, runar.Sha256("r1"), 20)
	if len(c.Outputs()) != 1 {
		t.Fatalf("Append: expected 1 output, got %d", len(c.Outputs()))
	}

	// F-19: short newOwnerPkh must be rejected in Rotate.
	assertPanics(t, "short pkh rotate", func() {
		c.Rotate(sig, pub, runar.HexToBytes("010203"))
	})
	c.Rotate(sig, pub, runar.Hash160(runar.Bob.PubKey))

	// F-19: oversized queryId (>75B) must be rejected in WitnessSettle.
	assertPanics(t, "oversized queryId", func() {
		c.WitnessSettle(sig, pub, runar.Sha256("rcpt"), runar.ByteString(make([]byte, 76)), pkh, runar.HexToBytes("51"), 1, 1, pkh)
	})
	c.WitnessSettle(sig, pub, runar.Sha256("rcpt"), runar.ByteString("q-ok"), pkh, runar.HexToBytes("51"), 1, 1, pkh)
}

func TestNektarZkClaimTurnStateMachine(t *testing.T) {
	proverSig := runar.SignTestMessage(runar.Alice.PrivKey)
	proverPub := runar.Alice.PubKey
	proverPkh := runar.Hash160(proverPub)

	chalSig := runar.SignTestMessage(runar.Bob.PrivKey)
	chalPub := runar.Bob.PubKey
	chalPkh := runar.Hash160(chalPub)

	stageCode := runar.HexToBytes("515293")
	stageCodeHash := runar.Hash256(stageCode)
	zero32 := runar.HexToBytes("0000000000000000000000000000000000000000000000000000000000000000")

	c := &NektarZkClaim{
		Status:         0,
		ReceiptHash:    runar.Sha256("rcpt"),
		JournalRoot:    runar.Sha256("jroot"),
		JournalSize:    16,
		QueryId:        runar.ByteString("q1"),
		BondSats:       1000,
		Deadline:       100,
		MaxFee:         5000,
		Window:         10,
		Stages:         4,
		ProverPkh:      proverPkh,
		VerdictCode:    runar.HexToBytes("51"),
		AnchorOutpoint: "",
		StageCodeHash:  stageCodeHash,
	}

	// Challenge moves 0 -> 1 (awaiting prover reveal) and locks 2*BondSats (F-20).
	c.Challenge(chalSig, chalPub, 10)
	if c.Status != 1 || c.ResponseDeadline != 10 || c.ChallengerPkh != chalPkh {
		t.Fatalf("unexpected post-challenge state: status=%d rd=%d", c.Status, c.ResponseDeadline)
	}
	if outs := c.Outputs(); len(outs) != 1 || outs[0].Satoshis != 2000 {
		t.Fatalf("F-20: expected 2*BondSats=2000, got %+v", outs)
	}

	// F-04: zero midCommit must be rejected in Reveal.
	assertPanics(t, "zero midCommit", func() {
		c.Reveal(proverSig, proverPub, zero32, 10)
	})
	// Reveal moves 1 -> 2 (awaiting challenger bisect).
	c.Reveal(proverSig, proverPub, runar.Sha256("mid1"), 10)
	if c.Status != 2 {
		t.Fatalf("expected status 2 after Reveal, got %d", c.Status)
	}
	// F-04: Challenger cannot SlashTimeout during status 2 (challenger's own turn!).
	assertPanics(t, "challenger slash on own turn", func() {
		c.SlashTimeout(chalSig, chalPub, 1500)
	})
	// Bisect [0,4] -> [2,4] (status=1), Reveal -> status=2, Bisect [2,4] -> [3,4] (status=3).
	c.Bisect(chalSig, chalPub, 1, 10)
	if c.Status != 1 || c.BisectLo != 2 || c.BisectHi != 4 {
		t.Fatalf("unexpected bisect 1 state: status=%d [%d,%d]", c.Status, c.BisectLo, c.BisectHi)
	}
	c.Reveal(proverSig, proverPub, runar.Sha256("mid2"), 10)
	c.Bisect(chalSig, chalPub, 1, 10)
	if c.Status != 3 || c.BisectLo != 3 || c.BisectHi != 4 {
		t.Fatalf("unexpected bisect 2 state: status=%d [%d,%d]", c.Status, c.BisectLo, c.BisectHi)
	}

	// F-03: ProveStage must reject unpinned stageCode.
	stageOut := runar.ByteString(make([]byte, 36))
	prevouts := runar.Cat(runar.ExtractOutpoint(c.TxPreimage), stageOut)
	assertPanics(t, "wrong stageCodeHash", func() {
		c.ProveStage(proverSig, proverPub, prevouts, stageOut, 5000, 1000, runar.HexToBytes("51"), runar.HexToBytes("01"))
	})
	c.ProveStage(proverSig, proverPub, prevouts, stageOut, 5000, 1000, stageCode, runar.HexToBytes("01"))
}

func TestNektarGrothClaimSettleValid(t *testing.T) {
	sig := runar.SignTestMessage(runar.Alice.PrivKey)
	pub := runar.Alice.PubKey
	pkh := runar.Hash160(pub)
	verOut := runar.ByteString(make([]byte, 36))

	c := &NektarGrothClaim{
		ReceiptHash:      runar.Sha256("rcpt"),
		JournalRoot:      runar.Sha256("jroot"),
		JournalSize:      32,
		QueryId:          runar.ByteString("gq1"),
		ProverPkh:        pkh,
		VerifierOutpoint: verOut,
		MaxFee:           5000,
		VerdictCode:      runar.HexToBytes("51"),
	}
	prevouts := runar.Cat(runar.ExtractOutpoint(c.TxPreimage), verOut)
	c.SettleValid(sig, pub, prevouts, 1000, 4000)

	// Wrong prevouts must panic.
	assertPanics(t, "wrong prevouts", func() {
		c.SettleValid(sig, pub, runar.ByteString(make([]byte, 72))+runar.HexToBytes("01"), 1000, 4000)
	})
}
