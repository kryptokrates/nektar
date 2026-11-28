package store

import (
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/nmu"
)

func TestStoreNMUStorageAndMultiModeSellListing(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// 1. Multi-mode SellListing persists and exposes independent EnabledModes.
	listing := SellListing{
		Shard:                   "market-ops",
		Domains:                 []string{"ops"},
		PriceSats:               5000,
		PricePerUnit:            true,
		QueryPriceSats:          15,
		SubscriptionPriceSats:   250,
		SubscriptionDurationSec: 7 * 86400,
		FreshnessClass:          "versioned",
		UsageRights:             "distill_resellable",
		MinAdvertisedGrade:      "B",
		Description:             "Multi-mode ops shard",
		Active:                  true,
	}
	if err := db.SaveSellListing(listing); err != nil {
		t.Fatalf("SaveSellListing: %v", err)
	}
	got, err := db.GetSellListing("market-ops")
	if err != nil || got == nil {
		t.Fatalf("GetSellListing: %v got=%+v", err, got)
	}
	if !got.PricePerUnit || got.QueryPriceSats != 15 || got.SubscriptionPriceSats != 250 || got.UsageRights != "distill_resellable" || got.MinAdvertisedGrade != "B" {
		t.Fatalf("SellListing NMU fields round-trip mismatch: %+v", got)
	}
	for _, mode := range []nmu.PricingMode{nmu.ModeSpot, nmu.ModePerQuery, nmu.ModeSubscription} {
		if !got.SupportsMode(string(mode)) {
			t.Fatalf("expected multi-mode listing to support %s", mode)
		}
	}

	// 2. FragmentNMUMeta persists and is atomically shredded by ForgetFragment.
	rec, _, err := db.AppendFragment(FragmentRecord{
		Shard:      "market-ops",
		HDP:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Leaf:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Data:       []byte("verified fact"),
		Provenance: []byte("{}"),
		Public:     true,
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("AppendFragment: %v", err)
	}
	if err := db.SaveFragmentNMUMeta(FragmentNMUMeta{
		Shard:          "market-ops",
		Seq:            rec.Seq,
		Grade:          "A",
		UnitCount:      5,
		FreshnessClass: "versioned",
		SubjectVersion: "go-1.27.4",
		UsageRights:    "distill_resellable",
	}); err != nil {
		t.Fatalf("SaveFragmentNMUMeta: %v", err)
	}
	meta, err := db.GetFragmentNMUMeta("market-ops", rec.Seq)
	if err != nil || meta == nil || meta.Grade != "A" || meta.UnitCount != 5 || meta.SubjectVersion != "go-1.27.4" {
		t.Fatalf("GetFragmentNMUMeta mismatch: %+v err=%v", meta, err)
	}
	if err := db.ForgetFragment("market-ops", rec.Seq); err != nil {
		t.Fatalf("ForgetFragment: %v", err)
	}
	shredded, err := db.GetFragmentNMUMeta("market-ops", rec.Seq)
	if err != nil || shredded != nil {
		t.Fatalf("expected fragment_nmu_meta shredded on ForgetFragment, got %+v err=%v", shredded, err)
	}

	// 3. ShardSubscription CRUD + unique payment_txid replay guard.
	now := time.Now().Unix()
	sub := ShardSubscription{
		SubID:       "sub-1",
		Shard:       "market-ops",
		BuyerPub:    "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SellerPub:   "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PriceSats:   250,
		PaymentTxID: "1111111111111111111111111111111111111111111111111111111111111111",
		UsageRights: "inference_only",
		StartedAt:   now,
		ExpiresAt:   now + 3600,
		Active:      true,
	}
	if err := db.CreateShardSubscription(sub); err != nil {
		t.Fatalf("CreateShardSubscription: %v", err)
	}
	// Duplicate payment_txid MUST fail closed!
	dup := sub
	dup.SubID = "sub-2"
	if err := db.CreateShardSubscription(dup); err == nil {
		t.Fatal("expected duplicate subscription payment_txid to fail closed")
	}
	active, err := db.ListActiveSubscriptions("market-ops", now+10)
	if err != nil || len(active) != 1 || active[0].SubID != "sub-1" {
		t.Fatalf("ListActiveSubscriptions mismatch: %+v err=%v", active, err)
	}
	expired, err := db.ListActiveSubscriptions("market-ops", now+4000)
	if err != nil || len(expired) != 0 {
		t.Fatalf("expected 0 active subs after expiry, got %+v", expired)
	}
}
