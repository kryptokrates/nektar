package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestServerSecurityOAuthProxyAdaptersMetricsAndBadge(t *testing.T) {
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
	hubNode, err := hub.NewNode(hub.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()

	// Seed a fragment so L0 proxy injects it.
	r1, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "ops", Text: "Chronicle hardfork activated smoothly at height 943816.", AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ga, err := mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:  "Chronicle hardfork activated smoothly at height 943816",
			Cites: []reasoning.FragmentRef{{Shard: "ops", Seq: r1.Seq}},
		}},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var capturedAuth, capturedAnthropicBody string
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/v1/messages") {
			capturedAnthropicBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"answer"}]}`))
			return
		}
		if strings.Contains(string(raw), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
			if fl != nil {
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer mockUpstream.Close()

	app, err := NewApp(mem, mkt, Config{
		APIToken:          "agent-secret-tok",
		OperatorToken:     "op-secret-tok",
		ToolProfile:       "agent",
		OpenAIUpstream:    mockUpstream.URL,
		AnthropicUpstream: mockUpstream.URL,
		Hub:               hubNode,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. Auth gates on /v1/status, /v1/wallet, and POST /a2a; /healthz remains public.
	if resp, err := http.Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /healthz expected 200, got %+v %v", resp, err)
	}
	for _, p := range []string{"/v1/status", "/v1/wallet"} {
		resp, _ := http.Get(ts.URL + p)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s without auth expected 401, got %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
	a2aUnauth, _ := http.Post(ts.URL+"/a2a", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"memory/recall"}`))
	if a2aUnauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /a2a without auth expected 401, got %d", a2aUnauth.StatusCode)
	}
	a2aUnauth.Body.Close()

	// 2. Agent ToolProfile must block operator-only tools like borgGroup.
	reqTool, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/tools/borgGroup", strings.NewReader(`{"action":"list"}`))
	reqTool.Header.Set("Authorization", "Bearer agent-secret-tok")
	respTool, _ := http.DefaultClient.Do(reqTool)
	if respTool.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected agent profile to block borgGroup, got %d", respTool.StatusCode)
	}
	respTool.Body.Close()

	// 3. L0 Proxy composite key `nkt:<nektarTok>:<providerKey>` + SSE streaming + Anthropic array system.
	reqProxy, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/proxy/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"Chronicle height?"}]}`))
	reqProxy.Header.Set("Content-Type", "application/json")
	reqProxy.Header.Set("Authorization", "Bearer nkt:agent-secret-tok:sk-real-openai")
	respProxy, err := http.DefaultClient.Do(reqProxy)
	if err != nil || respProxy.StatusCode != 200 {
		t.Fatalf("L0 OpenAI proxy failed: %+v %v", respProxy, err)
	}
	bodyProxy, _ := io.ReadAll(respProxy.Body)
	respProxy.Body.Close()
	if capturedAuth != "Bearer sk-real-openai" || !strings.Contains(string(bodyProxy), "[DONE]") {
		t.Fatalf("unexpected proxy auth=%q body=%q", capturedAuth, string(bodyProxy))
	}

	reqAnth, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/proxy/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-3-7-sonnet","system":[{"type":"text","text":"Be concise."}],"messages":[{"role":"user","content":"Chronicle activation?"}]}`))
	reqAnth.Header.Set("Content-Type", "application/json")
	reqAnth.Header.Set("X-Nektar-Token", "agent-secret-tok")
	reqAnth.Header.Set("x-api-key", "sk-ant-real")
	respAnth, err := http.DefaultClient.Do(reqAnth)
	if err != nil || respAnth.StatusCode != 200 {
		t.Fatalf("L0 Anthropic proxy failed: %+v %v", respAnth, err)
	}
	respAnth.Body.Close()
	if !strings.Contains(capturedAnthropicBody, "943816") || !strings.Contains(capturedAnthropicBody, "Be concise.") {
		t.Fatalf("expected Anthropic array system to preserve prompt and inject recall, got: %s", capturedAnthropicBody)
	}

	// 4. MCP OAuth 2.1 PKCE end-to-end flow!
	regResp, err := http.Post(ts.URL+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"test-client","redirect_uris":["http://127.0.0.1/cb"]}`))
	if err != nil || regResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /oauth/register failed: %+v %v", regResp, err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&reg)
	regResp.Body.Close()

	verifier := "super-secret-pkce-verifier-string-1234567890"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Direct POST without CSRF cookie/nonce must be rejected (403 Forbidden)!
	badAuthResp, err := noRedirect.PostForm(ts.URL+"/oauth/authorize", url.Values{
		"client_id":             {reg.ClientID},
		"redirect_uri":          {"http://127.0.0.1/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st1"},
		"token":                 {"agent-secret-tok"},
	})
	if err != nil || badAuthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /oauth/authorize without CSRF expected 403, got %+v %v", badAuthResp, err)
	}
	badAuthResp.Body.Close()

	// Real browser flow: GET /oauth/authorize -> capture CSRF cookie + hidden csrf_token -> POST /oauth/authorize!
	getURL := fmt.Sprintf("%s/oauth/authorize?client_id=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=st1",
		ts.URL, url.QueryEscape(reg.ClientID), url.QueryEscape("http://127.0.0.1/cb"), url.QueryEscape(challenge))
	getResp, err := http.Get(getURL)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /oauth/authorize failed: %+v %v", getResp, err)
	}
	var csrfCookie *http.Cookie
	for _, c := range getResp.Cookies() {
		if c.Name == "nektar_oauth_csrf" {
			csrfCookie = c
		}
	}
	getResp.Body.Close()
	if csrfCookie == nil || csrfCookie.Value == "" {
		t.Fatal("expected nektar_oauth_csrf cookie on GET /oauth/authorize")
	}

	form := url.Values{
		"csrf_token":            {csrfCookie.Value},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {"http://127.0.0.1/cb"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st1"},
		"token":                 {"agent-secret-tok"},
	}
	authReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	authReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authReq.AddCookie(csrfCookie)
	authResp, err := noRedirect.Do(authReq)
	if err != nil || authResp.StatusCode != http.StatusFound {
		t.Fatalf("POST /oauth/authorize expected 302, got %+v %v", authResp, err)
	}
	loc, _ := url.Parse(authResp.Header.Get("Location"))
	code := loc.Query().Get("code")
	authResp.Body.Close()
	if code == "" {
		t.Fatal("expected authorization code in redirect Location")
	}

	tokResp, err := http.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {reg.ClientID},
		"redirect_uri":  {"http://127.0.0.1/cb"},
		"code_verifier": {verifier},
	})
	if err != nil || tokResp.StatusCode != 200 {
		t.Fatalf("POST /oauth/token failed: %+v %v", tokResp, err)
	}
	var tokOut struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokOut)
	tokResp.Body.Close()
	if !strings.HasPrefix(tokOut.AccessToken, "nkt_at_") {
		t.Fatalf("expected nkt_at_ access token, got %+v", tokOut)
	}
	// Use OAuth access token to call protected /v1/status!
	stReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	stReq.Header.Set("Authorization", "Bearer "+tokOut.AccessToken)
	stResp, err := http.DefaultClient.Do(stReq)
	if err != nil || stResp.StatusCode != 200 {
		t.Fatalf("GET /v1/status with OAuth token failed: %+v %v", stResp, err)
	}
	stResp.Body.Close()

	// 5. OpenAPI, Adapters, Prometheus /v1/metrics & SVG /v1/badge/{token}!
	for _, ep := range []string{"/v1/openapi.json", "/v1/adapters/manifest", "/v1/metrics"} {
		r, err := http.Get(ts.URL + ep)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("GET %s failed: %+v %v", ep, r, err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if ep == "/v1/metrics" && !strings.Contains(string(b), "nektar_uptime_seconds") {
			t.Fatalf("expected Prometheus metrics, got: %s", string(b))
		}
	}
	badgeResp, err := http.Get(ts.URL + "/v1/badge/" + ga.ReceiptToken)
	if err != nil || badgeResp.StatusCode != 200 || !strings.Contains(badgeResp.Header.Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("GET /v1/badge failed: %+v %v", badgeResp, err)
	}
	badgeBytes, _ := io.ReadAll(badgeResp.Body)
	badgeResp.Body.Close()
	if !strings.Contains(string(badgeBytes), "verified") {
		t.Fatalf("expected verified badge SVG, got: %s", string(badgeBytes))
	}

	// 6. Pair code minting requires OperatorToken -> /pair?code= sets session cookie!
	pcReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/auth/pair-code", bytes.NewReader(nil))
	pcReq.Header.Set("Authorization", "Bearer op-secret-tok")
	pcResp, err := http.DefaultClient.Do(pcReq)
	if err != nil || pcResp.StatusCode != 200 {
		t.Fatalf("POST /v1/auth/pair-code failed: %+v %v", pcResp, err)
	}
	var pc struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(pcResp.Body).Decode(&pc)
	pcResp.Body.Close()
	pairResp, err := noRedirect.Get(ts.URL + "/pair?code=" + pc.Code)
	if err != nil || pairResp.StatusCode != http.StatusFound || len(pairResp.Cookies()) == 0 {
		t.Fatalf("GET /pair failed: %+v %v", pairResp, err)
	}
	pairResp.Body.Close()
}
