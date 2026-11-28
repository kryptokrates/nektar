package market

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/faststore"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

type testAutonomyNode struct {
	mem    *memory.Service
	eng    *Engine
	disc   *discovery.Service
	broker *Broker
	srv    *httptest.Server
}

func newAutonomyAgentNode(t *testing.T, runCtx context.Context, shared *chain.SharedMemoryChain, hubURLs []string, behindNAT bool, fastMode bool) *testAutonomyNode {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var fs *faststore.DB
	if fastMode {
		var fErr error
		fs, fErr = faststore.Open(faststore.Options{InMemory: true})
		if fErr != nil {
			t.Fatal(fErr)
		}
		t.Cleanup(func() { _ = fs.Close() })
	}

	port, err := chain.Open(runCtx, chain.Config{
		Network:           "memory",
		DataDir:           t.TempDir(),
		SharedMemoryChain: shared,
		FastStore:         fs,
		EnableFastLane:    fastMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })

	if fastMode {
		if _, pErr := port.ProvisionFastLane(runCtx, 64, 5_000); pErr != nil {
			t.Fatalf("ProvisionFastLane: %v", pErr)
		}
	}

	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	pol := DefaultPolicy()
	zc := uint64(5_000)
	_, err = ApprovePolicyCustom(db, pol, "both", nil, &PolicyLimitsOverride{
		MaxPerDeal:  10_000,
		MaxPerDay:   500_000,
		ZeroConfMax: &zc,
	}, "test-auto", nil)
	if err != nil {
		t.Fatalf("ApprovePolicyCustom: %v", err)
	}

	eng := NewEngine(mem, pol)
	if fastMode && fs != nil {
		fj := NewFastMarketJournal(fs, false, mem)
		t.Cleanup(func() { _ = fj.Close() })
		eng.SetFastJournal(fj)
	}
	_, err = eng.StartWorkflows(runCtx, t.TempDir())
	if err != nil {
		t.Fatalf("StartWorkflows: %v", err)
	}
	t.Cleanup(func() { _ = eng.CloseWorkflows() })

	hc := &http.Client{Timeout: 15 * time.Second}
	disc := discovery.New(mem, hubURLs, hc)
	disc.SetHubFeePolicy(pol.CanPayHubFee, pol.RecordHubFeeSpend)
	t.Cleanup(func() {
		if ct := disc.CarrierTree(nil); ct != nil {
			ct.Close()
		}
	})

	var srv *httptest.Server
	var brk *Broker
	if !behindNAT {
		mux := http.NewServeMux()
		srv = httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		brk = NewBroker(mem, eng, disc, BrokerConfig{
			PublicURL:      srv.URL,
			HTTPClient:     hc,
			CollectTimeout: 4 * time.Second,
			GraceTimeout:   50 * time.Millisecond,
		})
		mux.HandleFunc("POST /v1/market/purchase", func(w http.ResponseWriter, r *http.Request) {
			isCBOR := strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), ContentTypeCBOR)
			var req PurchaseRequest
			if isCBOR {
				raw, rErr := io.ReadAll(io.LimitReader(r.Body, 4<<20))
				if rErr != nil {
					http.Error(w, rErr.Error(), http.StatusBadRequest)
					return
				}
				var dErr error
				req, dErr = DecodePurchaseRequestCBOR(raw)
				if dErr != nil {
					http.Error(w, dErr.Error(), http.StatusBadRequest)
					return
				}
			} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			resp, err := brk.HandlePurchase(r.Context(), req)
			if err != nil {
				errResp := &PurchaseResponse{OK: false, Error: err.Error()}
				if isCBOR {
					if enc, eErr := EncodePurchaseResponseCBOR(errResp); eErr == nil {
						w.Header().Set("Content-Type", ContentTypeCBOR)
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write(enc)
						return
					}
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(errResp)
				return
			}
			if isCBOR && strings.Contains(strings.ToLower(r.Header.Get("Accept")), ContentTypeCBOR) {
				if enc, eErr := EncodePurchaseResponseCBOR(resp); eErr == nil {
					w.Header().Set("Content-Type", ContentTypeCBOR)
					_, _ = w.Write(enc)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
	} else {
		brk = NewBroker(mem, eng, disc, BrokerConfig{
			PublicURL:      "", // 100% behind NAT — must settle via cross-Hub bbox + Hub bstore CHIRP!
			HTTPClient:     hc,
			CollectTimeout: 6 * time.Second,
			GraceTimeout:   75 * time.Millisecond,
		})
	}
	eng.SetBroker(brk)
	brk.Start(runCtx)
	return &testAutonomyNode{
		mem:    mem,
		eng:    eng,
		disc:   disc,
		broker: brk,
		srv:    srv,
	}
}

// TestSpineLeafMulticastCrossHubNATAndLargeCapsuleTrade verifies end-to-end:
//  1. Spine (`PresetSpineWholesale`: 0 ingress, 5 sats/100 frames) <-> Leaf H1 & Leaf H2 (`PresetLightBSVEgress`: 0 ingress, 12 sats/100 frames).
//  2. Demand-driven upstream multicast: Leaf H2 subscribes to 0 Spine topics when 0 downstream agents are connected.
//  3. Unified BRC-100 identity across Chain, Discovery, and Market (`IdentityPubHex == AnnouncePubHex == Broker.PubHex`).
//  4. NAT Seller (connected ONLY to Leaf H1) <-> NAT Buyer (connected ONLY to Leaf H2) settling an offloaded
//     >20 KiB BRC-369 .nkt capsule over binary /plane/stream + cross-Hub bbox + Hub bstore CHIRP!
func TestSpineLeafMulticastCrossHubNATAndLargeCapsuleTrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	shared := chain.NewSharedMemoryChain()

	spineSched := hub.DefaultFeeSchedule(hub.PresetSpineWholesale)
	if spineSched.SubmitWantSats != 0 || spineSched.SubmitOfferSats != 0 || spineSched.SubmitAnnounceSats != 0 || spineSched.EgressSatsPer100Events != 5 {
		t.Fatalf("unexpected PresetSpineWholesale schedule: %+v", spineSched)
	}
	leafRetailSched := hub.DefaultFeeSchedule(hub.PresetLightBSVEgress)
	if leafRetailSched.SubmitWantSats != 0 || leafRetailSched.SubmitOfferSats != 0 || leafRetailSched.BboxSendSats != 0 || leafRetailSched.EgressSatsPer100Events != 12 {
		t.Fatalf("unexpected PresetLightBSVEgress schedule: %+v", leafRetailSched)
	}

	spineNode, err := hub.NewNode(hub.Config{
		Network:      "memory",
		EnableBbox:   true,
		EnableBstore: true,
		EnableFinger: true,
		ChainTracker: shared,
	})
	if err != nil {
		t.Fatalf("spine NewNode: %v", err)
	}
	spineSrv := httptest.NewServer(spineNode.Handler())

	makeUpstreamSubmitter := func() func(context.Context, string, []byte, []string) error {
		hc := &http.Client{Timeout: 10 * time.Second}
		return func(uCtx context.Context, spineURL string, beef []byte, topics []string) error {
			req, rErr := http.NewRequestWithContext(uCtx, http.MethodPost, strings.TrimRight(spineURL, "/")+"/submit", bytes.NewReader(beef))
			if rErr != nil {
				return rErr
			}
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("X-Topics", strings.Join(topics, ","))
			resp, dErr := hc.Do(req)
			if dErr != nil {
				return dErr
			}
			resp.Body.Close()
			return nil
		}
	}

	leaf1, err := hub.NewNode(hub.Config{
		Network:              "memory",
		EnableBbox:           true,
		EnableBstore:         true,
		EnableFinger:         true,
		ChainTracker:         shared,
		DemandDrivenUpstream: true,
		UpstreamSpines:       []string{spineSrv.URL},
		UpstreamSubmitter:    makeUpstreamSubmitter(),
	})
	if err != nil {
		t.Fatalf("leaf1 NewNode: %v", err)
	}
	leaf1Srv := httptest.NewServer(leaf1.Handler())

	leaf2, err := hub.NewNode(hub.Config{
		Network:              "memory",
		EnableBbox:           true,
		EnableBstore:         true,
		EnableFinger:         true,
		ChainTracker:         shared,
		DemandDrivenUpstream: true,
		UpstreamSpines:       []string{spineSrv.URL},
		UpstreamSubmitter:    makeUpstreamSubmitter(),
	})
	if err != nil {
		t.Fatalf("leaf2 NewNode: %v", err)
	}
	leaf2Srv := httptest.NewServer(leaf2.Handler())

	t.Cleanup(func() {
		cancel()
		leaf1Srv.CloseClientConnections()
		leaf2Srv.CloseClientConnections()
		spineSrv.CloseClientConnections()
		leaf1Srv.Close()
		leaf2Srv.Close()
		spineSrv.Close()
		_ = leaf1.Close()
		_ = leaf2.Close()
		_ = spineNode.Close()
	})

	leaf1.StartPlaneRelay(ctx, []string{spineSrv.URL})
	leaf2.StartPlaneRelay(ctx, []string{spineSrv.URL})

	// Before any downstream agent connects to Leaf H2, DemandDrivenUpstream MUST hold 0 active topics.
	if got := leaf2.ActiveDownstreamTopics(); len(got) != 0 {
		t.Fatalf("expected 0 active downstream topics on idle Leaf H2, got %v", got)
	}

	// Connect NAT Seller S ONLY to Leaf H1 and NAT Buyer B ONLY to Leaf H2.
	seller := newAutonomyAgentNode(t, ctx, shared, []string{leaf1Srv.URL}, true, false)
	buyer := newAutonomyAgentNode(t, ctx, shared, []string{leaf2Srv.URL}, true, false)

	// Verify unified BRC-100 identity across Chain, Discovery, and Market!
	if seller.broker.PubHex() != seller.mem.Chain().IdentityPubHex() || seller.disc.AnnouncePubHex() != seller.broker.PubHex() {
		t.Fatalf("expected unified BRC-100 identity: chain=%s disc=%s market=%s",
			seller.mem.Chain().IdentityPubHex(), seller.disc.AnnouncePubHex(), seller.broker.PubHex())
	}

	seller.broker.WaitConnected(ctx, 1, 4*time.Second)
	buyer.broker.WaitConnected(ctx, 1, 4*time.Second)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if len(leaf1.ActiveDownstreamTopics()) > 0 && len(leaf2.ActiveDownstreamTopics()) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(leaf1.ActiveDownstreamTopics()) == 0 || len(leaf2.ActiveDownstreamTopics()) == 0 {
		t.Fatalf("expected active downstream topics on Leaf H1 (%v) and Leaf H2 (%v)",
			leaf1.ActiveDownstreamTopics(), leaf2.ActiveDownstreamTopics())
	}
	// Allow Leaf -> Spine BRC-149 upstream demand-driven reconnect to attach cleanly.
	time.Sleep(150 * time.Millisecond)

	// Seller commits & lists a LARGE (>20 KiB) capsule on Leaf H1 so BOTH BRC-369 ciphertext staging
	// onto Hub bstore (`/chirp/v1/objects/{hash}`) AND cross-Hub NAT bbox (`office:nektar_nektarbbox`) run!
	var bigDoc strings.Builder
	bigDoc.WriteString("Runbook: Zero-Downtime Spine-and-Leaf BRC-149 Multicast Cutover.\n")
	for i := 0; i < 320; i++ {
		fmt.Fprintf(&bigDoc, "Step %03d: Shard-lane-%02x checkpoint digest=%08x-%08x verified.\n", i, i%16, i*2654435761, (i+7)*2246822519)
	}
	largeText := bigDoc.String()
	if len(largeText) < 20_000 {
		t.Fatalf("expected large capsule >20 KiB, got %d bytes", len(largeText))
	}

	if _, err := seller.mem.Remember(ctx, memory.RememberInput{
		Shard:      "capsules/multicast-cutover",
		Path:       "RUNBOOK.md",
		Text:       largeText,
		Public:     true,
		Distilled:  true,
		AutoAnchor: true,
	}); err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	armRes, err := seller.broker.ArmSell(ctx, SellInput{
		Shard:       "capsules/multicast-cutover",
		Domains:     []string{"multicast"},
		PriceSats:   75,
		Description: "20 KiB Spine-and-Leaf multicast cutover runbook",
		AllowRaw:    true,
	})
	if err != nil || !armRes.QuotingReady {
		t.Fatalf("seller ArmSell failed or not ready: res=%+v err=%v", armRes, err)
	}

	// Buyer B (on Leaf H2, behind NAT) buys from Seller S (on Leaf H1, behind NAT)!
	wantCtx, wantCancel := context.WithTimeout(ctx, 20*time.Second)
	defer wantCancel()
	deal, err := buyer.broker.Want(wantCtx, WantRequest{
		Domains:     []string{"multicast"},
		Question:    "Zero-Downtime Spine-and-Leaf BRC-149 Multicast Cutover",
		MaxSats:     150,
		TargetShard: "imported/multicast-cutover",
	})
	if err != nil {
		t.Fatalf("cross-Hub NAT Buyer Want failed: %v", err)
	}
	if deal == nil || deal.Status != "settled" {
		t.Fatalf("expected cross-Hub NAT large capsule deal settled, got %+v", deal)
	}
	if !strings.Contains(deal.DeliveredText, "Zero-Downtime Spine-and-Leaf BRC-149 Multicast Cutover") {
		t.Fatalf("unexpected delivered text prefix: %.120q", deal.DeliveredText)
	}

	recalled, err := buyer.mem.Recall(ctx, memory.RecallInput{Shard: "imported/multicast-cutover"})
	if err != nil || len(recalled) == 0 || !strings.Contains(recalled[0].Text, "Step 319:") {
		t.Fatalf("expected Buyer Recall to return full >20 KiB imported capsule, got len=%d err=%v", len(recalled), err)
	}
}

// TestTwoSidedAutonomousStandingOrderBountySweep verifies that when a Buyer posts a StandingOrder
// bounty BEFORE any Seller exists, a Seller coming online later and calling ArmSell immediately
// sweeps open WantAds on ls_nektar, quotes the waiting bounty, and settles the trade automatically.
func TestTwoSidedAutonomousStandingOrderBountySweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	shared := chain.NewSharedMemoryChain()

	hubNode, err := hub.NewNode(hub.Config{
		Network:      "memory",
		EnableBbox:   true,
		EnableBstore: true,
		ChainTracker: shared,
	})
	if err != nil {
		t.Fatalf("hub NewNode: %v", err)
	}
	hubSrv := httptest.NewServer(hubNode.Handler())
	t.Cleanup(func() {
		cancel()
		hubSrv.CloseClientConnections()
		hubSrv.Close()
		_ = hubNode.Close()
	})

	buyer := newAutonomyAgentNode(t, ctx, shared, []string{hubSrv.URL}, true, false)
	buyer.broker.WaitConnected(ctx, 1, 4*time.Second)

	so, err := buyer.broker.StartStandingOrder(ctx, StandingOrderInput{
		Domains:     []string{"bounty-ops"},
		Query:       "Chronicle VM opcode activation height runbook",
		MaxSats:     120,
		TargetShard: "bounty/chronicle",
		TTLSeconds:  3600,
		MaxFills:    1,
	})
	if err != nil {
		t.Fatalf("StartStandingOrder: %v", err)
	}

	// Verify Hub GET /bounties surfaces the live Buyer StandingOrder WantAd in standingBounties!
	var standingCount int
	bDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(bDeadline) {
		bResp, bErr := http.Get(hubSrv.URL + "/bounties")
		if bErr == nil {
			var bountyDoc struct {
				StandingBounties []any `json:"standingBounties"`
			}
			_ = json.NewDecoder(bResp.Body).Decode(&bountyDoc)
			bResp.Body.Close()
			if len(bountyDoc.StandingBounties) > 0 {
				standingCount = len(bountyDoc.StandingBounties)
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	if standingCount == 0 {
		t.Fatalf("expected Hub GET /bounties standingBounties to expose Buyer's live StandingOrder WantAd (orderID=%s)", so.OrderID)
	}

	// Seller comes online AFTER the WantAd was published and calls ArmSell -> sweeps ls_nektar!
	seller := newAutonomyAgentNode(t, ctx, shared, []string{hubSrv.URL}, true, false)
	seller.broker.WaitConnected(ctx, 1, 4*time.Second)

	if _, err := seller.mem.Remember(ctx, memory.RememberInput{
		Shard:      "capsules/chronicle-vm",
		Path:       "CHRONICLE.md",
		Text:       "Chronicle VM opcode activation height runbook: verify consensus script flags before deployment.",
		Public:     true,
		Distilled:  true,
		AutoAnchor: true,
	}); err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	if _, err := seller.broker.ArmSell(ctx, SellInput{
		Shard:       "capsules/chronicle-vm",
		Domains:     []string{"bounty-ops"},
		PriceSats:   60,
		Description: "Chronicle VM opcode runbook",
		AllowRaw:    true,
	}); err != nil {
		t.Fatalf("seller ArmSell: %v", err)
	}

	var finalState *StandingOrderState
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, gErr := buyer.eng.GetStandingOrderState(so.OrderID)
		if gErr == nil && st.Status == "filled" {
			finalState = st
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if finalState == nil || finalState.Status != "filled" || finalState.Fills != 1 {
		st, _ := buyer.eng.GetStandingOrderState(so.OrderID)
		t.Fatalf("expected StandingOrder %s to auto-fill via Seller ArmSell sweep, got %+v", so.OrderID, st)
	}

	recalled, err := buyer.mem.Recall(ctx, memory.RecallInput{Shard: "bounty/chronicle"})
	if err != nil || len(recalled) == 0 || !strings.Contains(recalled[0].Text, "Chronicle VM opcode activation height") {
		t.Fatalf("expected Buyer Recall to contain fulfilled bounty capsule, got %+v err=%v", recalled, err)
	}
}

// TestConcurrentMicroTradeBurstZeroDeadlock runs 40 concurrent parallel capsule purchases with
// FastMarketJournal + 64-shard FastLanePool enabled, verifying 0 deadlocks, 0 ErrDepthExceeded,
// and 100% immediate Recall availability of imported capsules!
func TestConcurrentMicroTradeBurstZeroDeadlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	shared := chain.NewSharedMemoryChain()

	hubNode, err := hub.NewNode(hub.Config{
		Network:         "memory",
		EnableBbox:      true,
		EnableBstore:    true,
		EnableFastStore: true,
		MaxAdmitsPerMin: 10_000,
		ChainTracker:    shared,
	})
	if err != nil {
		t.Fatalf("hub NewNode: %v", err)
	}
	hubSrv := httptest.NewServer(hubNode.Handler())
	t.Cleanup(func() {
		cancel()
		hubSrv.CloseClientConnections()
		hubSrv.Close()
		_ = hubNode.Close()
	})

	seller := newAutonomyAgentNode(t, ctx, shared, []string{hubSrv.URL}, false, true)
	buyer := newAutonomyAgentNode(t, ctx, shared, []string{hubSrv.URL}, false, true)

	seller.broker.WaitConnected(ctx, 1, 4*time.Second)
	buyer.broker.WaitConnected(ctx, 1, 4*time.Second)

	if _, err := seller.mem.Remember(ctx, memory.RememberInput{
		Shard:      "capsules/burst-bench",
		Path:       "BENCH.md",
		Text:       "LightBSV 64-shard parallel BRC-29 micro-trade capsule payload.",
		Public:     true,
		Distilled:  true,
		AutoAnchor: true,
	}); err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	if _, err := seller.broker.ArmSell(ctx, SellInput{
		Shard:       "capsules/burst-bench",
		Domains:     []string{"scale"},
		PriceSats:   25,
		Description: "Parallel micro-trade benchmark capsule",
		AllowRaw:    true,
	}); err != nil {
		t.Fatalf("seller ArmSell: %v", err)
	}

	const totalDeals = 40
	var settledCount atomic.Int64
	var wg sync.WaitGroup
	errCh := make(chan error, totalDeals)

	for i := 0; i < totalDeals; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			targetRepo := fmt.Sprintf("burst/deal-%02d", idx)
			deal, wErr := buyer.broker.Want(ctx, WantRequest{
				Domains:     []string{"scale"},
				Question:    "LightBSV 64-shard parallel BRC-29 micro-trade capsule",
				MaxSats:     100,
				TargetShard: targetRepo,
			})
			if wErr != nil {
				errCh <- fmt.Errorf("deal %d error: %w", idx, wErr)
				return
			}
			if deal == nil || deal.Status != "settled" {
				errCh <- fmt.Errorf("deal %d not settled: %+v", idx, deal)
				return
			}
			frags, rErr := buyer.mem.Recall(ctx, memory.RecallInput{Shard: targetRepo})
			if rErr != nil || len(frags) == 0 {
				errCh <- fmt.Errorf("deal %d Recall immediately after FastJournal settle returned %d frags (err=%v)", idx, len(frags), rErr)
				return
			}
			settledCount.Add(1)
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Errorf("concurrent burst error: %v", e)
	}
	if got := settledCount.Load(); got != totalDeals {
		t.Fatalf("expected all %d concurrent micro-trades settled & recallable, got %d", totalDeals, got)
	}
}
