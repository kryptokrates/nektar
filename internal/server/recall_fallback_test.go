package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/autonomy"
	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestPerQuery402BuyerRecallFallbackAndFailClosedSafety(t *testing.T) {
	ctx := context.Background()
	sharedChain := chain.NewSharedMemoryChain()

	openTestApp := func(t *testing.T) (*App, *market.Broker, *discovery.Service, func()) {
		t.Helper()
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		port, err := chain.Open(ctx, chain.Config{
			Network:           "memory",
			DataDir:           t.TempDir(),
			SharedMemoryChain: sharedChain,
		})
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
		mkt := market.NewEngine(mem, market.DefaultPolicy())
		disc := discovery.New(mem, nil, nil)
		wd := autonomy.NewWatchdog(autonomy.Config{Network: "memory", PeerConsecutiveFailures: 2})
		broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{Watchdog: wd})
		app, err := NewApp(mem, mkt, Config{Discovery: disc, Broker: broker, Watchdog: wd})
		if err != nil {
			broker.Stop()
			_ = port.Close()
			_ = db.Close()
			t.Fatalf("NewApp: %v", err)
		}
		return app, broker, disc, func() {
			app.Close()
			broker.Stop()
			_ = port.Close()
			_ = db.Close()
		}
	}

	// 1. Setup Seller with an anchored, distilled fact and an active per_query listing (15 sats).
	sellerApp, sellerBroker, _, closeSeller := openTestApp(t)
	defer closeSeller()
	if _, err := sellerApp.mem.Remember(ctx, memory.RememberInput{
		Shard:       "market-ops",
		Text:        "Kubernetes kubelet eviction hard threshold is memory.available<100Mi on production nodes",
		Distilled:   true,
		UsageRights: string(nmu.RightsInferenceOnly),
		AutoAnchor:  true,
	}); err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	if err := sellerApp.mem.DB().SaveSellListing(store.SellListing{
		Shard:          "market-ops",
		Domains:        []string{"ops"},
		PriceSats:      100,
		QueryPriceSats: 15,
		UsageRights:    string(nmu.RightsInferenceOnly),
		AllowRaw:       true,
		Active:         true,
	}); err != nil {
		t.Fatalf("seller UpsertSellListing: %v", err)
	}
	sellerTS := httptest.NewServer(sellerApp.Handler())
	defer sellerTS.Close()

	// 2. Setup Buyer with empty local memory + Seller discovered in peer directory.
	buyerApp, buyerBroker, buyerDisc, closeBuyer := openTestApp(t)
	defer closeBuyer()
	if _, err := market.ApprovePolicyPreset(buyerApp.mem.DB(), buyerApp.mkt.Policy(), "buyer", []string{"ops"}, "test"); err != nil {
		t.Fatalf("buyer ApprovePolicyPreset: %v", err)
	}
	buyerDisc.UpsertPeer(discovery.PeerInfo{
		Pub:         sellerBroker.PubHex(),
		PublicURL:   sellerTS.URL,
		SellDomains: []string{"ops"},
		Exp:         time.Now().Add(time.Hour).UnixMilli(),
		Packs: []bstack.PackRef{{
			Shard:          "market-ops",
			Domains:        []string{"ops"},
			PricingMode:    string(nmu.ModePerQuery),
			EnabledModes:   []string{string(nmu.ModePerQuery)},
			QueryPriceSats: 15,
			QueryEndpoint:  sellerTS.URL + "/v1/market/query",
		}},
	})

	// 3. Default posture (AutoQueryOnMiss == false) MUST fail closed: 0 hits, 0 sats spent, miss recorded!
	resOff, err := buyerApp.Toolbox().ExecRecall(ctx, memory.RecallInput{
		Shard: "ops",
		Query: "kubelet eviction hard threshold",
	})
	if err != nil {
		t.Fatalf("ExecRecall default off: %v", err)
	}
	if resOff["count"].(int) != 0 {
		t.Fatalf("expected 0 hits when AutoQueryOnMiss is off, got %v", resOff["count"])
	}
	if resOff["marketFallback"] != nil {
		t.Fatalf("expected nil marketFallback when disabled, got %+v", resOff["marketFallback"])
	}
	misses, _ := buyerApp.mem.DB().ListEligibleRecallMisses(1, 0, time.Now().Unix(), 10)
	if len(misses) != 1 || misses[0].MissCount != 1 {
		t.Fatalf("expected 1 recorded recall miss, got %+v", misses)
	}

	// 4. Enable opt-in AutoQueryOnMiss with 25-sat cap -> ExecRecall pays 402 (15 sats), SPV-verifies, caches Tainted!
	if err := buyerApp.mkt.Policy().SetAutonomyConfig(market.PolicyAutonomyConfig{
		AutoQueryOnMiss:  true,
		MaxAutoQuerySats: 25,
	}); err != nil {
		t.Fatalf("SetAutonomyConfig: %v", err)
	}
	resOn, err := buyerApp.Toolbox().ExecRecall(ctx, memory.RecallInput{
		Shard: "ops",
		Query: "kubelet eviction hard threshold",
	})
	if err != nil {
		t.Fatalf("ExecRecall opt-in fallback: %v", err)
	}
	if resOn["count"].(int) != 1 {
		t.Fatalf("expected 1 hit via per_query 402 fallback, got %v", resOn["count"])
	}
	fb, ok := resOn["marketFallback"].(map[string]any)
	if !ok || fb["used"] != true || fb["paidSats"].(uint64) != 15 || fb["spvVerifiedCount"].(int) != 1 {
		t.Fatalf("unexpected marketFallback metadata: %+v", resOn["marketFallback"])
	}
	items := resOn["items"].([]memory.RecalledFragment)
	if !items[0].Tainted {
		t.Fatalf("expected cached fallback fragment to be marked Tainted=true")
	}

	// 5. Anti-MITM spoofed sellerPub in 402 challenge MUST fail closed and trip per-peer breaker!
	spoofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-BSV-Payment-Satoshis-Required", "5")
		w.Header().Set("X-BSV-Payment-Identity", sellerApp.mem.Chain().IdentityPubHex())
		w.WriteHeader(http.StatusPaymentRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"amountSats":  5,
			"identityKey": sellerApp.mem.Chain().IdentityPubHex(),
			"sellerPub":   "02ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		})
	}))
	defer spoofSrv.Close()

	expectedPub := "021111111111111111111111111111111111111111111111111111111111111111"
	for i := 0; i < 2; i++ {
		if _, err := buyerBroker.QueryPeer402(ctx, market.PeerQueryRequest{
			QueryEndpoint: spoofSrv.URL,
			SellerPub:     expectedPub,
			Shard:         "market-ops",
			Query:         "test",
			Domains:       []string{"ops"},
			MaxSats:       25,
		}); err == nil {
			t.Fatalf("expected anti-MITM sellerPub mismatch error on attempt %d", i+1)
		}
	}
	if err := buyerBroker.Watchdog().AllowPeerQuery(expectedPub); err == nil {
		t.Fatalf("expected spoofed peer to be isolated by Watchdog peer circuit breaker")
	}
}
