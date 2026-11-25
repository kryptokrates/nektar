package store

import (
	"testing"
	"time"
)

func TestForgetFragmentAtomicallyShredsFTS(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec, _, err := db.AppendFragment(FragmentRecord{
		Shard:      "secrets",
		HDP:        "hdp-shred-1",
		Leaf:       "leaf-shred-1",
		Data:       []byte("confidential launch code zephyr99"),
		Provenance: []byte("prov"),
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := db.SearchFTS("secrets", "zephyr99", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("expected 1 FTS hit before forget, got %v err=%v", hits, err)
	}
	if err := db.ForgetFragment("secrets", rec.Seq); err != nil {
		t.Fatalf("ForgetFragment: %v", err)
	}
	hitsAfter, err := db.SearchFTS("secrets", "zephyr99", 10)
	if err != nil || len(hitsAfter) != 0 {
		t.Fatalf("expected 0 FTS hits after forget, got %v err=%v", hitsAfter, err)
	}
	got, err := db.GetFragment("secrets", rec.Seq)
	if err != nil || !got.Forgotten || len(got.Data) != 0 {
		t.Fatalf("expected shredded forgotten record, got %+v err=%v", got, err)
	}
}

func TestReleaseSpendReservationCrossMidnight(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert a reservation from yesterday UTC + today UTC in kv.
	yesterday := time.Now().UTC().Add(-25 * time.Hour)
	yKey := "market_daily_spend:" + yesterday.Format("2006-01-02")
	tKey := "market_daily_spend:" + time.Now().UTC().Format("2006-01-02")
	if err := db.SetJSON(yKey, uint64(500)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJSON(tKey, uint64(400)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO spend_reservations (op_id, attempt, amount_sats, status, created_at) VALUES ('res-y', 1, 500, 'reserved', ?)`, yesterday.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseSpendReservation("res-y", 1); err != nil {
		t.Fatalf("ReleaseSpendReservation: %v", err)
	}
	var ySpent, tSpent uint64
	_, _ = db.GetJSON(yKey, &ySpent)
	_, _ = db.GetJSON(tKey, &tSpent)
	if ySpent != 0 || tSpent != 400 {
		t.Fatalf("cross-midnight release decremented wrong day: yesterday=%d (want 0), today=%d (want 400)", ySpent, tSpent)
	}
}

func TestApprovalAwakeableSingleUse(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().Unix()
	if err := db.SaveApproval(ApprovalRecord{
		ID:          "appr-awk-1",
		DealID:      "deal-1",
		Reason:      "over cap",
		Status:      "pending",
		AwakeableID: "awk_123",
		ExpiresAt:   now + 600,
		CreatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	appr, err := db.MarkApprovalResolvedAwakeable("appr-awk-1", true)
	if err != nil || appr.Status != "consumed" {
		t.Fatalf("expected consumed status, got %+v err=%v", appr, err)
	}
	if err := db.ConsumeApprovalOverride("appr-awk-1"); err == nil {
		t.Fatal("consumed awakeable approval was replayed via ConsumeApprovalOverride!")
	}
}
