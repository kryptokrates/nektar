package store

import (
	"testing"
	"time"
)

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
