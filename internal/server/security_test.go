package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestSecurityBoundariesAuthCSRFPaywallProxyAndLeakage(t *testing.T) {
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
	objStore := bstack.NewMemoryObjectStore()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())

	// Seed a Level-0 public fragment, a Level-10 secret fragment, and a Borg-encrypted fragment.
	rPub, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "public-shard", Text: "public level zero fact about BSV", Level: 0, AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "secret-shard", Text: "TOP-SECRET-LEVEL-10-TOKEN-XYZ", Level: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Groups().CreateGroup("borg-secret", "Borg Secret", port.IdentityPubHex()); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(ctx, memory.RememberInput{
		Shard: "borg-shard", Text: "BORG-ENCRYPTED-SECRET-ABC", BorgGroup: "borg-secret", Level: 0,
	}); err != nil {
		t.Fatal(err)
	}
	ga, err := mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:  "public level zero fact about BSV",
			Cites: []reasoning.FragmentRef{{Shard: "public-shard", Seq: rPub.Seq}},
		}},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Put one private CHIRP object and one explicitly public CHIRP object.
	privObj := []byte("private chirp blob")
	privHash := sha256.Sum256(privObj)
	_ = objStore.PutObject(privHash, privObj)
	pubObj := []byte("public chirp blob")
	pubHash := sha256.Sum256(pubObj)
	_ = objStore.PutObject(pubHash, pubObj)
	mem.AllowPublicChirpHashes(pubHash)

	// Mock LLM upstream to verify L0 proxy strips Nektar creds and never injects secret/Borg fragments.
	var upstreamAuth, upstreamBody string
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-sec","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer mockLLM.Close()

	app, err := NewApp(mem, mkt, Config{
		ListenAddr:       "127.0.0.1:18420",
		APIToken:         "agent-api-secret",
		OperatorToken:    "operator-root-secret",
		ToolProfile:      "operator",
		ToolPrices:       map[string]uint64{"remember": 100},
		OpenAIUpstream:   mockLLM.URL,
		ExposeWalletWire: true,
		RestateAdminURL:  "http://127.0.0.1:9070",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. Unauthenticated /.well-known/nektar must NOT leak private shard names!
	resp, err := http.Get(ts.URL + "/.well-known/nektar")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /.well-known/nektar: err=%v resp=%+v", err, resp)
	}
	wkBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(wkBody), "secret-shard") || strings.Contains(string(wkBody), "borg-shard") {
		t.Fatalf("/.well-known/nektar leaked private shard names: %s", string(wkBody))
	}

	// 2. Unauthenticated GET /v1/pack/{shard} must return 401!
	resp, _ = http.Get(ts.URL + "/v1/pack/secret-shard")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauth GET /v1/pack/secret-shard, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. Unauthenticated GET /v1/chirp/{hash}: private hash returns 401, public allowlisted hash returns 200!
	resp, _ = http.Get(ts.URL + "/v1/chirp/" + hex.EncodeToString(privHash[:]))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauth private CHIRP fetch, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/v1/chirp/" + hex.EncodeToString(pubHash[:]))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on allowlisted public CHIRP fetch, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. GET /card with Accept: application/json must NOT dump private operational state or restateAdminUrl!
	cardReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/card", nil)
	cardReq.Header.Set("Accept", "application/json")
	resp, _ = http.DefaultClient.Do(cardReq)
	cardBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(cardBody), "restateAdminUrl") || strings.Contains(string(cardBody), "secret-shard") || strings.Contains(string(cardBody), "awakeables") {
		t.Fatalf("/card JSON leaked private state: %s", string(cardBody))
	}

	// 5. APIToken (non-operator) must be rejected (403) on operator-only routes (pair-code, fund-desktop, wallet/wire)!
	for _, path := range []string{"/v1/auth/pair-code", "/v1/wallet/fund-desktop"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer agent-api-secret")
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil || r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for APIToken on %s, got err=%v resp=%+v", path, err, r)
		}
		r.Body.Close()
	}

	// 6. Browser origin guard: cross-site Origin, Sec-Fetch-Site: cross-site, wrong Content-Type, DNS rebinding Host!
	csrfReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/recall", strings.NewReader(`{"query":"BSV"}`))
	csrfReq.Header.Set("Authorization", "Bearer agent-api-secret")
	csrfReq.Header.Set("Content-Type", "application/json")
	csrfReq.Header.Set("Origin", "https://evil.example.com")
	resp, _ = http.DefaultClient.Do(csrfReq)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-origin POST, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sfsReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/recall", strings.NewReader(`{"query":"BSV"}`))
	sfsReq.Header.Set("Authorization", "Bearer agent-api-secret")
	sfsReq.Header.Set("Content-Type", "application/json")
	sfsReq.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = http.DefaultClient.Do(sfsReq)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on Sec-Fetch-Site: cross-site, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	ctReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/recall", strings.NewReader(`{"query":"BSV"}`))
	ctReq.Header.Set("Authorization", "Bearer agent-api-secret")
	ctReq.Header.Set("Content-Type", "text/plain")
	resp, _ = http.DefaultClient.Do(ctReq)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 on text/plain JSON mutation, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	rebindReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	rebindReq.Host = "evil-rebind.example.com:18420"
	rebindReq.Header.Set("Authorization", "Bearer operator-root-secret")
	resp, _ = http.DefaultClient.Do(rebindReq)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on DNS-rebinding Host header against loopback server, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 7. /v1/wallet/wire/ rejects proxied X-Forwarded-For even with OperatorToken!
	wireReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/wallet/wire/getPublicKey", bytes.NewReader([]byte{0x01}))
	wireReq.Header.Set("Authorization", "Bearer operator-root-secret")
	wireReq.Header.Set("Content-Type", "application/octet-stream")
	wireReq.Header.Set("X-Forwarded-For", "203.0.113.5")
	resp, _ = http.DefaultClient.Do(wireReq)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for proxied /v1/wallet/wire/ request, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 8. Paywall (402) enforced consistently across /v1/remember, /v1/tools/remember, /a2a, AND /mcp!
	payTests := []struct {
		name string
		path string
		body string
	}{
		{"rest", "/v1/remember", `{"shard":"ops","text":"paywalled"}`},
		{"tools", "/v1/tools/remember", `{"shard":"ops","text":"paywalled"}`},
		{"a2a", "/a2a", `{"jsonrpc":"2.0","id":1,"method":"memory/remember","params":{"shard":"ops","text":"paywalled"}}`},
		{"mcp", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember","arguments":{"shard":"ops","text":"paywalled"}}}`},
	}
	for _, tc := range payTests {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer operator-root-secret")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		r, err := http.DefaultClient.Do(req)
		if err != nil || r.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("paywall %s (%s): expected 402, got err=%v resp=%+v", tc.name, tc.path, err, r)
		}
		r.Body.Close()
	}

	// 9. L0 LLM proxy strips Nektar credential before forwarding upstream & blocks Level>0 and Borg secrets!
	pxReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/proxy/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"secret token borg public BSV"}]}`))
	pxReq.Header.Set("Authorization", "Bearer agent-api-secret")
	pxReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(pxReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("L0 proxy call failed: err=%v resp=%+v", err, resp)
	}
	resp.Body.Close()
	if strings.Contains(upstreamAuth, "agent-api-secret") {
		t.Fatalf("L0 proxy leaked Nektar API token to upstream Authorization header: %q", upstreamAuth)
	}
	if strings.Contains(upstreamBody, "TOP-SECRET-LEVEL-10-TOKEN-XYZ") {
		t.Fatalf("L0 proxy leaked Level-10 secret fragment to upstream LLM: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "BORG-ENCRYPTED-SECRET-ABC") {
		t.Fatalf("L0 proxy leaked Borg-encrypted fragment to upstream LLM: %s", upstreamBody)
	}

	// 10. Non-operator APIToken CANNOT escalate Principal.Level in POST /v1/recall JSON body!
	escReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/recall", strings.NewReader(`{"shard":"secret-shard","query":"SECRET","principal":{"id":"admin","level":100}}`))
	escReq.Header.Set("Authorization", "Bearer agent-api-secret")
	escReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(escReq)
	if err != nil {
		t.Fatal(err)
	}
	var recallOut map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&recallOut)
	resp.Body.Close()
	if hits, _ := recallOut["fragments"].([]any); len(hits) > 0 {
		t.Fatalf("APIToken escalated Principal.Level to read Level-10 secret fragment: %+v", hits)
	}

	// 11. GET /v1/verify/{tok} ignores untrusted X-Forwarded-Host and escapes HTML attributes!
	vReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/verify/"+ga.ReceiptToken, nil)
	vReq.Header.Set("X-Forwarded-Host", `evil.com"><script>alert(1)</script>`)
	resp, err = http.DefaultClient.Do(vReq)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /v1/verify: err=%v resp=%+v", err, resp)
	}
	vBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(vBody), "<script>alert(1)</script>") || strings.Contains(string(vBody), "evil.com") {
		t.Fatalf("/v1/verify reflected untrusted X-Forwarded-Host: %s", string(vBody))
	}
}
