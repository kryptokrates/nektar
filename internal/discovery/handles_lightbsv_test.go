package discovery_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/resolve"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func newDiscoveryHarness(t *testing.T, hc *http.Client) (*memory.Service, *discovery.Service) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(dir + "/nektar.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return mem, discovery.New(mem, nil, hc)
}

func TestTier1WellKnownHandleAndTOFUPinFailClosed(t *testing.T) {
	ctx := context.Background()

	// Spin up an embedded Hub serving BRC-180 /manifest.json + BRC-169 /.well-known/metanet-handles/resolve.
	hubNode, err := hub.NewNode(hub.Config{
		Network:      "memory",
		EnableFinger: true,
		EnableBbox:   true,
	})
	if err != nil {
		t.Fatalf("hub.NewNode: %v", err)
	}
	defer hubNode.Close()

	_, aliceDisc := newDiscoveryHarness(t, nil)
	alicePub := aliceDisc.AnnouncePubHex()
	aliceOffice := hubNode.DefaultBboxOffice()
	aliceDisc.SetLocalHandle("alice", "", aliceOffice)

	rogueSigner, err := nwallet.NewEphemeralMarketSigner()
	if err != nil {
		t.Fatal(err)
	}
	var swapRogue atomic.Bool
	baseResolver := aliceDisc.WellKnownHandleHandler("nektar.local", aliceOffice)
	hubNode.SetHandleResolver(func(w http.ResponseWriter, r *http.Request) {
		if swapRogue.Load() {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metanetHandles": "1.0",
				"handle":         r.URL.Query().Get("handle"),
				"domain":         r.Host,
				"identityKey":    rogueSigner.PubHex(),
				"messagebox":     aliceOffice,
				"ttl":            300,
				"revoked":        false,
			})
			return
		}
		baseResolver(w, r)
	})

	ts := httptest.NewServer(hubNode.Handler())
	defer ts.Close()
	u, _ := url.Parse(ts.URL)

	_, bobDisc := newDiscoveryHarness(t, ts.Client())

	// 1. First resolution pins Alice's identity key via BRC-169 well-known endpoint.
	peer, err := bobDisc.ResolveAgentCtx(ctx, "alice@"+u.Host)
	if err != nil {
		t.Fatalf("ResolveAgentCtx(alice): %v", err)
	}
	if peer.Pub != alicePub {
		t.Fatalf("expected pub %s, got %s", alicePub, peer.Pub)
	}
	if peer.BboxOffice != aliceOffice {
		t.Fatalf("expected messagebox %s, got %s", aliceOffice, peer.BboxOffice)
	}
	pin, ok := bobDisc.GetHandlePin("alice@" + u.Host)
	if !ok || pin.IdentityPubHex != alicePub {
		t.Fatalf("expected persisted TOFU pin for alice, got ok=%v pin=%+v", ok, pin)
	}

	// 2. Silent key-swap attack on the handle server MUST fail closed with ErrHandleKeyChanged!
	swapRogue.Store(true)
	if _, err := bobDisc.ResolveAgentCtx(ctx, "alice@"+u.Host); !errors.Is(err, discovery.ErrHandleKeyChanged) {
		t.Fatalf("expected ErrHandleKeyChanged on silent key swap, got %v", err)
	}

	// 3. Explicit operator ResetHandlePin permits rotation to the new key.
	if err := bobDisc.ResetHandlePin("alice@" + u.Host); err != nil {
		t.Fatalf("ResetHandlePin: %v", err)
	}
	rotated, err := bobDisc.ResolveAgentCtx(ctx, "alice@"+u.Host)
	if err != nil || rotated.Pub != rogueSigner.PubHex() {
		t.Fatalf("expected rotated key %s after ResetHandlePin, got peer=%+v err=%v", rogueSigner.PubHex(), rotated, err)
	}
}

func TestTier2SovereignDomainDocsAndManifestJoin(t *testing.T) {
	ctx := context.Background()
	signer, err := nwallet.NewEphemeralMarketSigner()
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := discovery.GenerateDomainDocs(
		"agents.example.com",
		"researcher",
		signer.PubHex(),
		"https://agents.example.com/hub",
		"nektar_abcdefghij",
	)
	if err != nil {
		t.Fatalf("GenerateDomainDocs: %v", err)
	}
	if bundle.Acct != "researcher@agents.example.com" || bundle.BdnsLookupService != "ls_bdns" {
		t.Fatalf("unexpected DomainDocsBundle: %+v", bundle)
	}
	mf, err := resolve.ParseManifest(bundle.ManifestJSON)
	if err != nil {
		t.Fatalf("resolve.ParseManifest: %v", err)
	}
	if ov, ok := mf.Overlay(bstack.LookupService); !ok || ov != "https://agents.example.com/hub" {
		t.Fatalf("expected ls_nektar overlay in manifest, got ok=%v ov=%q", ok, ov)
	}
	if mf.Handles == nil || mf.Handles.Messagebox != "nektar_abcdefghij" || mf.Trust == nil || mf.Trust.PublicKey != signer.PubHex() {
		t.Fatalf("unexpected Handles/Trust in generated manifest: %+v %+v", mf.Handles, mf.Trust)
	}

	// Verify JoinHost discovers a Hub mounted at /hub via BRC-180 /manifest.json!
	hubNode, err := hub.NewNode(hub.Config{Network: "memory", EnableBbox: true})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()
	mux := http.NewServeMux()
	var tsURL string
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bsv": "0.1",
			"metanet": map[string]any{
				"overlays": map[string]string{
					bstack.LookupService: tsURL + "/custom-overlay-hub",
				},
			},
		})
	})
	mux.Handle("/custom-overlay-hub/", http.StripPrefix("/custom-overlay-hub", hubNode.Handler()))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	tsURL = ts.URL

	_, disc := newDiscoveryHarness(t, ts.Client())
	joinedURL, _, err := disc.JoinHost(ctx, ts.URL)
	if err != nil {
		t.Fatalf("JoinHost via /manifest.json: %v", err)
	}
	if joinedURL != ts.URL+"/custom-overlay-hub" {
		t.Fatalf("expected JoinHost to resolve /custom-overlay-hub from /manifest.json, got %s", joinedURL)
	}

	// Tier 3: zero-DNS nektar://pub/<pubkey>@<host> resolution.
	u, _ := url.Parse(ts.URL)
	uri := "nektar://pub/" + signer.PubHex() + "@" + u.Host + "?office=" + hubNode.DefaultBboxOffice()
	t3Peer, err := disc.ResolveAgentCtx(ctx, uri)
	if err != nil {
		t.Fatalf("ResolveAgentCtx(Tier 3 URI): %v", err)
	}
	if t3Peer.Pub != signer.PubHex() || t3Peer.BboxOffice != hubNode.DefaultBboxOffice() {
		t.Fatalf("unexpected Tier-3 PeerInfo: %+v", t3Peer)
	}
	_ = time.Second
}
