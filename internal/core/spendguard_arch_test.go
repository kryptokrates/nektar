package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSpendGuardCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spend-guard.json")
	if err := os.WriteFile(path, []byte(`{"dayKey":"2026-11-25","spentToday": BROKEN`), 0o600); err != nil {
		t.Fatal(err)
	}
	sg := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 1000,
		MaxSpendSatsDay: 5000,
		StatePath:       path,
	})
	if err := sg.Check(100, 100); !errors.Is(err, ErrSpendStateCorrupt) {
		t.Fatalf("expected ErrSpendStateCorrupt on Check, got %v", err)
	}
	if _, err := sg.Reserve(100, 100); !errors.Is(err, ErrSpendStateCorrupt) {
		t.Fatalf("expected ErrSpendStateCorrupt on Reserve, got %v", err)
	}
	if err := sg.CheckAndRecord("tx1", 100, 100); !errors.Is(err, ErrSpendStateCorrupt) {
		t.Fatalf("expected ErrSpendStateCorrupt on CheckAndRecord, got %v", err)
	}
}

func TestSpendGuardAtomicReserveAndCommit(t *testing.T) {
	dir := t.TempDir()
	sg := NewSpendGuard(SpendGuardOpts{
		MaxFeeSatsPerTx: 1000,
		MaxSpendSatsDay: 500,
		StatePath:       filepath.Join(dir, "spend-guard.json"),
	})
	res1, err := sg.Reserve(100, 300)
	if err != nil {
		t.Fatalf("Reserve op1: %v", err)
	}
	// Second concurrent reservation exceeding daily cap (300 + 300 > 500) must fail.
	if _, err := sg.Reserve(100, 300); !errors.Is(err, ErrDailyCapExceeded) {
		t.Fatalf("expected ErrDailyCapExceeded while op1 reserved, got %v", err)
	}
	// Cancel frees budget.
	sg.CancelReservation(res1)
	res2, err := sg.Reserve(100, 400)
	if err != nil {
		t.Fatalf("Reserve op2 after cancel: %v", err)
	}
	// CommitReservation records unconditionally and persists across reload.
	if err := sg.CommitReservation(res2, "tx-committed-1", 100, 400); err != nil {
		t.Fatalf("CommitReservation: %v", err)
	}
	spent, _, _, count := sg.Stats()
	if spent != 400 || count != 1 {
		t.Fatalf("unexpected stats after commit: spent=%d count=%d", spent, count)
	}
	// Idempotent re-record of same txid does not double-debit.
	if err := sg.CheckAndRecord("tx-committed-1", 100, 400); err != nil {
		t.Fatalf("idempotent re-record failed: %v", err)
	}
	spent2, _, _, count2 := sg.Stats()
	if spent2 != 400 || count2 != 1 {
		t.Fatalf("idempotent re-record double-counted: spent=%d count=%d", spent2, count2)
	}
}
