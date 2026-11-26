package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
