package market

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestBilateralDealReceiptSaleKeyMatchAndMismatchRejected(t *testing.T) {
	ctx := context.Background()
	_, buyerEng, buyerBroker := newTestNodeForEscrow(t, nil)
	_, sellerEng, sellerBroker := newTestNodeForEscrow(t, nil)

	dr := DealReceipt{
		Version:       1,
		DealID:        "want-bilat-1",
		OfferID:       "off-bilat-1",
		BuyerPub:      buyerBroker.PubHex(),
		SellerPub:     sellerBroker.PubHex(),
		PriceSats:     200,
		PaymentTxID:   strings.Repeat("aa", 32),
		KeyCommitment: strings.Repeat("bb", 32),
		FragmentLeaf:  strings.Repeat("cc", 32),
		SettledAt:     time.Now().Unix(),
	}
	if err := SignDealReceiptSeller(ctx, sellerEng.salePubHex(), sellerEng.signSaleDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptSeller: %v", err)
	}
	// Attempting to countersign with a DIFFERENT buyerPub MUST fail closed without mutating BuyerPub!
	otherPub := sellerEng.salePubHex()
	if err := SignDealReceiptBuyer(ctx, otherPub, sellerEng.signSaleDigest, &dr); err == nil {
		t.Fatal("expected SignDealReceiptBuyer to reject mismatched buyerPub on seller-signed receipt")
	}
	if dr.BuyerPub != buyerBroker.PubHex() {
		t.Fatalf("expected BuyerPub preserved after rejected countersign, got %s", dr.BuyerPub)
	}
	// Countersigning with matching buyer saleKey succeeds and preserves SellerSig!
	if err := SignDealReceiptBuyer(ctx, buyerEng.salePubHex(), buyerEng.signSaleDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptBuyer: %v", err)
	}
	vr := VerifyDealReceipt(dr, true)
	if !vr.OK || !vr.Bilateral || !vr.SellerVerified || !vr.BuyerVerified {
		t.Fatalf("expected valid bilateral DealReceipt, got %+v", vr)
	}
}

func TestStandingOrderInboxPreservesControlEvents(t *testing.T) {
	_, eng, _ := newTestNodeForEscrow(t, nil)
	orderID := "so-preserve-ctrl-1"

	eng.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "cancel", Reason: "operator cancel"})
	for i := 0; i < 70; i++ {
		eng.enqueueStandingOrderEvent(orderID, standingOrderEvent{
			Kind:  "offer",
			Offer: &bstack.OfferAd{OfferID: "off-" + string(rune('a'+i%26)) + "-" + time.Now().Format("150405") + string(rune('0'+i/26))},
		})
	}
	drained := eng.drainStandingOrderEvents(orderID)
	foundCancel := false
	for _, ev := range drained {
		if ev.Kind == "cancel" {
			foundCancel = true
			break
		}
	}
	if !foundCancel {
		t.Fatalf("expected cancel control event preserved under 70-offer flood (len=%d)", len(drained))
	}
}

func TestBboxMessageRejectsMismatchedAudience(t *testing.T) {
	ctx := context.Background()
	_, _, buyerBroker := newTestNodeForEscrow(t, nil)
	_, _, sellerBroker := newTestNodeForEscrow(t, nil)

	offerID := "off-aud-1"
	dealID := "want-aud-1"
	replyCh := make(chan PurchaseResponse, 2)
	buyerBroker.mu.Lock()
	buyerBroker.bboxReplies[offerID] = replyCh
	buyerBroker.registerBboxExpectationLocked(offerID, sellerBroker.PubHex(), dealID, time.Now().Unix())
	buyerBroker.mu.Unlock()

	bodyRaw, _ := json.Marshal(PurchaseResponse{
		OK:            true,
		Status:        "settled",
		OfferID:       offerID,
		SellerPub:     sellerBroker.PubHex(),
		ContentKeyHex: strings.Repeat("22", 32),
	})
	aad := bstack.BboxAAD("default", sellerBroker.PubHex(), buyerBroker.PubHex(), dealID, "reveal_key")
	sealed, err := bstack.SealBboxBody(buyerBroker.PubHex(), aad, bodyRaw)
	if err != nil {
		t.Fatalf("SealBboxBody: %v", err)
	}
	msg := hub.BboxMessage{
		Office:       "default",
		RecipientPub: buyerBroker.PubHex(),
		SenderPub:    sellerBroker.PubHex(),
		DealID:       dealID,
		Kind:         "reveal_key",
		TS:           time.Now().Unix(),
		Nonce:        "aud-nonce-1",
		Aud:          "hub-a.example",
		Body:         sealed,
	}
	sig, err := bstack.SignArtifact(sellerBroker.engine.saleKey, msg.Signable())
	if err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	msg.Sig = sig

	if err := buyerBroker.handleBboxMessage(ctx, "https://hub-b.example", msg); err != nil {
		t.Fatalf("handleBboxMessage: %v", err)
	}
	if len(replyCh) != 0 {
		t.Fatalf("expected mismatched m.Aud bbox message to be rejected, got %d replies", len(replyCh))
	}
}

func TestClearPendingPaymentAndOfferMatch(t *testing.T) {
	mem, eng, _ := newTestNodeForEscrow(t, nil)
	offJSON, _ := json.Marshal(bstack.OfferAd{OfferID: "off-seller-1", Pub: "seller-1"})
	eng.persistDealRecord(store.DealRecord{
		DealID:          "deal-failover-1",
		Role:            "buyer",
		Status:          "payment_pending",
		ChosenOfferJSON: offJSON,
		CounterpartyPub: "seller-1",
		PaymentTxID:     "tx-aborted-1",
		PaymentBEEF:     []byte{0x01, 0x02},
	})
	rec, err := mem.DB().GetDeal("deal-failover-1")
	if err != nil {
		t.Fatalf("GetDeal: %v", err)
	}
	if !offerMatchesDealRecord(rec, "off-seller-1") {
		t.Fatal("expected matching OfferID to pass offerMatchesDealRecord")
	}
	if offerMatchesDealRecord(rec, "off-seller-2") {
		t.Fatal("expected different OfferID to fail offerMatchesDealRecord")
	}
	eng.clearPendingPayment("deal-failover-1")
	after, err := mem.DB().GetDeal("deal-failover-1")
	if err != nil {
		t.Fatalf("GetDeal after clear: %v", err)
	}
	if after.PaymentTxID != "" || len(after.PaymentBEEF) != 0 {
		t.Fatalf("expected pending payment cleared, got txid=%q beefLen=%d", after.PaymentTxID, len(after.PaymentBEEF))
	}
}
