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

func TestNektarAgentVaultTwoKeyRules(t *testing.T) {
	opSig := runar.SignTestMessage(runar.Alice.PrivKey)
	opPub := runar.Alice.PubKey
	opPkh := runar.Hash160(opPub)

	agSig := runar.SignTestMessage(runar.Bob.PrivKey)
	agPub := runar.Bob.PubKey
	agPkh := runar.Hash160(agPub)

	c := &NektarAgentVault{
		OperatorPkh:   opPkh,
		AgentPkh:      agPkh,
		PerTxCap:      2000,
		WindowCap:     5000,
		WindowBlocks:  144,
		MaxFee:        500,
		SpentInWindow: 0,
		WindowStart:   0,
		Paused:        0,
	}

	dest := c.p2pkh(agPkh)
	// Valid spend within PerTxCap & WindowCap.
	// Note: in mock Go test mode, ExtractAmount(TxPreimage)=10000, ExtractLocktime=0, ExtractSequence=0.
	c.AgentSpend(agSig, agPub, dest, 1500, 1500, 0, 8200)
	if c.SpentInWindow != 1500 {
		t.Fatalf("expected SpentInWindow=1500, got %d", c.SpentInWindow)
	}

	// Exceeding PerTxCap must panic.
	assertPanics(t, "over PerTxCap", func() {
		c.AgentSpend(agSig, agPub, dest, 2500, 4000, 0, 7200)
	})

	// Exceeding WindowCap across cumulative spends must panic (1500 + 2000 + 2000 = 5500 > 5000).
	c.AgentSpend(agSig, agPub, dest, 2000, 3500, 0, 7700)
	assertPanics(t, "over WindowCap", func() {
		c.AgentSpend(agSig, agPub, dest, 2000, 5500, 0, 7700)
	})

	// Hot agent MUST NOT be able to call OperatorControl or OperatorSweep.
	assertPanics(t, "agent tries OperatorControl", func() {
		c.OperatorControl(agSig, agPub, agPkh, 999999, 999999, 1, 500, 0, 9500)
	})
	assertPanics(t, "agent tries OperatorSweep", func() {
		c.OperatorSweep(agSig, agPub, agPkh, 9500)
	})

	// Cold operator pauses the vault -> hot agent is immediately locked out.
	c.OperatorControl(opSig, opPub, agPkh, 2000, 5000, 144, 500, 1, 9500)
	if c.Paused != 1 {
		t.Fatalf("expected Paused=1, got %d", c.Paused)
	}
	assertPanics(t, "agent spend while paused", func() {
		c.AgentSpend(agSig, agPub, dest, 100, 3600, 0, 9600)
	})

	// Cold operator can unpause, rotate agent key, and sweep.
	c.OperatorControl(opSig, opPub, opPkh, 2000, 5000, 144, 500, 0, 9500)
	if c.Paused != 0 || c.AgentPkh != opPkh {
		t.Fatalf("expected unpaused rotated vault")
	}
	c.OperatorSweep(opSig, opPub, opPkh, 9500)
}
