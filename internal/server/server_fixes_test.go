package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestDockProbePropagatesCallerPrincipal(t *testing.T) {
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
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "secret-probe", Text: "TOP-SECRET-PROBE-LEVEL-10", Level: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Groups().CreateGroup("probe-borg", "Probe Borg", port.IdentityPubHex()); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "borg-probe", Text: "TOP-SECRET-BORG-PROBE", BorgGroup: "probe-borg", Level: 0,
	}); err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{
		APIToken:      "agent-probe-tok",
		OperatorToken: "op-probe-tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/dock/probe", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer agent-probe-tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/dock/probe status=%d body=%s", resp.StatusCode, string(body))
	}
	if strings.Contains(string(body), "secret-probe") || strings.Contains(string(body), "borg-probe") ||
		strings.Contains(string(body), "TOP-SECRET") {
		t.Fatalf("POST /v1/dock/probe leaked Level>0 or Borg shard to Level-0 agent token: %s", string(body))
	}
}

func TestRestateClientConcurrencyAndLivenessCheck(t *testing.T) {
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
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	disc := discovery.New(mem, nil, nil)
	broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{})
	app, err := NewApp(mem, mkt, Config{Discovery: disc, Broker: broker})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	var alive atomic.Bool
	alive.Store(true)
	rc := market.NewRestateClient("http://127.0.0.1:1", "http://127.0.0.1:1")
	broker.SetRestateClient(rc)
	app.SetRestateClient(rc, "http://127.0.0.1:9070")
	app.SetRestateLiveness(alive.Load)

	// Concurrent SetRestateClient + BuildDashboardState + metrics must be race-free.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			app.SetRestateClient(rc, "http://127.0.0.1:9070")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_ = app.BuildDashboardState(ctx, "http://127.0.0.1:18420")
		}
	}()
	wg.Wait()

	// Alive=true => nektar_restate_attached 1 and doctor restateAttached=true.
	rec1 := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	if !strings.Contains(rec1.Body.String(), "nektar_restate_attached 1\n") {
		t.Fatalf("expected nektar_restate_attached 1 when alive, got:\n%s", rec1.Body.String())
	}
	doc1 := app.Toolbox().ExecDoctor(ctx)
	if attached, _ := doc1["restateAttached"].(bool); !attached {
		t.Fatalf("expected doctor restateAttached=true when alive, got %v", doc1["restateAttached"])
	}

	// Alive=false (supervised restate-server exited unexpectedly) => metric 0 and doctor false.
	alive.Store(false)
	rec2 := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	if !strings.Contains(rec2.Body.String(), "nektar_restate_attached 0\n") {
		t.Fatalf("expected nektar_restate_attached 0 after exit, got:\n%s", rec2.Body.String())
	}
	doc2 := app.Toolbox().ExecDoctor(ctx)
	if attached, _ := doc2["restateAttached"].(bool); attached {
		t.Fatalf("expected doctor restateAttached=false after exit, got %v", doc2["restateAttached"])
	}
}

func TestRateLimiterIPv6Slash64AndHardCap(t *testing.T) {
	var mu sync.Mutex
	buckets := make(map[string][]int64)
	var lastPrune int64

	// 5 requests from different hosts in the same IPv6 /64 share one rate-limit bucket.
	for i := 1; i <= 5; i++ {
		ip := "2001:db8:abcd:0012::" + strconv.Itoa(i)
		if !allowRateLimitedIP(&mu, buckets, &lastPrune, ip, 5, 3600, 16) {
			t.Fatalf("expected IPv6 request %d allowed", i)
		}
	}
	if allowRateLimitedIP(&mu, buckets, &lastPrune, "2001:db8:abcd:0012::9999", 5, 3600, 16) {
		t.Fatal("expected 6th request in same IPv6 /64 prefix to be rate-limited")
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 normalized /64 bucket, got %d", len(buckets))
	}

	// Fill up to maxKeys=4 and ensure 5th distinct IP fails closed without growing map.
	buckets2 := make(map[string][]int64)
	var lastPrune2 int64
	for i := 1; i <= 4; i++ {
		if !allowRateLimitedIP(&mu, buckets2, &lastPrune2, "198.51.100."+strconv.Itoa(i), 5, 3600, 4) {
			t.Fatalf("expected distinct IP %d allowed under cap", i)
		}
	}
	if allowRateLimitedIP(&mu, buckets2, &lastPrune2, "198.51.100.200", 5, 3600, 4) {
		t.Fatal("expected new IP to fail closed when rate-limiter map is at hard cap")
	}
	if len(buckets2) != 4 {
		t.Fatalf("rate-limiter map exceeded hard cap: got %d want 4", len(buckets2))
	}
}

func TestResolvePaywallZeroConfLimitsActivePolicyZeroOverridesChain(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Without an active operator policy, non-zero chain default is preserved.
	pol := market.DefaultPolicy()
	maxSats, aggSats, _ := resolvePaywallZeroConfLimits(500, 2000, 15, pol)
	if maxSats != 500 || aggSats != 2000 {
		t.Fatalf("expected inactive default policy to preserve chain caps (500, 2000), got (%d, %d)", maxSats, aggSats)
	}

	// Approve an operator policy explicitly setting ZeroConfMax = 0 -> MUST override chain default 500 -> 0!
	zero := uint64(0)
	if _, err := market.ApprovePolicyCustom(db, pol, "both", nil, &market.PolicyLimitsOverride{
		MaxPerDeal:  250,
		MaxPerDay:   1000,
		ZeroConfMax: &zero,
	}, "test", nil); err != nil {
		t.Fatal(err)
	}
	maxSats, aggSats, _ = resolvePaywallZeroConfLimits(500, 2000, 15, pol)
	if maxSats != 0 || aggSats != 0 {
		t.Fatalf("expected active operator policy with zeroConfMax=0 to override chain caps, got (%d, %d)", maxSats, aggSats)
	}
}

func TestMCPPaywallGatesResourcesRead(t *testing.T) {
	app, _, _, _ := newServerHardeningFixture(t)
	defer app.Close()
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// Fixture prices "recall": 100, which backs nektar://welcome.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"nektar://welcome"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 on priced MCP resources/read, got %d", resp.StatusCode)
	}

	// Batched priced resources/read MUST be rejected with 400.
	bReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"nektar://welcome"}}]`))
	bReq.Header.Set("Content-Type", "application/json")
	bReq.Header.Set("Accept", "application/json, text/event-stream")
	bResp, err := http.DefaultClient.Do(bReq)
	if err != nil {
		t.Fatal(err)
	}
	bResp.Body.Close()
	if bResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on batched priced MCP resources/read, got %d", bResp.StatusCode)
	}
}

func TestProxyRefusesRedirectSSRFAndDrainsBackgroundOnClose(t *testing.T) {
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
	mkt := market.NewEngine(mem, market.DefaultPolicy())

	var followed atomic.Bool
	internalTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer internalTarget.Close()

	redirectUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internalTarget.URL+"/secret", http.StatusFound)
	}))
	defer redirectUpstream.Close()

	app, err := NewApp(mem, mkt, Config{
		OpenAIUpstream: redirectUpstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	cli := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/proxy/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected proxy to return 302 without following redirect, got %d", resp.StatusCode)
	}
	if followed.Load() {
		t.Fatal("proxy followed upstream 3xx redirect (SSRF/credential-leak regression)")
	}

	// Verify app.Close() cancels bgCtx and waits for tracked background goroutines.
	var bgFinished atomic.Bool
	app.goBackground(func(bgCtx context.Context) {
		<-bgCtx.Done()
		time.Sleep(10 * time.Millisecond)
		bgFinished.Store(true)
	})
	app.Close()
	if !bgFinished.Load() {
		t.Fatal("app.Close() returned before tracked background goroutine finished")
	}
}

func TestHubBundleDomainValidationAndJSONMutationRoutes(t *testing.T) {
	app, _, _, _ := newServerHardeningFixture(t)
	defer app.Close()
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// Malicious domain injection attempts MUST be rejected with 400.
	for _, bad := range []string{
		"hub.example.com;rm+-rf+/",
		"hub.example.com%0Aexport+FOO=bar",
		"$(whoami).example.com",
		"-bad.example.com",
	} {
		resp, err := http.Get(ts.URL + "/v1/onboard/hub-bundle?mode=domain&domain=" + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid hub-bundle domain %q, got %d", bad, resp.StatusCode)
		}
	}

	// Valid hostname succeeds.
	okResp, err := http.Get(ts.URL + "/v1/onboard/hub-bundle?mode=domain&domain=hub-1.example.com")
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]any
	_ = json.NewDecoder(okResp.Body).Decode(&bundle)
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK || !strings.Contains(bundle["cliCommand"].(string), "hub-1.example.com") {
		t.Fatalf("valid domain failed: status=%d bundle=%+v", okResp.StatusCode, bundle)
	}

	// Newly added isJSONMutation routes MUST reject text/plain POSTs with 415.
	for _, p := range []string{
		"/v1/market/standing",
		"/v1/dock/probe",
		"/v1/hubs/connect-all",
		"/v1/runtime/invocations/inv-1/resume",
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+p, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "text/plain")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("expected 415 on text/plain POST %s, got %d", p, r.StatusCode)
		}
	}
}
