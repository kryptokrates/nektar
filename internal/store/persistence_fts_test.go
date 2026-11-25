package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFTS5SearchAndForgetSync(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	r1, _, err := db.AppendFragment(FragmentRecord{
		Shard: "ops", HDP: "hdp-chronicle", Leaf: "leaf-1",
		Data:       []byte("BSV Chronicle activation occurred at block 943816."),
		Provenance: []byte("prov-1"),
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = db.AppendFragment(FragmentRecord{
		Shard: "ops", HDP: "hdp-restate", Leaf: "leaf-2",
		Data:       []byte("Restate Go SDK orchestrates durable knowledge trades."),
		Provenance: []byte("prov-2"),
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	hits, err := db.SearchFTS("ops", "Chronicle activation", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(hits) != 1 || hits[0].Seq != r1.Seq {
		t.Fatalf("expected 1 hit seq=%d, got %+v", r1.Seq, hits)
	}

	// Forgetting the fragment must remove it from FTS results immediately.
	if err := db.ForgetFragment("ops", r1.Seq); err != nil {
		t.Fatalf("ForgetFragment: %v", err)
	}
	hitsAfter, err := db.SearchFTS("ops", "Chronicle", 10)
	if err != nil {
		t.Fatalf("SearchFTS after forget: %v", err)
	}
	if len(hitsAfter) != 0 {
		t.Fatalf("expected 0 hits after ForgetFragment, got %+v", hitsAfter)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist.sqlite")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 1. Stable key survives reopen.
	k1, err := db.LoadOrCreateStableKey("node_market_key_v1", func() ([]byte, error) {
		return []byte("0123456789abcdef0123456789abcdef"), nil
	})
	if err != nil || len(k1) != 32 {
		t.Fatalf("LoadOrCreateStableKey: %v", err)
	}

	// 2. BorgGroup persistence.
	if err := db.SaveBorgGroup(BorgGroupRecord{
		GroupID: "sec", Name: "Security", CurrentEpoch: 2,
		MembersJSON: []byte(`{"02aa":"owner"}`), RevokedJSON: []byte(`{}`),
		EpochsJSON: []byte(`{}`), EpochMembersJSON: []byte(`{}`),
	}); err != nil {
		t.Fatalf("SaveBorgGroup: %v", err)
	}

	// 3. PendingSale persistence.
	if err := db.SavePendingSale(PendingSaleRecord{
		OfferID: "offer-1", WantID: "want-1", BuyerPub: "02bb", Shard: "ops", Seq: 1,
		SaleOfferJSON: []byte(`{"priceSats":200}`), RevealKeyHex: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("SavePendingSale: %v", err)
	}

	// 4. PaymentClaim + UnmatchedWant + DailySpend persistence.
	if err := db.SavePaymentClaim("tx-claim-1", []string{"in1:0"}); err != nil {
		t.Fatalf("SavePaymentClaim: %v", err)
	}
	if err := db.RecordUnmatchedWant(UnmatchedWantRecord{WantID: "w-q1", Domain: "quantum", Query: "stabilizer codes?", MaxSats: 500}); err != nil {
		t.Fatalf("RecordUnmatchedWant: %v", err)
	}
	if _, err := db.AddDailySpend("test_daily", 350); err != nil {
		t.Fatalf("AddDailySpend: %v", err)
	}
	_ = db.Close()

	// Reopen and verify all records survived!
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer db2.Close()

	k2, err := db2.LoadOrCreateStableKey("node_market_key_v1", func() ([]byte, error) {
		t.Fatal("generator must not run when key already persisted")
		return nil, nil
	})
	if err != nil || string(k2) != string(k1) {
		t.Fatalf("stable key mismatch: got %x want %x", k2, k1)
	}
	groups, err := db2.ListBorgGroups()
	if err != nil || len(groups) != 1 || groups[0].GroupID != "sec" || groups[0].CurrentEpoch != 2 {
		t.Fatalf("unexpected borg groups: %+v err=%v", groups, err)
	}
	sales, err := db2.ListPendingSales(time.Now().Unix())
	if err != nil || len(sales) != 1 || sales[0].OfferID != "offer-1" {
		t.Fatalf("unexpected pending sales: %+v err=%v", sales, err)
	}
	claims, err := db2.ListPaymentClaims()
	if err != nil || len(claims) != 1 || claims[0].TxID != "tx-claim-1" {
		t.Fatalf("unexpected payment claims: %+v err=%v", claims, err)
	}
	uw, err := db2.ListUnmatchedWants(10)
	if err != nil || len(uw) != 1 || uw[0].Domain != "quantum" {
		t.Fatalf("unexpected unmatched wants: %+v err=%v", uw, err)
	}
	if spent, err := db2.DailySpend("test_daily"); err != nil || spent != 350 {
		t.Fatalf("unexpected daily spend: %d err=%v", spent, err)
	}
}
