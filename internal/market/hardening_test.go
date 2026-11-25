package market

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestStableKeysAllowedDomainsAndZeroScoreRejection(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "market.sqlite")
	dataDir := t.TempDir()

	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	port1, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	mem1, err := memory.New(db1, port1, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng1 := NewEngine(mem1, DefaultPolicy())
	disc1 := discovery.New(mem1, nil, nil)
	brk1 := NewBroker(mem1, eng1, disc1, BrokerConfig{})
	pub1 := brk1.PubHex()
	if pub1 != disc1.AnnouncePubHex() {
		t.Fatalf("Engine saleKey (%s) and Discovery announceKey (%s) must match", pub1, disc1.AnnouncePubHex())
	}
	_ = db1.Close()

	// Reopen DB and verify identical node_market_key_v1 pubkey across restart!
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	port2, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dataDir})
	mem2, _ := memory.New(db2, port2, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	eng2 := NewEngine(mem2, DefaultPolicy())
	disc2 := discovery.New(mem2, nil, nil)
	brk2 := NewBroker(mem2, eng2, disc2, BrokerConfig{})
	if brk2.PubHex() != pub1 {
		t.Fatalf("market pubkey changed across restart: %s != %s", brk2.PubHex(), pub1)
	}

	// AllowedDomains enforcement on Policy.
	pol := DefaultPolicy()
	pol.AllowedDomains = map[string]bool{"bsv": true, "ops": true}
	if err := pol.CanSpendForDomains("seller-1", []string{"bsv"}, 100); err != nil {
		t.Fatalf("expected allowed domain bsv to pass, got %v", err)
	}
	if err := pol.CanSpendForDomains("seller-1", []string{"astrology"}, 100); err == nil {
		t.Fatal("expected disallowed domain astrology to be rejected")
	}

	// pickBestFragment must reject 0-score mismatches when query has substantive tokens.
	frags := []memory.RecalledFragment{
		{Seq: 1, Text: "BSV Chronicle opcodes and Teranode throughput benchmarks."},
	}
	if got := pickBestFragment(frags, "quantum strawberry pancake recipe"); got != nil {
		t.Fatalf("expected pickBestFragment to reject zero-overlap mismatch, got %+v", got)
	}
	if got := pickBestFragment(frags, "Teranode benchmarks"); got == nil || got.Seq != 1 {
		t.Fatalf("expected pickBestFragment to match Teranode benchmarks, got %+v", got)
	}
}
