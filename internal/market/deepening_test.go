package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

func testSigningPubHex(t *testing.T, ctx context.Context, m *memory.Service) string {
	t.Helper()
	pub, err := m.Chain().SigningPubKey(ctx)
	if err != nil {
		t.Fatalf("SigningPubKey: %v", err)
	}
	return hex.EncodeToString(pub.Compressed())
}

func TestTripleEntryDealReceiptBilateralSelectiveDisclosureAndTamper(t *testing.T) {
	ctx := context.Background()
	buyerMem := newTestMemory(t)
	sellerMem := newTestMemory(t)
	buyerPub := testSigningPubHex(t, ctx, buyerMem)
	sellerPub := testSigningPubHex(t, ctx, sellerMem)

	// Also attach a signed Cedar DecisionReceipt to the commercial receipt.
	pc := sellerMem.PolicyCommit()
	cedarRcp := policy.MakeReceipt(
		policy.Principal{Type: "Agent", ID: "buyer-1", Level: 0},
		"disclose",
		policy.Resource{Type: "Fragment", ID: "ops#1", Level: 0, Shard: "ops", Class: "ops"},
		pc,
		true,
	)
	if err := policy.SignDecisionReceipt(sellerPub, func(d []byte) ([]byte, error) {
		return sellerMem.Chain().SignDigest(ctx, d)
	}, &cedarRcp); err != nil {
		t.Fatalf("SignDecisionReceipt: %v", err)
	}

	dr := DealReceipt{
		Version:         1,
		DealID:          "want-tea-1",
		OfferID:         "off-tea-1",
		BuyerPub:        buyerPub,
		SellerPub:       sellerPub,
		Domains:         []string{"ops"},
		PriceSats:       250,
		WantHash:        sha256CanonicalHex(map[string]string{"q": "SECRET_CONFIDENTIAL_QUERY"}),
		OfferDigestHash: "1111111111111111111111111111111111111111111111111111111111111111",
		BuyerPolicyHash: "2222222222222222222222222222222222222222222222222222222222222222",
		CedarDecision:   &cedarRcp,
		PaymentTxID:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		KeyCommitment:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		FragmentLeaf:    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		SettledAt:       time.Now().Unix(),
	}

	// Seller signs first -> unilateral passes (requireBilateral=false), bilateral fails.
	if err := SignDealReceiptSeller(ctx, sellerPub, sellerMem.Chain().SignDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptSeller: %v", err)
	}
	if vr := VerifyDealReceipt(dr, false); !vr.OK || !vr.SellerVerified || vr.Bilateral || !vr.CedarVerified {
		t.Fatalf("expected unilateral seller-verified receipt, got %+v", vr)
	}
	if vr := VerifyDealReceipt(dr, true); vr.OK {
		t.Fatal("expected requireBilateral=true to fail before buyer countersignature")
	}

	// Buyer countersigns -> bilateral passes AND nektardr1.* token roundtrips cleanly!
	if err := SignDealReceiptBuyer(ctx, buyerPub, buyerMem.Chain().SignDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptBuyer: %v", err)
	}
	tok, err := EncodeDealReceiptToken(dr)
	if err != nil || !strings.HasPrefix(tok, DealReceiptTokenPrefix) {
		t.Fatalf("EncodeDealReceiptToken: tok=%q err=%v", tok, err)
	}
	decoded, err := DecodeDealReceiptToken(tok)
	if err != nil {
		t.Fatalf("DecodeDealReceiptToken: %v", err)
	}
	if vr := VerifyDealReceipt(*decoded, true); !vr.OK || !vr.Bilateral || !vr.CedarVerified {
		t.Fatalf("expected bilateral verified receipt after token roundtrip, got %+v", vr)
	}

	// Selective disclosure for external accountant: disclose ONLY priceSats + paymentTxid + buyerPolicyHash!
	auditProof, err := decoded.SelectiveDisclose([]string{"priceSats", "paymentTxid", "buyerPolicyHash"})
	if err != nil {
		t.Fatalf("SelectiveDisclose: %v", err)
	}
	if err := core.VerifySelectiveDisclosure(decoded.FieldsRoot, auditProof); err != nil {
		t.Fatalf("VerifySelectiveDisclosure: %v", err)
	}
	if len(auditProof.Fields) != 3 {
		t.Fatalf("expected 3 disclosed audit fields, got %d", len(auditProof.Fields))
	}

	// Tampering with priceSats invalidates both FieldsRoot and signatures!
	tampered := *decoded
	tampered.PriceSats = 99999
	if vr := VerifyDealReceipt(tampered, false); vr.OK {
		t.Fatal("expected tampered priceSats receipt to fail verification")
	}
}

func TestOperatorPolicyKeySeparationFailClosed(t *testing.T) {
	ctx := context.Background()
	opMem := newTestMemory(t)
	rogueMem := newTestMemory(t)
	pol := DefaultPolicy()

	opPub := testSigningPubHex(t, ctx, opMem)
	roguePub := testSigningPubHex(t, ctx, rogueMem)
	pol.SetPinnedOperatorPub(opPub)

	// 1. Policy signed by the pinned Operator key activates cleanly.
	rec, err := ApprovePolicyPresetSigned(ctx, opMem.DB(), pol, "buyer", []string{"ops"}, "cold-op", nil, opPub, opMem.Chain().SignDigest)
	if err != nil {
		t.Fatalf("ApprovePolicyPresetSigned: %v", err)
	}
	if !pol.BuyerApproved || pol.ActivePolicyHash() != rec.PolicyHash || pol.TamperedPoliciesCount() != 0 {
		t.Fatalf("expected signed policy active; buyer=%v hash=%s tampered=%d", pol.BuyerApproved, pol.ActivePolicyHash(), pol.TamperedPoliciesCount())
	}

	// 2. Rogue/agent-signed policy under a different key MUST be rejected fail-closed!
	_ = opMem.DB().RevokeOperatorPolicy(rec.PolicyID)
	if _, err := ApprovePolicyPresetSigned(ctx, opMem.DB(), pol, "both", []string{"ops"}, "rogue-agent", nil, roguePub, rogueMem.Chain().SignDigest); err != nil {
		t.Fatalf("save rogue policy: %v", err)
	}
	if err := SyncPolicyFromDB(opMem.DB(), pol); err != nil {
		t.Fatalf("SyncPolicyFromDB: %v", err)
	}
	if pol.BuyerApproved || pol.SellerApproved || pol.MaxSatsPerDeal != 0 || pol.TamperedPoliciesCount() != 1 {
		t.Fatalf("expected pinned operator check to fail closed on rogue policy, got buyer=%v seller=%v max=%d tampered=%d",
			pol.BuyerApproved, pol.SellerApproved, pol.MaxSatsPerDeal, pol.TamperedPoliciesCount())
	}

	// 3. Tampering with MaxPerDeal in SQLite after valid signing MUST also fail closed!
	validRec, err := ApprovePolicyPresetSigned(ctx, opMem.DB(), pol, "buyer", []string{"ops"}, "cold-op", nil, opPub, opMem.Chain().SignDigest)
	if err != nil {
		t.Fatal(err)
	}
	tampered := validRec
	tampered.MaxPerDeal = 999_999_999 // attacker raises cap in SQLite!
	if err := opMem.DB().SaveOperatorPolicy(tampered); err != nil {
		t.Fatal(err)
	}
	if err := SyncPolicyFromDB(opMem.DB(), pol); err != nil {
		t.Fatal(err)
	}
	if pol.BuyerApproved || pol.MaxSatsPerDeal != 0 || pol.TamperedPoliciesCount() < 1 {
		t.Fatalf("expected SQLite-tampered policy to fail closed, got buyer=%v max=%d tampered=%d",
			pol.BuyerApproved, pol.MaxSatsPerDeal, pol.TamperedPoliciesCount())
	}
}

func TestObligationsLifecycleAndBookReconciliation(t *testing.T) {
	ctx := context.Background()
	mem := newTestMemory(t)
	db := mem.DB()
	now := time.Now().Unix()

	// 1. Buyer pays -> reveal_owed obligation created with past deadline -> reconciler marks overdue + refund_claimable!
	if err := RecordRevealOwed(db, "deal-ghost-1", "02seller", "tx-ghost-1", 400, now-60); err != nil {
		t.Fatalf("RecordRevealOwed: %v", err)
	}
	rep, err := ReconcileObligations(ctx, db, nil, now)
	if err != nil {
		t.Fatalf("ReconcileObligations: %v", err)
	}
	if rep.MarkedOverdue != 1 || rep.RefundClaimable != 1 {
		t.Fatalf("expected 1 overdue + 1 refund_claimable, got %+v", rep)
	}
	openOb, overdueOb, err := db.CountObligations()
	if err != nil || openOb != 1 || overdueOb != 1 {
		t.Fatalf("expected open=1 (refund_claimable) overdue=1 (reveal_owed), got open=%d overdue=%d err=%v", openOb, overdueOb, err)
	}

	// 2. Book reconciliation flags the overdue obligation as an anomaly and detects daily spend drift!
	bookRep, err := ReconcileBooks(db, nil, time.Now())
	if err != nil {
		t.Fatalf("ReconcileBooks: %v", err)
	}
	if bookRep.OK || bookRep.OverdueObligations != 1 {
		t.Fatalf("expected ReconcileBooks to flag overdue obligation, got %+v", bookRep)
	}

	// 3. Clean settled deal + matching daily spend + valid bilateral receipt passes reconciliation when no overdue obligations exist.
	cleanMem := newTestMemory(t)
	sellerMem := newTestMemory(t)
	cleanPub := testSigningPubHex(t, ctx, cleanMem)
	sellerPub := testSigningPubHex(t, ctx, sellerMem)
	dr := DealReceipt{
		Version:       1,
		DealID:        "deal-ok-1",
		OfferID:       "off-ok-1",
		BuyerPub:      cleanPub,
		SellerPub:     sellerPub,
		PriceSats:     150,
		PaymentTxID:   "tx-ok-1",
		KeyCommitment: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		SettledAt:     now,
	}
	_ = SignDealReceiptSeller(ctx, sellerPub, sellerMem.Chain().SignDigest, &dr)
	_ = SignDealReceiptBuyer(ctx, cleanPub, cleanMem.Chain().SignDigest, &dr)
	drJSON, _ := json.Marshal(dr)

	_ = cleanMem.DB().SaveDeal(store.DealRecord{
		DealID:          "deal-ok-1",
		Role:            "buyer",
		Status:          "settled",
		DealReceiptJSON: drJSON,
		PaymentTxID:     "tx-ok-1",
		CounterpartyPub: sellerPub,
		PriceSats:       150,
	})
	_, _ = cleanMem.DB().AddDailySpend("market_daily_spend", 150)
	_ = cleanMem.DB().RecordReputation(sellerPub, true, 150)

	cleanRep, err := ReconcileBooks(cleanMem.DB(), nil, time.Now())
	if err != nil {
		t.Fatalf("ReconcileBooks clean: %v", err)
	}
	if !cleanRep.OK || cleanRep.SettledBuyerDeals != 1 || cleanRep.SettledBuyerSats != 150 || cleanRep.BilateralReceipts != 1 {
		t.Fatalf("expected clean book reconciliation report, got %+v", cleanRep)
	}
}
