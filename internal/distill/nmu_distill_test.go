package distill

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestDistillerNMU_UsageRightsEnforcementEdgesRootAndOnSupersede(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer port.Close()

	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	var supMu sync.Mutex
	var supEvents []memory.SupersessionEvent
	mem.SetOnSupersede(func(ev memory.SupersessionEvent) {
		supMu.Lock()
		supEvents = append(supEvents, ev)
		supMu.Unlock()
	})

	// 1. Purchased tainted fragment with inference_only MUST be blocked from SleepCycle resale.
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard:   "work-ops",
		Text:    "Go 1.27 fixes SQLite busy lock when WAL mode is enabled on production nodes.",
		Tainted: true,
		NMU: &nmu.Spec{
			Version:     nmu.SpecVersion,
			UsageRights: nmu.RightsInferenceOnly,
		},
	})
	if err != nil {
		t.Fatalf("Remember inference_only: %v", err)
	}

	// 2. Purchased tainted fragment with distill_resellable MUST be allowed with a RefinementRefines provenance link
	//    AND populate fragment_graph_meta (64-hex ontologyId + edgesRoot) + knowledge_edges!
	resLicensed, err := mem.Remember(ctx, memory.RememberInput{
		Shard:   "work-ops",
		Text:    "Go 1.27 requires SQLite WAL mode and busy_timeout to fix concurrent write lock failures.",
		Tainted: true,
		NMU: &nmu.Spec{
			Version:     nmu.SpecVersion,
			UsageRights: nmu.RightsDistillResellable,
		},
	})
	if err != nil {
		t.Fatalf("Remember distill_resellable: %v", err)
	}

	// 3. Purchased tainted fragment with distill_resellable BUT GradeMismatch=true MUST stay blocked!
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard:         "work-ops",
		Text:          "Docker mitigates TLS regression in Go 1.27 deployments.",
		Tainted:       true,
		GradeMismatch: true,
		NMU: &nmu.Spec{
			Version:     nmu.SpecVersion,
			UsageRights: nmu.RightsDistillResellable,
		},
	})
	if err != nil {
		t.Fatalf("Remember mismatched: %v", err)
	}

	d, err := NewDistiller(mem, Config{
		DefaultDomain:      "ops",
		DefaultUsageRights: nmu.RightsDistillResellable,
	})
	if err != nil {
		t.Fatalf("NewDistiller: %v", err)
	}
	sum, err := d.RunCycle(ctx, RunOpts{})
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if sum.Distilled != 1 {
		t.Fatalf("expected exactly 1 distilled fragment (licensed only), got %+v", sum)
	}

	mFrags, err := db.ListFragments("market-ops")
	if err != nil || len(mFrags) != 1 {
		t.Fatalf("expected 1 market-ops fragment, got %d err=%v", len(mFrags), err)
	}
	ontID, edgesRoot, err := db.GetFragmentGraphMeta("market-ops", mFrags[0].Seq)
	if err != nil || len(ontID) != 64 || len(edgesRoot) != 64 {
		t.Fatalf("expected 64-hex ontologyId & edgesRoot in fragment_graph_meta, got ont=%q edgesRoot=%q err=%v", ontID, edgesRoot, err)
	}
	edges, err := db.ListKnowledgeEdges(store.KnowledgeEdgeFilter{Shard: "market-ops", Seq: mFrags[0].Seq})
	if err != nil || len(edges) == 0 {
		t.Fatalf("expected distilled verbatim edges persisted in knowledge_edges, got %d err=%v", len(edges), err)
	}
	nmuMeta, err := db.GetFragmentNMUMeta("market-ops", mFrags[0].Seq)
	if err != nil || nmuMeta == nil || nmuMeta.Grade != "B" || nmuMeta.UnitCount == 0 || nmuMeta.FreshnessClass != "versioned" || nmuMeta.UsageRights != "distill_resellable" {
		t.Fatalf("unexpected distilled NMU meta: %+v err=%v", nmuMeta, err)
	}
	// Verify provenance edge links back to the licensed purchased fragment.
	recalled, err := mem.Recall(ctx, memory.RecallInput{Shard: "market-ops"})
	if err != nil || len(recalled) != 1 || len(recalled[0].Refines) == 0 || recalled[0].Refines[0].TargetSeq != resLicensed.Seq {
		t.Fatalf("expected RefinementRefines provenance edge to work-ops#%d, got %+v", resLicensed.Seq, recalled)
	}

	// 4. Commit a superseding fragment on market-ops and verify OnSupersede fires for Grade S live feeds.
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard:      "market-ops",
		Text:       "Go 1.27 requires SQLite WAL mode with busy_timeout=5000ms (updated guidance).",
		Public:     true,
		Distilled:  true,
		Supersedes: mFrags[0].Seq,
	})
	if err != nil {
		t.Fatalf("Remember superseding: %v", err)
	}
	_ = mem.Close() // drains hookWG synchronously
	supMu.Lock()
	defer supMu.Unlock()
	if len(supEvents) != 1 || supEvents[0].Shard != "market-ops" || supEvents[0].SupersededSeq != mFrags[0].Seq || supEvents[0].Timestamp <= 0 {
		t.Fatalf("expected 1 OnSupersede event for market-ops#%d, got %+v (time=%d)", mFrags[0].Seq, supEvents, time.Now().Unix())
	}
}
