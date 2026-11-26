package wallet

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/wirewallet"
)

func TestProbeDesktopWalletOfflineAndLiveWire(t *testing.T) {
	ctx := context.Background()

	// 1. Closed local port must return Available=false fast (never block).
	t0 := time.Now()
	off := ProbeDesktopWallet(ctx, "http://127.0.0.1:1")
	if off.Available {
		t.Fatalf("expected closed port to report Available=false, got %+v", off)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("expected fast closed-port probe, took %v", time.Since(t0))
	}

	// 2. Live BRC-100 binary wire endpoint must return Available=true with network & identity pubkey!
	eng, err := Open(Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Mainnet:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	ts := httptest.NewServer(wirewallet.Serve(eng))
	defer ts.Close()

	live := ProbeDesktopWallet(ctx, ts.URL)
	if !live.Available {
		t.Fatalf("expected live BRC-100 wirewallet to report Available=true, got %+v", live)
	}
	if live.Network != "testnet" {
		t.Fatalf("expected network=testnet, got %q", live.Network)
	}
	if live.IdentityPub != eng.IdentityPubHex() {
		t.Fatalf("expected identityPub=%s, got %s", eng.IdentityPubHex(), live.IdentityPub)
	}
}
