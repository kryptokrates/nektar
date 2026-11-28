package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/autonomy"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestSubscriptionSupervisorSurvivesRestartAndSlashesDeadFeed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nektar.sqlite")

	openNode := func(t *testing.T) (*memory.Service, *Engine, func()) {
		t.Helper()
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		port, err := chain.Open(context.Background(), chain.Config{Network: "memory", DataDir: filepath.Join(dir, "wallet")})
		if err != nil {
			_ = db.Close()
			t.Fatalf("chain.Open: %v", err)
		}
		mem, err := memory.New(db, port, nil, nil, memory.Config{})
		if err != nil {
			_ = port.Close()
			_ = db.Close()
			t.Fatalf("memory.New: %v", err)
		}
		eng := NewEngine(mem, DefaultPolicy())
		return mem, eng, func() {
			_ = port.Close()
			_ = db.Close()
		}
	}

	var mu sync.Mutex
	var seenSinceSeqs []int64
	var phase atomic.Int32
	phase.Store(1) // 1 = emit seq 1..2, 2 = emit seq 3 + supersede seq 1, 3 = HTTP 410 dead feed

	sseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subID := r.Header.Get(nmu.HeaderSubID)
		pub := r.Header.Get(nmu.HeaderPub)
		sig := r.Header.Get(nmu.HeaderSig)
		nonce := r.Header.Get(nmu.HeaderNonce)
		ts, _ := strconv.ParseInt(r.Header.Get(nmu.HeaderTS), 10, 64)
		if err := nmu.VerifyFeedAuth(pub, sig, "market-ops", subID, ts, time.Now().Unix(), nonce, strings.ToLower(r.Host)); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("sinceSeq"), 10, 64)
		mu.Lock()
		seenSinceSeqs = append(seenSinceSeqs, since)
		mu.Unlock()

		curPhase := phase.Load()
		if curPhase == 3 {
			http.Error(w, "gone", http.StatusGone)
			return
		}

		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeEv := func(ev string, payload any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
			fl.Flush()
		}

		if curPhase == 1 {
			if since < 1 {
				writeEv("fragment_distilled", map[string]any{
					"shard": "market-ops", "seq": 1, "text": "Redis cache TTL defaults to 300s in prod",
					"usageRights": "inference_only",
				})
			}
			if since < 2 {
				writeEv("fragment_distilled", map[string]any{
					"shard": "market-ops", "seq": 2, "text": "Postgres connection pool capped at 64 connections",
					"usageRights": "inference_only",
				})
			}
			<-r.Context().Done()
			return
		}

		if curPhase == 2 {
			if since < 3 {
				writeEv("fragment_distilled", map[string]any{
					"shard": "market-ops", "seq": 3, "text": "Redis cache TTL updated to 900s in prod v2",
					"usageRights": "inference_only",
				})
			}
			if since < 4 {
				writeEv("fragment_superseded", memory.SupersessionEvent{
					Shard: "market-ops", NewSeq: 4, SupersededSeq: 1, Reason: "TTL changed", Timestamp: time.Now().Unix(),
				})
			}
			<-r.Context().Done()
		}
	}))
	defer sseSrv.Close()

	// Boot #1: register active buyer subscription, stream seq 1 & 2, then shut down (simulating reboot).
	mem1, eng1, close1 := openNode(t)
	sellerPub := "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dr := DealReceipt{
		Version:     1,
		DealID:      "sub-reboot-1",
		OfferID:     "subscription:market-ops",
		BuyerPub:    eng1.SalePubHex(),
		SellerPub:   sellerPub,
		Domains:     []string{"ops"},
		PriceSats:   50,
		PaymentTxID: "1111111111111111111111111111111111111111111111111111111111111111",
		SettledAt:   time.Now().Unix(),
	}
	drJSON, _ := json.Marshal(dr)
	sup1 := NewSubscriptionSupervisor(mem1, eng1, SubscriptionSupervisorConfig{
		HTTPClient:   sseSrv.Client(),
		MaxRetries:   1,
		RetryBackoff: 20 * time.Millisecond,
	})
	if err := sup1.Ensure(store.ShardSubscription{
		SubID:           "sub-reboot-1",
		Shard:           "market-ops",
		BuyerPub:        eng1.SalePubHex(),
		SellerPub:       sellerPub,
		PriceSats:       50,
		PaymentTxID:     dr.PaymentTxID,
		UsageRights:     "inference_only",
		StartedAt:       time.Now().Unix(),
		ExpiresAt:       time.Now().Add(time.Hour).Unix(),
		Active:          true,
		FeedURL:         sseSrv.URL + "/v1/market/feeds/market-ops",
		Domains:         []string{"ops"},
		TargetShard:     "sub-ops",
		DealReceiptJSON: drJSON,
	}); err != nil {
		t.Fatalf("sup1.Ensure: %v", err)
	}
	if err := sup1.Start(context.Background()); err != nil {
		t.Fatalf("sup1.Start: %v", err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		sub, _ := mem1.DB().GetShardSubscription("sub-reboot-1")
		return sub != nil && sub.LastSeq >= 2
	})
	sup1.Stop()
	close1()

	// Boot #2 (after restart): reopen same SQLite DB; supervisor auto-reloads and resumes from sinceSeq=2!
	phase.Store(2)
	mem2, eng2, close2 := openNode(t)
	wd := autonomy.NewWatchdog(autonomy.Config{Network: "memory", SlashBurstThreshold: 1, BreakerCooldown: time.Minute})
	sup2 := NewSubscriptionSupervisor(mem2, eng2, SubscriptionSupervisorConfig{
		Watchdog:     wd,
		HTTPClient:   sseSrv.Client(),
		MaxRetries:   1,
		RetryBackoff: 20 * time.Millisecond,
	})
	if err := sup2.Start(context.Background()); err != nil {
		t.Fatalf("sup2.Start: %v", err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		sub, _ := mem2.DB().GetShardSubscription("sub-reboot-1")
		return sub != nil && sub.LastSeq >= 4
	})
	sup2.Stop()

	// Verify sinceSeq=2 was passed on reboot reconnect and seq 1..2 were NOT duplicated.
	mu.Lock()
	gotSince := append([]int64(nil), seenSinceSeqs...)
	mu.Unlock()
	if len(gotSince) < 2 || gotSince[1] != 2 {
		t.Fatalf("expected reconnect after restart to send sinceSeq=2, got %v", gotSince)
	}
	recalled, err := mem2.Recall(context.Background(), memory.RecallInput{Shard: "sub-ops"})
	if err != nil {
		t.Fatalf("Recall sub-ops: %v", err)
	}
	// Active fragments in sub-ops: seq 2 (Postgres), seq 3 (Redis v2), plus supersession notice replacing seq 1 (total 3 active, original seq 1 superseded!).
	if len(recalled) != 3 {
		t.Fatalf("expected 3 active non-superseded fragments in sub-ops after seq 1 supersession, got %d", len(recalled))
	}
	close2()

	// Boot #3: seller feed disappears (HTTP 410 Gone before expiresAt) -> dead-feed slash + watchdog alert + marked slashed in SQLite!
	phase.Store(3)
	mem3, eng3, close3 := openNode(t)
	defer close3()
	sup3 := NewSubscriptionSupervisor(mem3, eng3, SubscriptionSupervisorConfig{
		Watchdog:     wd,
		HTTPClient:   sseSrv.Client(),
		MaxRetries:   1,
		RetryBackoff: 20 * time.Millisecond,
	})
	if err := sup3.Start(context.Background()); err != nil {
		t.Fatalf("sup3.Start: %v", err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		sub, _ := mem3.DB().GetShardSubscription("sub-reboot-1")
		return sub != nil && sub.Slashed && !sub.Active
	})
	sup3.Stop()

	if wdSt := wd.Status(); !wdSt.SpendPaused || wdSt.SlashEventsWindow < 1 {
		t.Fatalf("expected watchdog to record dead-feed slash burst and pause spend, got %+v", wdSt)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
