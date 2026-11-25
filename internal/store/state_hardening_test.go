package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClaimPaymentAtomicCrossInterfaceAndInputConflict covers F4:
// - Duplicate txid replay rejected atomically
// - Different txid spending the same UTXO outpoint (double-spend conflict) rejected atomically
// - ReleasePaymentClaim frees both txid and outpoint cleanly on pre-delivery failure
func TestClaimPaymentAtomicCrossInterfaceAndInputConflict(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Concurrent race on identical txid: exactly 1 goroutine wins!
	const workers = 12
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := db.ClaimPaymentAtomic("tx-pay-1", []string{"prevtx:0", "prevtx:1"}); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly 1 ClaimPaymentAtomic winner, got %d", wins.Load())
	}

	// Second txid attempting to reuse outpoint "prevtx:1" must fail with ErrPaymentClaimConflict!
	if err := db.ClaimPaymentAtomic("tx-pay-2", []string{"other:0", "prevtx:1"}); !errors.Is(err, ErrPaymentClaimConflict) {
		t.Fatalf("expected ErrPaymentClaimConflict on overlapping outpoint, got %v", err)
	}

	// Releasing tx-pay-1 frees both txid and outpoints.
	if err := db.ReleasePaymentClaim("tx-pay-1"); err != nil {
		t.Fatalf("ReleasePaymentClaim: %v", err)
	}
	if err := db.ClaimPaymentAtomic("tx-pay-2", []string{"prevtx:1"}); err != nil {
		t.Fatalf("expected claim to succeed after release, got %v", err)
	}
}

// TestDecideApprovalCASExpiryAndSingleUseConsume covers F9.
func TestDecideApprovalCASExpiryAndSingleUseConsume(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 1. Expired approval cannot be approved!
	if err := db.SaveApproval(ApprovalRecord{
		ID:          "appr-expired",
		DealID:      "deal-exp",
		Reason:      "over cap",
		Status:      "pending",
		AwakeableID: "prom_1exp",
		ExpiresAt:   time.Now().Add(-10 * time.Second).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DecideApproval("appr-expired", true); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("expected ErrApprovalNotPending on expired approval, got %v", err)
	}

	// 2. Concurrent approvals on live pending record: strictly 1 CAS winner!
	if err := db.SaveApproval(ApprovalRecord{
		ID:          "appr-live",
		DealID:      "deal-live",
		Reason:      "over cap",
		Status:      "pending",
		AwakeableID: "prom_1live",
		ExpiresAt:   time.Now().Add(5 * time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		approve := i%2 == 0
		go func() {
			defer wg.Done()
			if rec, err := db.DecideApproval("appr-live", approve); err == nil {
				if rec.AwakeableID != "prom_1live" {
					t.Errorf("expected AwakeableID preserved, got %q", rec.AwakeableID)
				}
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly 1 DecideApproval CAS winner, got %d", wins.Load())
	}

	// 3. ConsumeApprovalOverride is strictly single-use!
	if err := db.SaveApproval(ApprovalRecord{
		ID:        "appr-once",
		DealID:    "deal-once",
		Reason:    "once",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DecideApproval("appr-once", true); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if err := db.ConsumeApprovalOverride("appr-once"); err != nil {
		t.Fatalf("first ConsumeApprovalOverride failed: %v", err)
	}
	if err := db.ConsumeApprovalOverride("appr-once"); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("expected second ConsumeApprovalOverride to fail, got %v", err)
	}
}

// TestAtomicDailySpendReserveAndRelease covers F15 + Custody ReserveSpendAtomic idempotency.
func TestAtomicDailySpendReserveAndRelease(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 10 concurrent AddDailySpend(100) must total exactly 1000!
	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			if _, err := db.AddDailySpend("test_daily", 100); err != nil {
				t.Errorf("AddDailySpend: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := db.DailySpend("test_daily")
	if err != nil || got != 1000 {
		t.Fatalf("expected exact daily spend 1000, got %d err=%v", got, err)
	}

	// ReserveSpendAtomic: idempotent on (opID, attempt), rejects over cap, releases cleanly.
	already, err := db.ReserveSpendAtomic("op-1", 1, 600, 1000, 1000)
	if err != nil || already {
		t.Fatalf("first ReserveSpendAtomic: already=%v err=%v", already, err)
	}
	// Replay of identical (op-1, 1) must be idempotent without double-charging!
	already, err = db.ReserveSpendAtomic("op-1", 1, 600, 1000, 1000)
	if err != nil || !already {
		t.Fatalf("replay ReserveSpendAtomic expected already=true, got already=%v err=%v", already, err)
	}
	// Second concurrent op-2 asking for 500 exceeds 1000 daily cap (600+500 > 1000).
	if _, err := db.ReserveSpendAtomic("op-2", 1, 500, 1000, 1000); !errors.Is(err, ErrSpendCapExceeded) {
		t.Fatalf("expected ErrSpendCapExceeded, got %v", err)
	}
	// Release op-1 -> op-2 can now reserve 500!
	if err := db.ReleaseSpendReservation("op-1", 1); err != nil {
		t.Fatalf("ReleaseSpendReservation: %v", err)
	}
	if _, err := db.ReserveSpendAtomic("op-2", 1, 500, 1000, 1000); err != nil {
		t.Fatalf("expected op-2 to succeed after release, got %v", err)
	}
}

// TestForgetFragmentWipesKeyedMetaAndUpdateDealCAS covers F18 & F34.
func TestForgetFragmentWipesKeyedMetaAndUpdateDealCAS(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// F18: ForgetFragment must zero out keyed_meta along with plaintext!
	if _, _, err := db.AppendFragment(FragmentRecord{
		Shard:      "sec",
		HDP:        "hdp1",
		Leaf:       "leaf1",
		Data:       []byte("top secret key material"),
		KeyedMeta:  []byte(`{"ciphertext":"deadbeef"}`),
		Provenance: []byte(`{"test":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetFragment("sec", 1); err != nil {
		t.Fatalf("ForgetFragment: %v", err)
	}
	frag, err := db.GetFragment("sec", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !frag.Forgotten || len(frag.Data) != 0 || len(frag.KeyedMeta) != 0 {
		t.Fatalf("expected ForgetFragment to clear data & keyed_meta, got forgotten=%v data=%q keyedMeta=%q", frag.Forgotten, string(frag.Data), string(frag.KeyedMeta))
	}

	// F34: UpdateDealCAS rejects stale revision writes!
	if err := db.SaveDeal(DealRecord{DealID: "d-cas", Role: "buyer", Status: "open"}); err != nil {
		t.Fatal(err)
	}
	d1, _ := db.GetDeal("d-cas")
	d2 := d1
	d1.Status = "paid"
	ok, err := db.UpdateDealCAS(d1)
	if err != nil || !ok {
		t.Fatalf("first CAS expected ok=true, got ok=%v err=%v", ok, err)
	}
	d2.Status = "expired"
	ok, err = db.UpdateDealCAS(d2)
	if err != nil || ok {
		t.Fatalf("stale CAS expected ok=false, got ok=%v err=%v", ok, err)
	}
	final, _ := db.GetDeal("d-cas")
	if final.Status != "paid" || final.Revision != 2 {
		t.Fatalf("expected status=paid rev=2, got status=%s rev=%d", final.Status, final.Revision)
	}
}

// TestReserveFaucetDripConcurrentCap covers F15 atomic faucet caps.
func TestReserveFaucetDripConcurrentCap(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		addr := fmt.Sprintf("1Addr%d", i)
		go func(a string) {
			defer wg.Done()
			// Global cap 6000 sats, drip 2000 sats -> strictly 3 winners!
			if err := db.ReserveFaucetDrip(a, 2000, 4000, 6000); err == nil {
				wins.Add(1)
			}
		}(addr)
	}
	wg.Wait()
	if wins.Load() != 3 {
		t.Fatalf("expected strictly 3 faucet drip winners under 6000-sat global cap, got %d", wins.Load())
	}
}
