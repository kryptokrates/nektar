package memory

import (
	"context"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestCedarWriteGateSupersededReceiptAndChirpAnchors(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Cedar write clearance: level-1 principal cannot write level-3 fragment.
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:     "sec",
		Text:      "Top secret note",
		Level:     3,
		Principal: &policy.Principal{Type: "Agent", ID: "intern", Level: 1},
	}); err == nil {
		t.Fatal("expected Cedar write gate to deny level-1 principal writing level-3 fragment")
	}

	// 2. GroundedAnswer receipt turns amber when cited fragment is superseded!
	r1, err := svc.Remember(ctx, RememberInput{
		Shard: "facts", Text: "API rate limit is 60 rpm.", AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:  "API rate limit is 60 rpm",
			Cites: []reasoning.FragmentRef{{Shard: "facts", Seq: r1.Seq}},
		}},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	v1, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil || v1.Verdict != "green" || v1.CitationsSuperseded != 0 {
		t.Fatalf("expected green verdict before supersede, got %+v err=%v", v1, err)
	}

	// Supersede r1 with r2.
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "facts", Text: "API rate limit raised to 120 rpm.", Supersedes: r1.Seq, AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	v2, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil || v2.Verdict != "amber" || v2.CitationsSuperseded != 1 {
		t.Fatalf("expected amber verdict with 1 superseded citation, got %+v err=%v", v2, err)
	}

	// 3. ExportShardPack -> ImportShardPack preserves batch anchors!
	pack, err := svc.ExportShardPack(ctx, "facts")
	if err != nil || pack == nil || pack.ChirpURL == "" {
		t.Fatalf("expected exported pack, got %+v err=%v", pack, err)
	}
	db2, _ := store.Open(":memory:")
	defer db2.Close()
	port2, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	svc2, _ := New(db2, port2, bstack.NewGroupRegistry(), svc.Objects(), Config{})
	if _, err := svc2.ImportShardPack(ctx, pack.ChirpURL, "imported-facts"); err != nil {
		t.Fatalf("ImportShardPack: %v", err)
	}
	importedAnchors, err := db2.ListAnchors("imported-facts")
	if err != nil || len(importedAnchors) == 0 {
		t.Fatalf("expected batch anchors preserved after CHIRP import, got %+v err=%v", importedAnchors, err)
	}
}
