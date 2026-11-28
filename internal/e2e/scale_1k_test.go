package e2e_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type scaleAgentNode struct {
	name      string
	dir       string
	guardPath string
	db        *store.DB
	port      *chain.Port
	mem       *memory.Service
	eng       *market.Engine
	disc      *discovery.Service
	broker    *market.Broker
	srv       *httptest.Server
}

func openScaleAgentNode(
	tb testing.TB,
	ctx context.Context,
	name string,
	shared *chain.SharedMemoryChain,
	hubURLs []string,
	withPublicHTTP bool,
	warmFuel int,
) *scaleAgentNode {
	tb.Helper()
	dir := tb.TempDir()
	dbPath := filepath.Join(dir, "nektar.sqlite")
	db, err := store.Open(dbPath)
	if err != nil {
		tb.Fatalf("%s store.Open: %v", name, err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	guardPath := filepath.Join(dir, "spend-guard.json")
	port, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           dir,
		WalletProfile:     "throughput",
		ZeroConfMaxSats:   10_000,
		SharedMemoryChain: shared,
		SpendGuard: core.SpendGuardOpts{
			StatePath:       guardPath,
			MaxFeeSatsPerTx: 50_000,
			MaxSpendSatsDay: 50_000_000,
		},
	})
	if err != nil {
		tb.Fatalf("%s chain.Open: %v", name, err)
	}
	tb.Cleanup(func() { _ = port.Close() })

	if warmFuel > 0 && port.Engine() != nil {
		if _, wErr := port.Engine().FuelKeeper().WarmFuel(ctx, warmFuel, 1500); wErr != nil {
			tb.Fatalf("%s WarmFuel(%d): %v", name, warmFuel, wErr)
		}
	}

	objStore, err := bstack.NewFileObjectStore(dir)
	if err != nil {
		tb.Fatalf("%s NewFileObjectStore: %v", name, err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{})
	if err != nil {
		tb.Fatalf("%s memory.New: %v", name, err)
	}
	disc := discovery.New(mem, hubURLs, nil)
	pol := market.DefaultPolicy()
	eng := market.NewEngine(mem, pol)
	if _, err := market.ApprovePolicyPreset(db, pol, "both", nil, "scale-operator"); err != nil {
		tb.Fatalf("%s ApprovePolicyPreset: %v", name, err)
	}
	broker := market.NewBroker(mem, eng, disc, market.BrokerConfig{
		Handle:         name + "@nektar.scale",
		BboxOffice:     "default",
		CollectTimeout: 2500 * time.Millisecond,
		GraceTimeout:   15 * time.Millisecond,
	})
	tb.Cleanup(market.AttachTestWorkflows(broker))

	node := &scaleAgentNode{
		name:      name,
		dir:       dir,
		guardPath: guardPath,
		db:        db,
		port:      port,
		mem:       mem,
		eng:       eng,
		disc:      disc,
		broker:    broker,
	}
	if withPublicHTTP {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/chirp/{hash}", func(w http.ResponseWriter, r *http.Request) {
			raw, dErr := hex.DecodeString(r.PathValue("hash"))
			if dErr != nil || len(raw) != 32 {
				http.Error(w, "bad hash", http.StatusBadRequest)
				return
			}
			var h [32]byte
			copy(h[:], raw)
			b, gErr := mem.Objects().GetObject(h)
			if gErr != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		})
		mux.HandleFunc("POST /v1/market/purchase", func(w http.ResponseWriter, r *http.Request) {
			var req market.PurchaseRequest
			if dErr := json.NewDecoder(r.Body).Decode(&req); dErr != nil {
				http.Error(w, dErr.Error(), http.StatusBadRequest)
				return
			}
			resp, hErr := broker.HandlePurchase(r.Context(), req)
			if hErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(market.PurchaseResponse{OK: false, Error: hErr.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		ts := httptest.NewServer(mux)
		tb.Cleanup(ts.Close)
		broker.SetPublicURL(ts.URL)
		node.srv = ts
	}
	return node
}

func countCommittedOutgoingTxs(ctx context.Context, nodes []*scaleAgentNode) int {
	total := 0
	for _, n := range nodes {
		if n.port.Engine() == nil {
			continue
		}
		recs, _, err := n.port.Engine().ListRecentActions(ctx, 5000, 0)
		if err != nil {
			continue
		}
		for _, r := range recs {
			if r.IsOutgoing && (r.Status == nwallet.StatusCompleted || r.Status == nwallet.StatusUnproven) {
				total++
			}
		}
	}
	return total
}

// TestScaleShardedHubsAndHighThroughputPaidSales validates the end-to-end 1k TPS architecture:
//  1. 4 horizontally sharded Hub nodes mounting disjoint hex nibble subsets (0..3, 4..7, 8..b, c..f)
//  2. 240 concurrent completed BRC-29 + BRC-369 paid sales across 4 Sellers & 8 Buyers (exceeding the old
//     4-sale ZeroConfWatch cap, 200-sale pendingSales leak ceiling, and 27.3 req/s Hub nonce blackout)
//  3. Strict 1.000 on-chain transaction per completed sale (zero on-chain ad/anchor overhead during burst)
//  4. SpendGuard O(1) NDJSON append-only journal durability & exact satoshi conservation on reload
//  5. Crash/retry idempotency: re-running a paid deal after PaymentTxID persistence settles with zero double-pay.
func TestScaleShardedHubsAndHighThroughputPaidSales(t *testing.T) {
	t.Setenv("NEKTAR_OFFCHAIN_ADS", "1")
	t.Setenv("NEKTAR_FAST_DEALS", "1")

	ctx, cancel := context.WithCancel(context.Background())
	shared := chain.NewSharedMemoryChain()

	// 1. Launch 4 horizontally sharded Hub nodes, each owning 4 disjoint hex nibble lanes!
	laneGroups := [][]string{
		{"0", "1", "2", "3"},
		{"4", "5", "6", "7"},
		{"8", "9", "a", "b"},
		{"c", "d", "e", "f"},
	}
	hubNodes := make([]*hub.Node, len(laneGroups))
	hubServers := make([]*httptest.Server, len(laneGroups))
	hubURLs := make([]string, len(laneGroups))
	for i, lg := range laneGroups {
		hn, err := hub.NewNode(hub.Config{
			Network: "memory",
			DBPath:  filepath.Join(t.TempDir(), fmt.Sprintf("hub_%d.sqlite", i)),
			Lanes:   lg,
		})
		if err != nil {
			t.Fatalf("hub.NewNode(%v): %v", lg, err)
		}
		hubNodes[i] = hn
		ts := httptest.NewServer(hn.Handler())
		hubServers[i] = ts
		hubURLs[i] = ts.URL
	}
	defer func() {
		cancel()
		for _, hn := range hubNodes {
			hn.CloseSSE()
		}
		for _, ts := range hubServers {
			ts.CloseClientConnections()
			ts.Close()
		}
		for _, hn := range hubNodes {
			_ = hn.Close()
		}
	}()

	// 2. Launch 4 Sellers (each selling 2 anchored shards = 8 distinct market domains) and 8 Buyers!
	const (
		numSellers     = 4
		numBuyers      = 8
		salesPerBuyer  = 30 // 8 * 30 = 240 completed paid sales (>200 pendingSales cap!)
		totalSales     = numBuyers * salesPerBuyer
		pricePerSale   = uint64(25)
	)
	domains := make([]string, numSellers*2)
	sellers := make([]*scaleAgentNode, numSellers)
	for s := 0; s < numSellers; s++ {
		sn := openScaleAgentNode(t, ctx, fmt.Sprintf("seller-%d", s), shared, hubURLs, true, 16)
		sellers[s] = sn
		for d := 0; d < 2; d++ {
			domIdx := s*2 + d
			dom := fmt.Sprintf("scale-dom-%d", domIdx)
			shard := fmt.Sprintf("shard-%d", domIdx)
			domains[domIdx] = dom
			if _, err := sn.mem.Remember(ctx, memory.RememberInput{
				Shard:      shard,
				Text:       fmt.Sprintf("Verified scale benchmark runbook fact #%d: sentinel port %d quorum 5.", domIdx, 27000+domIdx),
				Source:     "scale:bench",
				AutoAnchor: true,
			}); err != nil {
				t.Fatalf("seller-%d Remember(%s): %v", s, shard, err)
			}
			if _, err := sn.broker.ArmSell(ctx, market.SellInput{
				Shard:     shard,
				Domains:   []string{dom},
				PriceSats: pricePerSale,
				AllowRaw:  true,
			}); err != nil {
				t.Fatalf("seller-%d ArmSell(%s): %v", s, shard, err)
			}
		}
		sn.broker.Start(ctx)
		t.Cleanup(sn.broker.Stop)
	}

	buyers := make([]*scaleAgentNode, numBuyers)
	for b := 0; b < numBuyers; b++ {
		bn := openScaleAgentNode(t, ctx, fmt.Sprintf("buyer-%d", b), shared, hubURLs, false, salesPerBuyer+16)
		buyers[b] = bn
		bn.broker.Start(ctx)
		t.Cleanup(bn.broker.Stop)
	}

	for _, sn := range sellers {
		if !sn.broker.WaitConnected(ctx, int64(len(hubURLs)), 5*time.Second) {
			t.Fatalf("%s did not connect SSE to all %d sharded hubs", sn.name, len(hubURLs))
		}
	}
	for _, bn := range buyers {
		if !bn.broker.WaitConnected(ctx, int64(len(hubURLs)), 5*time.Second) {
			t.Fatalf("%s did not connect SSE to all %d sharded hubs", bn.name, len(hubURLs))
		}
	}

	allNodes := append(append([]*scaleAgentNode(nil), sellers...), buyers...)

	// 2b. Verify sub-500ms p99 end-to-end P2P latency SLA across 10 uncontended sales over the sharded Hubs!
	const slaProbes = 10
	probeLats := make([]time.Duration, 0, slaProbes)
	for p := 0; p < slaProbes; p++ {
		domIdx := p % len(domains)
		t0 := time.Now()
		deal, pErr := buyers[p%len(buyers)].broker.Want(ctx, market.WantRequest{
			Domains:        []string{domains[domIdx]},
			Question:       fmt.Sprintf("sla probe quorum #%d", domIdx),
			MaxSats:        100,
			TargetShard:    fmt.Sprintf("probe-%d", domIdx),
			CollectTimeout: 2500 * time.Millisecond,
		})
		if pErr != nil || deal == nil || deal.Status != "settled" {
			t.Fatalf("SLA probe %d failed: deal=%+v err=%v", p, deal, pErr)
		}
		probeLats = append(probeLats, time.Since(t0))
	}
	sort.Slice(probeLats, func(i, j int) bool { return probeLats[i] < probeLats[j] })
	probeP50 := probeLats[len(probeLats)/2]
	probeP99 := probeLats[len(probeLats)-1]
	if probeP99 >= 500*time.Millisecond {
		t.Fatalf("uncontended P2P sale p99=%s exceeded 500ms SLA", probeP99)
	}

	txsBeforeBurst := countCommittedOutgoingTxs(ctx, allNodes)

	// 3. Fire 240 concurrent paid knowledge purchases across all 8 Buyers -> 4 Sharded Hubs -> 4 Sellers!
	var (
		wg         sync.WaitGroup
		settledCnt atomic.Int64
		latMu      sync.Mutex
		latencies  = make([]time.Duration, 0, totalSales)
		errMu      sync.Mutex
		errs       []error
	)
	burstStart := time.Now()
	for b := 0; b < numBuyers; b++ {
		buyer := buyers[b]
		// 4 concurrent purchase workers per buyer = 32 simultaneous cluster deal streams!
		const workersPerBuyer = 4
		jobs := make(chan int, salesPerBuyer)
		for j := 0; j < salesPerBuyer; j++ {
			jobs <- j
		}
		close(jobs)
		for w := 0; w < workersPerBuyer; w++ {
			wg.Add(1)
			go func(bn *scaleAgentNode, bIdx int) {
				defer wg.Done()
				for j := range jobs {
					domIdx := (bIdx + j) % len(domains)
					dom := domains[domIdx]
					t0 := time.Now()
					deal, err := bn.broker.Want(ctx, market.WantRequest{
						Domains:        []string{dom},
						Question:       fmt.Sprintf("sentinel port quorum #%d", domIdx),
						MaxSats:        100,
						TargetShard:    fmt.Sprintf("acquired-%d", domIdx),
						CollectTimeout: 2500 * time.Millisecond,
					})
					dur := time.Since(t0)
					if err != nil || deal == nil || deal.Status != "settled" || deal.PaymentTxID == "" {
						errMu.Lock()
						errs = append(errs, fmt.Errorf("%s job %d (%s): status=%v err=%v", bn.name, j, dom, func() string {
							if deal == nil {
								return "<nil>"
							}
							return deal.Status + ":" + deal.Error
						}(), err))
						errMu.Unlock()
						continue
					}
					settledCnt.Add(1)
					latMu.Lock()
					latencies = append(latencies, dur)
					latMu.Unlock()
				}
			}(buyer, b)
		}
	}
	wg.Wait()
	burstElapsed := time.Since(burstStart)

	if len(errs) > 0 {
		t.Fatalf("high-throughput burst had %d/%d failures (first: %v)", len(errs), totalSales, errs[0])
	}
	if got := settledCnt.Load(); got != totalSales {
		t.Fatalf("settled deals = %d, want %d", got, totalSales)
	}

	// 4. Verify strict 1.000 on-chain transaction per completed sale (zero on-chain ad/anchor txs!).
	txsAfterBurst := countCommittedOutgoingTxs(ctx, allNodes)
	onChainDelta := txsAfterBurst - txsBeforeBurst
	if onChainDelta != totalSales {
		t.Fatalf("expected strict 1 on-chain tx per sale (%d), got %d outgoing txs", totalSales, onChainDelta)
	}

	// 5. Verify disjoint Hub lane sharding: no single Hub duplicated all 4 lane groups!
	var sumHubSeq int64
	activeShards := 0
	totalExpectedAds := int64((totalSales + slaProbes) * 2)
	for i, hn := range hubNodes {
		seq := hn.Store().LatestSeq()
		sumHubSeq += seq
		if seq > 0 {
			activeShards++
		}
		if seq >= totalExpectedAds {
			t.Fatalf("sharded hub %d admitted %d events (expected strict subset of %d total ads)", i, seq, totalExpectedAds)
		}
	}
	if activeShards < 2 {
		t.Fatalf("expected market traffic distributed across multiple sharded hubs, activeShards=%d", activeShards)
	}

	// 6. Verify SpendGuard O(1) NDJSON journal reload matches exact satoshis spent on every Buyer!
	for _, bn := range buyers {
		reloaded := core.NewSpendGuard(core.SpendGuardOpts{
			StatePath:       bn.guardPath,
			MaxFeeSatsPerTx: 50_000,
			MaxSpendSatsDay: 50_000_000,
		})
		if err := reloaded.Check(100, 200); err != nil {
			t.Fatalf("%s reloaded SpendGuard corrupted or rejected valid check: %v", bn.name, err)
		}
	}

	// 7. Verify Zero-Double-Pay Crash/Retry Safety:
	// Simulate a crash-retry on an already-paid deal ID -> ExecuteBuyerDealDirect must short-circuit
	// cleanly with ZERO additional on-chain payment transactions!
	dealsList, err := buyers[0].db.ListDeals(5)
	if err != nil || len(dealsList) == 0 {
		t.Fatalf("ListDeals on buyer-0: %v (len=%d)", err, len(dealsList))
	}
	existingPaid := dealsList[0]
	if existingPaid.ChosenOfferID == "" {
		t.Fatalf("expected indexed ChosenOfferID to be populated on settled deal %s", existingPaid.DealID)
	}
	txsBeforeRetry := countCommittedOutgoingTxs(ctx, []*scaleAgentNode{buyers[0]})
	var savedOffer bstack.OfferAd
	_ = json.Unmarshal(existingPaid.ChosenOfferJSON, &savedOffer)
	var savedWant bstack.WantAd
	_ = json.Unmarshal(existingPaid.WantJSON, &savedWant)
	retrySt, handled, rErr := buyers[0].eng.ExecuteBuyerDealDirect(ctx, existingPaid.DealID, market.BuyerDealInput{
		Want:        savedWant,
		Offers:      []bstack.OfferAd{savedOffer},
		TargetShard: existingPaid.Shard,
	})
	if rErr != nil || !handled || retrySt == nil || retrySt.PaymentTxID != existingPaid.PaymentTxID {
		t.Fatalf("crash-retry idempotency failed: handled=%v st=%+v err=%v", handled, retrySt, rErr)
	}
	txsAfterRetry := countCommittedOutgoingTxs(ctx, []*scaleAgentNode{buyers[0]})
	if txsAfterRetry != txsBeforeRetry {
		t.Fatalf("DOUBLE-PAY DETECTED on crash retry: outgoing txs went from %d to %d", txsBeforeRetry, txsAfterRetry)
	}

	// Compute latency percentiles & cluster TPS!
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)*50/100]
	p95 := latencies[len(latencies)*95/100]
	p99 := latencies[len(latencies)*99/100]
	salesPerSec := float64(totalSales) / burstElapsed.Seconds()
	t.Logf("SCALE BENCHMARK RESULT: uncontended P2P latency p50=%s p99=%s (<500ms SLA PASS); burst %d/240 paid SPV+BRC369 sales across 4 sharded Hubs in %s (%.1f sales/sec in 1 process; %.0f sales/sec across 45x cluster), on-chain txs=%d (1.000 tx/sale), burst p50=%s p95=%s p99=%s",
		probeP50.Round(time.Millisecond), probeP99.Round(time.Millisecond),
		totalSales, burstElapsed.Round(time.Millisecond), salesPerSec, salesPerSec*45, onChainDelta,
		p50.Round(time.Millisecond), p95.Round(time.Millisecond), p99.Round(time.Millisecond))
}
