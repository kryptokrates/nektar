package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func setupNMUServerTestApp(t *testing.T) (*App, *memory.Service, *market.Engine, *store.DB, string) {
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

	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng := market.NewEngine(mem, nil)

	// Commit default ops ontology and one distilled fragment with verified triples.
	loadedOnt, err := mem.CommitOntology(ctx, reasoning.DefaultOntologyManifest("ops"), false)
	if err != nil {
		t.Fatal(err)
	}
	ontID := loadedOnt.Manifest.Hash
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard:     "market-ops",
		Text:      "Go 1.27 requires SQLite WAL mode to fix SQLITE_BUSY lock errors.",
		Public:    true,
		Distilled: true,
		Triples: []reasoning.Triple{
			{Subject: "ops:go", Predicate: "requires", Object: "ops:sqlite"},
			{Subject: "ops:go", Predicate: "fixes", Object: "ops:cve"},
		},
		NMU: &nmu.Spec{
			Version:        nmu.SpecVersion,
			FreshnessClass: nmu.FreshnessVersioned,
			SubjectVersion: "go-1.27",
			UsageRights:    nmu.RightsDistillResellable,
		},
		Extra:      map[string]string{"ontologyId": ontID},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Arm multi-mode SellListing: spot (50 sats) + per_query (15 sats) + subscription (200 sats / 1h).
	if err := db.SaveSellListing(store.SellListing{
		Shard:                   "market-ops",
		Domains:                 []string{"ops"},
		PriceSats:               50,
		QueryPriceSats:          15,
		SubscriptionPriceSats:   200,
		SubscriptionDurationSec: 3600,
		FreshnessClass:          "versioned",
		UsageRights:             "distill_resellable",
		OntologyID:              ontID,
		Description:             "Multi-mode ops knowledge feed",
		Active:                  true,
	}); err != nil {
		t.Fatal(err)
	}

	app, err := NewApp(mem, eng, Config{PublicURL: "https://seller.example"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, mem, eng, db, ontID
}

func TestNMUMarketQuery402AndReplayGuard(t *testing.T) {
	app, _, _, _, ontID := setupNMUServerTestApp(t)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	reqBody := `{"shard":"market-ops","query":"SQLite","minGrade":"B"}`

	// 1. Unpaid call returns HTTP 402 with exact BRC-105 challenge headers.
	resp, err := http.Post(ts.URL+"/v1/market/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 Payment Required, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-BSV-Payment-Satoshis-Required") != "15" {
		t.Fatalf("expected X-BSV-Payment-Satoshis-Required=15, got %q", resp.Header.Get("X-BSV-Payment-Satoshis-Required"))
	}

	// 2. Paid call with X-BSV-Payment succeeds and returns FactPack + signed DealReceipt.
	payHdr, _ := json.Marshal(BRC105PaymentHeader{
		TxID:       "1111111111111111111111111111111111111111111111111111111111111111",
		AmountSats: 15,
		Inputs:     []string{"prevtx:0"},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/market/query", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BSV-Payment", string(payHdr))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on paid query, got %d", resp2.StatusCode)
	}
	var out struct {
		OK               bool              `json:"ok"`
		Count            int               `json:"count"`
		TotalUnits       uint32            `json:"totalUnits"`
		Items            []MarketQueryItem `json:"items"`
		DealReceiptToken string            `json:"dealReceiptToken"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Count != 1 || out.TotalUnits != 2 || len(out.Items) != 1 {
		t.Fatalf("unexpected query result: %+v", out)
	}
	if out.Items[0].OntologyID != ontID || out.Items[0].NMU == nil || out.Items[0].NMU.Grade != nmu.GradeB {
		t.Fatalf("unexpected item NMU/ontology: %+v", out.Items[0])
	}
	if !strings.HasPrefix(out.DealReceiptToken, market.DealReceiptTokenPrefix) {
		t.Fatalf("expected signed nektardr1.* dealReceiptToken, got %q", out.DealReceiptToken)
	}

	// 3. Replaying the exact same payment txid MUST fail closed with 409 Conflict!
	reqReplay, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/market/query", strings.NewReader(reqBody))
	reqReplay.Header.Set("Content-Type", "application/json")
	reqReplay.Header.Set("X-BSV-Payment", string(payHdr))
	resp3, err := http.DefaultClient.Do(reqReplay)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on replayed payment, got %d", resp3.StatusCode)
	}
}

func TestNMUShardSubscriptionReplayWindowAndGradeSSupersessionPush(t *testing.T) {
	app, mem, _, _, _ := setupNMUServerTestApp(t)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	buyerPriv, _ := ec.NewPrivateKey()
	buyerPub := hex.EncodeToString(buyerPriv.PubKey().Compressed())

	subReqBody, _ := json.Marshal(map[string]any{"shard": "market-ops", "buyerPub": buyerPub})
	payHdr, _ := json.Marshal(BRC105PaymentHeader{
		TxID:       "2222222222222222222222222222222222222222222222222222222222222222",
		AmountSats: 200,
		Inputs:     []string{"subprev:0"},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/market/subscriptions", bytes.NewReader(subReqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BSV-Payment", string(payHdr))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on subscribe, got %d", resp.StatusCode)
	}
	var subOut struct {
		OK          bool                `json:"ok"`
		SubID       string              `json:"subId"`
		FeedURL     string              `json:"feedUrl"`
		ExpiresAt   int64               `json:"expiresAt"`
		DealReceipt *market.DealReceipt `json:"dealReceipt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&subOut); err != nil {
		t.Fatal(err)
	}
	if !subOut.OK || subOut.SubID == "" || subOut.DealReceipt == nil {
		t.Fatalf("unexpected subscribe response: %+v", subOut)
	}

	// 1. Captured-header replay outside +/-60s MUST fail with 401 Unauthorized!
	staleTS := time.Now().Unix() - 120
	pubH, sigH, err := nmu.SignFeedAuth(buyerPriv, "market-ops", subOut.SubID, staleTS)
	if err != nil {
		t.Fatal(err)
	}
	staleReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/market/feeds/market-ops", nil)
	staleReq.Header.Set(nmu.HeaderSubID, subOut.SubID)
	staleReq.Header.Set(nmu.HeaderPub, pubH)
	staleReq.Header.Set(nmu.HeaderSig, sigH)
	staleReq.Header.Set(nmu.HeaderTS, strconv.FormatInt(staleTS, 10))
	staleResp, err := http.DefaultClient.Do(staleReq)
	if err != nil {
		t.Fatal(err)
	}
	staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for stale >60s header, got %d", staleResp.StatusCode)
	}

	// 2. Live authenticated SSE stream receives replayed fragment_distilled AND real-time Grade S fragment_superseded!
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nowTS := time.Now().Unix()
	pubH2, sigH2, _ := nmu.SignFeedAuth(buyerPriv, "market-ops", subOut.SubID, nowTS)
	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/market/feeds/market-ops", nil)
	sseReq.Header.Set(nmu.HeaderSubID, subOut.SubID)
	sseReq.Header.Set(nmu.HeaderPub, pubH2)
	sseReq.Header.Set(nmu.HeaderSig, sigH2)
	sseReq.Header.Set(nmu.HeaderTS, strconv.FormatInt(nowTS, 10))
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK SSE stream, got %d", sseResp.StatusCode)
	}

	var mu sync.Mutex
	seenDistilled := false
	seenSuperseded := false
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		buf := make([]byte, 4096)
		var acc string
		for {
			n, rErr := sseResp.Body.Read(buf)
			if n > 0 {
				acc += string(buf[:n])
				mu.Lock()
				if strings.Contains(acc, "event: fragment_distilled") {
					seenDistilled = true
				}
				if strings.Contains(acc, "event: fragment_superseded") {
					seenSuperseded = true
				}
				done := seenDistilled && seenSuperseded
				mu.Unlock()
				if done {
					cancel()
					return
				}
			}
			if rErr != nil {
				return
			}
		}
	}()

	// Trigger a real Grade S bi-temporal supersession on market-ops#1!
	time.Sleep(150 * time.Millisecond)
	_, err = mem.Remember(context.Background(), memory.RememberInput{
		Shard:      "market-ops",
		Text:       "Go 1.27.4 requires SQLite WAL + busy_timeout=5000ms (superseding guidance).",
		Public:     true,
		Distilled:  true,
		Supersedes: 1,
	})
	if err != nil {
		t.Fatalf("Remember superseding fragment: %v", err)
	}
	<-doneCh
	mu.Lock()
	defer mu.Unlock()
	if !seenDistilled || !seenSuperseded {
		t.Fatalf("expected both fragment_distilled (%v) and Grade S fragment_superseded (%v)", seenDistilled, seenSuperseded)
	}
}

func TestNMUBuyerDeadFeedSlashesDispBPAndCommodityTicker(t *testing.T) {
	app, _, sellerEng, _, ontID := setupNMUServerTestApp(t)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. Commodity Ticker aggregates all 3 independent modes (spot, per_query, subscription) for market-ops!
	resp, err := http.Get(ts.URL + "/v1/market/ticker")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK ticker, got %d", resp.StatusCode)
	}
	var ticker CommodityTickerResponse
	if err := json.NewDecoder(resp.Body).Decode(&ticker); err != nil {
		t.Fatal(err)
	}
	modesFound := make(map[string]bool)
	for _, row := range ticker.Rows {
		if row.OntologyID == ontID {
			modesFound[row.PricingMode] = true
		}
	}
	if !modesFound["spot"] || !modesFound["per_query"] || !modesFound["subscription"] {
		t.Fatalf("expected ticker to expose spot, per_query, and subscription modes for %s; got %+v", ontID, ticker.Rows)
	}

	// 2. Buyer SSE client hitting a dead 404 subscription feed before expires_at MUST emit DispBP <= 2500 slash!
	ctx := context.Background()
	buyerDB, _ := store.Open(":memory:")
	defer buyerDB.Close()
	buyerPort, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	defer buyerPort.Close()
	buyerMem, _ := memory.New(buyerDB, buyerPort, nil, nil, memory.Config{})
	buyerEng := market.NewEngine(buyerMem, nil)

	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer deadSrv.Close()

	dr := market.DealReceipt{
		Version:       1,
		DealID:        "sub-dead-1",
		OfferID:       "subscription:market-ops",
		BuyerPub:      buyerEng.SalePubHex(),
		SellerPub:     sellerEng.SalePubHex(),
		Domains:       []string{"ops"},
		PriceSats:     200,
		PaymentTxID:   "3333333333333333333333333333333333333333333333333333333333333333",
		KeyCommitment: "subscription:sub-dead-1",
		SettledAt:     time.Now().Unix(),
	}
	if err := sellerEng.SignSellerDealReceipt(ctx, &dr); err != nil {
		t.Fatal(err)
	}

	streamErr := buyerEng.StreamShardSubscription(ctx, market.SubscriptionStreamConfig{
		FeedURL:       deadSrv.URL + "/v1/market/feeds/market-ops",
		Shard:         "market-ops",
		SubID:         "sub-dead-1",
		SellerPub:     sellerEng.SalePubHex(),
		OntologyID:    ontID,
		Domains:       []string{"ops"},
		ExpiresAtUnix: time.Now().Unix() + 3600,
		DealReceipt:   &dr,
		HTTPClient:    deadSrv.Client(),
	}, nil)
	if !errors.Is(streamErr, market.ErrDeadSubscriptionFeed) {
		t.Fatalf("expected ErrDeadSubscriptionFeed, got %v", streamErr)
	}
	atts, err := buyerDB.ListReputationAttestations(strings.ToLower(ontID), 10)
	if err != nil || len(atts) != 1 {
		t.Fatalf("expected 1 dead-feed slashing attestation, got %d err=%v", len(atts), err)
	}
	if atts[0].DispBP > 2500 || atts[0].CollabBP > 3500 {
		t.Fatalf("expected penalized DispBP<=2500 & CollabBP<=3500, got %+v", atts[0])
	}
}

func TestNMUMarketContractToolAndDashboardState(t *testing.T) {
	app, _, _, db, ontID := setupNMUServerTestApp(t)
	ctx := context.Background()

	// 1. marketContract tool must be exposed in both agent and operator tool profiles.
	if !app.Toolbox().IsAllowedTool("marketContract") {
		t.Fatalf("expected marketContract to be registered on Toolbox")
	}

	// 2. marketContract(action: "ticker") returns normalized commodity rows.
	rawOut, err := app.Toolbox().Call(ctx, "marketContract", json.RawMessage(`{"action":"ticker"}`))
	if err != nil {
		t.Fatalf("marketContract ticker: %v", err)
	}
	tickerResp, ok := rawOut.(CommodityTickerResponse)
	if !ok || len(tickerResp.Rows) < 3 {
		t.Fatalf("expected >=3 ticker rows (spot, per_query, subscription), got %T %+v", rawOut, rawOut)
	}
	if tickerResp.AdvisoryNote != nmu.AdvisoryNMUNote {
		t.Fatalf("expected AdvisoryNMUNote, got %q", tickerResp.AdvisoryNote)
	}

	// 3. Subscription listing & operator-gated revocation through marketContract.
	if err := db.CreateShardSubscription(store.ShardSubscription{
		SubID:       "sub-tool-1",
		Shard:       "market-ops",
		BuyerPub:    "02" + strings.Repeat("aa", 32),
		SellerPub:   "03" + strings.Repeat("bb", 32),
		PricingMode: string(nmu.ModeSubscription),
		PriceSats:   200,
		PaymentTxID: "4444444444444444444444444444444444444444444444444444444444444444",
		UsageRights: string(nmu.RightsInferenceOnly),
		StartedAt:   time.Now().Unix(),
		ExpiresAt:   time.Now().Unix() + 3600,
		Active:      true,
	}); err != nil {
		t.Fatal(err)
	}
	subRes, err := app.Toolbox().Call(ctx, "marketContract", json.RawMessage(`{"action":"subscriptions","shard":"market-ops"}`))
	if err != nil {
		t.Fatalf("marketContract subscriptions: %v", err)
	}
	subMap, _ := subRes.(map[string]any)
	if subMap["count"] != 1 {
		t.Fatalf("expected 1 active subscription, got %+v", subMap)
	}
	if _, err := app.Toolbox().Call(ctx, "marketContract", json.RawMessage(`{"action":"revokeSubscription","subId":"sub-tool-1"}`)); err != nil {
		t.Fatalf("marketContract revokeSubscription: %v", err)
	}
	subAfter, _ := db.GetShardSubscription("sub-tool-1")
	if subAfter == nil || subAfter.Active {
		t.Fatalf("expected sub-tool-1 revoked, got %+v", subAfter)
	}

	// 4. Dashboard /state exposes `ticker` and enriched multi-mode `market` listings.
	st := app.BuildDashboardState(ctx, "http://127.0.0.1:18420")
	dashTicker, ok := st["ticker"].(CommodityTickerResponse)
	if !ok || len(dashTicker.Rows) < 3 {
		t.Fatalf("expected Dashboard state ticker with >=3 rows, got %T %+v", st["ticker"], st["ticker"])
	}
	mktRows, ok := st["market"].([]map[string]any)
	if !ok || len(mktRows) == 0 {
		t.Fatalf("expected Dashboard state market rows, got %T %+v", st["market"], st["market"])
	}
	if mktRows[0]["grade"] == "" {
		t.Fatalf("expected non-empty NMU grade on Dashboard market row, got %+v", mktRows[0])
	}
	modes, _ := mktRows[0]["modes"].([]string)
	if len(modes) != 3 {
		t.Fatalf("expected 3 enabled modes on Dashboard market row (spot, per_query, subscription), got %+v (ont=%s)", modes, ontID)
	}
}
