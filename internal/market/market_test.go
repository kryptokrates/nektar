package market

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func newTestMemory(t *testing.T) *memory.Service {
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
	svc, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestRestateGoSDKServicesClientAndAtomicKnowledgeTrade(t *testing.T) {
	ctx := context.Background()
	buyerMem := newTestMemory(t)
	sellerMem := newTestMemory(t)

	buyer := NewEngine(buyerMem, DefaultPolicy())
	seller := NewEngine(sellerMem, DefaultPolicy())

	// 1. Verify ALL 6 Restate Go SDK services (BuyerDeal, SellerDeal, Custody, Tx, AnchorCadence, NektarRuntime)
	//    bind and serve Restate's service discovery protocol, with ingressPrivate on Custody/Tx/AnchorCadence!
	rSrv := buyer.NewRestateServer()
	h, err := rSrv.Handler()
	if err != nil || h == nil {
		t.Fatalf("Restate Bind/Handler failed: %v", err)
	}
	rec := httptest.NewRecorder()
	discReq := httptest.NewRequest(http.MethodGet, "/discover", nil)
	discReq.Header.Set("Accept", "application/vnd.restate.endpointmanifest.v4+json")
	h.ServeHTTP(rec, discReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("Restate /discover expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	manifest := rec.Body.String()
	for _, svcName := range []string{"BuyerDeal", "SellerDeal", "Custody", "Tx", "AnchorCadence", "NektarRuntime"} {
		if !strings.Contains(manifest, `"`+svcName+`"`) {
			t.Fatalf("Restate Go manifest missing %s: %s", svcName, manifest)
		}
	}

	// 2. Verify pure-Go RestateClient Admin registration + sdk-go/ingress async NektarRuntime/emitTx send!
	var regURI, calledPath string
	mockRestate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/deployments" {
			var p map[string]any
			_ = json.Unmarshal(body, &p)
			regURI, _ = p["uri"].(string)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"dp_1"}`))
			return
		}
		calledPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"invocationId":"inv_tx123","status":"Accepted"}`))
	}))
	defer mockRestate.Close()

	rc := NewRestateClient(mockRestate.URL, mockRestate.URL)
	if err := rc.RegisterDeployment(ctx, "http://127.0.0.1:9080", true); err != nil {
		t.Fatalf("RegisterDeployment: %v", err)
	}
	if regURI != "http://127.0.0.1:9080" {
		t.Fatalf("unexpected registered URI: %q", regURI)
	}
	invID, err := rc.EmitTxEvent(ctx, "tx123", TxEventInput{Kind: "mined", BlockHeight: 900001})
	if err != nil || calledPath != "/NektarRuntime/emitTx/send" || invID != "inv_tx123" {
		t.Fatalf("EmitTxEvent failed: path=%q invID=%q err=%v", calledPath, invID, err)
	}

	// 3. Deterministic bps ranking & kill-switch refusal.
	want := bstack.WantAd{WantID: "w1", MaxSats: 1000}
	ranked := RankOffers(want, []bstack.OfferAd{
		{OfferID: "expensive", Pub: "s1", PriceSats: 900},
		{OfferID: "best", Pub: "s2", PriceSats: 200, Direct: true, KeyCommitHex: "aa", ChirpURL: "chirp://x"},
		{OfferID: "overcap", Pub: "s3", PriceSats: 5000},
	}, buyer.Policy())
	if len(ranked) != 2 || ranked[0].Offer.OfferID != "best" {
		t.Fatalf("unexpected ranking: %+v", ranked)
	}

	// 4. Seller remembers & anchors a fact, then sells it atomically via signed DisclosureSale + BRC-369 + CHIRP + on-chain pay!
	rem, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "research",
		Text:       "Teranode regtest sustained 1M TPS in benchmark run 42.",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	buyer.Policy().SetKillSwitch(true)
	if _, err := ExecuteAtomicKnowledgeTrade(ctx, buyer, seller, "research", rem.Seq, "acquired", 500); err == nil {
		t.Fatal("expected kill switch to block trade")
	}
	buyer.Policy().SetKillSwitch(false)

	deal, err := ExecuteAtomicKnowledgeTrade(ctx, buyer, seller, "research", rem.Seq, "acquired", 500)
	if err != nil {
		t.Fatalf("ExecuteAtomicKnowledgeTrade: %v", err)
	}
	if deal.Status != "settled" || deal.DeliveredTxt != "Teranode regtest sustained 1M TPS in benchmark run 42." {
		t.Fatalf("unexpected deal result: %+v", deal)
	}

	// Verify buyer now holds the anchored knowledge in its own memory shard!
	recalled, err := buyerMem.Recall(ctx, memory.RecallInput{Shard: "acquired", VerifySPV: true})
	if err != nil || len(recalled) != 1 || recalled[0].SPV == nil || !recalled[0].SPV.Settled {
		t.Fatalf("expected buyer to hold SPV-verified purchased fact, got %+v err=%v", recalled, err)
	}
}
