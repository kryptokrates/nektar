package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestColdStartHealthEndpointAndEmbeddedAdapters(t *testing.T) {
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
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = mem.EnsureWelcome(ctx)
	hubNode, err := hub.NewNode(hub.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()
	if _, err := hub.ArmHubLiquidity(ctx, hubNode, mem, hub.LiquidityConfig{SeedStarterShards: true, ArmBounties: true}); err != nil {
		t.Fatal(err)
	}
	disc := discovery.New(mem, nil, nil)
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{IsHub: true, Hub: hubNode})
	app, err := NewApp(mem, mkt, Config{Hub: hubNode, Discovery: disc, Broker: broker})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. GET /v1/coldstart/health returns bars, per-domain status, and published bounty taper.
	resp, err := http.Get(ts.URL + "/v1/coldstart/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/coldstart/health failed: resp=%v err=%v", resp, err)
	}
	var cs map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cs["bars"] == nil || cs["bountyTaper"] == nil || cs["domains"] == nil || cs["truthNote"] == "" {
		t.Fatalf("expected bars, bountyTaper, domains, truthNote in /v1/coldstart/health: %+v", cs)
	}

	// 2. Embedded Hermes & OpenClaw bundles MUST serve 200 OK directly from binary (zero 404!).
	for _, name := range []string{"hermes", "openclaw"} {
		bResp, err := http.Get(ts.URL + "/v1/adapters/bundle/" + name)
		if err != nil || bResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/adapters/bundle/%s failed: resp=%v err=%v", name, bResp, err)
		}
		var body struct {
			Files []struct{ Path string } `json:"files"`
		}
		_ = json.NewDecoder(bResp.Body).Decode(&body)
		bResp.Body.Close()
		if len(body.Files) < 3 {
			t.Fatalf("expected >=3 embedded files for %s, got %d", name, len(body.Files))
		}
	}

	// 3. POST /v1/dock/probe returns a REAL verified nektarr1.* receipt on welcome shard!
	pResp, err := http.Post(ts.URL+"/v1/dock/probe", "application/json", strings.NewReader(`{}`))
	if err != nil || pResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/dock/probe failed: resp=%v err=%v", pResp, err)
	}
	var pr map[string]any
	_ = json.NewDecoder(pResp.Body).Decode(&pr)
	pResp.Body.Close()
	tok, _ := pr["receiptToken"].(string)
	if !strings.HasPrefix(tok, "nektarr1.") || pr["verified"] != true {
		t.Fatalf("expected live verified nektarr1.* receipt from /v1/dock/probe, got %+v", pr)
	}

	// 4. GET /v1/verify/{tok} HTML includes 25-second onboarding CTA!
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/verify/"+tok, nil)
	req.Header.Set("Accept", "text/html")
	vResp, err := http.DefaultClient.Do(req)
	if err != nil || vResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/verify HTML failed: resp=%v err=%v", vResp, err)
	}
	var htmlBuf strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rErr := vResp.Body.Read(buf)
		if n > 0 {
			htmlBuf.Write(buf[:n])
		}
		if rErr != nil {
			break
		}
	}
	vResp.Body.Close()
	if !strings.Contains(htmlBuf.String(), "install.sh | sh -s -- --up") {
		t.Fatalf("expected viral 25s --up CTA in verify HTML page")
	}
}
