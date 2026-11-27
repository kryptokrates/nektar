package market

import (
	"context"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestSleepCycleClosedLoopDemandPriorityAndAutoWants(t *testing.T) {
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
	pol := DefaultPolicy()
	pol.AttachDB(db)
	eng := NewEngine(mem, pol)

	// 1. Seed two working shards where "zebra-k8s" sorts AFTER "alpha-notes",
	//    and inject an unmatched buyer WantAd demanding "k8s" knowledge.
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "alpha-notes",
		Text:  "Alpha general note about local editor font size preferences and spacing.",
	}); err != nil {
		t.Fatalf("remember alpha-notes: %v", err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "zebra-k8s",
		Text:  "Kubernetes CoreDNS cache plugin defaults to 30s TTL for upstream cluster DNS lookups in v1.30.",
	}); err != nil {
		t.Fatalf("remember zebra-k8s: %v", err)
	}
	if err := db.RecordUnmatchedWant(store.UnmatchedWantRecord{
		WantID:  "want-k8s-demand-1",
		Domain:  "k8s",
		Query:   "Kubernetes CoreDNS cache TTL defaults",
		MaxSats: 400,
		SeenAt:  time.Now().Unix(),
	}); err != nil {
		t.Fatalf("RecordUnmatchedWant: %v", err)
	}

	// 2. Seed 3 repeated recall misses for a missing topic.
	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		if err := db.RecordRecallMiss("bsv", "Chronicle OP_SUBSTR script limits", now); err != nil {
			t.Fatalf("RecordRecallMiss: %v", err)
		}
	}

	// 3. Run SleepCycle with MaxBatch=1 and default AutoWantOnMiss=false:
	//    - MUST prioritize "zebra-k8s" before "alpha-notes" (only 1 fragment scanned -> zebra-k8s!)
	//    - MUST NOT emit auto-wants while AutoWantOnMiss is false (fail-closed!).
	st1, err := eng.RunSleepCycle(ctx, SleepCycleInput{
		Force:    true,
		MaxBatch: 1,
		AutoList: true,
	})
	if err != nil {
		t.Fatalf("RunSleepCycle 1: %v", err)
	}
	if len(st1.DemandPrioritizedShards) == 0 || st1.DemandPrioritizedShards[0] != "zebra-k8s" {
		t.Fatalf("expected zebra-k8s prioritized first by unmatched_wants, got %+v", st1.DemandPrioritizedShards)
	}
	if st1.AutoWantsPosted != 0 {
		t.Fatalf("expected 0 auto-wants when AutoWantOnMiss=false, got %d", st1.AutoWantsPosted)
	}
	k8sCur, _ := db.GetDistillCursor("zebra-k8s")
	alphaCur, _ := db.GetDistillCursor("alpha-notes")
	if k8sCur == 0 || alphaCur != 0 {
		t.Fatalf("expected zebra-k8s scanned first (cur=%d) and alpha-notes untouched (cur=%d) with MaxBatch=1", k8sCur, alphaCur)
	}

	// 4. Enable opt-in AutoWantOnMiss + verify bounded single WantAd emission and 24h cooldown!
	if err := pol.SetAutonomyConfig(PolicyAutonomyConfig{
		AutoWantOnMiss:        true,
		AutoWantMissThreshold: 3,
		AutoWantMaxPerDay:     2,
		AutoWantBudgetSats:    200,
	}); err != nil {
		t.Fatalf("SetAutonomyConfig: %v", err)
	}
	st2, err := eng.RunSleepCycle(ctx, SleepCycleInput{Force: true, MaxBatch: 4})
	if err != nil {
		t.Fatalf("RunSleepCycle 2: %v", err)
	}
	if st2.AutoWantsPosted != 1 || len(st2.AutoWantIDs) != 1 {
		t.Fatalf("expected exactly 1 bounded auto-want posted, got count=%d ids=%v", st2.AutoWantsPosted, st2.AutoWantIDs)
	}

	// Immediate second cycle MUST NOT re-post (cooldown + miss_count reset).
	st3, err := eng.RunSleepCycle(ctx, SleepCycleInput{Force: true, MaxBatch: 4})
	if err != nil {
		t.Fatalf("RunSleepCycle 3: %v", err)
	}
	if st3.AutoWantsPosted != 0 {
		t.Fatalf("expected 0 auto-wants during 24h cooldown, got %d", st3.AutoWantsPosted)
	}
}
