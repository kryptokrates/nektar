package bstack

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func TestDynamicTopicsAndSealedPaymentCarrier(t *testing.T) {
	if got := DomainTopic("testnet", "Travel-Deals"); got != "tm_nektar_testnet_d_travel-deals" {
		t.Fatalf("DomainTopic mismatch: %q", got)
	}
	if b := TopicBand("tm_uhrp_files", 8); b != 0x104B {
		t.Fatalf("TopicBand(tm_uhrp_files, 8) = 0x%04X, want 0x104B", b)
	}

	alicePriv, _ := ec.NewPrivateKey()
	bobPriv, _ := ec.NewPrivateKey()
	alicePub := hex.EncodeToString(alicePriv.PubKey().Compressed())
	bobPub := hex.EncodeToString(bobPriv.PubKey().Compressed())

	// Pairwise BRC-42/ECDH rendezvous topic is symmetric and private to Alice & Bob.
	tAlice, tidAlice, err := DerivedRendezvousTopic("testnet", alicePriv, bobPub, "deal-42")
	if err != nil {
		t.Fatalf("DerivedRendezvousTopic(alice): %v", err)
	}
	tBob, tidBob, err := DerivedRendezvousTopic("testnet", bobPriv, alicePub, "deal-42")
	if err != nil {
		t.Fatalf("DerivedRendezvousTopic(bob): %v", err)
	}
	if tAlice != tBob || tidAlice != tidBob {
		t.Fatalf("rendezvous topic not symmetric: %q vs %q", tAlice, tBob)
	}

	now := time.Unix(1795700000, 0)
	// Reject lifetime < 2h (Paper 011 Fig. 11)
	if _, err := SealPaymentForPlane(alicePriv, SealedPaymentCarrier{
		DealID:       "deal-42",
		SenderPub:    alicePub,
		RecipientPub: bobPub,
		Kind:         "purchase",
		CreatedAt:    now.Unix(),
		ExpiresAt:    now.Add(30 * time.Minute).Unix(),
		PaymentBEEF:  "01010101deadbeef",
	}); err == nil {
		t.Fatal("expected sealed payment with <2h lifetime to be rejected")
	}

	exp := now.Add(3 * time.Hour)
	env, err := SealPaymentForPlane(alicePriv, SealedPaymentCarrier{
		DealID:       "deal-42",
		SenderPub:    alicePub,
		RecipientPub: bobPub,
		Kind:         "purchase",
		CreatedAt:    now.Unix(),
		ExpiresAt:    exp.Unix(),
		PaymentBEEF:  "01010101deadbeef",
		Body:         json.RawMessage(`{"sats":110}`),
	})
	if err != nil {
		t.Fatalf("SealPaymentForPlane: %v", err)
	}

	// Bob opens safely inside payee window (now + 30m <= exp - 1h).
	opened, err := OpenPaymentFromPlane(bobPriv, env, "purchase", now.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("OpenPaymentFromPlane: %v", err)
	}
	if opened.PaymentBEEF != "01010101deadbeef" || opened.DealID != "deal-42" {
		t.Fatalf("unexpected opened sealed payment: %+v", opened)
	}

	// Inside the 2-hour quiet buffer (exp - 30m): NEITHER Bob may claim NOR Alice may reclaim!
	quietTime := exp.Add(-30 * time.Minute)
	if _, err := OpenPaymentFromPlane(bobPriv, env, "purchase", quietTime); err == nil {
		t.Fatal("expected payee claim inside quiet buffer (exp-30m) to fail closed")
	}
	if PayerCanReclaimSealedPayment(now.Unix(), exp.Unix(), quietTime) {
		t.Fatal("payer must NOT reclaim before exp + 1h")
	}
	if PayerCanReclaimSealedPayment(now.Unix(), exp.Unix(), exp.Add(30*time.Minute)) {
		t.Fatal("payer must NOT reclaim at exp + 30m")
	}
	if !PayerCanReclaimSealedPayment(now.Unix(), exp.Unix(), exp.Add(61*time.Minute)) {
		t.Fatal("payer SHOULD be allowed to reclaim at exp + 61m")
	}
}
