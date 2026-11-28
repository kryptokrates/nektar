package market

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestEpistemicShardConcurrentTransitionsZeroUTXOContention(t *testing.T) {
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
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(mem, DefaultPolicy())

	// 1. Verify embedded pure-Go go-workflows runtime starts cleanly alongside EpistemicShard coordinator.
	wc, err := eng.StartWorkflows(ctx, t.TempDir())
	if err != nil || wc == nil {
		t.Fatalf("expected non-nil WorkflowClient, got err=%v", err)
	}
	defer func() { _ = eng.CloseWorkflows() }()

	// 2. Fire 8 concurrent transitions with Anchor:true against one shard — MUST serialize cleanly
	//    with zero UTXO contention and produce contiguous N = 0..7 anchors + verified chain!
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, tErr := eng.ExecuteEpistemicTransition(ctx, "concurrent-automaton", EpistemicTransitionInput{
				Remember: memory.RememberInput{
					Text: fmt.Sprintf("concurrent epistemic event #%d", idx),
				},
				Anchor: true,
			})
			if tErr != nil {
				errs <- fmt.Errorf("goroutine %d: %w", idx, tErr)
				return
			}
			if res == nil || res.AnchorTxID == "" || res.State.StateQ == "" {
				errs <- fmt.Errorf("goroutine %d: incomplete transition result %+v", idx, res)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	anchors, err := db.ListAnchors("concurrent-automaton")
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != n {
		t.Fatalf("expected %d contiguous anchors, got %d", n, len(anchors))
	}
	for i, a := range anchors {
		if a.N != int64(i) || a.StateHash == "" || a.MmrRoot == "" {
			t.Fatalf("anchor %d invalid: %+v", i, a)
		}
	}
	rep, err := mem.VerifyShardChain("concurrent-automaton")
	if err != nil || !rep.OK || rep.AnchorCount != n || rep.TransitionCount != n {
		t.Fatalf("VerifyShardChain failed after concurrent transitions: err=%v rep=%+v", err, rep)
	}
}
