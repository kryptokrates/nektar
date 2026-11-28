package market

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestMarketSecurityPolicyCapsOfferIntegrityAndInternalAuth(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Policy 0-cap must fail CLOSED (not unlimited!).
	zeroCap := &Policy{
		MaxSatsPerDeal: 0,
		MaxSatsPerDay:  0,
	}
	if err := zeroCap.CanSpend("02abcd", 1); err == nil {
		t.Fatal("expected 0-cap spending policy to fail closed")
	}
	if _, err := db.ReserveSpendAtomic("deal-zero-cap", 1, 1, 0, 0); err == nil {
		t.Fatal("expected SQLite ReserveSpendAtomic with 0 maxPerDeal/maxPerDay to fail closed")
	}

	// 2. Unstarted WorkflowClient must fail CLOSED (zero sidecar/HTTP ports exposed; must start embedded runtime first)!
	eng := NewEngine(mem, DefaultPolicy())
	stoppedWC := NewWorkflowClient(eng, nil)
	if err := stoppedWC.HealthCheck(ctx); err == nil {
		t.Fatal("expected unstarted WorkflowClient HealthCheck to fail closed")
	}
	if _, err := stoppedWC.StartBuyerDealAsync(ctx, "w-unstarted", BuyerDealInput{}); err == nil {
		t.Fatal("expected unstarted WorkflowClient StartBuyerDealAsync to fail closed")
	}

	// 3. KeyedSaleOffer integrity: expired offer, missing leaf, and tampered KeyCommitment/Ciphertext must be rejected!
	sellerKey, _ := ec.NewPrivateKey()
	buyerKey, _ := ec.NewPrivateKey()
	buyerPub := hex.EncodeToString(buyerKey.PubKey().Compressed())
	testLeaf := strings.Repeat("aa", 32)

	expiredOffer, err := contracts.CreateKeyedSaleOffer(
		sellerKey,
		[]byte("secret fact"),
		500, 1000, 100,
		uint64(time.Now().Add(-time.Minute).Unix()),
		buyerPub, testLeaf, true,
	)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	if err := contracts.VerifySaleOffer(expiredOffer, buyerPub, testLeaf); err == nil {
		t.Fatal("expected VerifySaleOffer to reject expired offer")
	}

	validOffer, err := contracts.CreateKeyedSaleOffer(
		sellerKey,
		[]byte("secret fact"),
		500, 1000, 100,
		uint64(time.Now().Add(time.Hour).Unix()),
		buyerPub, testLeaf, true,
	)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer valid: %v", err)
	}
	if err := contracts.VerifySaleOffer(validOffer, buyerPub, testLeaf); err != nil {
		t.Fatalf("VerifySaleOffer valid failed: %v", err)
	}
	if err := contracts.VerifySaleOffer(validOffer, buyerPub, ""); err == nil {
		t.Fatal("expected VerifySaleOffer to reject empty leafHex")
	}

	tamperedCT := *validOffer
	tamperedCT.CiphertextHex = strings.Repeat("00", len(validOffer.CiphertextHex)/2)
	if err := contracts.VerifySaleOffer(&tamperedCT, buyerPub, testLeaf); err == nil {
		t.Fatal("expected VerifySaleOffer to reject tampered CiphertextHex")
	}

	tamperedCommit := *validOffer
	tamperedCommit.KeyCommitment = strings.Repeat("11", 32)
	if err := contracts.VerifySaleOffer(&tamperedCommit, buyerPub, testLeaf); err == nil {
		t.Fatal("expected VerifySaleOffer to reject tampered KeyCommitment")
	}

	// 4. Broker HandlePurchase must reject unsigned purchase/re-reveal and expired RAM offers.
	broker := NewBroker(mem, eng, nil, BrokerConfig{})
	fundAddr, err := port.FundAddress()
	if err != nil {
		t.Fatalf("FundAddress: %v", err)
	}
	payTx, payTxID, err := port.SendToAddress(ctx, fundAddr, 500)
	if err != nil {
		t.Fatalf("SendToAddress: %v", err)
	}
	payBeef, err := payTx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}
	payBeefHex := hex.EncodeToString(payBeef)
	ps := &pendingSale{
		OfferID:   "offer-sec-1",
		WantID:    "want-sec-1",
		BuyerPub:  buyerPub,
		Shard:     "ops",
		SaleOffer: validOffer,
		RevealKey: validOffer.ContentKey(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Settled:   true,
	}
	broker.mu.Lock()
	broker.pendingSales[ps.OfferID] = ps
	broker.mu.Unlock()
	if err := db.SaveDeal(store.DealRecord{
		DealID:          "sell-offer-sec-1",
		Role:            "seller",
		Status:          "settled",
		PaymentTxID:     payTxID,
		ClaimKeyHex:     hex.EncodeToString(ps.RevealKey[:]),
		CounterpartyPub: buyerPub,
		PriceSats:       500,
	}); err != nil {
		t.Fatalf("UpsertDeal: %v", err)
	}

	// Unsigned re-reveal must fail!
	if _, err := broker.HandlePurchase(ctx, PurchaseRequest{
		OfferID:        "offer-sec-1",
		WantID:         "want-sec-1",
		BuyerPub:       buyerPub,
		TxID:           payTxID,
		PaymentBEEFHex: payBeefHex,
	}); err == nil {
		t.Fatal("expected HandlePurchase to reject unsigned re-reveal request")
	}

	// Wrong signer (impersonating buyerPub) must fail!
	attackerKey, _ := ec.NewPrivateKey()
	reqWrongSig := PurchaseRequest{
		OfferID:        "offer-sec-1",
		WantID:         "want-sec-1",
		BuyerPub:       buyerPub,
		TxID:           payTxID,
		PaymentBEEFHex: payBeefHex,
		TS:             time.Now().Unix(),
	}
	reqWrongSig.BuyerSig, _ = bstack.SignArtifact(attackerKey, reqWrongSig.Signable())
	if _, err := broker.HandlePurchase(ctx, reqWrongSig); err == nil {
		t.Fatal("expected HandlePurchase to reject mismatched BuyerSig on key re-reveal")
	}

	// Legitimate buyer signed re-reveal with matching payment BEEF succeeds!
	reqValid := PurchaseRequest{
		OfferID:        "offer-sec-1",
		WantID:         "want-sec-1",
		BuyerPub:       buyerPub,
		TxID:           payTxID,
		PaymentBEEFHex: payBeefHex,
		TS:             time.Now().Unix(),
	}
	reqValid.BuyerSig, _ = bstack.SignArtifact(buyerKey, reqValid.Signable())
	resp, err := broker.HandlePurchase(ctx, reqValid)
	if err != nil || !resp.OK || resp.ContentKeyHex == "" {
		t.Fatalf("expected valid signed buyer re-reveal to succeed, got resp=%+v err=%v", resp, err)
	}

	// Expired unsettled pendingSale must be rejected!
	broker.mu.Lock()
	broker.pendingSales["offer-expired"] = &pendingSale{
		OfferID:   "offer-expired",
		WantID:    "want-exp",
		BuyerPub:  buyerPub,
		SaleOffer: validOffer,
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
		Settled:   false,
	}
	broker.mu.Unlock()
	reqExp := PurchaseRequest{
		OfferID:        "offer-expired",
		WantID:         "want-exp",
		BuyerPub:       buyerPub,
		TxID:           payTxID,
		PaymentBEEFHex: payBeefHex,
		TS:             time.Now().Unix(),
	}
	reqExp.BuyerSig, _ = bstack.SignArtifact(buyerKey, reqExp.Signable())
	if _, err := broker.HandlePurchase(ctx, reqExp); err == nil {
		t.Fatal("expected HandlePurchase to reject expired RAM offer")
	}
}
