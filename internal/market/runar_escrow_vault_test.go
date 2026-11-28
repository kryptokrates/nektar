package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

func opKeySigner(k *ec.PrivateKey) contracts.CovenantSighashSigner {
	return func(_ context.Context, _ []byte, sighash32 []byte) ([]byte, []byte, error) {
		sig, err := contracts.SignDigestLowS(k, sighash32, sighash.AllForkID)
		if err != nil {
			return nil, nil, err
		}
		return sig, k.PubKey().Compressed(), nil
	}
}

func newTestNodeForEscrow(t *testing.T, sharedPort *chain.Port) (*memory.Service, *Engine, *Broker) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(dir + "/nektar.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	port := sharedPort
	if port == nil {
		port, err = chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir})
		if err != nil {
			t.Fatalf("chain.Open: %v", err)
		}
		t.Cleanup(func() { _ = port.Close() })
	}

	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	pol := DefaultPolicy()
	eng := NewEngine(mem, pol)
	disc := discovery.New(mem, nil, nil)
	brk := NewBroker(mem, eng, disc, BrokerConfig{})
	pol.ZeroConfMaxSats = 10_000
	pol.SetEscrowMode(EscrowModeAlways, 1)
	return mem, eng, brk
}

// TestVaultComposedDisclosureSaleEscrowBuyAndGhostExtractE2E proves end-to-end on real BSV testchain:
//  1. Cold operator deploys NektarAgentVault -> Buyer funds DisclosureSale escrow DIRECTLY from
//     NektarAgentVault.AgentSpend in 1 atomic tx (Output 0 = continued vault, Output 1 = DisclosureSale escrow!).
//  2. Seller HandlePurchase verifies the DisclosureSale locking script, executes DisclosureSale.Buy(k)
//     on-chain (passing real post-Chronicle Script VM & testchain!), and returns ClaimTxID + ClaimTxBEEFHex.
//  3. Even if Seller drops the HTTP response after broadcasting Buy(k), ReconcileEscrowObligations
//     scans the chain, extracts `k` via ExtractRevealedKeyFromBuyTx, decrypts the BRC-369 ciphertext,
//     and fulfills the buyer's reveal_owed obligation!
func TestVaultComposedDisclosureSaleEscrowBuyAndGhostExtractE2E(t *testing.T) {
	ctx := context.Background()
	// Single shared testchain so Seller's Buy(k) tx spends Buyer's vault-funded DisclosureSale UTXO directly on-chain!
	sharedDir := t.TempDir()
	sharedPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: sharedDir})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer sharedPort.Close()

	buyerMem, _, buyerBroker := newTestNodeForEscrow(t, sharedPort)
	sellerMem, sellerEng, sellerBroker := newTestNodeForEscrow(t, sharedPort)

	// Seller commits a secret knowledge fragment and prepares a signed KeyedSaleOffer.
	secretText := "Confidential BSV L1 covenant settlement invariant #42"
	rem, err := sellerMem.Remember(ctx, memory.RememberInput{Shard: "alpha", Text: secretText, AutoAnchor: true})
	if err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	saleOffer, revealKey, chirpURL, err := sellerEng.PrepareDirectSale(ctx, "alpha", rem.Seq, 1200, buyerBroker.PubHex())
	if err != nil {
		t.Fatalf("PrepareDirectSale: %v", err)
	}
	offerID := "off-escrow-e2e-1"
	dealID := "deal-escrow-e2e-1"
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

	// 1. Cold Operator deploys a two-key NektarAgentVault (20,000 sats, PerTxCap=5,000, WindowCap=10,000).
	operatorKey, _ := ec.NewPrivateKey()
	opPubHex := hex.EncodeToString(operatorKey.PubKey().Compressed())
	vault0, err := DeployAgentVault(ctx, buyerMem.DB(), sharedPort, opPubHex, 20_000, 5_000, 10_000, 144, 600)
	if err != nil {
		t.Fatalf("DeployAgentVault: %v", err)
	}

	// 2. Buyer builds DisclosureSaleParams and funds the escrow directly from NektarAgentVault in 1 atomic tx!
	sellerPKH, _ := sharedPort.FundPKH(ctx)
	depPKH, _ := sharedPort.FundPKH(ctx)
	hWrapped, _ := hex.DecodeString(saleOffer.Config.HWrapped)
	curH, _ := sharedPort.Height(ctx)
	escrowParams := contracts.DisclosureSaleParams{
		SellerPKH:    sellerPKH,
		Price:        1200,
		HWrapped:     hWrapped,
		RefundAfter:  uint64(curH) + 5,
		MaxFee:       500,
		DepositorPKH: depPKH,
	}
	escrowLock, err := contracts.EncodeDisclosureSaleLockingScript(escrowParams)
	if err != nil {
		t.Fatalf("EncodeDisclosureSaleLockingScript: %v", err)
	}
	escrowSats := escrowParams.RequiredEscrowSatoshis() + 100 // 1801 sats

	vaultSpendTx, vaultSpendBEEF, escrowVout, _, vault1, err := SpendActiveVault(ctx, buyerMem.DB(), sharedPort, escrowLock, escrowSats)
	if err != nil {
		t.Fatalf("SpendActiveVault -> DisclosureSale failed: %v", err)
	}
	if escrowVout != 1 {
		t.Fatalf("expected DisclosureSale at vout 1 (vout 0 is continued vault), got %d", escrowVout)
	}
	if vault1.State.SpentInWindow != escrowSats || vault1.Satoshis >= vault0.Satoshis {
		t.Fatalf("vault state did not advance properly: %+v", vault1)
	}

	// Record open buyer reveal_owed obligation with full EscrowObligationPayload (simulating BuyerDeal before Seller replies).
	_ = buyerMem.DB().SaveDeal(store.DealRecord{
		DealID:          dealID,
		Role:            "buyer",
		Status:          "paid_awaiting_reveal",
		PaymentTxID:     vaultSpendTx.TxID().String(),
		CounterpartyPub: sellerBroker.PubHex(),
		PriceSats:       1200,
	})
	if err := RecordRevealOwedWithEscrow(buyerMem.DB(), dealID, sellerBroker.PubHex(), vaultSpendTx.TxID().String(), 1200, time.Now().Add(time.Hour).Unix(), &EscrowObligationPayload{
		OfferID:      offerID,
		EscrowTxID:   vaultSpendTx.TxID().String(),
		EscrowVout:   escrowVout,
		EscrowBEEF:   hex.EncodeToString(vaultSpendBEEF),
		SellerPKH:    hex.EncodeToString(sellerPKH),
		DepositorPKH: hex.EncodeToString(depPKH),
		Price:        1200,
		HWrapped:     saleOffer.Config.HWrapped,
		RefundAfter:  escrowParams.RefundAfter,
		MaxFee:       escrowParams.MaxFee,
	}); err != nil {
		t.Fatalf("RecordRevealOwedWithEscrow: %v", err)
	}

	// 3. Seller HandlePurchase validates the DisclosureSale UTXO and broadcasts DisclosureSale.Buy(k) on-chain!
	pReq := PurchaseRequest{
		OfferID:           offerID,
		WantID:            dealID,
		BuyerPub:          buyerBroker.PubHex(),
		TxID:              vaultSpendTx.TxID().String(),
		PaymentBEEFHex:    hex.EncodeToString(vaultSpendBEEF),
		OutputIndex:       escrowVout,
		EscrowMode:        true,
		DepositorPKH:      hex.EncodeToString(depPKH),
		EscrowSellerPKH:   hex.EncodeToString(sellerPKH),
		EscrowRefundAfter: escrowParams.RefundAfter,
		EscrowMaxFee:      escrowParams.MaxFee,
	}
	buyerBroker.signPurchaseRequest(&pReq)
	resp, err := sellerBroker.HandlePurchase(ctx, pReq)
	if err != nil {
		t.Fatalf("HandlePurchase(escrow): %v", err)
	}
	if !resp.OK || resp.Status != "settled" || resp.ClaimTxID == "" || resp.ClaimTxBEEFHex == "" {
		t.Fatalf("expected settled escrow PurchaseResponse with ClaimTxID + ClaimTxBEEFHex, got %+v", resp)
	}

	// Verify ClaimTxBEEF directly carries the real DisclosureSale.Buy(k) tx and yields k via ExtractRevealedKeyFromBuyTx!
	claimRaw, _ := hex.DecodeString(resp.ClaimTxBEEFHex)
	_, claimTx, _, err := guard.ParseBEEF(claimRaw, guard.DefaultBound)
	if err != nil || claimTx == nil {
		t.Fatalf("parse ClaimTxBEEF: %v", err)
	}
	extracted, err := contracts.ExtractRevealedKeyFromBuyTx(claimTx, 0, saleOffer.Config.HWrapped)
	if err != nil {
		t.Fatalf("ExtractRevealedKeyFromBuyTx: %v", err)
	}
	plain, err := contracts.ClaimKeyedSale(saleOffer, extracted)
	if err != nil {
		t.Fatalf("ClaimKeyedSale with extracted key failed: err=%v", err)
	}
	decCap, err := memrepo.DecodeCapsule(plain)
	if err != nil || len(decCap.Fragments) == 0 || string(decCap.Fragments[0].Data) != secretText {
		t.Fatalf("ClaimKeyedSale extracted .nkt capsule mismatch: cap=%+v err=%v", decCap, err)
	}

	// 4. Ghost-HTTP recovery drill: pretend Buyer NEVER received Seller's HTTP response!
	//    ReconcileEscrowObligations discovers Buy(k) on-chain, extracts k, updates the deal, and fulfills reveal_owed!
	rep, err := ReconcileEscrowObligations(ctx, buyerMem.DB(), sharedPort, time.Now().Unix())
	if err != nil {
		t.Fatalf("ReconcileEscrowObligations: %v", err)
	}
	if rep.KeysExtractedOnChain != 1 || rep.Fulfilled != 1 {
		t.Fatalf("expected 1 on-chain key extraction, got %+v", rep)
	}
	dealAfter, err := buyerMem.DB().GetDeal(dealID)
	if err != nil || dealAfter.ClaimKeyHex == "" {
		t.Fatalf("expected extracted ClaimKeyHex persisted on deal, got %+v err=%v", dealAfter, err)
	}
	kBytes, _ := hex.DecodeString(dealAfter.ClaimKeyHex)
	plain2, err := contracts.ClaimKeyedSale(saleOffer, kBytes)
	if err != nil {
		t.Fatalf("ClaimKeyedSale after ReconcileEscrowObligations failed: err=%v", err)
	}
	decCap2, err := memrepo.DecodeCapsule(plain2)
	if err != nil || len(decCap2.Fragments) == 0 || string(decCap2.Fragments[0].Data) != secretText {
		t.Fatalf("decrypted .nkt capsule mismatch after ReconcileEscrowObligations: cap=%+v err=%v", decCap2, err)
	}
}

// TestDisclosureSaleSellerGhostAutoRefundE2E proves that when a Seller completely ghosts
// after Buyer funds a DisclosureSale escrow, ReconcileEscrowObligations refuses premature refunds
// and automatically broadcasts DisclosureSale.Refund() once RefundAfter height is reached!
func TestDisclosureSaleSellerGhostAutoRefundE2E(t *testing.T) {
	ctx := context.Background()
	buyerMem, _, _ := newTestNodeForEscrow(t, nil)
	port := buyerMem.Chain()

	sellerPriv, _ := ec.NewPrivateKey()
	sellerPKH := core.Hash160(sellerPriv.PubKey().Compressed())
	depPKH, err := port.FundPKH(ctx)
	if err != nil {
		t.Fatalf("FundPKH: %v", err)
	}
	offer, err := contracts.CreateKeyedSaleOffer(sellerPriv, []byte("ghosted content"), 2000, 9999999999, 500, uint64(time.Now().Add(time.Hour).Unix()), "", hex.EncodeToString(core.SHA256([]byte("leaf"))), false)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	hWrapped, _ := hex.DecodeString(offer.Config.HWrapped)
	curH, err := port.Height(ctx)
	if err != nil {
		t.Fatalf("Height: %v", err)
	}
	refundHeight := uint64(curH) + 3
	params := contracts.DisclosureSaleParams{
		SellerPKH:    sellerPKH,
		Price:        2000,
		HWrapped:     hWrapped,
		RefundAfter:  refundHeight,
		MaxFee:       500,
		DepositorPKH: depPKH,
	}
	escrowLock, err := contracts.EncodeDisclosureSaleLockingScript(params)
	if err != nil {
		t.Fatalf("EncodeDisclosureSaleLockingScript: %v", err)
	}
	escrowSats := params.RequiredEscrowSatoshis() + 100
	escrowTx, escrowBEEF, fee, spend, err := port.BuildFundedTxWithMeta(ctx, []*transaction.TransactionOutput{{
		Satoshis:      escrowSats,
		LockingScript: escrowLock,
	}}, "", nil)
	if err != nil {
		t.Fatalf("BuildFundedTxWithMeta: %v", err)
	}
	if _, err := port.Broadcast(ctx, escrowTx, fee, spend); err != nil {
		t.Fatalf("Broadcast escrow: %v", err)
	}
	if fresh, bErr := chain.SafeAtomicBEEF(escrowTx); bErr == nil && len(fresh) > 0 {
		escrowBEEF = fresh
	}

	dealID := "deal-ghost-refund-1"
	_ = buyerMem.DB().SaveDeal(store.DealRecord{
		DealID:          dealID,
		Role:            "buyer",
		Status:          "paid_awaiting_reveal",
		PaymentTxID:     escrowTx.TxID().String(),
		CounterpartyPub: offer.SellerPub,
		PriceSats:       2000,
	})
	if err := RecordRevealOwedWithEscrow(buyerMem.DB(), dealID, offer.SellerPub, escrowTx.TxID().String(), 2000, time.Now().Add(time.Hour).Unix(), &EscrowObligationPayload{
		OfferID:      "off-ghost-1",
		EscrowTxID:   escrowTx.TxID().String(),
		EscrowVout:   0,
		EscrowBEEF:   hex.EncodeToString(escrowBEEF),
		SellerPKH:    hex.EncodeToString(sellerPKH),
		DepositorPKH: hex.EncodeToString(depPKH),
		Price:        2000,
		HWrapped:     offer.Config.HWrapped,
		RefundAfter:  refundHeight,
		MaxFee:       500,
	}); err != nil {
		t.Fatalf("RecordRevealOwedWithEscrow: %v", err)
	}

	// Before RefundAfter height: MUST NOT refund yet!
	repEarly, err := ReconcileEscrowObligations(ctx, buyerMem.DB(), port, time.Now().Unix())
	if err != nil {
		t.Fatalf("ReconcileEscrowObligations(early): %v", err)
	}
	if repEarly.RefundsBroadcast != 0 || repEarly.StillOpen != 1 {
		t.Fatalf("expected premature refund to stay open, got %+v", repEarly)
	}

	// Advance chain by 3 blocks so currentHeight >= RefundAfter.
	fundAddr, _ := port.FundAddress()
	if _, err := port.TestChain().Generate(3, fundAddr); err != nil {
		t.Fatalf("testchain Generate: %v", err)
	}

	// Now ReconcileEscrowObligations MUST automatically broadcast DisclosureSale.Refund()!
	repRefund, err := ReconcileEscrowObligations(ctx, buyerMem.DB(), port, time.Now().Unix())
	if err != nil {
		t.Fatalf("ReconcileEscrowObligations(after RefundAfter): %v", err)
	}
	if repRefund.RefundsBroadcast != 1 || repRefund.Fulfilled != 1 || len(repRefund.RefundTxIDs) != 1 {
		t.Fatalf("expected automatic on-chain Refund broadcast, got %+v", repRefund)
	}
	dealAfter, err := buyerMem.DB().GetDeal(dealID)
	if err != nil || dealAfter.Status != "refunded" {
		t.Fatalf("expected deal status=refunded, got %+v err=%v", dealAfter, err)
	}
	// Verify the refund tx exists on testchain and pays DepositorPKH!
	refundTx := port.Tx(repRefund.RefundTxIDs[0])
	if refundTx == nil || len(refundTx.Outputs) != 1 {
		t.Fatalf("expected mined/mempool refund tx %s on testchain", repRefund.RefundTxIDs[0])
	}
	gotPKH, ok := contracts.ExtractP2PKH(*refundTx.Outputs[0].LockingScript)
	if !ok || !bytes.Equal(gotPKH, depPKH) {
		t.Fatalf("refund output did not pay buyer DepositorPKH")
	}
}

// TestAgentVaultOperatorPauseRotateAndSweepE2E tests the full live two-key vault lifecycle on testchain:
// Deploy -> AgentSpend within caps -> PerTxCap rejection -> Cold Operator Pause locks out Agent ->
// Cold Operator Resume + Rotate AgentPKH locks out old Agent -> Cold Operator Sweep closes vault!
func TestAgentVaultOperatorPauseRotateAndSweepE2E(t *testing.T) {
	ctx := context.Background()
	mem, _, _ := newTestNodeForEscrow(t, nil)
	port := mem.Chain()
	db := mem.DB()

	opKey, _ := ec.NewPrivateKey()
	opPubHex := hex.EncodeToString(opKey.PubKey().Compressed())
	opPKH := core.Hash160(opKey.PubKey().Compressed())

	destKey, _ := ec.NewPrivateKey()
	destPKH := core.Hash160(destKey.PubKey().Compressed())
	destLock, _ := contracts.P2PKHLockingScript(destPKH)

	// 1. Deploy 15,000-sat vault with PerTxCap=2,000, WindowCap=5,000.
	if _, err := DeployAgentVault(ctx, db, port, opPubHex, 15_000, 2_000, 5_000, 144, 600); err != nil {
		t.Fatalf("DeployAgentVault: %v", err)
	}

	// 2. AgentSpend 1,500 sats succeeds on testchain!
	if _, _, _, _, rec, err := SpendActiveVault(ctx, db, port, destLock, 1_500); err != nil || rec.State.SpentInWindow != 1_500 {
		t.Fatalf("SpendActiveVault(1500) failed: rec=%+v err=%v", rec, err)
	}

	// 3. AgentSpend 2,500 sats (> PerTxCap 2,000) is rejected!
	if _, _, _, _, _, err := SpendActiveVault(ctx, db, port, destLock, 2_500); err == nil {
		t.Fatal("expected PerTxCap rejection on SpendActiveVault(2500)")
	}

	// 4. Cold operator pauses vault -> hot agent is immediately locked out!
	pausedTrue := true
	if rec, err := ApplyOperatorVaultControl(ctx, db, port, contracts.OperatorControlUpdate{NewPaused: &pausedTrue}, opKeySigner(opKey)); err != nil || !rec.State.Paused {
		t.Fatalf("ApplyOperatorVaultControl(pause): rec=%+v err=%v", rec, err)
	}
	if _, _, _, _, _, err := SpendActiveVault(ctx, db, port, destLock, 100); err == nil {
		t.Fatal("expected SpendActiveVault to fail while vault is paused")
	}

	// 5. Cold operator resumes + rotates AgentPKH to a brand-new key -> old local hot agent is locked out!
	newAgentKey, _ := ec.NewPrivateKey()
	newAgentPKH := core.Hash160(newAgentKey.PubKey().Compressed())
	pausedFalse := false
	if rec, err := ApplyOperatorVaultControl(ctx, db, port, contracts.OperatorControlUpdate{
		NewAgentPKH: newAgentPKH,
		NewPaused:   &pausedFalse,
	}, opKeySigner(opKey)); err != nil || rec.State.Paused || !bytes.Equal(rec.State.AgentPKH, newAgentPKH) {
		t.Fatalf("ApplyOperatorVaultControl(rotate+resume): rec=%+v err=%v", rec, err)
	}
	if _, _, _, _, _, err := SpendActiveVault(ctx, db, port, destLock, 100); err == nil {
		t.Fatal("expected old hot agent to fail after Operator rotated AgentPKH")
	}

	// 6. Cold operator sweeps all remaining funds to opPKH and closes vault cleanly!
	sweepTxID, sweptSats, err := SweepActiveVault(ctx, db, port, opPKH, opKeySigner(opKey))
	if err != nil || sweepTxID == "" || sweptSats < 10_000 {
		t.Fatalf("SweepActiveVault failed: txid=%s sats=%d err=%v", sweepTxID, sweptSats, err)
	}
	after, _, err := LoadActiveVault(db)
	if err != nil || after != nil {
		t.Fatalf("expected active vault cleared after sweep, got %+v err=%v", after, err)
	}
	_ = json.Marshal
}
