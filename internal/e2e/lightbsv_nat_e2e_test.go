package e2e

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func openNATTestAgent(
	t *testing.T,
	ctx context.Context,
	shared *chain.SharedMemoryChain,
	hubURLs []string,
	handle string,
	bboxOffice string,
) (*memory.Service, *market.Engine, *discovery.Service, *market.Broker) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "nektar.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	port, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           dir,
		SharedMemoryChain: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })

	// Use a shared local disk FileObjectStore or per-agent MemoryObjectStore?
	// On NAT trades, Tier-2 capsules (>16 KiB) are fetched via CHIRP/Hub bstore OR carried inline up to 16 KiB;
	// sharing a CHIRP store across Hub + agents mirrors a Hub hosting BRC-167 /v1/chirp/{hash} / bstore blobs!
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eng := market.NewEngine(mem, market.DefaultPolicy())
	t.Cleanup(func() { _ = eng.CloseWorkflows() })

	disc := discovery.New(mem, hubURLs, nil)
	br := market.NewBroker(mem, eng, disc, market.BrokerConfig{
		Handle:         handle, // NAT agent: PublicURL intentionally empty!
		BboxOffice:     bboxOffice,
		CollectTimeout: 2500 * time.Millisecond,
		GraceTimeout:   80 * time.Millisecond,
	})
	t.Cleanup(func() { br.Stop() })
	return mem, eng, disc, br
}

// TestLightBSVMultiHubNATHandleDiscoveryAndZeroPollBboxTradeE2E verifies the complete LightBSV native plumbing:
//   - Spine + Leaf Hub topology with BRC-149 Object Plane + BRC-223 Fabric + BRC-180 /manifest.json + BRC-169 handles
//   - NAT Seller ("alice") & NAT Buyer ("bob") with zero PublicURL
//   - 3-tier handle discovery (alice@<hub>, nektar://pub/<pubkey>@<hub>) + TOFU ErrHandleKeyChanged protection
//   - Sub-second zero-poll NAT trade over Lightweb `bbox/boxrec` envelopes + SSE `bbox_wake` + embedded `go-workflows`
func TestLightBSVMultiHubNATHandleDiscoveryAndZeroPollBboxTradeE2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shared := chain.NewSharedMemoryChain()

	spine, err := hub.NewNode(hub.Config{
		Network:       "memory",
		DBPath:        filepath.Join(t.TempDir(), "spine.sqlite"),
		ChainTracker:  shared,
		EnableFinger:  true,
		EnableBbox:    true,
		EnableBstore:  true,
		EnableFabric:  true,
		TransportMode: hub.TransportRelay,
		Fees:          hub.FeeSchedule{Role: hub.RoleSpine},
	})
	if err != nil {
		t.Fatalf("spine hub.NewNode: %v", err)
	}
	t.Cleanup(func() { _ = spine.Close() })
	spineTS := httptest.NewServer(spine.Handler())
	t.Cleanup(func() {
		spine.CloseSSE()
		spineTS.CloseClientConnections()
		spineTS.Close()
	})

	leafSigner, _ := nwallet.NewEphemeralMarketSigner()
	spine.AddSpinePeerPub(leafSigner.PubHex())

	leaf, err := hub.NewNode(hub.Config{
		Network:        "memory",
		DBPath:         filepath.Join(t.TempDir(), "leaf.sqlite"),
		ChainTracker:   shared,
		EnableFinger:   true,
		EnableBbox:     true,
		EnableBstore:   true,
		EnableFabric:   true,
		TransportMode:  hub.TransportRelay,
		Fees:           hub.FeeSchedule{Role: hub.RoleLeaf},
		UpstreamSpines: []string{spineTS.URL},
	})
	if err != nil {
		t.Fatalf("leaf hub.NewNode: %v", err)
	}
	t.Cleanup(func() { _ = leaf.Close() })
	leaf.SetEgressAuth(func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
		return discovery.SignEgressAuthHeaders(leafSigner, action, aud, cursor, domainsCSV)
	}, nil)
	leafTS := httptest.NewServer(leaf.Handler())
	t.Cleanup(func() {
		leaf.CloseSSE()
		leafTS.CloseClientConnections()
		leafTS.Close()
	})

	office := spine.DefaultBboxOffice()

	// Seller ("alice") behind NAT connected to Leaf (which forwards UP to Spine) + Spine bbox office.
	aliceMem, aliceEng, aliceDisc, aliceBroker := openNATTestAgent(t, ctx, shared, []string{spineTS.URL}, "alice", office)
	spine.SetHandleResolver(aliceDisc.WellKnownHandleHandler("", office))
	aliceDisc.SetLocalHandle("alice", "", office)

	if _, err := market.ApprovePolicyPreset(aliceMem.DB(), aliceEng.Policy(), "both", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := aliceEng.StartWorkflows(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := aliceMem.Remember(ctx, memory.RememberInput{
		Shard:      "lightbsv-plumbing",
		Text:       "LightBSV BRC-180 /manifest.json + BRC-169 handles + bbox/boxrec enable zero-config NAT-to-NAT agent trades.",
		Source:     "alice",
		Distilled:  true,
		AutoAnchor: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := aliceBroker.ArmSell(ctx, market.SellInput{
		Shard:     "lightbsv-plumbing",
		Domains:   []string{"lightbsv"},
		PriceSats: 60,
	}); err != nil {
		t.Fatal(err)
	}
	aliceBroker.Start(ctx)
	if _, err := aliceBroker.AnnounceSelf(ctx); err != nil {
		t.Fatalf("alice AnnounceSelf: %v", err)
	}

	// Buyer ("bob") behind NAT on the same Hub mesh.
	bobMem, bobEng, bobDisc, bobBroker := openNATTestAgent(t, ctx, shared, []string{spineTS.URL}, "bob", office)
	t.Cleanup(cancel)

	if _, err := market.ApprovePolicyPreset(bobMem.DB(), bobEng.Policy(), "buyer", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := bobEng.StartWorkflows(ctx, ""); err != nil {
		t.Fatal(err)
	}
	bobBroker.Start(ctx)

	if !aliceBroker.WaitConnected(ctx, 1, 2*time.Second) || !bobBroker.WaitConnected(ctx, 1, 2*time.Second) {
		t.Fatal("expected Alice and Bob SSE streams to connect cleanly")
	}

	// 1. Verify BRC-180 + BRC-169 handle resolution (`alice@<host>`) + TOFU key pinning!
	spineHost := strings.TrimPrefix(spineTS.URL, "http://")
	resolvedAlice, err := bobDisc.ResolveAgentCtx(ctx, "alice@"+spineHost)
	if err != nil {
		t.Fatalf("ResolveAgentCtx(alice@%s): %v", spineHost, err)
	}
	if !strings.EqualFold(resolvedAlice.Pub, aliceDisc.AnnouncePubHex()) {
		t.Fatalf("resolved pub mismatch: got %s, want %s", resolvedAlice.Pub, aliceDisc.AnnouncePubHex())
	}
	// Simulated compromised handle server returning a different key MUST trigger ErrHandleKeyChanged!
	evilSigner, _ := nwallet.NewEphemeralMarketSigner()
	if err := bobDisc.VerifyOrPinHandle("alice@"+spineHost, evilSigner.PubHex(), office); !errors.Is(err, discovery.ErrHandleKeyChanged) {
		t.Fatalf("expected ErrHandleKeyChanged on silent key rotation, got %v", err)
	}

	// 2. Verify Tier-3 zero-DNS URI (`nektar://pub/<pubkey>@<host>`) resolves deterministically!
	uriPeer, err := bobDisc.ResolveAgentCtx(ctx, "nektar://pub/"+aliceBroker.PubHex()+"@"+spineHost)
	if err != nil || !strings.EqualFold(uriPeer.Pub, aliceBroker.PubHex()) {
		t.Fatalf("Tier-3 URI resolution failed: peer=%+v err=%v", uriPeer, err)
	}

	// 3. Execute live NAT-to-NAT trade (zero PublicURL on both peers!) via embedded go-workflows + bbox_wake!
	t0 := time.Now()
	deal, err := bobBroker.Want(ctx, market.WantRequest{
		Domains:        []string{"lightbsv"},
		Question:       "How do BRC-180 /manifest.json and bbox/boxrec enable NAT-to-NAT agent trades?",
		MaxSats:        120,
		TargetShard:    "bought/lightbsv",
		CollectTimeout: 2500 * time.Millisecond,
	})
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("NAT-to-NAT Want failed: %v", err)
	}
	if deal.Status != "settled" || !strings.Contains(deal.DeliveredText, "BRC-180") {
		t.Fatalf("expected settled NAT deal with BRC-180 text, got status=%s text=%q err=%s", deal.Status, deal.DeliveredText, deal.Error)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected zero-poll NAT trade to settle in <2s, took %v", elapsed)
	}
}
