package market

import (
	"context"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func newTestMemory(t *testing.T) *memory.Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestEmbeddedWorkflowsClientAndAtomicKnowledgeTrade(t *testing.T) {
	ctx := context.Background()
	buyerMem := newTestMemory(t)
	sellerMem := newTestMemory(t)

	buyer := NewEngine(buyerMem, DefaultPolicy())
	seller := NewEngine(sellerMem, DefaultPolicy())

	// 1. Verify embedded pure-Go go-workflows runtime starts cleanly over SQLite WAL (zero external sidecars).
	wc, err := buyer.StartWorkflows(ctx, t.TempDir())
	if err != nil || wc == nil || !wc.IsRunning() {
		t.Fatalf("StartWorkflows failed: wc=%v err=%v", wc, err)
	}
	defer func() { _ = buyer.CloseWorkflows() }()

	// 2. Verify in-process Tx coordinator & real workflows.sqlite QueryInvocations!
	invID, err := wc.EmitTxEvent(ctx, "tx123", TxEventInput{Kind: "mined", BlockHeight: 900001})
	if err != nil || invID != "tx123" {
		t.Fatalf("EmitTxEvent failed: invID=%q err=%v", invID, err)
	}
	txSt, err := wc.GetTxState(ctx, "tx123")
	if err != nil || txSt.Status != "mined" || txSt.BlockHeight != 900001 {
		t.Fatalf("GetTxState mismatch: %+v err=%v", txSt, err)
	}
	if _, err := wc.QueryInvocations(ctx, 10); err != nil {
		t.Fatalf("QueryInvocations failed: %v", err)
	}

	// 3. Deterministic bps ranking & kill-switch refusal.
	want := bstack.WantAd{WantID: "w1", MaxSats: 1000}
	ranked := RankOffersWithRepAndBonds(want, []bstack.OfferAd{
		{OfferID: "expensive", Pub: "s1", PriceSats: 900},
		{OfferID: "best", Pub: "s2", PriceSats: 200, Direct: true, KeyCommitHex: "aa", ChirpURL: "chirp://x"},
		{OfferID: "overcap", Pub: "s3", PriceSats: 5000},
	}, buyer.Policy(), nil, nil)
	if len(ranked) != 2 || ranked[0].Offer.OfferID != "best" {
		t.Fatalf("unexpected ranking: %+v", ranked)
	}

	// 4. Seller remembers & anchors a fact, then sells it atomically via signed DisclosureSale + BRC-369 + CHIRP + on-chain pay!
	rem, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "research",
		Text:       "Teranode regtest sustained 1M TPS in benchmark run 42.",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	buyer.Policy().SetKillSwitch(true)
	if _, err := ExecuteAtomicKnowledgeTrade(ctx, buyer, seller, "research", rem.Seq, "acquired", 500); err == nil {
		t.Fatal("expected kill switch to block trade")
	}
	buyer.Policy().SetKillSwitch(false)

	deal, err := ExecuteAtomicKnowledgeTrade(ctx, buyer, seller, "research", rem.Seq, "acquired", 500)
	if err != nil {
		t.Fatalf("ExecuteAtomicKnowledgeTrade: %v", err)
	}
	if deal.Status != "settled" || deal.DeliveredTxt != "Teranode regtest sustained 1M TPS in benchmark run 42." {
		t.Fatalf("unexpected deal result: %+v", deal)
	}

	// Verify buyer now holds the anchored knowledge in its own memory shard!
	recalled, err := buyerMem.Recall(ctx, memory.RecallInput{Shard: "acquired", VerifySPV: true})
	if err != nil || len(recalled) != 1 || recalled[0].SPV == nil || !recalled[0].SPV.Settled {
		t.Fatalf("expected buyer to hold SPV-verified purchased fact, got %+v err=%v", recalled, err)
	}
}
