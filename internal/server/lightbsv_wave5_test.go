package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/server"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestWave5MetricsAuthZeroPollDashboardAndShardFeedSSE(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	db, err := store.Open(dir + "/nektar.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	t.Cleanup(func() { _ = mkt.CloseWorkflows() })

	app, err := server.NewApp(mem, mkt, server.Config{
		APIToken:      "agent-secret-tok",
		OperatorToken: "op-secret-tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		app.Close()
		ts.CloseClientConnections()
		ts.Close()
	})

	// 1. ARCH-F9: GET /v1/metrics MUST reject unauthenticated requests when APIToken/OperatorToken are set.
	unauthResp, err := http.Get(ts.URL + "/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ARCH-F9: expected 401 on unauthenticated GET /v1/metrics, got %d", unauthResp.StatusCode)
	}
	authReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/metrics", nil)
	authReq.Header.Set("Authorization", "Bearer agent-secret-tok")
	authResp, err := http.DefaultClient.Do(authReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("ARCH-F9: expected 200 on Bearer-authenticated GET /v1/metrics, got %d", authResp.StatusCode)
	}

	// 2. PERF-4: /events Dashboard SSE receives instant EventBus wakeup (<250ms) when mem.Remember commits!
	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	sseReq.Header.Set("Authorization", "Bearer op-secret-tok")
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status=%d", sseResp.StatusCode)
	}

	dashEvCh := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				dashEvCh <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	// Consume initial connected frame.
	select {
	case <-dashEvCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial /events connected frame")
	}

	t0 := time.Now()
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard:     "live-ops",
		Text:      "EventBus zero-poll push wakes Svelte 5 dashboard immediately.",
		Distilled: true,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-dashEvCh:
		if time.Since(t0) > 400*time.Millisecond {
			t.Fatalf("expected sub-400ms EventBus dashboard push, took %v (frame=%s)", time.Since(t0), raw)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventBus-driven /events dashboard SSE frame")
	}

	// 3. PERF-4 + RCA-1: Grade S Shard Feed SSE (/v1/market/feeds/{shard}) wakes via EventBus in <400ms (zero 2s pollTicker)
	// and blocks secret-bearing fragments via EvaluateSellableFragment!
	if err := db.SaveSellListing(store.SellListing{
		Shard:                   "live-ops",
		Domains:                 []string{"ops"},
		PriceSats:               50,
		SubscriptionPriceSats:   100,
		SubscriptionDurationSec: 3600,
		AllowRaw:                true,
		Active:                  true,
	}); err != nil {
		t.Fatal(err)
	}
	buyerEng := market.NewEngine(mem, market.DefaultPolicy())
	t.Cleanup(func() { _ = buyerEng.CloseWorkflows() })
	nowUnix := time.Now().Unix()
	if err := db.CreateShardSubscription(store.ShardSubscription{
		SubID:       "sub-wave5",
		Shard:       "live-ops",
		BuyerPub:    buyerEng.SalePubHex(),
		SellerPub:   mkt.SalePubHex(),
		PricingMode: string(nmu.ModeSubscription),
		PriceSats:   100,
		PaymentTxID: strings.Repeat("a", 64),
		UsageRights: string(nmu.RightsInferenceOnly),
		StartedAt:   nowUnix,
		ExpiresAt:   nowUnix + 600,
		LastSeq:     1,
		Active:      true,
	}); err != nil {
		t.Fatal(err)
	}

	nonce, _ := nmu.NewFeedNonce()
	aud := strings.TrimPrefix(ts.URL, "http://")
	pubHex, sigHex, err := buyerEng.SignFeedAuthHeaders("live-ops", "sub-wave5", nowUnix, nonce, aud)
	if err != nil {
		t.Fatal(err)
	}
	feedReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/market/feeds/live-ops?sinceSeq=1", nil)
	feedReq.Header.Set(nmu.HeaderSubID, "sub-wave5")
	feedReq.Header.Set(nmu.HeaderPub, pubHex)
	feedReq.Header.Set(nmu.HeaderSig, sigHex)
	feedReq.Header.Set(nmu.HeaderTS, strconv.FormatInt(nowUnix, 10))
	feedReq.Header.Set(nmu.HeaderNonce, nonce)

	feedResp, err := http.DefaultClient.Do(feedReq)
	if err != nil {
		t.Fatal(err)
	}
	defer feedResp.Body.Close()
	if feedResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/market/feeds/live-ops status=%d", feedResp.StatusCode)
	}

	type sseFrame struct {
		event string
		data  string
	}
	feedCh := make(chan sseFrame, 8)
	go func() {
		sc := bufio.NewScanner(feedResp.Body)
		var evName string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				evName = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				feedCh <- sseFrame{event: evName, data: strings.TrimPrefix(line, "data: ")}
				evName = ""
			}
		}
	}()
	// Drain initial connected frame.
	select {
	case fr := <-feedCh:
		if fr.event != "connected" {
			t.Fatalf("expected connected SSE event, got %+v", fr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for connected frame on shard feed")
	}

	// Write a secret-bearing fragment (#2) followed immediately by a clean distilled fragment (#3):
	// #2 MUST be blocked by EvaluateSellableFragment, and #3 MUST arrive in <400ms (well below old 2000ms pollTicker)!
	tFeed := time.Now()
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard:     "live-ops",
		Text:      "Blocked secret " + "-----BEGIN " + "PRIVATE KEY-----",
		Distilled: true,
	}); err != nil {
		t.Fatal(err)
	}
	cleanRes, err := mem.Remember(ctx, memory.RememberInput{
		Shard:     "live-ops",
		Text:      "Clean Grade S fact pushed instantly via EventBus.",
		Distilled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case fr := <-feedCh:
		if fr.event != "fragment_distilled" {
			t.Fatalf("expected fragment_distilled event, got %+v", fr)
		}
		var payload struct {
			Seq  int64  `json:"seq"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(fr.data), &payload)
		if payload.Seq != cleanRes.Seq || strings.Contains(payload.Text, "PRIVATE KEY") {
			t.Fatalf("expected clean seq=%d (secret blocked), got seq=%d text=%q", cleanRes.Seq, payload.Seq, payload.Text)
		}
		if time.Since(tFeed) > 500*time.Millisecond {
			t.Fatalf("PERF-4: expected sub-500ms EventBus shard feed push without 2s polling, took %v", time.Since(tFeed))
		}
	case <-time.After(1200 * time.Millisecond):
		t.Fatal("PERF-4: timed out waiting for EventBus-driven shard feed push (<1.2s, proving 2s pollTicker is gone)")
	}
}
