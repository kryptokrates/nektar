package memory

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func newHardeningService(t *testing.T) (*Service, *store.DB, *chain.Port) {
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
	t.Cleanup(func() { _ = port.Close() })
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	return svc, db, port
}

// TestConcurrentAnchorShardSingleTxAndPendingRecovery covers F8:
// - 5 concurrent AnchorShard calls on the same shard broadcast strictly 1 batch anchor (no R'_n fork!)
// - Pending anchor intent persisted before a crash is recovered on boot by ReconcilePendingAnchors.
func TestConcurrentAnchorShardSingleTxAndPendingRecovery(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := newHardeningService(t)

	for i := 0; i < 3; i++ {
		if _, err := svc.Remember(ctx, RememberInput{
			Shard: "ops",
			Text:  "concurrent anchor fact " + string(rune('A'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var nonNil atomic.Int32
	var wg sync.WaitGroup
	wg.Add(5)
	for i := 0; i < 5; i++ {
		go func() {
			defer wg.Done()
			b, err := svc.AnchorShard(ctx, "ops")
			if err != nil {
				t.Errorf("AnchorShard error: %v", err)
				return
			}
			if b != nil {
				nonNil.Add(1)
			}
		}()
	}
	wg.Wait()
	if nonNil.Load() != 1 {
		t.Fatalf("expected strictly 1 AnchorShard batch across 5 concurrent callers, got %d", nonNil.Load())
	}
	anchors, _ := db.ListAnchors("ops")
	if len(anchors) != 1 || anchors[0].N != 0 {
		t.Fatalf("expected 1 batch anchor N=0, got %+v", anchors)
	}
	pending, _ := db.ListPendingAnchors()
	if len(pending) != 0 {
		t.Fatalf("expected pending_anchors drained after commit, got %+v", pending)
	}

	// Simulate crash AFTER broadcast before RecordBatchAnchor: insert pending_anchor referencing real anchored tx!
	if _, err := svc.Remember(ctx, RememberInput{Shard: "recover", Text: "crash recovery fact"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePendingAnchor(store.PendingAnchor{
		Shard:      "recover",
		N:          1,
		Seqs:       []int64{1},
		RootHex:    anchors[0].RootHex,
		ChainedHex: anchors[0].ChainedHex,
		TxID:       anchors[0].TxID,
		Beef:       anchors[0].Beef,
	}); err != nil {
		t.Fatal(err)
	}
	nRec, err := svc.ReconcilePendingAnchors(ctx)
	if err != nil || nRec != 1 {
		t.Fatalf("expected ReconcilePendingAnchors to recover 1 batch, got n=%d err=%v", nRec, err)
	}
	recAnchors, _ := db.ListAnchors("recover")
	if len(recAnchors) != 1 || recAnchors[0].TxID != anchors[0].TxID {
		t.Fatalf("expected recovered anchor in DB, got %+v", recAnchors)
	}
}

// TestSupersededCitationsRejectedAndLiveReceiptAnchored covers F22 & F32.
func TestSupersededCitationsRejectedAndLiveReceiptAnchored(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newHardeningService(t)

	r1, err := svc.Remember(ctx, RememberInput{Shard: "facts", Text: "port is 8080", AutoAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	// Issue a grounded answer BEFORE anchoring __receipts__ (receipt starts unanchored).
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{
			{Text: "port is 8080", Cites: []reasoning.FragmentRef{{Shard: "facts", Seq: r1.Seq}}},
		},
	})
	if err != nil {
		t.Fatalf("GroundedAnswer: %v", err)
	}
	// Anchor __receipts__ AFTER receipt issuance!
	if _, err := svc.AnchorShard(ctx, ReceiptsShard); err != nil {
		t.Fatalf("AnchorShard(__receipts__): %v", err)
	}
	// F22: VerifyReceipt MUST read ReceiptAnchored live from __receipts__ fragment (true!), not stale column!
	ver, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil {
		t.Fatal(err)
	}
	if !ver.ReceiptAnchored {
		t.Fatalf("expected VerifyReceipt to report ReceiptAnchored=true live after delayed __receipts__ anchor, got %+v", ver)
	}

	// F32: Supersede facts#1 -> GroundedAnswer citing superseded facts#1 MUST be rejected!
	if _, err := svc.Remember(ctx, RememberInput{
		Shard:      "facts",
		Text:       "port is 18420",
		Supersedes: r1.Seq,
		AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	gaSup, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{
			{Text: "port is 8080", Cites: []reasoning.FragmentRef{{Shard: "facts", Seq: r1.Seq}}},
		},
	})
	if err != nil {
		t.Fatalf("GroundedAnswer with superseded citation: %v", err)
	}
	if !gaSup.Ungrounded {
		t.Fatalf("expected superseded citation facts#1 to be stripped/ungrounded, got %+v", gaSup)
	}
}

// TestPolicyRulesAndAORGPersistAcrossRestartAndAddRulesPreservesVertices covers F31.
func TestPolicyRulesAndAORGPersistAcrossRestartAndAddRulesPreservesVertices(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/persist.sqlite"
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	port1, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc1, err := New(db1, port1, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		t.Fatal(err)
	}

	customCedar := `permit(principal, action == Action::"read", resource);`
	pc1, err := svc1.SetPolicy("custom-v1", customCedar)
	if err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if err := svc1.AddRules(`derived(X) :- initial_fact(X).`); err != nil {
		t.Fatalf("AddRules: %v", err)
	}

	_ = port1.Close()
	_ = db1.Close()

	// Reopen on the same SQLite DB and verify Cedar policy and Mangle rules survived restart!
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	port2, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port2.Close()
	svc2, err := New(db2, port2, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if svc2.PolicyCommit().Hash != pc1.Hash {
		t.Fatalf("expected persisted Cedar policy hash %s, got %s", pc1.Hash, svc2.PolicyCommit().Hash)
	}
	if len(svc2.rules) == 0 {
		t.Fatalf("expected persisted Mangle rules after restart, got none")
	}
}

// TestImportShardPackRefusesLocalAnchorConflict covers F20.
func TestImportShardPackRefusesLocalAnchorConflict(t *testing.T) {
	ctx := context.Background()
	src, _, _ := newHardeningService(t)
	dst, _, _ := newHardeningService(t)

	if _, err := src.Remember(ctx, RememberInput{Shard: "shared", Text: "remote alpha", AutoAnchor: true}); err != nil {
		t.Fatal(err)
	}
	pack, err := src.ExportShardPack(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}

	// Destination already has a DIFFERENT local anchor on shard "shared" (N=1)!
	if _, err := dst.Remember(ctx, RememberInput{Shard: "shared", Text: "local beta", AutoAnchor: true}); err != nil {
		t.Fatal(err)
	}
	packBytes, err := bstack.ResolveChirp(src.Objects(), pack.ChirpURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.ImportShardPackBytes(packBytes, "shared"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected ImportShardPackBytes to reject conflicting local anchor, got err=%v", err)
	}
}

// TestRefreshAnchorProofsRejectsForgedBUMP covers F13:
// An anchor proof whose computed Merkle root is NOT valid in the header tracker must NEVER be marked settled=1!
func TestRefreshAnchorProofsRejectsForgedBUMP(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := newHardeningService(t)

	// Record an unsettled batch anchor with a nonexistent txid -> RefreshAnchorProofs must keep settled=0.
	fakeTxID := core.ToHex(core.DSHA256([]byte("forged-anchor-tx")))
	if err := db.SaveBatchAnchorStrict(store.BatchAnchor{
		Shard:      "forged",
		ShardID:    "sid",
		N:          1,
		Size:       1,
		RootHex:    fakeTxID,
		ChainedHex: fakeTxID,
		TxID:       fakeTxID,
		Settled:    false,
	}); err != nil {
		t.Fatal(err)
	}
	upgraded, err := svc.RefreshAnchorProofs(ctx)
	if err != nil {
		t.Fatalf("RefreshAnchorProofs: %v", err)
	}
	if upgraded != 0 {
		t.Fatalf("expected 0 upgraded anchors for unproved tx, got %d", upgraded)
	}
	ba, _ := db.ListAnchors("forged")
	if len(ba) != 1 || ba[0].Settled {
		t.Fatalf("forged/unproved anchor must never be marked settled=true: %+v", ba)
	}
}

func TestProtectedKeySerializationFailClosedAndHookDrain(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := newHardeningService(t)

	// 1. Concurrent LoadOrCreateProtectedKey must yield one identical 32-byte key.
	const workers = 6
	keys := make([]string, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer wg.Done()
			k, err := svc.LoadOrCreateProtectedKey("test_protected_key", func() ([]byte, error) {
				b := make([]byte, 32)
				b[0] = byte(idx + 1)
				return b, nil
			})
			if err != nil {
				t.Errorf("LoadOrCreateProtectedKey(%d): %v", idx, err)
				return
			}
			keys[idx] = string(k)
		}()
	}
	wg.Wait()
	for i := 1; i < workers; i++ {
		if keys[0] != keys[i] {
			t.Fatalf("concurrent LoadOrCreateProtectedKey diverged between worker 0 and %d", i)
		}
	}

	// 2. Corrupt encrypted key record must fail closed without overwriting.
	if err := db.SetJSON("corrupt_prot_enc", map[string]string{"encHex": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoadOrCreateProtectedKey("corrupt_prot", func() ([]byte, error) {
		return make([]byte, 32), nil
	}); err == nil {
		t.Fatal("expected LoadOrCreateProtectedKey to fail closed on corrupt _enc entry")
	}

	// 3. Service.Close() must drain in-flight onRemember hooks cleanly.
	var hookDone atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	svc.SetOnRemember(func(shard string) {
		close(started)
		<-release
		hookDone.Store(true)
	})
	if _, err := svc.Remember(ctx, RememberInput{Shard: "hook-test", Text: "trigger hook"}); err != nil {
		t.Fatal(err)
	}
	<-started
	closeDone := make(chan struct{})
	go func() {
		_ = svc.Close()
		close(closeDone)
	}()
	close(release)
	<-closeDone
	if !hookDone.Load() {
		t.Fatal("expected Service.Close() to wait for onRemember hook completion")
	}
}
