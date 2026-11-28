package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
)

func TestOneCallCapsuleSellImportAutoBountyAndInstantBuy(t *testing.T) {
	t.Setenv("NEKTAR_ALLOW_LOCAL_PEERS", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	shared := chain.NewSharedMemoryChain()

	// 1. Spin up a shared LightBSV Overlay Hub with starter Coding-Agent Atomic Network bounties.
	hubDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer hubDB.Close()
	hubPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir(), SharedMemoryChain: shared})
	if err != nil {
		t.Fatal(err)
	}
	defer hubPort.Close()
	hubMem, err := memory.New(hubDB, hubPort, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	hubNode, err := hub.NewNode(hub.Config{
		Network:      "memory",
		ChainTracker: shared,
		EnableFinger: true,
		EnableBbox:   true,
		EnableBstore: true,
		EnableFabric: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hubNode.Close()
	liqState, err := hub.ArmHubLiquidity(ctx, hubNode, hubMem, hub.LiquidityConfig{
		SeedStarterShards: true,
		ArmBounties:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Verify Coding-Agent Atomic Network domains are present in Hub bounties.
	bountyDoms := strings.Join(liqState.BountyDomains, ",")
	for _, wantDom := range []string{"ops", "debugging", "mcp", "api-quirks", "bsv"} {
		if !strings.Contains(bountyDoms, wantDom) {
			t.Fatalf("expected Hub starter bounties to include %q, got %v", wantDom, liqState.BountyDomains)
		}
	}

	// Host both the real LightBSV Hub (native BRC-180 /manifest.json + /v1/health) AND a static .nkt download route!
	capsuleDir := t.TempDir()
	hubMux := http.NewServeMux()
	hubMux.Handle("/capsules/", http.StripPrefix("/capsules/", http.FileServer(http.Dir(capsuleDir))))
	hubMux.Handle("/", hubNode.Handler())
	hubSrv := httptest.NewServer(hubMux)
	defer hubSrv.Close()
	hubURL := hubSrv.URL

	makeAgent := func(name string) (*memory.Service, *market.Engine, *market.Broker, *Toolbox, *httptest.Server, func()) {
		dir := t.TempDir()
		db, oErr := store.Open(filepath.Join(dir, "nektar.sqlite"))
		if oErr != nil {
			t.Fatal(oErr)
		}
		port, pErr := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir, SharedMemoryChain: shared})
		if pErr != nil {
			t.Fatal(pErr)
		}
		mem, mErr := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
		if mErr != nil {
			t.Fatal(mErr)
		}
		disc := discovery.New(mem, []string{hubURL}, nil)
		mkt := market.NewEngine(mem, market.DefaultPolicy())
		wfClient, wErr := mkt.StartWorkflows(ctx, "")
		if wErr != nil {
			t.Fatal(wErr)
		}
		broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{
			Handle:         name + "@nektar.run",
			CollectTimeout: 1200 * time.Millisecond,
			GraceTimeout:   60 * time.Millisecond,
			RestateClient:  wfClient,
		})
		app, aErr := NewApp(mem, mkt, Config{
			Discovery:     disc,
			Broker:        broker,
			ToolProfile:   "agent",
			RestateClient: wfClient,
		})
		if aErr != nil {
			t.Fatal(aErr)
		}
		srv := httptest.NewServer(app.Handler())
		broker.SetPublicURL(srv.URL)
		broker.Start(ctx)
		tb := NewToolbox(mem, mkt, broker, disc, nil, "agent")
		cleanup := func() {
			broker.Stop()
			_ = mkt.CloseWorkflows()
			app.Close()
			srv.Close()
			_ = port.Close()
			_ = db.Close()
		}
		return mem, mkt, broker, tb, srv, cleanup
	}

	sellerMem, _, sellerBroker, sellerTB, _, cleanSeller := makeAgent("seller")
	defer cleanSeller()
	buyerMem, _, buyerBroker, buyerTB, _, cleanBuyer := makeAgent("buyer")
	defer cleanBuyer()

	sellerBroker.WaitConnected(ctx, 1, 3*time.Second)
	buyerBroker.WaitConnected(ctx, 1, 3*time.Second)

	// 2. Verify fail-closed secret scrubbing on 1-call `sellCapsule`!
	_, leakErr := sellerTB.ExecSellCapsule(ctx, SellCapsuleInput{
		Text:    "OPENAI_API_KEY=sk-proj-9kX2mQpL8vR4tWz1aBcDeFgHiJkLmNoP in prod config",
		Domains: []string{"ops"},
	})
	if leakErr == nil {
		t.Fatal("expected sellCapsule to block secret leak fail-closed")
	}

	// 3. Cold-Start Zero-State -> Auto-Bounty (`buyCapsule` when 0 offers match yet)!
	bountyRaw, err := buyerTB.ExecBuyCapsule(ctx, BuyCapsuleInput{
		Question: "Unseen quantum kernel MCP bridge runbook",
		Domains:  []string{"mcp"},
		MaxSats:  80,
	})
	if err != nil {
		t.Fatalf("buyCapsule zero-state failed: %v", err)
	}
	bountyMap, _ := bountyRaw.(map[string]any)
	if bountyMap["mode"] != "bounty_posted" || bountyMap["ok"] != true {
		t.Fatalf("expected mode=bounty_posted when 0 offers match, got %+v", bountyMap)
	}
	watchRaw, err := buyerTB.ExecWatchKnowledge(ctx, WatchKnowledgeInput{Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	watchBytes, _ := json.Marshal(watchRaw)
	if !strings.Contains(string(watchBytes), "Unseen quantum kernel") {
		t.Fatalf("expected active StandingOrder bounty in watchKnowledge list, got %s", string(watchBytes))
	}

	// 4. 1-call `sellCapsule` (inline text -> scrub -> commit -> anchor -> auto-policy -> OfferAd + .nkt export)!
	nktFile := filepath.Join(capsuleDir, "sqlite-wal.nkt")
	sellRaw, err := sellerTB.ExecSellCapsule(ctx, SellCapsuleInput{
		Text:        "SQLite WAL concurrency fix: set busy_timeout=5000ms and split single-writer / multi-reader pools.",
		Domains:     []string{"ops"},
		PriceSats:   50,
		Description: "SQLite WAL tuning runbook",
		ExportPath:  nktFile,
	})
	if err != nil {
		t.Fatalf("sellCapsule failed: %v", err)
	}
	sellMap, _ := sellRaw.(map[string]any)
	if sellMap["ok"] != true || sellMap["quotingReady"] != true {
		t.Fatalf("expected sellCapsule ok=true quotingReady=true, got %+v", sellMap)
	}
	if _, statErr := os.Stat(nktFile); statErr != nil {
		t.Fatalf("expected exported .nkt file at %s: %v", nktFile, statErr)
	}
	sellerShard, _ := sellMap["repo"].(string)
	if frags, _ := sellerMem.DB().ListFragments(sellerShard); len(frags) == 0 {
		t.Fatalf("expected committed fragment in seller repo %q", sellerShard)
	}

	// 5. 1-call `buyCapsule` Instant SPV Fill (`mode: "instant_fill"`)!
	buyRaw, err := buyerTB.ExecBuyCapsule(ctx, BuyCapsuleInput{
		Question:    "SQLite WAL concurrency fix busy_timeout",
		Domains:     []string{"ops"},
		MaxSats:     100,
		TargetShard: "bought/ops",
	})
	if err != nil {
		t.Fatalf("buyCapsule instant fill failed: %v", err)
	}
	buyMap, _ := buyRaw.(map[string]any)
	if buyMap["mode"] != "instant_fill" || !strings.Contains(strings.ToLower(buyMap["deliveredText"].(string)), "busy_timeout=5000ms") {
		t.Fatalf("expected instant_fill with deliveredText, got %+v", buyMap)
	}
	if recalled, _ := buyerMem.Recall(ctx, memory.RecallInput{Shard: "bought/ops"}); len(recalled) == 0 {
		t.Fatal("expected purchased capsule to be recalled from buyer's bought/ops shard")
	}

	// 6. Viral Cold-Start Bridge: 1-call `importCapsule` over HTTP URL + automatic BRC-180 creator Hub join!
	soloDir := t.TempDir()
	soloDB, _ := store.Open(filepath.Join(soloDir, "nektar.sqlite"))
	defer soloDB.Close()
	soloPort, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: soloDir, SharedMemoryChain: shared})
	defer soloPort.Close()
	soloMem, _ := memory.New(soloDB, soloPort, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	soloDisc := discovery.New(soloMem, nil, nil)
	soloTB := NewToolbox(soloMem, market.NewEngine(soloMem, market.DefaultPolicy()), nil, soloDisc, nil, "agent")

	joinTrue := true
	impRaw, err := soloTB.ExecImportCapsule(ctx, ImportCapsuleInput{
		Source:         hubSrv.URL + "/capsules/sqlite-wal.nkt",
		Repo:           "imported/sqlite-wal",
		JoinCreatorHub: &joinTrue,
	})
	if err != nil {
		t.Fatalf("importCapsule over HTTP URL failed: %v", err)
	}
	impMap, _ := impRaw.(map[string]any)
	if impMap["ok"] != true || impMap["joinedHub"] != hubURL {
		t.Fatalf("expected importCapsule to SPV-import and join creator Hub %q via BRC-180 /manifest.json, got %+v", hubURL, impMap)
	}
	if items, _ := soloMem.Recall(ctx, memory.RecallInput{Shard: "imported/sqlite-wal"}); len(items) == 0 {
		t.Fatal("expected imported .nkt capsule content in solo agent's imported/sqlite-wal repo")
	}

	// 7. Verify `peers` joinHub / leaveHub via ExecPeersWithInput!
	peersOut, err := soloTB.ExecPeersWithInput(ctx, PeersToolInput{Refresh: true})
	if err != nil {
		t.Fatalf("ExecPeersWithInput failed: %v", err)
	}
	if _, hasHubs := peersOut["hubs"]; !hasHubs {
		t.Fatalf("expected hubs catalog in peers output, got %+v", peersOut)
	}
}
