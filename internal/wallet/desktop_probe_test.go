package wallet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/wirewallet"
)

func TestNormalizeDesktopOrigin(t *testing.T) {
	cases := []struct {
		in       string
		wantOrig string
		wantHost string
	}{
		{"", "https://nektar.run", "nektar.run"},
		{"nektar", "https://nektar.run", "nektar.run"},
		{"nektar.run", "https://nektar.run", "nektar.run"},
		{"https://nektar.run/dashboard", "https://nektar.run", "nektar.run"},
		{"http://localhost:18420", "http://localhost:18420", "localhost:18420"},
		{"localhost:18420", "http://localhost:18420", "localhost:18420"},
		{"127.0.0.1:18420", "http://127.0.0.1:18420", "127.0.0.1:18420"},
	}
	for _, tc := range cases {
		o, h := NormalizeDesktopOrigin(tc.in)
		if o != tc.wantOrig || h != tc.wantHost {
			t.Errorf("NormalizeDesktopOrigin(%q) = (%q, %q), want (%q, %q)", tc.in, o, h, tc.wantOrig, tc.wantHost)
		}
	}
}

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
	if live.Transport != string(DesktopTransportBinary) {
		t.Fatalf("expected transport=%q, got %q", DesktopTransportBinary, live.Transport)
	}
	if live.Network != "testnet" {
		t.Fatalf("expected network=testnet, got %q", live.Network)
	}
	if live.IdentityPub != eng.IdentityPubHex() {
		t.Fatalf("expected identityPub=%s, got %s", eng.IdentityPubHex(), live.IdentityPub)
	}
}

func TestDialDesktopWalletHTTPJSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	eng, err := Open(Config{
		Dir:           t.TempDir(),
		InMemoryStore: true,
		Mainnet:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Mock BSV Desktop HTTP-JSON server on loopback enforcing strict Origin URL + TS wallet-toolbox JSON shapes.
	var seenOrigins []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orig := r.Header.Get("Origin")
		if orig == "" || (!bytes.HasPrefix([]byte(orig), []byte("http://")) && !bytes.HasPrefix([]byte(orig), []byte("https://"))) {
			http.Error(w, "invalid Origin header", http.StatusBadRequest)
			return
		}
		seenOrigins = append(seenOrigins, orig)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/getNetwork":
			_ = json.NewEncoder(w).Encode(map[string]any{"network": "testnet"})
		case "/getPublicKey":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if id, _ := req["identityKey"].(bool); id {
				_ = json.NewEncoder(w).Encode(map[string]any{"publicKey": eng.IdentityPubHex()})
				return
			}
			// Verify protocolID serialized as JSON array [level, protocol], NOT struct object!
			if _, ok := req["protocolID"].([]any); !ok {
				http.Error(w, "protocolID must be JSON array [level, protocol]", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"publicKey": eng.IdentityPubHex()})
		case "/encrypt":
			var args sdkwallet.EncryptArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			res, err := eng.Encrypt(r.Context(), args, DefaultDesktopOriginator)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(res)
		case "/decrypt":
			var args sdkwallet.DecryptArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			res, err := eng.Decrypt(r.Context(), args, DefaultDesktopOriginator)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(res)
		case "/listOutputs":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if _, hasEmptyInclude := req["include"]; hasEmptyInclude {
				if s, _ := req["include"].(string); s == "" {
					http.Error(w, "empty include not allowed", http.StatusBadRequest)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"totalOutputs": 0, "outputs": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 1. Probe auto-detects HTTP-JSON transport.
	st := ProbeDesktopWallet(ctx, srv.URL)
	if !st.Available || st.Transport != string(DesktopTransportHTTPJSON) || st.Network != "testnet" || st.IdentityPub != eng.IdentityPubHex() {
		t.Fatalf("unexpected HTTP-JSON desktop probe: %+v", st)
	}

	// 2. DialDesktopWallet returns DesktopJSONClient & round-trips GetPublicKey, Encrypt/Decrypt, ListOutputs.
	cli, err := DialDesktopWallet(ctx, "nektar", srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("DialDesktopWallet: %v", err)
	}
	if _, ok := cli.(*DesktopJSONClient); !ok {
		t.Fatalf("expected *DesktopJSONClient, got %T", cli)
	}

	pub, err := cli.GetPublicKey(ctx, sdkwallet.GetPublicKeyArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar fund"},
			KeyID:        "nektar-fund",
			Counterparty: sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf},
		},
	}, "nektar")
	if err != nil || pub == nil || pub.PublicKey == nil {
		t.Fatalf("GetPublicKey derived: pub=%+v err=%v", pub, err)
	}

	enc, err := cli.Encrypt(ctx, sdkwallet.EncryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar storage"},
			KeyID:        "k1",
			Counterparty: sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf},
		},
		Plaintext: []byte("hello bsv desktop"),
	}, "nektar")
	if err != nil || len(enc.Ciphertext) == 0 {
		t.Fatalf("Encrypt: %+v err=%v", enc, err)
	}

	dec, err := cli.Decrypt(ctx, sdkwallet.DecryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar storage"},
			KeyID:        "k1",
			Counterparty: sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf},
		},
		Ciphertext: enc.Ciphertext,
	}, "nektar")
	if err != nil || string(dec.Plaintext) != "hello bsv desktop" {
		t.Fatalf("Decrypt: %+v err=%v", dec, err)
	}

	lim := uint32(10)
	lo, err := cli.ListOutputs(ctx, sdkwallet.ListOutputsArgs{Basket: "nektar funding", Limit: &lim}, "nektar")
	if err != nil || lo.TotalOutputs != 0 {
		t.Fatalf("ListOutputs: %+v err=%v", lo, err)
	}

	for _, o := range seenOrigins {
		if o != "https://nektar.run" {
			t.Fatalf("expected normalized Origin https://nektar.run, got %q", o)
		}
	}
}

func TestUniformNektarActionNaming(t *testing.T) {
	if got := FormatActionDescription("Anchor shard", `"desktop-demo" (batch #0, 1 fragment)`); got != `Nektar · Anchor shard: "desktop-demo" (batch #0, 1 fragment)` {
		t.Fatalf("FormatActionDescription = %q", got)
	}
	cases := map[string]string{
		"":                         "Nektar · Transaction",
		"nektar funded tx":         "Nektar · Transaction",
		"nektar hub fee advertise": "Nektar · Hub fee advertise",
		"market purchase off_123":  "Nektar · Market purchase off_123",
		"Nektar · Anchor shard":    "Nektar · Anchor shard",
	}
	for in, want := range cases {
		if got := NormalizeActionDescription(in); got != want {
			t.Errorf("NormalizeActionDescription(%q) = %q, want %q", in, got, want)
		}
	}
	labels := NormalizeActionLabels("Anchor", "nektar", "anchor", "  Market ")
	wantLabels := []string{"nektar", "anchor", "market"}
	if len(labels) != len(wantLabels) {
		t.Fatalf("NormalizeActionLabels = %v, want %v", labels, wantLabels)
	}
	for i := range wantLabels {
		if labels[i] != wantLabels[i] {
			t.Errorf("label[%d] = %q, want %q", i, labels[i], wantLabels[i])
		}
	}
}
