package chain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/nodeapi"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func makeDummyTx(t *testing.T) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	s, err := script.NewFromHex("006a046e656b74")
	if err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: s})
	return tx
}

func TestBSVBroadcasterImmediateAcceptAndFailover(t *testing.T) {
	ctx := context.Background()
	tx := makeDummyTx(t)
	txid := tx.TxID().String()

	var srv1Hits, srv2Hits atomic.Int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv1Hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 503, "detail": "overloaded"})
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv2Hits.Add(1)
		if r.Method == http.MethodPost && r.URL.Path == "/tx" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["rawTx"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"txid":     txid,
				"txStatus": "RECEIVED",
			})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/tx/"+txid {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"txid":     txid,
				"txStatus": "SEEN_ON_NETWORK",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv2.Close()

	bcast, urls, err := ParseBroadcaster("broadcaster:"+srv1.URL+",broadcaster:"+srv2.URL, BroadcasterOptions{
		Client: srv1.Client(),
	})
	if err != nil {
		t.Fatalf("ParseBroadcaster: %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 broadcaster URLs, got %v", urls)
	}

	// First broadcast hits srv1 (503) -> automatically fails over to srv2 (202 RECEIVED) and returns immediately!
	if err := bcast.Broadcast(ctx, tx); err != nil {
		t.Fatalf("expected failover broadcast to succeed, got %v", err)
	}
	if srv1Hits.Load() != 1 || srv2Hits.Load() != 1 {
		t.Fatalf("expected 1 hit on srv1 and 1 failover hit on srv2, got %d/%d", srv1Hits.Load(), srv2Hits.Load())
	}

	// Second broadcast round-robins directly to srv2 first (0 extra hits on srv1)!
	if err := bcast.Broadcast(ctx, tx); err != nil {
		t.Fatalf("expected round-robin broadcast to srv2 to succeed, got %v", err)
	}
	if srv1Hits.Load() != 1 || srv2Hits.Load() != 2 {
		t.Fatalf("expected round-robin direct hit on srv2 (1/2), got %d/%d", srv1Hits.Load(), srv2Hits.Load())
	}

	// Proof lookup on unmined tx returns (nil, 0, false, nil) cleanly.
	mp, h, found, err := bcast.Proof(ctx, txid)
	if err != nil || found || mp != nil || h != 0 {
		t.Fatalf("expected unmined Proof=(nil,0,false,nil), got (%v,%d,%v,%v)", mp, h, found, err)
	}
}

func TestBSVBroadcasterDeterministicRejectionDoesNotFailoverAndAll5xxIsUnknown(t *testing.T) {
	ctx := context.Background()
	tx := makeDummyTx(t)
	txid := tx.TxID().String()

	var rejHits, backupHits atomic.Int32
	srvReject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rejHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"txid":      txid,
			"txStatus":  "REJECTED",
			"extraInfo": "bad-txns-inputs-missingorspent",
		})
	}))
	defer srvReject.Close()

	srvBackup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"txid": txid, "txStatus": "RECEIVED"})
	}))
	defer srvBackup.Close()

	bcast, _, err := ParseBroadcaster("broadcaster:"+srvReject.URL+",broadcaster:"+srvBackup.URL, BroadcasterOptions{
		Client: srvReject.Client(),
	})
	if err != nil {
		t.Fatalf("ParseBroadcaster: %v", err)
	}
	err = bcast.Broadcast(ctx, tx)
	if err == nil {
		t.Fatal("expected deterministic REJECTED error")
	}
	if errors.Is(err, nwallet.ErrBroadcastUnknown) {
		t.Fatalf("deterministic REJECTED should not wrap ErrBroadcastUnknown: %v", err)
	}
	if rejHits.Load() != 1 || backupHits.Load() != 0 {
		t.Fatalf("expected deterministic rejection to stop without failover (1/0), got %d/%d", rejHits.Load(), backupHits.Load())
	}

	// All endpoints returning 503 MUST fail closed wrapping nwallet.ErrBroadcastUnknown!
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 503, "detail": "down"})
	}))
	defer srv503.Close()
	b503, _, err := ParseBroadcaster("broadcaster:"+srv503.URL, BroadcasterOptions{Client: srv503.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := b503.Broadcast(ctx, tx); !errors.Is(err, nwallet.ErrBroadcastUnknown) {
		t.Fatalf("expected 503 outage to wrap ErrBroadcastUnknown, got %v", err)
	}
}

type stubNotMinedChain struct {
	nodeapi.Chain
}

func (stubNotMinedChain) Proof(_ context.Context, _ string) (*transaction.MerklePath, uint32, error) {
	return nil, 0, nodeapi.ErrNotMined
}

type blockingWitness struct {
	canceled atomic.Bool
}

func (b *blockingWitness) HeaderAt(ctx context.Context, _ uint32) (*headers.Header, error) {
	<-ctx.Done()
	b.canceled.Store(true)
	return nil, ctx.Err()
}

func (b *blockingWitness) CurrentHeight(ctx context.Context) (uint32, error) {
	<-ctx.Done()
	b.canceled.Store(true)
	return 0, ctx.Err()
}

func (b *blockingWitness) IsValidRootForHeight(ctx context.Context, _ *chainhash.Hash, _ uint32) (bool, error) {
	<-ctx.Done()
	b.canceled.Store(true)
	return false, ctx.Err()
}

func TestBroadcaster404ProofAndFallbackClearsLastErr(t *testing.T) {
	ctx := context.Background()
	tx := makeDummyTx(t)
	txid := tx.TxID().String()

	// 1. HTTP 404 on GET /tx/{txid} must return (nil, 0, false, nil) rather than poisoning callers.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv404.Close()
	b404, _, err := ParseBroadcaster("broadcaster:"+srv404.URL, BroadcasterOptions{Client: srv404.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if mp, h, found, pErr := b404.Proof(ctx, txid); pErr != nil || found || mp != nil || h != 0 {
		t.Fatalf("expected 404 Proof=(nil,0,false,nil), got (%v,%d,%v,%v)", mp, h, found, pErr)
	}

	// 2. Even if broadcaster returns 503 outage, ProofChecked clears lastErr when fallback Chain.Proof returns ErrNotMined.
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv503.Close()
	b503, _, err := ParseBroadcaster("broadcaster:"+srv503.URL, BroadcasterOptions{Client: srv503.Client()})
	if err != nil {
		t.Fatal(err)
	}
	p := &Port{
		network:     "testnet",
		tracker:     &stubWitness{headers: map[uint32]*headers.Header{}, tip: 10},
		broadcaster: b503,
		nodeChain:   stubNotMinedChain{},
	}
	if mp, h, found, pErr := p.ProofChecked(ctx, txid); pErr != nil || found || mp != nil || h != 0 {
		t.Fatalf("expected ProofChecked to clear broadcaster 503 error on fallback ErrNotMined, got (%v,%d,%v,%v)", mp, h, found, pErr)
	}
}

func TestWitnessedTrackerEarlyQuorumCancelsSlowStraggler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w1 := &stubWitness{headers: make(map[uint32]*headers.Header), tip: 8}
	w2 := &stubWitness{headers: make(map[uint32]*headers.Header), tip: 8}
	slow := &blockingWitness{}

	var prev chainhash.Hash
	for h := uint32(0); h <= 8; h++ {
		mr := chainhash.DoubleHashH([]byte{byte(h), 0x99})
		hdr := mineStubHeader(h, prev, mr, 1_700_000_000+h*600)
		w1.headers[h] = hdr
		cp := *hdr
		w2.headers[h] = &cp
		prev = hdr.Hash
	}

	wt, err := NewWitnessedTracker("regtest", []NamedWitness{
		{Name: "w1", Origin: "https://w1.example", Kind: headers.WhatsOnChain, Source: w1},
		{Name: "w2", Origin: "https://w2.example", Kind: headers.Arcade, Source: w2},
		{Name: "slow", Origin: "https://slow.example", Kind: headers.BlockHeadersService, Source: slow},
	})
	if err != nil {
		t.Fatalf("NewWitnessedTracker: %v", err)
	}
	wt.quorum = 2
	wt.minDifficulty = 0

	t0 := time.Now()
	tip, err := wt.CurrentHeight(ctx)
	if err != nil || tip != 8 {
		t.Fatalf("expected early-quorum CurrentHeight=8, got %d err=%v", tip, err)
	}
	if time.Since(t0) > time.Second {
		t.Fatalf("early-quorum CurrentHeight stalled on slow witness: %v", time.Since(t0))
	}
	hdr, err := wt.HeaderAt(ctx, 5)
	if err != nil || hdr == nil || hdr.Height != 5 {
		t.Fatalf("expected early-quorum HeaderAt(5), got %v err=%v", hdr, err)
	}
}

func TestZeroConfWatchConflictEvictionAndCap(t *testing.T) {
	w := NewZeroConfWatch()
	w.defaultTTL = 50 * time.Millisecond

	if err := w.CheckAndTrack("tx-1", 100, []string{"op:0"}, 1000); err != nil {
		t.Fatalf("track tx-1: %v", err)
	}
	if err := w.CheckAndTrack("tx-2", 100, []string{"op:0"}, 1000); !errors.Is(err, ErrConflictDetected) {
		t.Fatalf("expected ErrConflictDetected, got %v", err)
	}
	if !w.HasConflict("tx-1") || !w.HasConflict("tx-2") {
		t.Fatal("expected both tx-1 and tx-2 flagged as conflicted")
	}
	// Settling tx-1 removes its conflict entry (and untracked competing entry).
	w.MarkSettled("tx-1")
	if w.HasConflict("tx-1") {
		t.Fatal("expected MarkSettled to clear conflict entry for tx-1")
	}

	// TTL expiry evicts stale conflicts.
	w.mu.Lock()
	w.recordConflictLocked("tx-old", "tx-comp", time.Now().Add(-time.Hour))
	w.mu.Unlock()
	if w.HasConflict("tx-old") {
		t.Fatal("expected expired conflict entry to be evicted")
	}
}
