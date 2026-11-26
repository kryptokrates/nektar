package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestDockDetectProbeWalletQRAndHubBundle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(":memory:")
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
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	app, err := NewApp(mem, mkt, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	// 1. GET /v1/dock/detect
	resp, err := http.Get(ts.URL + "/v1/dock/detect")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("dock detect failed: resp=%v err=%v", resp, err)
	}
	var det struct {
		Clients      []DockClientStatus `json:"clients"`
		SamplePrompt string             `json:"samplePrompt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&det); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(det.Clients) < 10 || det.SamplePrompt == "" {
		t.Fatalf("expected >=10 dock clients & sample prompt, got %d", len(det.Clients))
	}

	// 2. POST /v1/dock/probe
	prResp, err := http.Post(ts.URL+"/v1/dock/probe", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil || prResp.StatusCode != http.StatusOK {
		t.Fatalf("dock probe failed: resp=%v err=%v", prResp, err)
	}
	var pr map[string]any
	_ = json.NewDecoder(prResp.Body).Decode(&pr)
	prResp.Body.Close()
	if pr["ok"] != true {
		t.Fatalf("expected ok=true from dock probe, got %v", pr)
	}

	// 3. GET /v1/wallet/qr (both SVG and JSON modes)
	qrResp, err := http.Get(ts.URL + "/v1/wallet/qr?mode=uri")
	if err != nil || qrResp.StatusCode != http.StatusOK {
		t.Fatalf("wallet qr failed: resp=%v err=%v", qrResp, err)
	}
	var svgBuf bytes.Buffer
	_, _ = svgBuf.ReadFrom(qrResp.Body)
	qrResp.Body.Close()
	if !strings.HasPrefix(svgBuf.String(), "<svg ") {
		t.Fatalf("expected SVG from /v1/wallet/qr, got %q", svgBuf.String()[:min(60, svgBuf.Len())])
	}

	// 4. GET /v1/onboard/hub-bundle with Mainnet + role + feePreset
	bunResp, err := http.Get(ts.URL + "/v1/onboard/hub-bundle?mode=auto-ip&network=main&role=leaf&feePreset=hybrid")
	if err != nil || bunResp.StatusCode != http.StatusOK {
		t.Fatalf("hub bundle failed: resp=%v err=%v", bunResp, err)
	}
	var bun map[string]any
	_ = json.NewDecoder(bunResp.Body).Decode(&bun)
	bunResp.Body.Close()
	cliCmd, _ := bun["cliCommand"].(string)
	if !strings.Contains(cliCmd, "--confirm I_UNDERSTAND") || !strings.Contains(cliCmd, "--fee-preset hybrid") {
		t.Fatalf("expected mainnet confirm & fee-preset in cliCommand, got %q", cliCmd)
	}

	// 5. dockMergeJSON roundtrip (install + uninstall without clobbering sibling servers)
	cfgPath := filepath.Join(t.TempDir(), "mcp.json")
	_ = os.WriteFile(cfgPath, []byte(`{"mcpServers":{"other":{"command":"foo"}}}`), 0o600)
	changed, err := dockMergeJSON(cfgPath, "mcpServers", "nektar", map[string]any{"command": "nektar", "args": []string{"mcp"}}, false)
	if err != nil || !changed || !isJSONEntryDocked(cfgPath, "mcpServers", "nektar") {
		t.Fatalf("expected dockMergeJSON install to succeed, changed=%v err=%v", changed, err)
	}
	changed, err = dockMergeJSON(cfgPath, "mcpServers", "nektar", nil, true)
	if err != nil || !changed || isJSONEntryDocked(cfgPath, "mcpServers", "nektar") {
		t.Fatalf("expected dockMergeJSON uninstall to succeed, changed=%v err=%v", changed, err)
	}
	rawAfter, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(rawAfter), `"other"`) {
		t.Fatalf("expected sibling MCP entry to be preserved, got %s", string(rawAfter))
	}
}
