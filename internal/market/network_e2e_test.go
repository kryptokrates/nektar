package market_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func newTestNode(t *testing.T, ctx context.Context, envHosts []string, isHub bool, withPublicHTTP bool) (*memory.Service, *market.Broker, *httptest.Server) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	disc := discovery.New(mem, envHosts, nil)
	eng := market.NewEngine(mem, market.DefaultPolicy())
	broker := market.NewBroker(mem, eng, disc, market.BrokerConfig{
		IsHub:          isHub,
		CollectTimeout: 1200 * time.Millisecond,
	})
	t.Cleanup(market.AttachTestRestate(broker))

	if !withPublicHTTP {
		return mem, broker, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/chirp/{hash}", func(w http.ResponseWriter, r *http.Request) {
		raw, err := hex.DecodeString(r.PathValue("hash"))
		if err != nil || len(raw) != 32 {
			http.Error(w, "bad hash", http.StatusBadRequest)
			return
		}
		var h [32]byte
		copy(h[:], raw)
		b, err := mem.Objects().GetObject(h)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("POST /v1/market/purchase", func(w http.ResponseWriter, r *http.Request) {
		var req market.PurchaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := broker.HandlePurchase(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(market.PurchaseResponse{OK: false, Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	broker.SetPublicURL(ts.URL)
	return mem, broker, ts
}

func TestLiveThreeNodeA2AHubBountyAndNATBboxMarket(t *testing.T) {
	// 1. Start the Hub (durable SQLite + Liquidity Engine + Curated Starter Shards + Bounties + Faucet!).
	hubNode, err := hub.NewNode(hub.Config{Network: "memory", EnableFinger: true})
	if err != nil {
		t.Fatalf("hub.NewNode: %v", err)
	}
	defer hubNode.Close()
	hubTS := httptest.NewServer(hubNode.Handler())
	defer hubTS.Close()

	// Cancel SSE loops BEFORE hubTS.Close() runs (Go defers execute LIFO)!
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		hubTS.CloseClientConnections()
	}()

	hubMem, hubBroker, _ := newTestNode(t, ctx, []string{hubTS.URL}, true, true)
	liq, err := hub.ArmHubLiquidity(ctx, hubNode, hubMem, hub.LiquidityConfig{
		SeedStarterShards: true,
		ArmBounties:       true,
		StarterPriceSats:  90,
		BountyRewardSats:  120,
	})
	if err != nil {
		t.Fatalf("ArmHubLiquidity: %v", err)
	}
	hubBroker.SetAdvertisedPacks(liq.Packs, liq.BountyDomains)
	_, _ = hubBroker.AnnounceSelf(ctx)
	hubBroker.Start(ctx)

	// 2. Start Organic Seller Agent B (arms its own domain shard for 60 sats AND sells a verified fact TO the Hub!).
	sellerMem, sellerBroker, _ := newTestNode(t, ctx, []string{hubTS.URL}, false, true)
	if _, err := market.ApprovePolicyPreset(sellerMem.DB(), sellerBroker.Engine().Policy(), "both", nil, "test"); err != nil {
		t.Fatalf("seller approve policy: %v", err)
	}
	remRes, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "ops-runbooks",
		Text:       "Production Redis failover sentinel runs on port 26379 with quorum 3.",
		Source:     "sre:alice",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	if _, err := sellerBroker.ArmSell(ctx, market.SellInput{
		Shard:     "ops-runbooks",
		Domains:   []string{"ops"},
		PriceSats: 60,
		AllowRaw:  true,
	}); err != nil {
		t.Fatalf("seller ArmSell: %v", err)
	}
	sellerBroker.Start(ctx)

	// 2b. Organic Seller Agent B sells its verified anchored fragment TO THE HUB for the `bounty-ops` reward!
	payout, err := sellerBroker.SellToHub(ctx, hubTS.URL, "bounty-ops", "ops-runbooks", remRes.Seq)
	if err != nil {
		t.Fatalf("SellToHub failed: %v", err)
	}
	if !payout.Accepted || payout.RewardSats != 120 || payout.TxID == "" {
		t.Fatalf("unexpected Hub bounty payout: %+v", payout)
	}
	// Verify Hub actually stored the acquired verified knowledge into curated/ops!
	curated, _ := hubMem.Recall(ctx, memory.RecallInput{Shard: "curated/ops"})
	if len(curated) == 0 || !strings.Contains(curated[0].Text, "26379") {
		t.Fatalf("expected Hub curated/ops to hold bounty-acquired fragment, got %+v", curated)
	}

	// 3. Start Cold-Start Buyer Agent A (joins Hub, discovers peers & packs, tests policy gate, then buys A2A!).
	buyerMem, buyerBroker, _ := newTestNode(t, ctx, []string{hubTS.URL}, false, false)
	buyerBroker.Start(ctx)
	time.Sleep(150 * time.Millisecond) // allow SSE subscriptions to attach cleanly

	// 3a. Cold-start Layer-2 network pack seeding!
	seeded, err := buyerBroker.Discovery().SeedFromNetwork(ctx, []string{"ops"})
	if err != nil || seeded < 2 {
		t.Fatalf("SeedFromNetwork expected >=2 starter fragments, got n=%d err=%v", seeded, err)
	}

	// 3b. Load-bearing friction check: BEFORE approving buyer policy, Want() MUST pause as approval_pending!
	pausedDeal, err := buyerBroker.Want(ctx, market.WantRequest{
		Domains:  []string{"ops"},
		Question: "What port does Redis sentinel run on?",
		MaxSats:  150,
	})
	if err != nil {
		t.Fatalf("paused Want error: %v", err)
	}
	if pausedDeal.Status != "approval_pending" {
		t.Fatalf("expected approval_pending before buyer policy approval, got %s", pausedDeal.Status)
	}
	pendingApprs, _ := buyerMem.DB().ListPendingApprovals()
	if len(pendingApprs) != 1 {
		t.Fatalf("expected 1 pending approval record, got %d", len(pendingApprs))
	}

	// 3c. Approve buyer policy preset, then execute live A2A purchase!
	// Seller B (60 sats, organic) beats Hub fallback (90 sats) in bps ranking!
	if _, err := market.ApprovePolicyPreset(buyerMem.DB(), buyerBroker.Engine().Policy(), "buyer", nil, "operator"); err != nil {
		t.Fatalf("buyer approve policy: %v", err)
	}
	deal, err := buyerBroker.Want(ctx, market.WantRequest{
		Domains:     []string{"ops"},
		Question:    "Redis failover sentinel port",
		MaxSats:     150,
		TargetShard: "bought/ops",
	})
	if err != nil {
		t.Fatalf("buyer Want A2A failed: %v", err)
	}
	if deal.Status != "settled" || !strings.Contains(deal.DeliveredText, "26379") {
		t.Fatalf("expected settled A2A deal with Redis port 26379, got %+v", deal)
	}
	if deal.CounterpartyPub != sellerBroker.PubHex() {
		t.Fatalf("expected organic seller B (%s) to beat hub seller, got %s", sellerBroker.PubHex(), deal.CounterpartyPub)
	}
	// Verify Seller B's reputation was recorded in Buyer A's SQLite!
	rep, _ := buyerMem.DB().GetReputation(sellerBroker.PubHex())
	if rep.SettledDeals != 1 || rep.TotalSats != 60 {
		t.Fatalf("expected seller reputation {settled:1, sats:60}, got %+v", rep)
	}
	// Verify Buyer A committed the verified fact to its own memory shard!
	bought, _ := buyerMem.Recall(ctx, memory.RecallInput{Shard: "bought/ops"})
	if len(bought) != 1 || !strings.Contains(bought[0].Text, "26379") {
		t.Fatalf("expected purchased fragment in bought/ops, got %+v", bought)
	}

	// 4. Start NAT-ed Seller Agent C (ZERO inbound HTTP port: PublicURL == "") & trade purely over Hub /bbox!
	natMem, natBroker, _ := newTestNode(t, ctx, []string{hubTS.URL}, false, false)
	_, _ = market.ApprovePolicyPreset(natMem.DB(), natBroker.Engine().Policy(), "seller", nil, "test")
	_, err = natMem.Remember(ctx, memory.RememberInput{
		Shard:      "quantum-notes",
		Text:       "NAT-relayed secret: Lattice ring dimension n=1024 uses modulus q=12289.",
		Source:     "nat:carol",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := natBroker.ArmSell(ctx, market.SellInput{
		Shard:     "quantum-notes",
		Domains:   []string{"crypto"},
		PriceSats: 55,
		AllowRaw:  true,
	}); err != nil {
		t.Fatal(err)
	}
	natBroker.Start(ctx)
	time.Sleep(150 * time.Millisecond)

	natDeal, err := buyerBroker.Want(ctx, market.WantRequest{
		Domains:     []string{"crypto"},
		Question:    "Lattice ring dimension modulus",
		MaxSats:     100,
		TargetShard: "bought/crypto",
	})
	if err != nil {
		t.Fatalf("NAT bbox Want failed: %v", err)
	}
	if natDeal.Status != "settled" || !strings.Contains(natDeal.DeliveredText, "12289") {
		t.Fatalf("expected NAT bbox trade to settle with 12289, got %+v", natDeal)
	}

	// 5. Verify Hub Testnet Faucet rate-limited drip works!
	buyerAddr, _ := buyerMem.Chain().FundAddress()
	fTxID, _, sentSats, err := hub.DripFaucetBEEF(ctx, hubMem, buyerAddr, 500, 1000)
	if err != nil || fTxID == "" || sentSats != 500 {
		t.Fatalf("DripFaucet failed: tx=%s sats=%d err=%v", fTxID, sentSats, err)
	}

	// 6. Verify LiquidityMetrics scorecard reports healthy metrics & SSE wakes!
	metrics := buyerBroker.LiquidityMetrics()
	if metrics["sseWakes"].(int64) == 0 {
		t.Fatalf("expected non-zero sseWakes, got %+v", metrics)
	}
	if metrics["fillRate"].(float64) <= 0 {
		t.Fatalf("expected positive fillRate, got %+v", metrics)
	}
}
