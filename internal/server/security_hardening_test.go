package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// TestHardeningOAuthOperatorBoundaryAndMCPBatchPaywall verifies F-16 and F-17.
func TestHardeningOAuthOperatorBoundaryAndMCPBatchPaywall(t *testing.T) {
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
	app, err := NewApp(mem, mkt, Config{
		APIToken:   "nkt_admin_secret_123",
		ToolPrices: map[string]uint64{"groundedAnswer": 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Register a real OAuth access token (nkt_at_...) hashed under oauthSecretKey (F-34).
	oauthTok := "nkt_at_agent_oauth_token_xyz"
	app.oauthMu.Lock()
	app.oauthTokens[oauthSecretKey(oauthTok)] = oauthAccessToken{
		ClientID:  "agent-client",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	app.oauthMu.Unlock()

	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// F-16: Even when OperatorToken == "" (defaulting to APIToken), an OAuth nkt_at_* token
	// MUST be rejected with 403 Forbidden on operator-only endpoints like /v1/wallet/sweep!
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/wallet/sweep", strings.NewReader(`{"toAddress":"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}`))
	req.Header.Set("Authorization", "Bearer "+oauthTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/wallet/sweep: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("F-16 REGRESSION: OAuth token nkt_at_* was not blocked on /v1/wallet/sweep (status=%d)", resp.StatusCode)
	}

	// F-17: JSON-RPC batch containing a priced MCP tool call MUST NOT bypass the 402 paywall!
	batchBody := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"groundedAnswer","arguments":{}}}]`
	mReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(batchBody))
	mReq.Header.Set("Authorization", "Bearer nkt_admin_secret_123")
	mReq.Header.Set("Content-Type", "application/json")
	mReq.Header.Set("Accept", "application/json, text/event-stream")
	mResp, err := http.DefaultClient.Do(mReq)
	if err != nil {
		t.Fatalf("POST /mcp batch: %v", err)
	}
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusPaymentRequired && mResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("F-17 REGRESSION: JSON-RPC batch bypassed /mcp paywall (status=%d)", mResp.StatusCode)
	}
}
