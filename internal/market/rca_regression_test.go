package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
)

// TestRCA_S2_DeferredEscrowOnPaymentTxMinedBroadcastsBuyBeforeReveal verifies that when a
// DisclosureSale escrow triggers needsConfirm=true (over zero-conf cap), HandlePurchase persists
// the escrow parameters in pendingConfirmations, and OnPaymentTxMined broadcasts DisclosureSale.Buy(k)
// on-chain BEFORE marking the sale settled or revealing ContentKeyHex — failing closed if Buy(k)
// cannot be built/broadcast.
func TestRCA_S2_DeferredEscrowOnPaymentTxMinedBroadcastsBuyBeforeReveal(t *testing.T) {
	ctx := context.Background()
	sharedDir := t.TempDir()
	sharedPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: sharedDir})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer sharedPort.Close()

	_, _, buyerBroker := newTestNodeForEscrow(t, sharedPort)
	sellerMem, sellerEng, sellerBroker := newTestNodeForEscrow(t, sharedPort)
	// Force needsConfirm=true for any unconfirmed escrow payment!
	sellerEng.Policy().ZeroConfMaxSats = 0

	rem, err := sellerMem.Remember(ctx, memory.RememberInput{Shard: "alpha", Text: "deferred escrow secret", AutoAnchor: true})
	if err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	saleOffer, revealKey, chirpURL, err := sellerEng.PrepareDirectSale(ctx, "alpha", rem.Seq, 1500, buyerBroker.PubHex())
	if err != nil {
		t.Fatalf("PrepareDirectSale: %v", err)
	}
	offerID := "off-deferred-escrow-1"
	dealID := "deal-deferred-escrow-1"
	sellerBroker.mu.Lock()
	sellerBroker.pendingSales[offerID] = &pendingSale{
		OfferID:   offerID,
		WantID:    dealID,
		BuyerPub:  buyerBroker.PubHex(),
		Shard:     "alpha",
		Seq:       rem.Seq,
		SaleOffer: saleOffer,
		RevealKey: revealKey,
		ChirpURL:  chirpURL,
		ExpiresAt: int64(saleOffer.ExpiresAt),
	}
	sellerBroker.mu.Unlock()

	// Build a real DisclosureSale escrow tx on sharedPort, but strip its MerklePath in the
	// delivered BEEF so HandlePurchase treats it as unconfirmed -> needsConfirm=true.
	sellerPKH, err := sharedPort.FundPKH(ctx)
	if err != nil {
		t.Fatalf("FundPKH: %v", err)
	}
	hWrapped, err := hex.DecodeString(saleOffer.Config.HWrapped)
	if err != nil {
		t.Fatalf("decode HWrapped: %v", err)
	}
	escrowParams := contracts.DisclosureSaleParams{
		SellerPKH:    sellerPKH,
		Price:        saleOffer.Config.Price,
		HWrapped:     hWrapped,
		RefundAfter:  uint64(time.Now().Add(time.Hour).Unix()),
		MaxFee:       500,
		DepositorPKH: sellerPKH,
	}
	escrowLock, err := contracts.EncodeDisclosureSaleLockingScript(escrowParams)
	if err != nil {
		t.Fatalf("EncodeDisclosureSaleLockingScript: %v", err)
	}
	escrowTx, escrowFee, escrowSpend, err := sharedPort.BuildFundedTx(ctx, []*transaction.TransactionOutput{{
		Satoshis:      escrowParams.RequiredEscrowSatoshis(),
		LockingScript: escrowLock,
	}})
	if err != nil {
		t.Fatalf("BuildFundedTx: %v", err)
	}
	escrowTxID, err := sharedPort.Broadcast(ctx, escrowTx, escrowFee, escrowSpend)
	if err != nil {
		t.Fatalf("Broadcast escrow: %v", err)
	}
	mp, height, ok := sharedPort.Proof(ctx, escrowTxID)
	if !ok || mp == nil {
		t.Fatalf("expected testchain proof for escrow tx %s", escrowTxID)
	}
	// Encode an unconfirmed copy (MerklePath=nil) to exercise needsConfirm=true.
	unconfirmedTx, err := transaction.NewTransactionFromBytes(escrowTx.Bytes())
	if err != nil {
		t.Fatalf("clone escrow tx: %v", err)
	}
	for i := range unconfirmedTx.Inputs {
		unconfirmedTx.Inputs[i].SourceTransaction = escrowTx.Inputs[i].SourceTransaction
	}
	unconfirmedBEEF, err := chain.SafeAtomicBEEF(unconfirmedTx)
	if err != nil {
		t.Fatalf("SafeAtomicBEEF(unconfirmed): %v", err)
	}

	pReq := PurchaseRequest{
		OfferID:           offerID,
		WantID:            dealID,
		BuyerPub:          buyerBroker.PubHex(),
		TxID:              escrowTxID,
		PaymentBEEFHex:    hex.EncodeToString(unconfirmedBEEF),
		OutputIndex:       0,
		EscrowMode:        true,
		EscrowSellerPKH:   hex.EncodeToString(escrowParams.SellerPKH),
		DepositorPKH:      hex.EncodeToString(escrowParams.DepositorPKH),
		EscrowRefundAfter: escrowParams.RefundAfter,
		EscrowMaxFee:      escrowParams.MaxFee,
	}
	buyerBroker.signPurchaseRequest(&pReq)

	pResp, err := sellerBroker.HandlePurchase(ctx, pReq)
	if err != nil {
		t.Fatalf("HandlePurchase(needsConfirm): %v", err)
	}
	if pResp.Status != "awaiting_confirmation" || pResp.ContentKeyHex != "" {
		t.Fatalf("expected awaiting_confirmation with withheld key, got status=%q key=%q", pResp.Status, pResp.ContentKeyHex)
	}
	sellerBroker.mu.RLock()
	pendingEntry := sellerBroker.pendingConfirmations[escrowTxID]
	sellerBroker.mu.RUnlock()
	if pendingEntry == nil || !pendingEntry.IsEscrow || pendingEntry.EscrowBEEFHex == "" {
		t.Fatalf("expected persisted escrow pendingConfirmEntry, got %+v", pendingEntry)
	}

	// Fail-closed check: corrupting EscrowBEEFHex must prevent OnPaymentTxMined from marking settled!
	origBEEF := pendingEntry.EscrowBEEFHex
	pendingEntry.EscrowBEEFHex = "deadbeef"
	sellerBroker.OnPaymentTxMined(ctx, escrowTxID, height, mp)
	sellerBroker.mu.RLock()
	stillPending := sellerBroker.pendingConfirmations[escrowTxID] != nil
	sellerBroker.mu.RUnlock()
	if !stillPending {
		t.Fatal("expected OnPaymentTxMined to fail closed and keep pendingConfirmations when escrow BEEF is corrupt")
	}
	pendingEntry.EscrowBEEFHex = origBEEF

	// Now run OnPaymentTxMined with valid escrow BEEF -> MUST broadcast Buy(k) on-chain and settle!
	sellerBroker.OnPaymentTxMined(ctx, escrowTxID, height, mp)

	spendTx, vin, found, fErr := sharedPort.FindSpendingTx(ctx, escrowTxID, 0)
	if fErr != nil || !found || spendTx == nil {
		t.Fatalf("expected OnPaymentTxMined to broadcast DisclosureSale.Buy(k) spending %s:0, got found=%v err=%v", escrowTxID, found, fErr)
	}
	extractedKey, eErr := contracts.ExtractRevealedKeyFromBuyTx(spendTx, vin, saleOffer.Config.HWrapped)
	if eErr != nil {
		t.Fatalf("ExtractRevealedKeyFromBuyTx: %v", eErr)
	}
	if hex.EncodeToString(extractedKey) != hex.EncodeToString(revealKey[:]) {
		t.Fatalf("extracted key mismatch: got %x want %x", extractedKey, revealKey)
	}
	deal, gErr := sellerMem.DB().GetDeal("sell-" + offerID)
	if gErr != nil || deal.Status != "settled" {
		t.Fatalf("expected seller deal settled after OnPaymentTxMined Buy(k), got status=%q err=%v", deal.Status, gErr)
	}
}

// TestRCA_S6_BboxDuplicateNonceRejected proves handleBboxMessage rejects replayed signed bbox
// frames carrying an already-processed (SenderPub, Nonce) pair.
func TestRCA_S6_BboxDuplicateNonceRejected(t *testing.T) {
	ctx := context.Background()
	_, _, buyerBroker := newTestNodeForEscrow(t, nil)
	_, _, sellerBroker := newTestNodeForEscrow(t, nil)

	offerID := "off-bbox-replay-1"
	dealID := "want-bbox-replay-1"
	replyCh := make(chan PurchaseResponse, 4)
	buyerBroker.mu.Lock()
	buyerBroker.bboxReplies[offerID] = replyCh
	buyerBroker.bboxExpected[offerID] = sellerBroker.PubHex()
	buyerBroker.bboxWantByOffer[offerID] = dealID
	buyerBroker.mu.Unlock()

	bodyRaw, _ := json.Marshal(PurchaseResponse{
		OK:            true,
		Status:        "settled",
		OfferID:       offerID,
		SellerPub:     sellerBroker.PubHex(),
		ContentKeyHex: strings.Repeat("11", 32),
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
		Nonce:        "fixed-replay-nonce-1",
		Body:         sealed,
	}
	sig, err := bstack.SignArtifactWithSigner(sellerBroker.engine.signer, msg.Signable())
	if err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	msg.Sig = sig

	if err := buyerBroker.handleBboxMessage(ctx, "http://hub.local", msg); err != nil {
		t.Fatalf("first handleBboxMessage: %v", err)
	}
	if len(replyCh) != 1 {
		t.Fatalf("expected 1 delivered reply on first frame, got %d", len(replyCh))
	}
	// Replay exact same signed message -> MUST be dropped by hasSeenBboxNonce!
	if err := buyerBroker.handleBboxMessage(ctx, "http://hub.local", msg); err != nil {
		t.Fatalf("replayed handleBboxMessage: %v", err)
	}
	if len(replyCh) != 1 {
		t.Fatalf("expected duplicate bbox nonce replay to be rejected (len=1), got len=%d", len(replyCh))
	}
}

// TestFriction_7_3_ArmSellReadinessFields verifies ArmSell returns additive quotingReady and
// blockedReasons fields while preserving flat SellListing JSON compatibility.
func TestFriction_7_3_ArmSellReadinessFields(t *testing.T) {
	ctx := context.Background()
	_, eng, broker := newTestNodeForEscrow(t, nil)

	// Without seller policy approved -> quotingReady=false with seller_policy_needed.
	res, err := broker.ArmSell(ctx, SellInput{Shard: "research", Domains: []string{"ai"}, PriceSats: 150})
	if err != nil {
		t.Fatalf("ArmSell(unapproved): %v", err)
	}
	if res.QuotingReady {
		t.Fatalf("expected QuotingReady=false before seller policy approval, got %+v", res)
	}
	if !strings.Contains(strings.Join(res.BlockedReasons, ","), "seller_policy_needed") {
		t.Fatalf("expected blockedReasons to include seller_policy_needed, got %v", res.BlockedReasons)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal(ArmSellResult): %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["shard"] != "research" || m["quotingReady"] != false {
		t.Fatalf("expected flat JSON with shard=research and quotingReady=false, got %s", string(raw))
	}

	// Approve seller policy -> quotingReady=true with zero blockedReasons.
	if _, err := ApprovePolicyPreset(broker.mem.DB(), eng.Policy(), "seller", nil, "test"); err != nil {
		t.Fatalf("ApprovePolicyPreset(seller): %v", err)
	}
	res2, err := broker.ArmSell(ctx, SellInput{Shard: "research", Domains: []string{"ai"}, PriceSats: 150})
	if err != nil {
		t.Fatalf("ArmSell(approved): %v", err)
	}
	if !res2.QuotingReady || len(res2.BlockedReasons) != 0 {
		t.Fatalf("expected QuotingReady=true after seller policy approval, got %+v", res2)
	}
}

// TestFriction_7_10_SellToHubSurfacesHubReason verifies SellToHub parses and surfaces the Hub's
// JSON error reason on HTTP >= 300 instead of returning only the status code.
func TestFriction_7_10_SellToHubSurfacesHubReason(t *testing.T) {
	ctx := context.Background()
	mem, _, broker := newTestNodeForEscrow(t, nil)
	if _, err := mem.Remember(ctx, memory.RememberInput{Shard: "ops", Text: "duplicate bounty test fact", AutoAnchor: true}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"duplicate fragment HDP"}`))
	}))
	defer srv.Close()

	_, err := broker.SellToHub(ctx, srv.URL, "ops", "ops", 0)
	if err == nil {
		t.Fatal("expected error on HTTP 400 bounty rejection")
	}
	if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "duplicate fragment HDP") {
		t.Fatalf("expected error to include HTTP 400 and Hub JSON reason, got: %v", err)
	}
}
