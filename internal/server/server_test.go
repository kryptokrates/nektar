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
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestUnifiedServerRESTA2AProxyPaywallAndDashboard(t *testing.T) {
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
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	hubNode, err := hub.NewNode(hub.Config{Network: "regtest", EnableFinger: true})
	if err != nil {
		t.Fatal(err)
	}

	// Mock upstream LLM capturing L0 proxy memory injection!
	var capturedOpenAIBody string
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedOpenAIBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer mockLLM.Close()

	app, err := NewApp(mem, mkt, Config{
		ToolPrices:     map[string]uint64{"groundedAnswer": 100},
		OpenAIUpstream: mockLLM.URL,
		Hub:            hubNode,
		MountRestate:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. Embedded Svelte 5 Dashboard HTML & /dashboard/state!
	resp, err := http.Get(ts.URL + "/")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / dashboard failed: resp=%+v err=%v", resp, err)
	}
	htmlBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(strings.ToLower(string(htmlBytes)), "<html") {
		t.Fatalf("expected embedded Svelte index.html, got: %.120s", string(htmlBytes))
	}

	// 2. Remember + Anchor via HTTP REST.
	remBody := `{"shard":"ops","text":"Nektar v0.4 compiles into one static Go binary.","autoAnchor":true}`
	rResp, err := http.Post(ts.URL+"/v1/remember", "application/json", strings.NewReader(remBody))
	if err != nil || rResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/remember failed: %+v %v", rResp, err)
	}
	var remRes memory.RememberResult
	_ = json.NewDecoder(rResp.Body).Decode(&remRes)
	rResp.Body.Close()
	if remRes.Seq != 1 || remRes.AnchorTxID == "" {
		t.Fatalf("unexpected remember result: %+v", remRes)
	}

	// 3. BRC-105 HTTP 402 Paywall + bcommon/payee.Claims replay protection!
	ansPayload, _ := json.Marshal(memory.GroundedAnswerInput{
		Claims: []reasoning.Claim{
			{Text: "Nektar v0.4 compiles into one static Go binary", Cites: []reasoning.FragmentRef{{Shard: "ops", Seq: 1}}},
		},
		AutoAnchor: true,
	})
	// Missing payment -> 402!
	p402, err := http.Post(ts.URL+"/v1/answer", "application/json", bytes.NewReader(ansPayload))
	if err != nil || p402.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 Payment Required, got %+v err=%v", p402, err)
	}
	p402.Body.Close()

	// Valid X-BSV-Payment -> 200 OK with nektarr1.* receipt!
	payHdr, _ := json.Marshal(BRC105PaymentHeader{TxID: "aabbcc112233", AmountSats: 100, Inputs: []string{"prevtx:0"}})
	reqPaid, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/answer", bytes.NewReader(ansPayload))
	reqPaid.Header.Set("Content-Type", "application/json")
	reqPaid.Header.Set("X-BSV-Payment", string(payHdr))
	p200, err := http.DefaultClient.Do(reqPaid)
	if err != nil || p200.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on paid /v1/answer, got %+v err=%v", p200, err)
	}
	var ga memory.GroundedAnswerResult
	_ = json.NewDecoder(p200.Body).Decode(&ga)
	p200.Body.Close()
	if !strings.HasPrefix(ga.ReceiptToken, "nektarr1.") {
		t.Fatalf("expected nektarr1 token, got %+v", ga)
	}

	// Replay of same payment txid or conflicting input -> 409 Conflict via bcommon/payee.Claims!
	reqReplay, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/answer", bytes.NewReader(ansPayload))
	reqReplay.Header.Set("Content-Type", "application/json")
	reqReplay.Header.Set("X-BSV-Payment", string(payHdr))
	p409, _ := http.DefaultClient.Do(reqReplay)
	if p409.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on payment replay, got %d", p409.StatusCode)
	}
	p409.Body.Close()

	// 4. Receipt verification endpoint (JSON + HTML)!
	vResp, err := http.Get(ts.URL + "/v1/verify/" + ga.ReceiptToken)
	if err != nil || vResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/verify failed: %+v %v", vResp, err)
	}
	var ver memory.ReceiptVerification
	_ = json.NewDecoder(vResp.Body).Decode(&ver)
	vResp.Body.Close()
	if ver.Verdict != "green" {
		t.Fatalf("expected green receipt verdict, got %+v", ver)
	}

	// 5. L0 Transparent OpenAI Memory Proxy injects verified memory context!
	proxyReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/proxy/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"What is Nektar v0.4?"}]}`))
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("X-Nektar-Shard", "ops")
	proxyResp, err := http.DefaultClient.Do(proxyReq)
	if err != nil || proxyResp.StatusCode != http.StatusOK {
		t.Fatalf("L0 OpenAI proxy failed: %+v %v", proxyResp, err)
	}
	proxyResp.Body.Close()
	if !strings.Contains(capturedOpenAIBody, "Nektar v0.4 compiles into one static Go binary.") {
		t.Fatalf("expected L0 proxy to inject verified shard memory into OpenAI messages, got: %s", capturedOpenAIBody)
	}

	// 6. A2A Agent Card & JSON-RPC 2.0!
	cardResp, err := http.Get(ts.URL + "/.well-known/agent.json")
	if err != nil || cardResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /.well-known/agent.json failed: %+v %v", cardResp, err)
	}
	cardResp.Body.Close()

	a2aResp, err := http.Post(ts.URL+"/a2a", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"memory/verify","params":{"token":"`+ga.ReceiptToken+`"}}`))
	if err != nil || a2aResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /a2a failed: %+v %v", a2aResp, err)
	}
	a2aBody, _ := io.ReadAll(a2aResp.Body)
	a2aResp.Body.Close()
	if !strings.Contains(string(a2aBody), `"verdict":"green"`) {
		t.Fatalf("unexpected A2A verify response: %s", string(a2aBody))
	}

	// 7. Embedded Hub mounted under /hub/v1/health!
	hResp, err := http.Get(ts.URL + "/hub/v1/health")
	if err != nil || hResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /hub/v1/health failed: %+v %v", hResp, err)
	}
	hResp.Body.Close()

	// 8. Mounted Restate Go SDK endpoint discovery (/restate/discover) exposes all 5 services!
	rsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/restate/discover", nil)
	rsReq.Header.Set("Accept", "application/vnd.restate.endpointmanifest.v3+json")
	rsResp, err := http.DefaultClient.Do(rsReq)
	if err != nil || rsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /restate/discover failed: %+v %v", rsResp, err)
	}
	rsBody, _ := io.ReadAll(rsResp.Body)
	rsResp.Body.Close()
	for _, svc := range []string{"BuyerDeal", "SellerDeal", "Custody", "Tx", "AnchorCadence"} {
		if !strings.Contains(string(rsBody), `"`+svc+`"`) {
			t.Fatalf("/restate/discover missing %s: %s", svc, string(rsBody))
		}
	}
}
