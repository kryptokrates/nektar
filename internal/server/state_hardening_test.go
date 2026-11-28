package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func newServerHardeningFixture(t *testing.T) (*App, *store.DB, *market.Engine, *market.Broker) {
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
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	disc := discovery.New(mem, nil, nil)
	broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{})
	hubNode, err := hub.NewNode(hub.Config{Network: "memory", DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubNode.Close() })

	app, err := NewApp(mem, mkt, Config{
		ToolPrices: map[string]uint64{"recall": 100},
		Hub:        hubNode,
		Discovery:  disc,
		Broker:     broker,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, db, mkt, broker
}

// TestRestateNotMountedOnPublicPortAndHonestTelemetry covers R1, R12, and F26.
func TestRestateNotMountedOnPublicPortAndHonestTelemetry(t *testing.T) {
	app, _, _, _ := newServerHardeningFixture(t)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// R1/R12: Every /restate/* route on :18420 must return 404!
	for _, p := range []string{"/restate/discover", "/restate/invoke/Custody/reserve", "/restate/invoke/AnchorCadence/trigger"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for public %s, got %d", p, resp.StatusCode)
		}
	}

	// F26: Without RestateClient, /state reports executor="unattached" (never fake "go-restate-sdk")!
	resp, err := http.Get(ts.URL + "/state")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /state failed: err=%v resp=%+v", err, resp)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	rt := st["automation"].(map[string]any)["rt"].(map[string]any)
	if rt["executor"] != "unattached" {
		t.Fatalf("expected honest executor=unattached when no RestateClient, got %v", rt["executor"])
	}

	// With stopped WorkflowClient, /state reports executor="workflows-unavailable"!
	app.cfg.RestateClient = market.NewWorkflowClient(nil, nil)
	st2 := app.BuildDashboardState(context.Background(), ts.URL)
	rt2 := st2["automation"].(map[string]any)["rt"].(map[string]any)
	if rt2["executor"] != "workflows-unavailable" {
		t.Fatalf("expected honest executor=workflows-unavailable on stopped runtime, got %v", rt2["executor"])
	}
}

// TestCrossInterfacePaymentReplayRejected covers F4:
// A payment txid / UTXO outpoint claimed via HTTP 402 paywall is atomically rejected on replay or conflicting outpoint!
func TestCrossInterfacePaymentReplayRejected(t *testing.T) {
	app, db, _, _ := newServerHardeningFixture(t)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// Pre-claim txid via shared SQLite ClaimPaymentAtomic (simulating /v1/market/purchase).
	if err := db.ClaimPaymentAtomic("tx-shared-1", []string{"utxo-a:0"}); err != nil {
		t.Fatal(err)
	}

	// Replaying the same txid OR a different txid spending utxo-a:0 on 402-gated /v1/recall MUST return 409 Conflict!
	for _, hdr := range []string{
		`{"txid":"tx-shared-1","amountSats":100,"inputs":["utxo-a:0"]}`,
		`{"txid":"tx-double-spend","amountSats":100,"inputs":["utxo-a:0"]}`,
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/recall", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-BSV-Payment", hdr)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected HTTP 409 on cross-interface replay/double-spend (%s), got %d", hdr, resp.StatusCode)
		}
	}
}

// TestFaucetIgnoresSpoofedXForwardedForByDefault covers F15.
func TestFaucetIgnoresSpoofedXForwardedForByDefault(t *testing.T) {
	app, _, _, _ := newServerHardeningFixture(t)
	t.Setenv("NEKTAR_TRUST_PROXY", "")

	// 5 requests from same RemoteAddr with DIFFERENT spoofed X-Forwarded-For headers must hit the 5/hr rate limit on the 6th!
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/faucet", nil)
		req.RemoteAddr = "198.51.100.10:5555"
		req.Header.Set("X-Forwarded-For", "203.0.113."+string(rune('1'+i)))
		if !app.allowFaucetIP(req) {
			t.Fatalf("expected request %d allowed", i+1)
		}
	}
	req6 := httptest.NewRequest(http.MethodPost, "/v1/faucet", nil)
	req6.RemoteAddr = "198.51.100.10:5555"
	req6.Header.Set("X-Forwarded-For", "203.0.113.99")
	if app.allowFaucetIP(req6) {
		t.Fatal("expected 6th faucet request from same RemoteAddr to be rate-limited despite spoofed X-Forwarded-For")
	}
}

// TestKillSwitchPersistsAcrossRestart covers F16.
func TestKillSwitchPersistsAcrossRestart(t *testing.T) {
	dbPath := t.TempDir() + "/ks.sqlite"
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	p1 := market.DefaultPolicy()
	p1.AttachDB(db1)
	p1.SetKillSwitch(true)
	_ = db1.Close()

	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	p2 := market.DefaultPolicy()
	p2.AttachDB(db2)
	kill, _, _, _, _, _, _ := p2.Snapshot()
	if !kill {
		t.Fatal("expected persisted market_kill_switch to remain engaged after restart")
	}
}
