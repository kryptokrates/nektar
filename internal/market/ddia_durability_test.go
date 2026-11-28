package market

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/faststore"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestDealRevisionFencingAndTerminalMonotonicity(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Initial insert gets revision=1.
	if err := db.SaveDeal(store.DealRecord{DealID: "d-fence", Role: "buyer", Status: "open"}); err != nil {
		t.Fatalf("initial SaveDeal: %v", err)
	}
	r1, err := db.GetDeal("d-fence")
	if err != nil || r1.Revision != 1 {
		t.Fatalf("expected revision 1, got %+v err=%v", r1, err)
	}

	// Advance to revision=2 via SaveDealWithObligations.
	r1.Status = "paid_awaiting_reveal"
	if err := db.SaveDealWithObligations(r1); err != nil {
		t.Fatalf("SaveDealWithObligations rev 1->2: %v", err)
	}
	r2, _ := db.GetDeal("d-fence")
	if r2.Revision != 2 {
		t.Fatalf("expected revision 2, got %d", r2.Revision)
	}

	// Stale writer still holding revision=1 MUST fail with ErrStaleDealRevision (DDIA Ch. 9 fencing token).
	stale := r1
	stale.Status = "expired"
	if err := db.SaveDealWithObligations(stale); !errors.Is(err, store.ErrStaleDealRevision) {
		t.Fatalf("expected ErrStaleDealRevision for stale revision=1, got %v", err)
	}

	// Settle at revision=2 -> revision=3.
	r2.Status = "settled"
	r2.CounterpartyPub = "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r2.PriceSats = 25
	if err := db.SettleDealAndFulfillObligations(r2, nil, nil); err != nil {
		t.Fatalf("SettleDealAndFulfillObligations: %v", err)
	}

	// Attempting to regress a 'settled' deal to 'expired' (even with Revision=0 or Revision=99) MUST fail closed.
	if err := db.SaveDeal(store.DealRecord{DealID: "d-fence", Role: "buyer", Status: "expired", Revision: 99}); !errors.Is(err, store.ErrStaleDealRevision) {
		t.Fatalf("expected ErrStaleDealRevision preventing settled->expired regression, got %v", err)
	}
	cur, _ := db.GetDeal("d-fence")
	if cur.Status != "settled" {
		t.Fatalf("settled deal regressed to %q", cur.Status)
	}

	// ProjectSettledDealsBatch MUST be idempotent for already-settled deals (no double-counted reputation).
	if err := db.ProjectSettledDealsBatch([]store.DealRecord{{
		DealID:          "d-fence",
		Role:            "buyer",
		Status:          "settled",
		CounterpartyPub: r2.CounterpartyPub,
		PriceSats:       25,
	}}); err != nil {
		t.Fatalf("ProjectSettledDealsBatch: %v", err)
	}
	rep, _ := db.GetReputation(r2.CounterpartyPub)
	if rep.SettledDeals != 0 {
		t.Fatalf("expected already-settled deal not to double-increment reputation, got %+v", rep)
	}
}

func TestFastMarketJournalSpendPersistenceAndStrictCBOR(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pebble")
	cur := time.Date(2026, 11, 29, 0, 5, 0, 0, time.UTC)

	fs1, err := faststore.Open(faststore.Options{Dir: dir, SyncWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	fj1 := NewFastMarketJournal(fs1, true, nil)
	fj1.nowFn = func() time.Time { return cur }
	fj1.hydrateSpendWAL()

	already, err := fj1.ReserveSpendAtomic("op-1", 1, 70, 100, 100)
	if err != nil || already {
		t.Fatalf("initial reserve failed: already=%v err=%v", already, err)
	}
	// Backward NTP step across UTC midnight MUST NOT reset spendToday.
	cur = time.Date(2026, 11, 28, 23, 55, 0, 0, time.UTC)
	if _, err := fj1.ReserveSpendAtomic("op-2", 1, 50, 100, 100); err == nil {
		t.Fatalf("expected daily cap enforcement across backward midnight step")
	}
	_ = fj1.Close()

	// Reopen from disk on same UTC day -> MUST hydrate 70 sats from Pebble WAL and reject +50 > 100!
	cur = time.Date(2026, 11, 29, 0, 10, 0, 0, time.UTC)
	fs2, err := faststore.Open(faststore.Options{Dir: dir, SyncWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	fj2 := NewFastMarketJournal(fs2, true, nil)
	fj2.nowFn = func() time.Time { return cur }
	fj2.hydrateSpendWAL()
	defer fj2.Close()

	if _, err := fj2.ReserveSpendAtomic("op-after-crash", 1, 50, 100, 100); err == nil {
		t.Fatalf("expected crash-recovered FastMarketJournal to enforce 70+50 > 100 daily cap")
	}
	if already, err := fj2.ReserveSpendAtomic("op-1", 1, 70, 100, 100); err != nil || !already {
		t.Fatalf("expected idempotent re-reservation for op-1:1 after restart, got already=%v err=%v", already, err)
	}

	// Strict CBOR decode MUST propagate field type mismatches fail-closed.
	badReq, _ := core.EncodeCBOR(cbor.Map{{Key: uint64(1), Val: uint64(12345)}}) // key 1 expects text OfferID
	if _, err := DecodePurchaseRequestCBOR(badReq); err == nil {
		t.Fatalf("expected DecodePurchaseRequestCBOR to fail on wrong type for key 1")
	}
	badResp, _ := core.EncodeCBOR(cbor.Map{{Key: uint64(1), Val: "not-a-bool"}})
	if _, err := DecodePurchaseResponseCBOR(badResp); err == nil {
		t.Fatalf("expected DecodePurchaseResponseCBOR to fail on wrong type for key 1")
	}
	badWAL, _ := core.EncodeCBOR(cbor.Map{{Key: uint64(8), Val: "not-uint-sats"}})
	if _, err := decodeDealWAL(badWAL); err == nil {
		t.Fatalf("expected decodeDealWAL to fail on wrong type for key 8")
	}
}

func TestWorkflowContinueAsNewRollover(t *testing.T) {
	mem := newTestMemory(t)
	eng := NewEngine(mem, DefaultPolicy())
	wc, err := eng.StartWorkflows(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.CloseWorkflows()

	// StandingOrderWorkflow with MaxIterationsBeforeRollover=2 exercises ContinueAsNew cleanly across 4 signals!
	orderID := "so-rollover-1"
	if _, err := wc.StartStandingOrderAsync(context.Background(), orderID, StandingOrderInput{
		OrderID:                     orderID,
		Domains:                     []string{"ops"},
		Query:                       "continue-as-new test",
		MaxSats:                     10,
		TTL:                         time.Hour,
		MaxIterationsBeforeRollover: 2,
	}); err != nil {
		t.Fatalf("StartStandingOrderAsync: %v", err)
	}

	// Wait for initial state and capture initial ExecutionID (InvocationID).
	var initExecID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := wc.StandingOrderStatus(context.Background(), orderID); err == nil && st.InvocationID != "" {
			initExecID = st.InvocationID
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if initExecID == "" {
		t.Fatalf("standing order did not initialize")
	}

	// Send first 2 overpriced offers to trigger ContinueAsNew rollover when channel drains.
	for i := 1; i <= 2; i++ {
		off := bstack.OfferAd{
			OfferID:   "off-high-" + string(rune('0'+i)),
			Domains:   []string{"ops"},
			PriceSats: 9999,
		}
		if _, err := wc.SignalStandingOrderOffer(context.Background(), orderID, off); err != nil {
			t.Fatalf("signal offer %d: %v", i, err)
		}
	}

	// Wait until ContinueAsNew rollover has created a fresh ExecutionID while preserving OffersSeen=2.
	rolledOver := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := wc.StandingOrderStatus(context.Background(), orderID); err == nil && st.OffersSeen == 2 && st.InvocationID != "" && st.InvocationID != initExecID {
			rolledOver = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !rolledOver {
		st, _ := wc.StandingOrderStatus(context.Background(), orderID)
		t.Fatalf("standing order did not roll over via ContinueAsNew (initExec=%q, state=%+v)", initExecID, st)
	}

	// Post-rollover: send 3rd unique offer + duplicate off-high-1 (must be deduped via carried SeenOfferIDs) + cancel.
	if _, err := wc.SignalStandingOrderOffer(context.Background(), orderID, bstack.OfferAd{
		OfferID:   "off-high-3",
		Domains:   []string{"ops"},
		PriceSats: 9999,
	}); err != nil {
		t.Fatalf("signal offer 3: %v", err)
	}
	if _, err := wc.SignalStandingOrderOffer(context.Background(), orderID, bstack.OfferAd{
		OfferID:   "off-high-1",
		Domains:   []string{"ops"},
		PriceSats: 9999,
	}); err != nil {
		t.Fatalf("signal duplicate offer 1: %v", err)
	}
	if _, err := wc.CancelStandingOrder(context.Background(), orderID, "done-rollover"); err != nil {
		t.Fatalf("CancelStandingOrder: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := wc.StandingOrderStatus(context.Background(), orderID)
		if err == nil && st.Status == "cancelled" {
			if st.OffersSeen != 3 {
				t.Fatalf("expected OffersSeen=3 (with duplicate off-high-1 deduped across ContinueAsNew), got %d", st.OffersSeen)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := wc.StandingOrderStatus(context.Background(), orderID)
	t.Fatalf("standing order did not reach cancelled after ContinueAsNew rollover; state=%+v", st)
}
