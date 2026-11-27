package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestExtractProxyUsage_OpenAIAndAnthropicJSONAndSSE(t *testing.T) {
	// 1. OpenAI JSON response
	oaJSON := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1200,"completion_tokens":340}}`
	in1, out1 := extractProxyUsage("openai", oaJSON, false)
	if in1 != 1200 || out1 != 340 {
		t.Fatalf("OpenAI JSON usage = (%d, %d), want (1200, 340)", in1, out1)
	}

	// 2. Anthropic SSE stream (message_start + message_delta)
	antSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2500,\"output_tokens\":10}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":620}}\n\n"
	in2, out2 := extractProxyUsage("anthropic", antSSE, true)
	if in2 != 2500 || out2 != 620 {
		t.Fatalf("Anthropic SSE usage = (%d, %d), want (2500, 620)", in2, out2)
	}
}

func TestAutonomousHTTPAndDashboardStateEndpoints(t *testing.T) {
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
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	disc := discovery.New(mem, nil, nil)
	broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{})
	defer broker.Stop()

	// Seed a private working fact so POST /v1/distill/run promotes it!
	_, err = mem.Remember(ctx, memory.RememberInput{
		Shard: "dev-work",
		Text:  "BSV SPV verification checks Merkle paths offline against headers without trusting any RPC.",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fake OpenAI upstream returning usage.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Use offline SPV verification."}}],"usage":{"prompt_tokens":1800,"completion_tokens":240}}`))
	}))
	defer up.Close()

	app, err := NewApp(mem, mkt, Config{
		OpenAIUpstream: up.URL,
		Broker:         broker,
		Discovery:      disc,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. Proxy request records proxy_usage + conservative tokens_saved into economics_events!
	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"How does BSV SPV work?"}]}`)
	resp, err := http.Post(ts.URL+"/v1/proxy/openai/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy POST: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()

	// Wait briefly for async postProxyTurn goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sum, _ := db.SummarizeEconomics(0); sum.TokensIn == 1800 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sum, err := db.SummarizeEconomics(0)
	if err != nil || sum.TokensIn != 1800 || sum.TokensOut != 240 || sum.TokensSaved == 0 {
		t.Fatalf("expected recorded proxy economics (in=1800, out=240, saved>0), got %+v err=%v", sum, err)
	}

	// 2. POST /v1/distill/run promotes dev-work into market-ops!
	dResp, err := http.Post(ts.URL+"/v1/distill/run", "application/json", bytes.NewReader([]byte(`{"autoList":true}`)))
	if err != nil || dResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/distill/run: resp=%v err=%v", dResp, err)
	}
	var dState market.SleepCycleState
	_ = json.NewDecoder(dResp.Body).Decode(&dState)
	dResp.Body.Close()
	if dState.Status != "completed" || dState.TotalDistilled < 1 {
		t.Fatalf("unexpected distill state: %+v", dState)
	}

	// 3. GET /state exposes economics, reputation, distill, microEarner for the Svelte 5 dashboard!
	sResp, err := http.Get(ts.URL + "/state")
	if err != nil || sResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /state: resp=%v err=%v", sResp, err)
	}
	var state map[string]any
	_ = json.NewDecoder(sResp.Body).Decode(&state)
	sResp.Body.Close()
	for _, key := range []string{"economics", "reputation", "distill", "microEarner"} {
		if _, ok := state[key]; !ok {
			t.Fatalf("GET /state missing %q key", key)
		}
	}

	// 4. GET /v1/reputation and POST /v1/microearner/toggle return 200 OK.
	rResp, err := http.Get(ts.URL + "/v1/reputation")
	if err != nil || rResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/reputation: resp=%v err=%v", rResp, err)
	}
	rResp.Body.Close()

	mResp, err := http.Post(ts.URL+"/v1/microearner/toggle", "application/json", bytes.NewReader([]byte(`{"enabled":false}`)))
	if err != nil || mResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/microearner/toggle: resp=%v err=%v", mResp, err)
	}
	var me market.MicroEarnerState
	_ = json.NewDecoder(mResp.Body).Decode(&me)
	mResp.Body.Close()
	if me.Enabled || me.Status != "disabled" {
		t.Fatalf("expected disabled microEarner, got %+v", me)
	}
}
