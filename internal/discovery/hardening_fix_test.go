package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestHubTermsCacheEnforcesHardCap(t *testing.T) {
	c := NewHubTermsCache()
	for i := 0; i < maxHubTermsCache+50; i++ {
		c.Put(fmt.Sprintf("https://hub-%d.example", i), HubTerms{
			Network:         "regtest",
			PrefixExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		})
	}
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	if n > maxHubTermsCache {
		t.Fatalf("expected HubTermsCache len <= %d, got %d", maxHubTermsCache, n)
	}
}

func TestBuildAndSubmitMarketAdCumulativeFeeCap(t *testing.T) {
	ctx := context.Background()
	var submitCount atomic.Int32

	makePaidHub := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/terms":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(HubTerms{
					Network: "memory",
					Fees:    map[string]uint64{"submit:want": 40},
				})
			case "/submit":
				submitCount.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}
	h1 := makePaidHub()
	defer h1.Close()
	h2 := makePaidHub()
	defer h2.Close()
	h3 := makePaidHub()
	defer h3.Close()

	svc := New(nil, nil, h1.Client())
	svc.network = "memory"
	// Cap cumulative spend at 80 sats: 2 hubs (40+40=80) allowed, 3rd hub (120) MUST be skipped!
	svc.SetHubFeePolicy(func(_ string, sats uint64) error {
		if sats > 80 {
			return fmt.Errorf("cap exceeded: %d > 80", sats)
		}
		return nil
	}, nil)

	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	want := bstack.WantAd{
		WantID:  "w-cap-1",
		Pub:     svc.AnnouncePubHex(),
		Net:     "memory",
		Domains: []string{"ops"},
		Query:   "cap check",
		MaxSats: 100,
		Exp:     time.Now().Add(10 * time.Minute).UnixMilli(),
	}
	sig, err := bstack.SignArtifact(priv, want)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	submitted, err := svc.BuildAndSubmitMarketAd(ctx, priv, bstack.MarketPayload{Kind: bstack.KindWant, Body: body}, []string{h1.URL, h2.URL, h3.URL}, []string{bstack.LaneOf("memory", "ops")})
	if err != nil {
		t.Fatalf("BuildAndSubmitMarketAd: %v", err)
	}
	if submitted != 2 || submitCount.Load() != 2 {
		t.Fatalf("expected cumulative fee cap to allow exactly 2 hubs (80 sats), got submitted=%d calls=%d", submitted, submitCount.Load())
	}
}

func TestTopUpHubEgressCreditsRecordsSpendBeforeHTTPPost(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(dir + "/nektar.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}

	payeeKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	payeePub := fmt.Sprintf("%x", payeeKey.PubKey().Compressed())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/terms":
			var terms HubTerms
			terms.Network = "memory"
			terms.PayeeIdentityKey = payeePub
			terms.Egress.MinTopupSats = 25
			terms.Egress.TopupPath = "/v1/credits/topup"
			terms.Prefixes = map[string]string{"egress:topup": "dGVzdC1wcmVmaXg="}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(terms)
		case "/v1/credits/topup":
			// Simulate Hub 500 AFTER buyer already broadcast the on-chain BRC-29 payment!
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"hub transient db error"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	svc := New(mem, nil, srv.Client())
	var recorded uint64
	svc.SetHubFeePolicy(func(_ string, _ uint64) error { return nil }, func(sats uint64) {
		recorded += sats
	})

	_, err = svc.TopUpHubEgressCredits(ctx, srv.URL, 25, svc.AnnouncePubHex())
	if err == nil {
		t.Fatal("expected HTTP 500 error from TopUpHubEgressCredits")
	}
	if recorded != 25 {
		t.Fatalf("expected recorder(25) called immediately after on-chain broadcast despite HTTP 500, got %d", recorded)
	}
}
