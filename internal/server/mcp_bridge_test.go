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
)

func TestDetectAnyLiveDaemonFindsMainnetAndActivePointer(t *testing.T) {
	home := t.TempDir()

	// Start a mock Mainnet health server and write ONLY ~/.nektar/main/daemon.json!
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "network": "main"})
			return
		}
		http.NotFound(w, r)
	}))
	defer mainSrv.Close()

	mainDir := filepath.Join(home, ".nektar", "main")
	if err := WriteDaemonInfo(mainDir, DaemonInfo{
		PID:       os.Getpid(),
		URL:       mainSrv.URL,
		Network:   "main",
		DataDir:   mainDir,
		AuthToken: "main-secret",
	}); err != nil {
		t.Fatal(err)
	}

	// Zero-flag discovery (preferredDir="") MUST find the live Mainnet daemon instead of falling back to empty testnet!
	found, ok := DetectAnyLiveDaemonInHome(home, "")
	if !ok || found == nil {
		t.Fatal("expected DetectAnyLiveDaemonInHome to discover ~/.nektar/main/daemon.json")
	}
	if found.Network != "main" || found.URL != mainSrv.URL || found.AuthToken != "main-secret" {
		t.Fatalf("unexpected detected daemon: %+v", found)
	}

	// Now start a second Testnet server and point active-daemon.json at it — active pointer should take precedence!
	testSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "network": "testnet"})
			return
		}
		http.NotFound(w, r)
	}))
	defer testSrv.Close()

	testDir := filepath.Join(home, ".nektar")
	testInfo := DaemonInfo{
		PID:       os.Getpid(),
		URL:       testSrv.URL,
		Network:   "testnet",
		DataDir:   testDir,
		AuthToken: "test-secret",
	}
	if err := WriteDaemonInfo(testDir, testInfo); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveDaemonPointer(home, testDir, testInfo); err != nil {
		t.Fatal(err)
	}

	active, ok := DetectAnyLiveDaemonInHome(home, "")
	if !ok || active == nil || active.Network != "testnet" || active.URL != testSrv.URL {
		t.Fatalf("expected active-daemon.json to select latest active testnet daemon, got %+v ok=%v", active, ok)
	}

	// Explicit preferredDir (mainDir) still wins over active-daemon.json!
	explicitMain, ok := DetectAnyLiveDaemonInHome(home, mainDir)
	if !ok || explicitMain == nil || explicitMain.Network != "main" {
		t.Fatalf("expected explicit preferredDir to select main daemon, got %+v ok=%v", explicitMain, ok)
	}

	// If active-daemon.json points to a dead server, fall through cleanly to live Mainnet daemon!
	testSrv.Close()
	fallbackMain, ok := DetectAnyLiveDaemonInHome(home, "")
	if !ok || fallbackMain == nil || fallbackMain.Network != "main" {
		t.Fatalf("expected fallback to live main daemon when active pointer is dead, got %+v ok=%v", fallbackMain, ok)
	}
}

func TestRunStdioBridgeErrorWrappingAndDaemonRestartRecovery(t *testing.T) {
	// 1. Non-2xx (401) and non-JSON-RPC 200 HTML MUST be wrapped into JSON-RPC errors preserving request id.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad-tok" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "hint": "run nektar open"})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not json-rpc</html>"))
	}))
	defer badSrv.Close()

	var out401 bytes.Buffer
	in401 := strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"tools/list"}` + "\n")
	if err := RunStdioBridge(context.Background(), badSrv.URL, "bad-tok", in401, &out401); err != nil {
		t.Fatalf("RunStdioBridge 401: %v", err)
	}
	var rpc401 struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      int            `json:"id"`
		Error   map[string]any `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out401.Bytes()), &rpc401); err != nil {
		t.Fatalf("expected valid JSON-RPC error on 401, got %q (%v)", out401.String(), err)
	}
	if rpc401.JSONRPC != "2.0" || rpc401.ID != 42 || rpc401.Error == nil {
		t.Fatalf("unexpected wrapped 401 frame: %+v", rpc401)
	}

	var outHTML bytes.Buffer
	inHTML := strings.NewReader(`{"jsonrpc":"2.0","id":"req-html","method":"tools/list"}` + "\n")
	if err := RunStdioBridge(context.Background(), badSrv.URL, "good-tok", inHTML, &outHTML); err != nil {
		t.Fatalf("RunStdioBridge HTML: %v", err)
	}
	var rpcHTML struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      string         `json:"id"`
		Error   map[string]any `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(outHTML.Bytes()), &rpcHTML); err != nil {
		t.Fatalf("expected valid JSON-RPC error on non-JSON-RPC 200 HTML, got %q (%v)", outHTML.String(), err)
	}
	if rpcHTML.ID != "req-html" || rpcHTML.Error == nil {
		t.Fatalf("unexpected wrapped HTML frame: %+v", rpcHTML)
	}

	// 2. Connection refused triggers one DetectAnyLiveDaemon re-discovery and succeeds against restarted daemon!
	restartedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rotated-tok" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": 99, "result": map[string]any{"ok": true}})
	}))
	defer restartedSrv.Close()

	deadSrv := httptest.NewServer(http.NotFoundHandler())
	deadURL := deadSrv.URL
	deadSrv.Close()

	origDetect := detectLiveDaemonFn
	detectCalls := 0
	detectLiveDaemonFn = func(string) (*DaemonInfo, bool) {
		detectCalls++
		return &DaemonInfo{URL: restartedSrv.URL, AuthToken: "rotated-tok"}, true
	}
	defer func() { detectLiveDaemonFn = origDetect }()

	var outRecov bytes.Buffer
	inRecov := strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"ping"}` + "\n")
	if err := RunStdioBridge(context.Background(), deadURL, "stale-tok", inRecov, &outRecov); err != nil {
		t.Fatalf("RunStdioBridge recovery: %v", err)
	}
	if detectCalls != 1 {
		t.Fatalf("expected exactly 1 daemon re-discovery call on connection refused, got %d", detectCalls)
	}
	if !strings.Contains(outRecov.String(), `"ok":true`) || !strings.Contains(outRecov.String(), `"id":99`) {
		t.Fatalf("expected recovered JSON-RPC result after daemon restart, got %q", outRecov.String())
	}
}
