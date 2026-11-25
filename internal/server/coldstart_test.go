package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestColdStartDashboardParityPairingAndStdioMCPBridge(t *testing.T) {
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
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := mem.EnsureWelcome(ctx)
	if err != nil || !seeded {
		t.Fatalf("EnsureWelcome expected seeded=true, got %v err=%v", seeded, err)
	}
	hubNode, err := hub.NewNode(hub.Config{Network: "memory", EnableFinger: true})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()
	disc := discovery.New(mem, nil, nil)
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{IsHub: true})

	app, err := NewApp(mem, mkt, Config{
		APIToken:      "http-secret-1",
		OperatorToken: "op-secret-2",
		Hub:           hubNode,
		Discovery:     disc,
		Broker:        broker,
		ToolProfile:   "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. Auth gate: GET /state without token returns 401!
	r401, err := http.Get(ts.URL + "/state")
	if err != nil || r401.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated GET /state, got %+v err=%v", r401, err)
	}
	r401.Body.Close()

	// 2. 1-click pairing URL `/pair?code=...` sets HttpOnly session cookie!
	code := app.CreatePairCode()
	noRedirClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	pResp, err := noRedirClient.Get(ts.URL + "/pair?code=" + code)
	if err != nil || pResp.StatusCode != http.StatusFound {
		t.Fatalf("GET /pair expected 302, got %+v err=%v", pResp, err)
	}
	cookies := pResp.Cookies()
	pResp.Body.Close()
	if len(cookies) == 0 || cookies[0].Name != "nektar_session" || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly nektar_session cookie from /pair, got %+v", cookies)
	}

	// 3. GET /state with session cookie returns full Svelte 5 typed State!
	reqState, _ := http.NewRequest(http.MethodGet, ts.URL+"/state", nil)
	reqState.AddCookie(cookies[0])
	stResp, err := http.DefaultClient.Do(reqState)
	if err != nil || stResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /state with cookie failed: %+v err=%v", stResp, err)
	}
	var state map[string]any
	_ = json.NewDecoder(stResp.Body).Decode(&state)
	stResp.Body.Close()
	for _, k := range []string{"network", "pub", "loop", "treasury", "receipts", "memory", "market", "net", "automation", "share", "settings"} {
		if _, ok := state[k]; !ok {
			t.Fatalf("GET /state missing expected Svelte key %q", k)
		}
	}

	// 4. OnboardingCard step 2: POST /v1/tools/recall with `{}` recalls welcome shard!
	reqRec, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/tools/recall", strings.NewReader(`{}`))
	reqRec.Header.Set("Authorization", "Bearer http-secret-1")
	recResp, err := http.DefaultClient.Do(reqRec)
	if err != nil || recResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/tools/recall failed: %+v err=%v", recResp, err)
	}
	var recBody struct {
		Shards []string `json:"shards"`
		Items  []struct {
			Shard string `json:"shard"`
			Seq   int64  `json:"seq"`
			Text  string `json:"text"`
		} `json:"items"`
	}
	_ = json.NewDecoder(recResp.Body).Decode(&recBody)
	recResp.Body.Close()
	if len(recBody.Items) < 3 || len(recBody.Shards) == 0 {
		t.Fatalf("expected >=3 welcome items from recall {}, got %+v", recBody)
	}

	// 5. OnboardingCard step 2b: groundedAnswer + Hermes GET /v1/verify?r=<token>!
	gaPayload, _ := json.Marshal(map[string]any{
		"claims": []map[string]any{
			{"text": recBody.Items[0].Text[:60], "cites": []map[string]any{{"shard": recBody.Items[0].Shard, "seq": recBody.Items[0].Seq}}},
		},
	})
	reqGA, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/tools/groundedAnswer", bytes.NewReader(gaPayload))
	reqGA.Header.Set("Authorization", "Bearer http-secret-1")
	gaResp, err := http.DefaultClient.Do(reqGA)
	if err != nil || gaResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/tools/groundedAnswer failed: %+v err=%v", gaResp, err)
	}
	var gaOut struct {
		ReceiptToken string `json:"receiptToken"`
		VerifyURL    string `json:"verifyUrl"`
	}
	_ = json.NewDecoder(gaResp.Body).Decode(&gaOut)
	gaResp.Body.Close()
	if !strings.HasPrefix(gaOut.ReceiptToken, "nektarr1.") {
		t.Fatalf("expected nektarr1.* receiptToken, got %+v", gaOut)
	}
	vResp, err := http.Get(ts.URL + "/v1/verify?r=" + gaOut.ReceiptToken)
	if err != nil || vResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/verify?r= failed: %+v err=%v", vResp, err)
	}
	vBody, _ := io.ReadAll(vResp.Body)
	vResp.Body.Close()
	if !strings.Contains(string(vBody), `"receiptToken":"nektarr1.`) {
		t.Fatalf("unexpected /v1/verify?r= body: %s", string(vBody))
	}

	// 6. Hermes agent card discovery checks `name == "nektar"`!
	cResp, err := http.Get(ts.URL + "/.well-known/agent.json")
	if err != nil || cResp.StatusCode != http.StatusOK {
		t.Fatal(err)
	}
	var card map[string]any
	_ = json.NewDecoder(cResp.Body).Decode(&card)
	cResp.Body.Close()
	if card["name"] != "nektar" {
		t.Fatalf("expected agent.json name==\"nektar\" for Hermes discovery, got %v", card["name"])
	}

	// 7. OnboardingCard policy-preset + hub-bundle + faucet + host join/leave!
	reqPol, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/onboard/policy-preset", strings.NewReader(`{"preset":"both"}`))
	reqPol.Header.Set("Authorization", "Bearer op-secret-2")
	polResp, err := http.DefaultClient.Do(reqPol)
	if err != nil || polResp.StatusCode != http.StatusOK {
		t.Fatalf("policy-preset failed: %+v err=%v", polResp, err)
	}
	polResp.Body.Close()

	reqBun, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/onboard/hub-bundle?mode=auto-ip&network=testnet", nil)
	reqBun.Header.Set("Authorization", "Bearer http-secret-1")
	bunResp, err := http.DefaultClient.Do(reqBun)
	if err != nil || bunResp.StatusCode != http.StatusOK {
		t.Fatalf("hub-bundle failed: %+v err=%v", bunResp, err)
	}
	bunBody, _ := io.ReadAll(bunResp.Body)
	bunResp.Body.Close()
	if !strings.Contains(string(bunBody), "nektar serve --hub") {
		t.Fatalf("expected v0.4 single-binary command in hub-bundle, got %s", string(bunBody))
	}

	// Join self /hub via POST /v1/hosts!
	reqJoin, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/hosts", strings.NewReader(`{"url":"`+ts.URL+`"}`))
	reqJoin.Header.Set("Authorization", "Bearer op-secret-2")
	jResp, err := http.DefaultClient.Do(reqJoin)
	if err != nil || jResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/hosts failed: %+v err=%v", jResp, err)
	}
	jResp.Body.Close()

	// 8. Multi-client stdio -> Streamable HTTP `/mcp` bridge ("one brain, many mouths")!
	stdinPayload := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test-cli","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall","arguments":{}}}`,
	}, "\n") + "\n"
	var stdout bytes.Buffer
	if err := RunStdioBridge(ctx, ts.URL, "http-secret-1", strings.NewReader(stdinPayload), &stdout); err != nil {
		t.Fatalf("RunStdioBridge failed: %v", err)
	}
	outStr := stdout.String()
	if !strings.Contains(outStr, `"name":"want"`) || !strings.Contains(outStr, `"name":"doctor"`) {
		t.Fatalf("expected MCP bridge tools/list to include want & doctor, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "welcome") {
		t.Fatalf("expected MCP bridge recall {} to return welcome shard, got:\n%s", outStr)
	}
}
