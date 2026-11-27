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

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

type paidHubHarness struct {
	node     *hub.Node
	ts       *httptest.Server
	port     *chain.Port
	disc     *discovery.Service
	startBal uint64
}

func newSharedTestNode(t *testing.T, ctx context.Context, shared *chain.SharedMemoryChain, envHosts []string, isHub bool, withPublicHTTP bool) (*memory.Service, *market.Broker, *httptest.Server) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           t.TempDir(),
		SharedMemoryChain: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	disc := discovery.New(mem, envHosts, nil)
	eng := market.NewEngine(mem, market.DefaultPolicy())
	broker := market.NewBroker(mem, eng, disc, market.BrokerConfig{
		IsHub:          isHub,
		CollectTimeout: 3 * time.Second,
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

func newPaidHubHarness(t *testing.T, ctx context.Context, shared *chain.SharedMemoryChain, fs hub.FeeSchedule, upstreamSpines []string) *paidHubHarness {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           t.TempDir(),
		SharedMemoryChain: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	disc := discovery.New(mem, nil, nil)
	pol := market.DefaultPolicy()
	disc.SetHubFeePolicy(pol.CanPayHubFee, pol.RecordHubFeeSpend)

	node, err := hub.NewNode(hub.Config{
		Network:        "memory",
		Fees:           fs,
		FeeCollector:   hub.NewPortFeeCollector(port),
		UpstreamSpines: upstreamSpines,
	})
	if err != nil {
		t.Fatalf("hub.NewNode: %v", err)
	}
	node.SetUpstreamSubmitter(disc.SubmitBEEF)
	node.SetEgressAuth(
		func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
			return discovery.SignEgressAuthHeaders(disc.AnnounceKey(), action, aud, cursor, domainsCSV)
		},
		func(tCtx context.Context, spineURL string, minTopupSats uint64) error {
			_, tErr := disc.TopUpHubEgressCredits(tCtx, spineURL, minTopupSats, disc.AnnouncePubHex())
			return tErr
		},
	)
	ts := httptest.NewServer(node.Handler())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		_ = node.Close()
	})
	return &paidHubHarness{
		node:     node,
		ts:       ts,
		port:     port,
		disc:     disc,
		startBal: port.BalanceSats(),
	}
}

func TestSpineAndLeafHubFeesMultiHubEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shared := chain.NewSharedMemoryChain()

	// 1. Spine Hub S: hybrid ingress fees + metered egress (1 sat / event, 5 sat min topup).
	spine := newPaidHubHarness(t, ctx, shared, hub.FeeSchedule{
		Role:                   hub.RoleSpine,
		SubmitWantSats:         5,
		SubmitOfferSats:        2,
		EgressSatsPer100Events: 100, // 1000 msats = 1 sat/event
		EgressMinTopupSats:     5,
	}, nil)

	// 2. Leaf Hub A (Buyer's local edge): charges 3 sats WantAd ingress, forwards UP to Spine S, pays Spine S egress DOWN.
	leafA := newPaidHubHarness(t, ctx, shared, hub.FeeSchedule{
		Role:           hub.RoleLeaf,
		SubmitWantSats: 3,
	}, []string{spine.ts.URL})

	// 3. Leaf Hub B (Seller's local edge): charges 2 sats OfferAd ingress + 4 sats unbypassable NAT /bbox/send toll,
	//    forwards UP to Spine S, pays Spine S egress DOWN.
	leafB := newPaidHubHarness(t, ctx, shared, hub.FeeSchedule{
		Role:            hub.RoleLeaf,
		SubmitOfferSats: 2,
		BboxSendSats:    4,
	}, []string{spine.ts.URL})

	// 4. Fee-Gouging Hub D: demands 10,000 sats per WantAd (far exceeding operator MaxHubSubmitFeeSats = 25).
	gougingHub := newPaidHubHarness(t, ctx, shared, hub.FeeSchedule{
		Role:           hub.RoleLeaf,
		SubmitWantSats: 10000,
	}, nil)

	// Cancel SSE & federation loops FIRST on cleanup (LIFO) before httptest servers close!
	t.Cleanup(func() {
		cancel()
		leafA.ts.CloseClientConnections()
		leafB.ts.CloseClientConnections()
		spine.ts.CloseClientConnections()
		gougingHub.ts.CloseClientConnections()
	})

	// Start Leaf <-> Spine 1BSV BRC-149 Object Plane Relay DOWN (metered egress with auto-topup!).
	leafA.node.StartPlaneRelay(ctx, []string{spine.ts.URL})
	leafB.node.StartPlaneRelay(ctx, []string{spine.ts.URL})

	// 5. Seller Agent 1 (connected ONLY to Leaf Hub B, with direct PublicURL so Buyer on Leaf A settles across Spine!).
	sellerMem, sellerBroker, _ := newSharedTestNode(t, ctx, shared, []string{leafB.ts.URL}, false, true)
	if _, err := market.ApprovePolicyPreset(sellerMem.DB(), sellerBroker.Engine().Policy(), "both", nil, "test"); err != nil {
		t.Fatalf("seller approve policy: %v", err)
	}
	if _, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "spine-ops",
		Text:       "Spine-and-Leaf BSV overlay quorum uses 4096 deterministic multicast shard slices.",
		Source:     "architect:bob",
		AutoAnchor: true,
	}); err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	if _, err := sellerBroker.ArmSell(ctx, market.SellInput{
		Shard:     "spine-ops",
		Domains:   []string{"ops"},
		PriceSats: 60,
	}); err != nil {
		t.Fatalf("seller ArmSell: %v", err)
	}
	sellerBroker.Start(ctx)

	// 6. Buyer Agent 1 (connected ONLY to Leaf Hub A + Fee-Gouging Hub D; knows NOTHING about Leaf Hub B or Spine S!).
	buyerMem, buyerBroker, _ := newSharedTestNode(t, ctx, shared, []string{leafA.ts.URL, gougingHub.ts.URL}, false, false)
	if _, err := market.ApprovePolicyPreset(buyerMem.DB(), buyerBroker.Engine().Policy(), "buyer", nil, "test"); err != nil {
		t.Fatalf("buyer approve policy: %v", err)
	}
	buyerBroker.Start(ctx)
	buyerBroker.WaitConnected(ctx, 1, 3*time.Second)
	sellerBroker.WaitConnected(ctx, 1, 3*time.Second)
	time.Sleep(150 * time.Millisecond) // let Leaf <-> Spine SSE attach

	// Execute cross-hub trade:
	// Buyer -> Leaf A (3 sats) -> Spine S (5 sats) -> Leaf B (paid Spine egress) -> Seller
	// Seller -> Leaf B (2 sats) -> Spine S (2 sats) -> Leaf A (paid Spine egress) -> Buyer
	// Gouging Hub D (10,000 sats) -> skipped fail-closed with 0 sats spent!
	deal, err := buyerBroker.Want(ctx, market.WantRequest{
		Domains:        []string{"ops"},
		Question:       "How many shard slices does Spine-and-Leaf BSV overlay use?",
		MaxSats:        100,
		TargetShard:    "bought/ops",
		CollectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("cross-hub Want failed: %v", err)
	}
	if deal.Status != "settled" || !strings.Contains(deal.DeliveredText, "4096") {
		t.Fatalf("expected cross-hub Spine-and-Leaf deal to settle with 4096, got status=%s text=%q err=%s", deal.Status, deal.DeliveredText, deal.Error)
	}
	recalled, rErr := buyerMem.Recall(ctx, memory.RecallInput{Shard: "bought/ops"})
	if rErr != nil {
		t.Fatalf("buyerMem.Recall(bought/ops) failed: %v", rErr)
	}
	if len(recalled) == 0 || !strings.Contains(recalled[0].Text, "4096") {
		t.Fatalf("expected Buyer memory to contain Seller's verified fact across Spine-and-Leaf, got %+v", recalled)
	}

	// 7. NAT /bbox/send trade on Leaf Hub B (Valve 3 unbypassable toll: 4 sats per message = 8 sats across purchase + reveal_key!).
	natSellerMem, natSellerBroker, _ := newSharedTestNode(t, ctx, shared, []string{leafB.ts.URL}, false, false) // behind NAT!
	if _, err := market.ApprovePolicyPreset(natSellerMem.DB(), natSellerBroker.Engine().Policy(), "both", nil, "test"); err != nil {
		t.Fatalf("natSeller approve policy: %v", err)
	}
	if _, err := natSellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "nat-sec",
		Text:       "Hardware HSM attestation slot 7 rotates every 24 hours via BRC-369.",
		Source:     "nat-seller",
		AutoAnchor: true,
	}); err != nil {
		t.Fatalf("natSeller Remember: %v", err)
	}
	if _, err := natSellerBroker.ArmSell(ctx, market.SellInput{
		Shard:     "nat-sec",
		Domains:   []string{"security"},
		PriceSats: 55,
	}); err != nil {
		t.Fatalf("natSeller ArmSell: %v", err)
	}
	natSellerBroker.Start(ctx)

	natBuyerMem, natBuyerBroker, _ := newSharedTestNode(t, ctx, shared, []string{leafB.ts.URL}, false, false)
	if _, err := market.ApprovePolicyPreset(natBuyerMem.DB(), natBuyerBroker.Engine().Policy(), "buyer", nil, "test"); err != nil {
		t.Fatalf("natBuyer approve policy: %v", err)
	}
	natBuyerBroker.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	natDeal, err := natBuyerBroker.Want(ctx, market.WantRequest{
		Domains:        []string{"security"},
		Question:       "How often does HSM attestation slot 7 rotate?",
		MaxSats:        100,
		TargetShard:    "bought/security",
		CollectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NAT bbox Want failed: %v", err)
	}
	if natDeal.Status != "settled" || !strings.Contains(natDeal.DeliveredText, "slot 7") {
		t.Fatalf("expected NAT bbox deal to settle through paid /bbox/send toll, got status=%s text=%q err=%s", natDeal.Status, natDeal.DeliveredText, natDeal.Error)
	}
	natRecalled, _ := natBuyerMem.Recall(ctx, memory.RecallInput{Shard: "bought/security"})
	if len(natRecalled) == 0 || !strings.Contains(natRecalled[0].Text, "slot 7") {
		t.Fatalf("expected NAT Buyer memory to contain NAT Seller fact, got %+v", natRecalled)
	}

	// 8. Free-riding Leaf Hub C (0 credits) attempts to pull Spine S market feed -> MUST be blocked with HTTP 402!
	freeriderKey, _ := ec.NewPrivateKey()
	pub, tsStr, nonce, sig, err := discovery.SignEgressAuthHeaders(freeriderKey, "hub_sync", spine.ts.URL, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	frReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, spine.ts.URL+"/sync?cursor=0", nil)
	frReq.Header.Set("X-Nektar-Pub", pub)
	frReq.Header.Set("X-Nektar-Ts", tsStr)
	frReq.Header.Set("X-Nektar-Nonce", nonce)
	frReq.Header.Set("X-Nektar-Sig", sig)
	frResp, err := http.DefaultClient.Do(frReq)
	if err != nil {
		t.Fatal(err)
	}
	frResp.Body.Close()
	if frResp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected Free-Rider Leaf Hub C to receive HTTP 402 on Spine S, got %d", frResp.StatusCode)
	}

	// 9. Verify real earnings & wallet balances across every Hub!
	spineEarn, _ := spine.node.Store().FeeEarningsSummary(0)
	if spineEarn.ByActionSats[hub.ActionSubmitWant] < 5 ||
		spineEarn.ByActionSats[hub.ActionSubmitOffer] < 2 ||
		spineEarn.ByActionSats[hub.ActionEgressTopup] < 10 ||
		spineEarn.TotalEgressDelivered < 2 {
		t.Fatalf("Spine Hub S earnings incomplete: %+v", spineEarn)
	}
	if spine.port.BalanceSats() <= spine.startBal {
		t.Fatalf("expected Spine Hub S wallet balance to increase (start=%d now=%d)", spine.startBal, spine.port.BalanceSats())
	}

	leafAEarn, _ := leafA.node.Store().FeeEarningsSummary(0)
	if leafAEarn.ByActionSats[hub.ActionSubmitWant] < 3 {
		t.Fatalf("expected Leaf Hub A to earn >= 3 sats on submit:want, got %+v", leafAEarn)
	}

	leafBEarn, _ := leafB.node.Store().FeeEarningsSummary(0)
	if leafBEarn.ByActionSats[hub.ActionSubmitOffer] < 2 || leafBEarn.ByActionSats[hub.ActionBboxSend] < 8 {
		t.Fatalf("expected Leaf Hub B to earn >= 2 sats on submit:offer and >= 8 sats on bbox:send, got %+v", leafBEarn)
	}

	gougingEarn, _ := gougingHub.node.Store().FeeEarningsSummary(0)
	if gougingEarn.TotalEarnedSats != 0 || gougingHub.port.BalanceSats() != gougingHub.startBal {
		t.Fatalf("Fee-Gouging Hub D MUST have 0 sats earned (fail-closed policy), got earn=%+v bal=%d vs start=%d",
			gougingEarn, gougingHub.port.BalanceSats(), gougingHub.startBal)
	}
}

