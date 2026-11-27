package discovery

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func newNodeMem(t *testing.T) *memory.Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return mem
}

func TestThreeLayerDiscoveryFederationAndRemoteChirpSeed(t *testing.T) {
	ctx := context.Background()

	// 1. Boot Hub 1 with SQLite persistence & Hub 2 federating from Hub 1!
	hub1, err := hub.NewNode(hub.Config{Network: "memory", DBPath: t.TempDir() + "/hub1.sqlite", EnableFinger: true})
	if err != nil {
		t.Fatal(err)
	}
	defer hub1.Close()
	hub1Srv := httptest.NewServer(hub1.Handler())
	defer hub1Srv.Close()

	// 2. Seed Seller Agent B with a domain shard & serve its CHIRP objects over HTTP!
	sellerMem := newNodeMem(t)
	if _, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:  "ops",
		Text:   "Teranode UTXO fan-out keeps 16 parallel spend slots hot.",
		Public: true,
	}); err != nil {
		t.Fatal(err)
	}
	pack, err := sellerMem.ExportShardPack(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	sellerHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hashHex := r.URL.Path[len("/v1/chirp/"):]
		b, _ := hex.DecodeString(hashHex)
		var h [32]byte
		copy(h[:], b)
		obj, oErr := sellerMem.Objects().GetObject(h)
		if oErr != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(obj)
	}))
	defer sellerHTTP.Close()

	sellerDisc := New(sellerMem, []string{hub1Srv.URL}, nil)
	nSub, err := sellerDisc.PublishAnnounce(ctx, bstack.AnnounceAd{
		Handle:      "bob@nektar.run",
		PublicURL:   sellerHTTP.URL,
		Hosts:       []string{hub1Srv.URL},
		SellDomains: []string{"ops"},
		Packs: []bstack.PackRef{{
			Shard:    "ops",
			Domains:  []string{"ops"},
			ChirpURL: pack.ChirpURL,
			RootHex:  pack.RootHex,
			Count:    1,
		}},
	})
	if err != nil || nSub != 1 {
		t.Fatalf("PublishAnnounce failed: nSub=%d err=%v", nSub, err)
	}

	// 3. Buyer Agent A joins Hub 1, discovers Bob via dir lane + handle, AND auto-seeds Bob's CHIRP pack!
	buyerMem := newNodeMem(t)
	buyerDisc := New(buyerMem, nil, nil)
	joinedURL, _, err := buyerDisc.JoinHost(ctx, hub1Srv.URL)
	if err != nil || joinedURL != hub1Srv.URL {
		t.Fatalf("JoinHost: url=%s err=%v", joinedURL, err)
	}
	peers, err := buyerDisc.DiscoverPeers(ctx, nil)
	if err != nil || len(peers) != 1 {
		t.Fatalf("DiscoverPeers: peers=%+v err=%v", peers, err)
	}
	bob, ok := buyerDisc.ResolveHandle("bob@nektar.run")
	if !ok || bob.PublicURL != sellerHTTP.URL {
		t.Fatalf("ResolveHandle bob failed: ok=%v bob=%+v", ok, bob)
	}
	imported, err := buyerDisc.SeedFromNetwork(ctx, []string{"*"})
	if err != nil || imported != 1 {
		t.Fatalf("SeedFromNetwork: imported=%d err=%v", imported, err)
	}
	_, recalled, err := buyerMem.RecallAllShards(ctx, memory.RecallInput{Query: "Teranode"})
	if err != nil || len(recalled) != 1 {
		t.Fatalf("buyer recall after remote CHIRP seed failed: recalled=%+v err=%v", recalled, err)
	}
}
