package chain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

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
