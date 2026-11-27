package market

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestAutonomousSleepCycle_CedarSellGateAndSecretBlock(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pol := DefaultPolicy()
	pol.SellerApproved = true
	eng := NewEngine(mem, pol)
	disc := discovery.New(mem, nil, nil)
	br := NewBroker(mem, eng, disc, BrokerConfig{IsHub: true})
	defer br.Stop()

	// 1. ArmSell MUST fail closed when a shard contains a leaked API secret!
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard:  "secret-leak",
		Text:   "Production Anthropic key sk-ant-api03-1234567890abcdef1234567890abcdef1234567890",
		Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, armErr := br.ArmSell(ctx, SellInput{
		Shard:     "secret-leak",
		Domains:   []string{"ops"},
		PriceSats: 50,
		AllowRaw:  true,
	}); armErr == nil {
		t.Fatal("expected ArmSell to fail closed on shard containing secret")
	}

	// 2. Write a clean private working fact and run SleepCycle -> promotes to market-ops (Distilled=true) and auto-lists!
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard: "dev-notes",
		Text:  "BSV SPV verification checks Merkle paths offline against block headers without trusting an RPC server.",
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := eng.RunSleepCycle(ctx, SleepCycleInput{
		Shards:     []string{"dev-notes"},
		AutoAnchor: true,
		AutoList:   true,
		Force:      true,
	})
	if err != nil {
		t.Fatalf("RunSleepCycle: %v", err)
	}
	if st.Status != "completed" || st.TotalDistilled != 1 {
		t.Fatalf("expected completed sleep cycle with 1 distilled fact, got %+v", st)
	}

	// 3. Verify autoQuoteWant quotes market-ops (Distilled=true) with AllowRaw=false!
	listings, err := db.ListSellListings()
	if err != nil || len(listings) != 1 || listings[0].Shard != "market-ops" || listings[0].AllowRaw {
		t.Fatalf("expected auto-listed market-ops with AllowRaw=false, got %+v err=%v", listings, err)
	}
}

func TestZeroSatMicroEarner_EndToEndWorkAndSponsoredBounty(t *testing.T) {
	ctx := context.Background()

	// Boot a funded Hub with Track A work bounties & Track B sponsored bounties armed!
	hubDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer hubDB.Close()
	hubPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer hubPort.Close()
	hubMem, err := memory.New(hubDB, hubPort, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	hubNode, err := hub.NewNode(hub.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()
	if _, err := hub.ArmHubLiquidity(ctx, hubNode, hubMem, hub.LiquidityConfig{
		SeedStarterShards: false,
		ArmBounties:       true,
		BountyRewardSats:  100,
	}); err != nil {
		t.Fatalf("ArmHubLiquidity: %v", err)
	}
	srv := httptest.NewServer(hubNode.Handler())
	defer srv.Close()

	// Boot a fresh worker node with a distilled fact ready for Track B sponsored bounty as well.
	nodeDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer nodeDB.Close()
	nodePort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer nodePort.Close()
	nodeMem, err := memory.New(nodeDB, nodePort, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeMem.Remember(ctx, memory.RememberInput{
		Shard:     "market-ops",
		Text:      "Go 1.27 zero-CGO builds use modernc.org/sqlite with SetMaxOpenConns(1) and WAL for reliable concurrent writes.",
		Public:    true,
		Distilled: true,
	}); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(nodeMem, DefaultPolicy())

	// Target above the memory testchain initial balance so MicroEarner actively runs Track A + Track B!
	initBal := nodePort.BalanceSats()
	st, err := eng.RunMicroEarnerCycle(ctx, MicroEarnerInput{
		TargetFloatSats: initBal + 500,
		MaxTasks:        5,
		HubURLs:         []string{srv.URL},
	})
	if err != nil {
		t.Fatalf("RunMicroEarnerCycle: %v", err)
	}
	if st.TasksCompleted < 1 {
		t.Fatalf("expected MicroEarner to complete >= 1 Track A work bounty, got %+v", st)
	}
	if st.EarnedSatsTotal == 0 {
		t.Fatalf("expected non-zero earned sats from MicroEarner, got %+v", st)
	}

	// Verify economics_events recorded the work_earn telemetry!
	econ, err := nodeDB.SummarizeEconomics(0)
	if err != nil {
		t.Fatalf("SummarizeEconomics: %v", err)
	}
	if econ.WorkBounties < 1 || econ.EarnedSats == 0 {
		t.Fatalf("expected economics work_earn recorded, got %+v", econ)
	}

	// Once target float is reached, MicroEarner parks cleanly at funded_idle!
	idleSt, err := eng.RunMicroEarnerCycle(ctx, MicroEarnerInput{
		TargetFloatSats: 1,
		HubURLs:         []string{srv.URL},
	})
	if err != nil || idleSt.Status != "funded_idle" {
		t.Fatalf("expected funded_idle when balance >= target, got %+v err=%v", idleSt, err)
	}
}

func TestBilateralAttestationMangleTrustAndOutcomeUpdate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}

	buyerEng := NewEngine(mem, DefaultPolicy())
	sellerDB, _ := store.Open(":memory:")
	defer sellerDB.Close()
	sellerPort, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	defer sellerPort.Close()
	sellerMem, _ := memory.New(sellerDB, sellerPort, nil, nil, memory.Config{})
	sellerEng := NewEngine(sellerMem, DefaultPolicy())

	now := time.Now().Unix()
	dr := DealReceipt{
		Version:       1,
		DealID:        "deal-rep-1",
		OfferID:       "off-rep-1",
		BuyerPub:      buyerEng.salePubHex(),
		SellerPub:     sellerEng.salePubHex(),
		Domains:       []string{"ops"},
		PriceSats:     50,
		PaymentTxID:   strings.Repeat("a", 64),
		KeyCommitment: strings.Repeat("b", 64),
		FragmentLeaf:  strings.Repeat("c", 64),
		SettledAt:     now,
	}
	if err := SignDealReceiptSeller(ctx, sellerEng.salePubHex(), sellerEng.signSaleDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptSeller: %v", err)
	}
	if err := SignDealReceiptBuyer(ctx, buyerEng.salePubHex(), buyerEng.signSaleDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptBuyer: %v", err)
	}

	scores := reputation.EvaluatePurchasedFragment(reputation.BuyerEvalInput{
		SellerPub: sellerEng.salePubHex(), OntologyID: "ont-ops-test",
		SPVVerified: true, MPEVerified: true, HasEvidenceSpans: true, HasPinnedOntology: true, SellerBonded: true,
	})
	tok, err := buyerEng.EmitPostDealAttestation(ctx, reputation.RoleBuyerRatesSeller, dr, "ont-ops-test", scores, []string{"ops"})
	if err != nil || !strings.HasPrefix(tok, reputation.TokenPrefix) {
		t.Fatalf("EmitPostDealAttestation: tok=%q err=%v", tok, err)
	}

	eval, vecs, err := EvaluateTrustForOntology(db, buyerEng.salePubHex(), "ont-ops-test", buyerEng.Policy(), nil, nil)
	if err != nil {
		t.Fatalf("EvaluateTrustForOntology: %v", err)
	}
	sellerPubLower := strings.ToLower(sellerEng.salePubHex())
	if !eval.TrustedSellers[sellerPubLower] {
		t.Fatalf("expected seller to be Mangle trusted_seller after verified clean deal, vec=%+v", vecs[sellerPubLower])
	}
}
