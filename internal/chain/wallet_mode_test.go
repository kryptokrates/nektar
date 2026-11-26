package chain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func startMockDesktopWallet(t *testing.T, network string) (*httptest.Server, string) {
	t.Helper()
	eng, err := nwallet.Open(nwallet.Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Mainnet:       network == "mainnet",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	idHex := eng.IdentityPubHex()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/getNetwork":
			_ = json.NewEncoder(w).Encode(map[string]any{"network": network})
		case "/getPublicKey":
			_ = json.NewEncoder(w).Encode(map[string]any{"publicKey": idHex})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, idHex
}

func TestWalletModeAutoStickyAndFallback(t *testing.T) {
	ctx := context.Background()

	// 1. Fresh dir + live BSV Desktop on matching network -> auto picks "client" and creates ZERO wallet/ dir.
	desktopSrv, wantID := startMockDesktopWallet(t, "testnet")
	clientDir := t.TempDir()
	p, err := Open(ctx, Config{
		Network:       "testnet",
		DataDir:       clientDir,
		WalletMode:    "auto",
		WalletWireURL: desktopSrv.URL,
	})
	if err != nil {
		t.Fatalf("Open(auto with desktop): %v", err)
	}
	if p.WalletMode() != "client" {
		t.Fatalf("expected WalletMode=client, got %q", p.WalletMode())
	}
	if p.IdentityPubHex() != wantID {
		t.Fatalf("expected identityPub=%s, got %s", wantID, p.IdentityPubHex())
	}
	if _, statErr := os.Stat(filepath.Join(clientDir, "wallet")); !os.IsNotExist(statErr) {
		t.Fatalf("expected zero wallet/ directory in client mode, stat err=%v", statErr)
	}
	_ = p.Close()

	// 2. Reopen the SAME sticky client dir when BSV Desktop is offline -> MUST fail closed (never silently create embedded wallet!).
	desktopSrv.Close()
	if p2, err := Open(ctx, Config{
		Network:       "testnet",
		DataDir:       clientDir,
		WalletMode:    "auto",
		WalletWireURL: desktopSrv.URL,
	}); err == nil {
		_ = p2.Close()
		t.Fatalf("expected sticky client dir to fail closed when BSV Desktop is offline, but opened with mode=%s", p2.WalletMode())
	}
	if _, statErr := os.Stat(filepath.Join(clientDir, "wallet")); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed sticky client reopen NOT to create wallet/ dir")
	}

	// 3. Fresh dir + BSV Desktop offline -> auto falls back cleanly to "embedded" and persists sticky marker.
	embDir := t.TempDir()
	p3, err := Open(ctx, Config{
		Network:       "testnet",
		DataDir:       embDir,
		WalletMode:    "auto",
		WalletWireURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Open(auto offline fallback): %v", err)
	}
	if p3.WalletMode() != "embedded" {
		t.Fatalf("expected WalletMode=embedded, got %q", p3.WalletMode())
	}
	embID := p3.IdentityPubHex()
	_ = p3.Close()

	// 4. Reopen sticky embedded dir when a live BSV Desktop IS now running -> MUST stay "embedded" (never flip identity keys!).
	desktopSrv2, _ := startMockDesktopWallet(t, "testnet")
	p4, err := Open(ctx, Config{
		Network:       "testnet",
		DataDir:       embDir,
		WalletMode:    "auto",
		WalletWireURL: desktopSrv2.URL,
	})
	if err != nil {
		t.Fatalf("Open(auto sticky embedded): %v", err)
	}
	if p4.WalletMode() != "embedded" || p4.IdentityPubHex() != embID {
		t.Fatalf("expected sticky embedded identity %s, got mode=%s id=%s", embID, p4.WalletMode(), p4.IdentityPubHex())
	}
	_ = p4.Close()

	// 5. Legacy pre-upgrade embedded dir (has wallet/wallet.sqlite, no wallet-mode.json) + live BSV Desktop -> stays "embedded"!
	legacyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacyDir, "wallet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "wallet", "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	mode, sticky, err := resolveWalletMode(ctx, legacyDir, "testnet", "auto", desktopSrv2.URL)
	if err != nil || mode != "embedded" || !sticky {
		t.Fatalf("legacy embedded dir resolution = (%q, %v, %v), want (embedded, true, nil)", mode, sticky, err)
	}
}
