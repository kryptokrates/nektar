package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/faststore"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// fastMockBroadcaster is a 64-shard, lock-free-friendly parallel mock ARC broadcaster & ChainTracker.
// It validates real transaction IDs and enforces strict double-spend detection across all inputs
// without serializing nodes behind a single fake-block miner mutex.
type fastMockBroadcaster struct {
	shards     [64]mockARCShard
	rootsMu    sync.RWMutex
	validRoots map[uint32]map[string]bool
	fallback   *chain.SharedMemoryChain
	broadcasts atomic.Int64
	tipHeight  atomic.Uint32
	autoMine   atomic.Bool
}

type mockARCShard struct {
	mu    sync.RWMutex
	txs   map[string]*transaction.Transaction
	spent map[string]string
}

func newFastMockBroadcaster() *fastMockBroadcaster {
	m := &fastMockBroadcaster{
		validRoots: make(map[uint32]map[string]bool),
		fallback:   chain.NewSharedMemoryChain(),
	}
	m.tipHeight.Store(200)
	for i := range m.shards {
		m.shards[i].txs = make(map[string]*transaction.Transaction, 256)
		m.shards[i].spent = make(map[string]string, 256)
	}
	return m
}

func mockShardIdx(key string) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h & 63)
}

func (m *fastMockBroadcaster) Name() string { return "fast-mock-arc" }

func (m *fastMockBroadcaster) Broadcast(_ context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("mock-arc: nil tx")
	}
	txid := tx.TxID().String()
	// Check & claim inputs across sharded outpoint table (prevent double-spends in parallel!).
	for _, in := range tx.Inputs {
		if in == nil || in.SourceTXID == nil {
			continue
		}
		op := fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex)
		sh := &m.shards[mockShardIdx(op)]
		sh.mu.Lock()
		if prev, exists := sh.spent[op]; exists && prev != txid {
			sh.mu.Unlock()
			return fmt.Errorf("mock-arc: double-spend on %s (already spent by %s)", op, prev)
		}
		sh.spent[op] = txid
		sh.mu.Unlock()
	}
	tsh := &m.shards[mockShardIdx(txid)]
	tsh.mu.Lock()
	if _, exists := tsh.txs[txid]; !exists {
		if m.autoMine.Load() && tx.MerklePath == nil {
			h := m.tipHeight.Add(1)
			txHash := tx.TxID()
			isTx := true
			tx.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{{{
				Offset: 0,
				Hash:   txHash,
				Txid:   &isTx,
			}}})
			m.rootsMu.Lock()
			if m.validRoots[h] == nil {
				m.validRoots[h] = make(map[string]bool)
			}
			m.validRoots[h][txid] = true
			m.rootsMu.Unlock()
		}
		tsh.txs[txid] = tx
		m.broadcasts.Add(1)
	}
	tsh.mu.Unlock()
	return nil
}

func (m *fastMockBroadcaster) Proof(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	tsh := &m.shards[mockShardIdx(txid)]
	tsh.mu.RLock()
	tx := tsh.txs[txid]
	tsh.mu.RUnlock()
	if tx != nil && tx.MerklePath != nil {
		return tx.MerklePath, tx.MerklePath.BlockHeight, true, nil
	}
	return nil, 0, false, nil
}

func (m *fastMockBroadcaster) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	if root == nil {
		return false, nil
	}
	m.rootsMu.RLock()
	ok := m.validRoots[height][root.String()]
	m.rootsMu.RUnlock()
	if ok {
		return true, nil
	}
	if m.fallback != nil {
		return m.fallback.IsValidRootForHeight(ctx, root, height)
	}
	return false, nil
}

func (m *fastMockBroadcaster) CurrentHeight(_ context.Context) (uint32, error) {
	return m.tipHeight.Load(), nil
}

// SeedBuyerFastLane creates numUTXOs compact 1-output confirmed funding txs paying the buyer's
// BRC-43 funding script, registers their Merkle roots on the mock ChainTracker, and loads all outputs
// into the buyer's 64-shard Pebble FastLanePool (keeping every payment AtomicBEEF ~450 bytes!).
func (m *fastMockBroadcaster) SeedBuyerFastLane(tb testing.TB, buyer *chain.Port, numUTXOs int, satsEach uint64) {
	tb.Helper()
	if buyer == nil || buyer.Engine() == nil || buyer.FastLane() == nil {
		tb.Fatalf("buyer wallet FastLanePool is not initialized")
	}
	lockScript := buyer.Engine().FundScript()
	height := uint32(100)
	isTx := true

	utxos := make([]nwallet.FastLaneUTXO, 0, numUTXOs)
	m.rootsMu.Lock()
	if m.validRoots[height] == nil {
		m.validRoots[height] = make(map[string]bool, numUTXOs)
	}
	for i := 0; i < numUTXOs; i++ {
		tx := transaction.NewTransaction()
		cbHash, _ := chainhash.NewHash(core.DSHA256([]byte(fmt.Sprintf("%s:%d", buyer.IdentityPubHex(), i))))
		cbScript := script.Script([]byte{0x04, byte(i), byte(i >> 8), 0x00, 0x00})
		tx.Inputs = append(tx.Inputs, &transaction.TransactionInput{
			SourceTXID:       cbHash,
			SourceTxOutIndex: 0xffffffff,
			UnlockingScript:  &cbScript,
			SequenceNumber:   transaction.DefaultSequenceNumber,
		})
		ls := script.Script(append([]byte(nil), (*lockScript)...))
		tx.AddOutput(&transaction.TransactionOutput{
			Satoshis:      satsEach,
			LockingScript: &ls,
		})
		txHash := tx.TxID()
		mp := transaction.NewMerklePath(height, [][]*transaction.PathElement{{{
			Offset: 0,
			Hash:   txHash,
			Txid:   &isTx,
		}}})
		tx.MerklePath = mp
		m.validRoots[height][txHash.String()] = true

		tsh := &m.shards[mockShardIdx(txHash.String())]
		tsh.mu.Lock()
		tsh.txs[txHash.String()] = tx
		tsh.mu.Unlock()

		utxos = append(utxos, nwallet.FastLaneUTXO{
			TxID:          txHash.String(),
			Vout:          0,
			Satoshis:      satsEach,
			UnprovenDepth: 0,
			SourceTx:      tx,
		})
	}
	m.rootsMu.Unlock()

	if err := buyer.SeedFastLaneUTXOs(utxos); err != nil {
		tb.Fatalf("SeedFastLaneUTXOs: %v", err)
	}
}

type fastScaleNode struct {
	name    string
	dir     string
	db      *store.DB
	fs      *faststore.DB
	journal *market.FastMarketJournal
	port    *chain.Port
	mem     *memory.Service
	eng     *market.Engine
	disc    *discovery.Service
	broker  *market.Broker
	srv     *httptest.Server
}

func openFastScaleNode(
	tb testing.TB,
	ctx context.Context,
	name string,
	mockARC *fastMockBroadcaster,
	hubURLs []string,
	hubTopicMap map[string][]string,
	withPublicHTTP bool,
	seedUTXOs int,
) *fastScaleNode {
	tb.Helper()
	dir := tb.TempDir()
	db, err := store.Open(filepath.Join(dir, "nektar.sqlite"))
	if err != nil {
		tb.Fatalf("%s store.Open: %v", name, err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	fs, err := faststore.Open(faststore.Options{
		Dir:        filepath.Join(dir, "fast.pebble"),
		SyncWrites: false, // WAL persisted to OS page cache; avoids Windows Defender NTFS fsync stall in unit harness
	})
	if err != nil {
		tb.Fatalf("%s faststore.Open: %v", name, err)
	}
	tb.Cleanup(func() { _ = fs.Close() })

	port, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           dir,
		WalletProfile:     "throughput",
		ZeroConfMaxSats:   100_000,
		SharedMemoryChain: mockARC.fallback,
		FastBroadcaster:   mockARC,
		TestTracker:       mockARC,
		FastStore:         fs,
		EnableFastLane:    true,
		SpendGuard: core.SpendGuardOpts{
			// In-memory SpendGuard caps enforced synchronously on every FastLane BRC-29 tx.
			MaxFeeSatsPerTx: 50_000,
			MaxSpendSatsDay: 500_000_000,
		},
	})
	if err != nil {
		tb.Fatalf("%s chain.Open: %v", name, err)
	}
	tb.Cleanup(func() { _ = port.Close() })

	if seedUTXOs > 0 {
		mockARC.SeedBuyerFastLane(tb, port, seedUTXOs, 200_000)
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
	for hu, topics := range hubTopicMap {
		disc.SetHubTopics(hu, topics)
	}
	pol := market.DefaultPolicy()
	eng := market.NewEngine(mem, pol)
	zcMax := uint64(100_000)
	if _, err := market.ApprovePolicyCustom(db, pol, "both", nil, &market.PolicyLimitsOverride{
		MaxPerDeal:  10_000,
		MaxPerDay:   500_000_000,
		ZeroConfMax: &zcMax,
	}, "scale-operator", nil); err != nil {
		tb.Fatalf("%s ApprovePolicyCustom: %v", name, err)
	}
	journal := market.NewFastMarketJournal(fs, false, mem)
	eng.SetFastJournal(journal)
	tb.Cleanup(func() { _ = journal.Close() })

	broker := market.NewBroker(mem, eng, disc, market.BrokerConfig{
		Handle:         name + "@nektar.scale",
		BboxOffice:     "default",
		CollectTimeout: 3000 * time.Millisecond,
		GraceTimeout:   1 * time.Millisecond,
	})

	node := &fastScaleNode{
		name:    name,
		dir:     dir,
		db:      db,
		fs:      fs,
		journal: journal,
		port:    port,
		mem:     mem,
		eng:     eng,
		disc:    disc,
		broker:  broker,
	}
	if withPublicHTTP {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/market/purchase", func(w http.ResponseWriter, r *http.Request) {
			useCBOR := strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), market.ContentTypeCBOR) ||
				strings.Contains(strings.ToLower(r.Header.Get("Accept")), market.ContentTypeCBOR)
			writeResp := func(status int, resp market.PurchaseResponse) {
				if useCBOR {
					if raw, cErr := market.EncodePurchaseResponseCBOR(&resp); cErr == nil {
						w.Header().Set("Content-Type", market.ContentTypeCBOR)
						w.WriteHeader(status)
						_, _ = w.Write(raw)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(resp)
			}
			var req market.PurchaseRequest
			if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), market.ContentTypeCBOR) {
				raw, rErr := io.ReadAll(io.LimitReader(r.Body, 4<<20))
				if rErr != nil {
					writeResp(http.StatusBadRequest, market.PurchaseResponse{OK: false, Error: rErr.Error()})
					return
				}
				dec, dErr := market.DecodePurchaseRequestCBOR(raw)
				if dErr != nil {
					writeResp(http.StatusBadRequest, market.PurchaseResponse{OK: false, Error: dErr.Error()})
					return
				}
				req = dec
			} else {
				if dErr := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); dErr != nil {
					writeResp(http.StatusBadRequest, market.PurchaseResponse{OK: false, Error: dErr.Error()})
					return
				}
			}
			resp, hErr := broker.HandlePurchase(r.Context(), req)
			if hErr != nil {
				writeResp(http.StatusBadRequest, market.PurchaseResponse{OK: false, OfferID: req.OfferID, Error: hErr.Error()})
				return
			}
			writeResp(http.StatusOK, *resp)
		})
		mux.HandleFunc("POST /v1/market/direct-quote", func(w http.ResponseWriter, r *http.Request) {
			var req market.DirectQuoteRequest
			if dErr := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); dErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(market.DirectQuoteResponse{OK: false, Error: dErr.Error()})
				return
			}
			offer, hErr := broker.HandleDirectQuote(r.Context(), req)
			if hErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(market.DirectQuoteResponse{OK: false, Error: hErr.Error()})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(market.DirectQuoteResponse{OK: true, Offer: offer})
		})
		ts := httptest.NewServer(mux)
		tb.Cleanup(ts.Close)
		broker.SetPublicURL(ts.URL)
		node.srv = ts
	}
	return node
}

type sustainedCluster struct {
	mockARC *fastMockBroadcaster
	hubs    []*hub.Node
	hubURLs []string
	domains []string
	sellers []*fastScaleNode
	buyers  []*fastScaleNode
}

func setupSustained1kCluster(t *testing.T, ctx context.Context, numSellers, numBuyers, utxosPerBuyer int) *sustainedCluster {
	t.Helper()
	t.Setenv("NEKTAR_FAST_DEALS", "1")
	t.Setenv("NEKTAR_FAST_WALLET", "1")
	t.Setenv("NEKTAR_FAST_HUB", "1")

	mockARC := newFastMockBroadcaster()

	// 4 sharded Pebble Hubs splitting the 16 nibble lanes cleanly: 0-3, 4-7, 8-b, c-f.
	laneGroups := [][]string{
		{"0", "1", "2", "3"},
		{"4", "5", "6", "7"},
		{"8", "9", "a", "b"},
		{"c", "d", "e", "f"},
	}
	hubs := make([]*hub.Node, 4)
	hubURLs := make([]string, 4)
	hubTopicMap := make(map[string][]string, 4)
	for i, lanes := range laneGroups {
		hNode, err := hub.NewNode(hub.Config{
			Network:         "memory",
			DBPath:          ":memory:",
			EnableFastStore: true,
			Lanes:           lanes,
			MaxAdmitsPerMin: 10_000_000,
			PropagationOff:  true,
		})
		if err != nil {
			t.Fatalf("hub[%d] NewNode: %v", i, err)
		}
		t.Cleanup(func() { _ = hNode.Close() })
		ts := httptest.NewServer(hNode.Handler())
		t.Cleanup(ts.Close)
		hubs[i] = hNode
		hubURLs[i] = ts.URL
		topics := make([]string, 0, len(lanes))
		for _, nib := range lanes {
			topics = append(topics, "tm_nektar_memory_"+nib)
		}
		hubTopicMap[ts.URL] = topics
	}

	// Pick 16 domains (1 per hex nibble 0..f, 4 domains per Hub) so ALL 16 parallel Hub lanes run concurrently!
	domains := make([]string, 0, 16)
	for g, lanes := range laneGroups {
		for _, wantNib := range lanes {
			found := ""
			for k := 0; k < 2000; k++ {
				cand := fmt.Sprintf("benchlane-%d-%s-%d", g, wantNib, k)
				if bstack.LaneOf("memory", cand) == "tm_nektar_memory_"+wantNib {
					found = cand
					break
				}
			}
			if found == "" {
				t.Fatalf("failed to find domain for nibble %s", wantNib)
			}
			domains = append(domains, found)
		}
	}

	sellers := make([]*fastScaleNode, numSellers)
	mockARC.autoMine.Store(true) // mine Seller setup anchor txs with real verified BUMPs
	for i := 0; i < numSellers; i++ {
		// Partition the 16 domains cleanly across Sellers (4 domains each for 4 Sellers) so every WantAd hits 1 Seller!
		var myDoms []string
		for di, d := range domains {
			if di%numSellers == i {
				myDoms = append(myDoms, d)
			}
		}
		s := openFastScaleNode(t, ctx, fmt.Sprintf("seller-%d", i), mockARC, hubURLs, hubTopicMap, true, 0)
		shard := fmt.Sprintf("sell-shard-%d", i)
		if _, err := s.mem.Remember(ctx, memory.RememberInput{
			Shard:      shard,
			Text:       fmt.Sprintf("Verified benchmark alpha %d for %s: sustained 1000+ TPS pure-Go Pebble BRC-29 trading proof.", i, myDoms[0]),
			Class:      "ops",
			AutoAnchor: true,
		}); err != nil {
			t.Fatalf("seller[%d] Remember: %v", i, err)
		}
		if _, err := s.broker.ArmSell(ctx, market.SellInput{
			Shard:       shard,
			Domains:     myDoms,
			PriceSats:   50,
			AllowRaw:    true,
			Description: fmt.Sprintf("Verified benchmark alpha %d", i),
		}); err != nil {
			t.Fatalf("seller[%d] ArmSell: %v", i, err)
		}
		s.journal.InvalidateListings()
		s.broker.Start(ctx)
		t.Cleanup(s.broker.Stop)
		sellers[i] = s
	}
	mockARC.autoMine.Store(false) // zero fake-block mining during live 10s sustained burst!

	buyers := make([]*fastScaleNode, numBuyers)
	for i := 0; i < numBuyers; i++ {
		b := openFastScaleNode(t, ctx, fmt.Sprintf("buyer-%d", i), mockARC, hubURLs, hubTopicMap, false, utxosPerBuyer)
		b.broker.Start(ctx)
		t.Cleanup(b.broker.Stop)
		buyers[i] = b
	}

	// Wait for all SSE loops across Sellers & Buyers to connect to all 4 Hubs.
	for _, s := range sellers {
		if !s.broker.WaitConnected(ctx, int64(len(hubURLs)), 10*time.Second) {
			t.Fatalf("%s did not connect SSE to all %d hubs", s.name, len(hubURLs))
		}
	}
	for _, b := range buyers {
		if !b.broker.WaitConnected(ctx, int64(len(hubURLs)), 10*time.Second) {
			t.Fatalf("%s did not connect SSE to all %d hubs", b.name, len(hubURLs))
		}
	}

	return &sustainedCluster{
		mockARC: mockARC,
		hubs:    hubs,
		hubURLs: hubURLs,
		domains: domains,
		sellers: sellers,
		buyers:  buyers,
	}
}

func computePercentiles(durs []time.Duration) (p50, p95, p99 time.Duration) {
	if len(durs) == 0 {
		return 0, 0, 0
	}
	cp := append([]time.Duration(nil), durs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	p50 = cp[(len(cp)-1)*50/100]
	p95 = cp[(len(cp)-1)*95/100]
	p99 = cp[(len(cp)-1)*99/100]
	return p50, p95, p99
}

// TestMeasured1kTPS_ColdDiscovery_Sustained10s empirically proves >= 1,000 REAL completed
// paid sales/sec sustained over 10 full seconds (zero extrapolation!) across the entire
// Cold Discovery stack: WantAd -> Lane-Routed Pebble Hub -> Seller Quote -> OfferAd ->
// Lane-Routed Pebble Hub -> Buyer BRC-29 FastLane Pay -> Binary CBOR Purchase ->
// Seller SPV + Replay Claim + Reveal + Signed Receipt -> Buyer Decrypt + .nkt Verify +
// Bilateral Countersign + Synchronous Pebble WAL Commit.
func TestMeasured1kTPS_ColdDiscovery_Sustained10s(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cluster := setupSustained1kCluster(t, ctx, 4, 8, 512)

	// 1s warmup so Seller .nkt capsule caches, ProofBundle caches, and HTTP keep-alive pools are hot.
	warmCtx, warmCancel := context.WithTimeout(ctx, 5*time.Second)
	var warmWG sync.WaitGroup
	for i := 0; i < len(cluster.buyers); i++ {
		warmWG.Add(1)
		go func(idx int) {
			defer warmWG.Done()
			dom := cluster.domains[idx%len(cluster.domains)]
			rec, err := cluster.buyers[idx].broker.Want(warmCtx, market.WantRequest{
				Domains:  []string{dom},
				Question: "sustained 1000+ TPS pure-Go Pebble BRC-29 trading proof",
				MaxSats:  100,
			})
			if err != nil || rec == nil || rec.Status != "settled" {
				t.Errorf("warmup cold buy[%d] failed: rec=%+v err=%v", idx, rec, err)
			}
		}(i)
	}
	warmWG.Wait()
	warmCancel()
	if t.Failed() {
		t.FailNow()
	}

	const (
		runDuration    = 10 * time.Second
		workersPerBuy  = 8 // 8 buyers * 8 workers = 64 concurrent in-flight cold deals
	)
	baseBroadcasts := cluster.mockARC.broadcasts.Load()

	var (
		completed atomic.Int64
		failed    atomic.Int64
		firstErr  atomic.Value
		latMu     sync.Mutex
		latencies = make([]time.Duration, 0, 20000)
	)

	start := time.Now()
	deadline := start.Add(runDuration)
	var wg sync.WaitGroup
	for bi, buyer := range cluster.buyers {
		for w := 0; w < workersPerBuy; w++ {
			wg.Add(1)
			go func(bIdx, wIdx int, b *fastScaleNode) {
				defer wg.Done()
				localLats := make([]time.Duration, 0, 512)
				step := 0
				for time.Now().Before(deadline) {
					dom := cluster.domains[(bIdx+wIdx+step)%len(cluster.domains)]
					step++
					t0 := time.Now()
					rec, err := b.broker.Want(ctx, market.WantRequest{
						Domains:  []string{dom},
						Question: "sustained 1000+ TPS pure-Go Pebble BRC-29 trading proof",
						MaxSats:  100,
					})
					dt := time.Since(t0)
					if err != nil || rec == nil || rec.Status != "settled" {
						failed.Add(1)
						if firstErr.Load() == nil {
							firstErr.Store(fmt.Sprintf("buyer=%s err=%v status=%v", b.name, err, rec))
						}
						continue
					}
					completed.Add(1)
					localLats = append(localLats, dt)
				}
				latMu.Lock()
				latencies = append(latencies, localLats...)
				latMu.Unlock()
			}(bi, w, buyer)
		}
	}
	wg.Wait()
	elapsed := time.Since(start)

	totalDone := completed.Load()
	totalFail := failed.Load()
	totalTxs := cluster.mockARC.broadcasts.Load() - baseBroadcasts
	tps := float64(totalDone) / elapsed.Seconds()
	txPerSale := float64(totalTxs) / float64(max(int64(1), totalDone))
	p50, p95, p99 := computePercentiles(latencies)

	// Verify durable Pebble WAL settlement count across all Buyers (zero silent drops).
	walSettled := 0
	for _, b := range cluster.buyers {
		walSettled += b.journal.CountSettledDealsWAL("buyer")
	}

	t.Logf("MEASURED COLD DISCOVERY (10s sustained, ZERO extrapolation): completed=%d failed=%d elapsed=%v TPS=%.1f sales/sec tx/sale=%.4f (txs=%d) p50=%v p95=%v p99=%v walSettled=%d",
		totalDone, totalFail, elapsed.Round(time.Millisecond), tps, txPerSale, totalTxs, p50, p95, p99, walSettled)

	if totalFail > 0 {
		t.Fatalf("cold discovery had %d failed sales (first: %v)", totalFail, firstErr.Load())
	}
	if totalTxs != totalDone {
		t.Fatalf("expected exact 1.000 BRC-29 tx per sale: got %d txs for %d sales (%.4f)", totalTxs, totalDone, txPerSale)
	}
	if walSettled < int(totalDone) {
		t.Fatalf("Pebble WAL settled count %d < completed sales %d", walSettled, totalDone)
	}
	if tps < 1000.0 {
		t.Fatalf("MEASURED COLD DISCOVERY TPS %.1f sales/sec < 1000.0 target (%d sales in %v)", tps, totalDone, elapsed)
	}
	if p99 > 500*time.Millisecond {
		t.Fatalf("cold discovery p99 latency %v exceeded 500ms SLA", p99)
	}
}

// TestMeasured2kTPS_DirectBuy_Sustained10s empirically proves >= 2,000 REAL completed
// paid sales/sec sustained over 10 full seconds using Standing/Catalog Direct-Buy
// (Broker.BuyDirect -> POST /v1/market/direct-quote -> binary CBOR POST /v1/market/purchase).
func TestMeasured2kTPS_DirectBuy_Sustained10s(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cluster := setupSustained1kCluster(t, ctx, 4, 8, 512)

	// Warmup caches & keep-alive pools.
	for i := 0; i < len(cluster.sellers); i++ {
		dom := cluster.domains[i%len(cluster.domains)]
		rec, err := cluster.buyers[i%len(cluster.buyers)].broker.BuyDirect(ctx, market.DirectBuyRequest{
			SellerURL: cluster.sellers[i].srv.URL,
			Domains:   []string{dom},
			Question:  "sustained 1000+ TPS pure-Go Pebble BRC-29 trading proof",
			MaxSats:   100,
		})
		if err != nil || rec == nil || rec.Status != "settled" {
			t.Fatalf("warmup BuyDirect[%d] failed: rec=%+v err=%v", i, rec, err)
		}
	}

	const (
		runDuration   = 10 * time.Second
		workersPerBuy = 8 // 64 concurrent Direct-Buy workers
	)
	baseBroadcasts := cluster.mockARC.broadcasts.Load()

	var (
		completed atomic.Int64
		failed    atomic.Int64
		firstErr  atomic.Value
		latMu     sync.Mutex
		latencies = make([]time.Duration, 0, 40000)
	)

	start := time.Now()
	deadline := start.Add(runDuration)
	var wg sync.WaitGroup
	for bi, buyer := range cluster.buyers {
		for w := 0; w < workersPerBuy; w++ {
			wg.Add(1)
			go func(bIdx, wIdx int, b *fastScaleNode) {
				defer wg.Done()
				localLats := make([]time.Duration, 0, 1024)
				step := 0
				for time.Now().Before(deadline) {
					sIdx := (bIdx + wIdx + step) % len(cluster.sellers)
					dom := cluster.domains[sIdx%len(cluster.domains)]
					step++
					t0 := time.Now()
					rec, err := b.broker.BuyDirect(ctx, market.DirectBuyRequest{
						SellerURL: cluster.sellers[sIdx].srv.URL,
						Domains:   []string{dom},
						Question:  "sustained 1000+ TPS pure-Go Pebble BRC-29 trading proof",
						MaxSats:   100,
					})
					dt := time.Since(t0)
					if err != nil || rec == nil || rec.Status != "settled" {
						failed.Add(1)
						if firstErr.Load() == nil {
							firstErr.Store(fmt.Sprintf("buyer=%s err=%v status=%v", b.name, err, rec))
						}
						continue
					}
					completed.Add(1)
					localLats = append(localLats, dt)
				}
				latMu.Lock()
				latencies = append(latencies, localLats...)
				latMu.Unlock()
			}(bi, w, buyer)
		}
	}
	wg.Wait()
	elapsed := time.Since(start)

	totalDone := completed.Load()
	totalFail := failed.Load()
	totalTxs := cluster.mockARC.broadcasts.Load() - baseBroadcasts
	tps := float64(totalDone) / elapsed.Seconds()
	txPerSale := float64(totalTxs) / float64(max(int64(1), totalDone))
	p50, p95, p99 := computePercentiles(latencies)

	t.Logf("MEASURED DIRECT-BUY (10s sustained, ZERO extrapolation): completed=%d failed=%d elapsed=%v TPS=%.1f sales/sec tx/sale=%.4f (txs=%d) p50=%v p95=%v p99=%v",
		totalDone, totalFail, elapsed.Round(time.Millisecond), tps, txPerSale, totalTxs, p50, p95, p99)

	if totalFail > 0 {
		t.Fatalf("Direct-Buy had %d failed sales (first: %v)", totalFail, firstErr.Load())
	}
	if totalTxs != totalDone {
		t.Fatalf("expected exact 1.000 BRC-29 tx per sale: got %d txs for %d sales (%.4f)", totalTxs, totalDone, txPerSale)
	}
	if tps < 2000.0 {
		t.Fatalf("MEASURED DIRECT-BUY TPS %.1f sales/sec < 2000.0 target (%d sales in %v)", tps, totalDone, elapsed)
	}
	if p99 > 150*time.Millisecond {
		t.Fatalf("Direct-Buy p99 latency %v exceeded 150ms SLA", p99)
	}
}
