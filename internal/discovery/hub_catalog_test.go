package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestDefaultSeedHostsIncludesFullFleet(t *testing.T) {
	seeds := DefaultSeedHosts("testnet")
	if len(seeds) < 6 {
		t.Fatalf("expected >=6 default testnet fleet hubs, got %d: %v", len(seeds), seeds)
	}
	wantTestnetFirst := "https://testnet.nektar.run/hub"
	if seeds[0] != wantTestnetFirst {
		t.Fatalf("expected primary testnet spine %q first, got %q", wantTestnetFirst, seeds[0])
	}
	wantMainFirst := "https://nektar.run/hub"
	if mainSeeds := DefaultSeedHosts("main"); len(mainSeeds) != 1 || mainSeeds[0] != wantMainFirst {
		t.Fatalf("unexpected mainnet seeds: %v", mainSeeds)
	}
}

func TestProbeHubCatalogAndConnectAllReachable(t *testing.T) {
	// 1. Healthy matching-network Spine hub
	spineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "network": "memory"})
		case "/hub/v1/terms":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"network": "memory",
				"hubRole": "spine",
				"fees": map[string]uint64{
					"submit:want":  5,
					"submit:offer": 2,
					"bbox:send":    2,
				},
				"egress": map[string]uint64{
					"satsPer100Events": 10,
					"minTopupSats":     20,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer spineSrv.Close()

	// 2. Healthy matching-network Leaf hub
	leafSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "network": "memory"})
		case "/hub/v1/terms":
			_ = json.NewEncoder(w).Encode(map[string]any{"network": "memory", "hubRole": "leaf", "feePreset": "free"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer leafSrv.Close()

	// 3. Mismatched-network hub (must NOT be auto-connected!)
	wrongNetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "network": "main"})
		case "/hub/v1/terms":
			_ = json.NewEncoder(w).Encode(map[string]any{"network": "main", "hubRole": "leaf", "feePreset": "hybrid"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer wrongNetSrv.Close()

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	port, err := chain.Open(context.Background(), chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, nil, nil, memory.Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}

	// Seed spine as envHost, leaf and wrongNet as blocked/known candidates so ConnectAllReachable tests filtering.
	svc := New(mem, []string{spineSrv.URL + "/hub"}, spineSrv.Client())
	_ = db.BlockHost(leafSrv.URL + "/hub")
	_ = db.BlockHost(wrongNetSrv.URL + "/hub")

	catalog := svc.ProbeHubCatalog(context.Background(), true)
	if len(catalog) != 3 {
		t.Fatalf("expected 3 catalog hubs, got %d: %+v", len(catalog), catalog)
	}
	// Spine must sort first as connected + reachable + spine with parsed exact satoshi fees.
	if catalog[0].Role != "spine" || !catalog[0].Reachable || !catalog[0].Connected {
		t.Fatalf("expected healthy connected spine first, got %+v", catalog[0])
	}
	if catalog[0].Fees == nil || catalog[0].Fees.WantSats != 5 || catalog[0].Fees.OfferSats != 2 || catalog[0].Fees.EgressSatsPer100 != 10 {
		t.Fatalf("expected parsed exact satoshi fees (5/2/10), got %+v", catalog[0].Fees)
	}
	if catalog[0].FeePreset != "hybrid" {
		t.Fatalf("expected inferred hybrid fee preset, got %q", catalog[0].FeePreset)
	}

	connected, skipped, updated := svc.ConnectAllReachable(context.Background())
	if len(connected) != 2 {
		t.Fatalf("expected 2 matching-network hubs connected (spine+leaf), got connected=%v skipped=%v", connected, skipped)
	}
	if len(skipped) != 1 || skipped[0] != wrongNetSrv.URL+"/hub" {
		t.Fatalf("expected wrong-network hub skipped, got %v", skipped)
	}
	mismatchFound := false
	for _, h := range updated {
		if h.URL == wrongNetSrv.URL+"/hub" && h.NetworkMismatch && !h.Connected {
			mismatchFound = true
		}
	}
	if !mismatchFound {
		t.Fatalf("expected wrong-network hub marked NetworkMismatch & disconnected, got %+v", updated)
	}
}
