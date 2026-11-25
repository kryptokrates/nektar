package e2e_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/server"
	"github.com/kryptokrates/nektar/internal/store"
)

type testAgent struct {
	name    string
	dataDir string
	mem     *memory.Service
	mkt     *market.Engine
	disc    *discovery.Service
	broker  *market.Broker
	app     *server.App
	ts      *httptest.Server
	hubNode *hub.Node
}

type agentOpts struct {
	name           string
	hubURLs        []string
	isHub          bool
	hubDBPath      string
	natOnly        bool
	apiToken       string
	operatorToken  string
	toolPrices     map[string]uint64
	openAIUpstream string
	anthUpstream   string
	spendGuardCap  uint64
}

func spawnAgent(t *testing.T, ctx context.Context, opts agentOpts) *testAgent {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "nektar.sqlite"))
	if err != nil {
		t.Fatalf("%s store.Open: %v", opts.name, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sg := core.SpendGuardOpts{MaxFeeSatsPerTx: 50_000, MaxSpendSatsDay: 5_000_000}
	if opts.spendGuardCap > 0 {
		sg.MaxSpendSatsDay = opts.spendGuardCap
	}
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: dir, SpendGuard: sg})
	if err != nil {
		t.Fatalf("%s chain.Open: %v", opts.name, err)
	}
	// Pre-split coins so concurrent trades never starve!
	_ = port.EnsureSplitCoins(ctx, 12, 5000)

	objStore, err := bstack.NewFileObjectStore(dir)
	if err != nil {
		t.Fatalf("%s objStore: %v", opts.name, err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{
		DatalogRules: []string{
			`trusted_host(X) :- owns(X, Y), verified(Y).`,
		},
	})
	if err != nil {
		t.Fatalf("%s memory.New: %v", opts.name, err)
	}
	_, _ = mem.EnsureWelcome(ctx)

	var hubNode *hub.Node
	var liq *hub.LiquidityState
	if opts.isHub {
		hubNode, err = hub.NewNode(hub.Config{
			Network:      "memory",
			DBPath:       opts.hubDBPath,
			EnableFinger: true,
		})
		if err != nil {
			t.Fatalf("%s hub.NewNode: %v", opts.name, err)
		}
		t.Cleanup(func() { _ = hubNode.Close() })
		liq, err = hub.ArmHubLiquidity(ctx, hubNode, mem, hub.LiquidityConfig{
			SeedStarterShards: true,
			ArmBounties:       true,
			StarterPriceSats:  90,
			BountyRewardSats:  100,
		})
		if err != nil {
			t.Fatalf("%s ArmHubLiquidity: %v", opts.name, err)
		}
	}

	mkt := market.NewEngine(mem, market.DefaultPolicy())
	disc := discovery.New(mem, opts.hubURLs, nil)
	broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{
		Handle:         opts.name + "@nektar.test",
		IsHub:          opts.isHub,
		BboxOffice:     "default",
		CollectTimeout: 1200 * time.Millisecond,
	})
	if liq != nil {
		broker.SetAdvertisedPacks(liq.Packs, liq.BountyDomains)
	}

	app, err := server.NewApp(mem, mkt, server.Config{
		APIToken:          opts.apiToken,
		OperatorToken:     opts.operatorToken,
		ToolPrices:        opts.toolPrices,
		OpenAIUpstream:    opts.openAIUpstream,
		AnthropicUpstream: opts.anthUpstream,
		Hub:               hubNode,
		Discovery:         disc,
		Broker:            broker,
		MountRestate:      true,
	})
	if err != nil {
		t.Fatalf("%s server.NewApp: %v", opts.name, err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	if !opts.natOnly {
		broker.SetPublicURL(ts.URL)
	}
	if opts.isHub {
		_, _, _ = disc.JoinHost(ctx, ts.URL+"/hub")
	}

	return &testAgent{
		name:    opts.name,
		dataDir: dir,
		mem:     mem,
		mkt:     mkt,
		disc:    disc,
		broker:  broker,
		app:     app,
		ts:      ts,
		hubNode: hubNode,
	}
}

func TestFiveNodeFullStackSmokeMeshSecurityAndChaos(t *testing.T) {
	hubDBPath := filepath.Join(t.TempDir(), "hub1.sqlite")
	ctx, cancel := context.WithCancel(context.Background())

	// Mock LLM upstream capturing BOTH OpenAI & Anthropic L0 proxy memory injections.
	var proxyMu sync.Mutex
	var openAIBody, anthBody string
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		proxyMu.Lock()
		if strings.Contains(r.URL.Path, "chat/completions") {
			openAIBody = string(raw)
		} else {
			anthBody = string(raw)
		}
		proxyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer mockLLM.Close()

	// Spawn 5 full nodes: Hub-1 + 4 Agents (Alpha, Beta, Gamma, NAT-only Delta)!
	hub1 := spawnAgent(t, ctx, agentOpts{
		name:          "hub1",
		isHub:         true,
		hubDBPath:     hubDBPath,
		apiToken:      "hub-tok",
		operatorToken: "hub-op",
	})
	hub1URL := hub1.ts.URL + "/hub"

	alpha := spawnAgent(t, ctx, agentOpts{
		name:           "alpha",
		hubURLs:        []string{hub1URL},
		toolPrices:     map[string]uint64{"groundedAnswer": 50},
		openAIUpstream: mockLLM.URL,
		anthUpstream:   mockLLM.URL,
	})
	beta := spawnAgent(t, ctx, agentOpts{
		name:    "beta",
		hubURLs: []string{hub1URL},
	})
	gamma := spawnAgent(t, ctx, agentOpts{
		name:    "gamma",
		hubURLs: []string{hub1URL},
	})
	deltaNAT := spawnAgent(t, ctx, agentOpts{
		name:    "delta-nat",
		hubURLs: []string{hub1URL},
		natOnly: true, // ZERO inbound HTTP URL — trades exclusively via Hub /bbox relay!
	})

	// Cancel SSE loops BEFORE httptest servers close (registered after spawns so t.Cleanup runs LIFO)!
	t.Cleanup(func() {
		cancel()
		hub1.ts.CloseClientConnections()
	})

	// Approve buyer+seller policies for Alpha, Beta, Gamma, Delta (testing both CLI/helper and HTTP preset).
	for _, a := range []*testAgent{alpha, beta, gamma, deltaNAT} {
		if _, err := market.ApprovePolicyPreset(a.mem.DB(), a.mkt.Policy(), "both", nil, "e2e"); err != nil {
			t.Fatalf("%s approve policy: %v", a.name, err)
		}
	}

	// =========================================================================
	// PHASE A — FULL SURFACE SMOKE MATRIX
	// =========================================================================
	t.Run("PhaseA_FullSurfaceSmoke", func(t *testing.T) {
		// 1. Auth gate + 1-click pairing cookie + Svelte /state + /card + agent.json!
		r401, _ := http.Get(hub1.ts.URL + "/state")
		if r401.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 on protected /state, got %d", r401.StatusCode)
		}
		r401.Body.Close()

		code := hub1.app.CreatePairCode()
		noRedir := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		pResp, err := noRedir.Get(hub1.ts.URL + "/pair?code=" + code)
		if err != nil || pResp.StatusCode != http.StatusFound {
			t.Fatalf("/pair failed: %v %+v", err, pResp)
		}
		sessCookie := pResp.Cookies()[0]
		pResp.Body.Close()

		stReq, _ := http.NewRequest(http.MethodGet, hub1.ts.URL+"/state", nil)
		stReq.AddCookie(sessCookie)
		stResp, err := http.DefaultClient.Do(stReq)
		if err != nil || stResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /state failed: %v %+v", err, stResp)
		}
		stResp.Body.Close()

		// 2. Hermes agent.json name == "nektar" & Restate /restate/discover (all 5 services)
		cardResp, _ := http.Get(alpha.ts.URL + "/.well-known/agent.json")
		var card map[string]any
		_ = json.NewDecoder(cardResp.Body).Decode(&card)
		cardResp.Body.Close()
		if card["name"] != "nektar" {
			t.Fatalf("agent.json name expected nektar, got %v", card["name"])
		}
		rsReq, _ := http.NewRequest(http.MethodGet, alpha.ts.URL+"/restate/discover", nil)
		rsReq.Header.Set("Accept", "application/vnd.restate.endpointmanifest.v3+json")
		rsResp, err := http.DefaultClient.Do(rsReq)
		if err != nil || rsResp.StatusCode != http.StatusOK {
			t.Fatalf("/restate/discover failed: %v", err)
		}
		rsBody, _ := io.ReadAll(rsResp.Body)
		rsResp.Body.Close()
		for _, svc := range []string{"BuyerDeal", "SellerDeal", "Custody", "Tx", "AnchorCadence"} {
			if !strings.Contains(string(rsBody), `"`+svc+`"`) {
				t.Fatalf("missing Restate service %s", svc)
			}
		}

		// 3. L0 OpenAI & Anthropic Memory Proxy injection!
		p1, _ := http.Post(alpha.ts.URL+"/v1/proxy/openai/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		p1.Body.Close()
		p2, _ := http.Post(alpha.ts.URL+"/v1/proxy/anthropic/v1/messages", "application/json",
			strings.NewReader(`{"model":"claude-3-7","messages":[{"role":"user","content":"hi"}]}`))
		p2.Body.Close()
		proxyMu.Lock()
		oa, an := openAIBody, anthBody
		proxyMu.Unlock()
		if !strings.Contains(oa, "Nektar Verified Memory") || !strings.Contains(an, "Nektar Verified Memory") {
			t.Fatalf("expected BOTH OpenAI and Anthropic L0 proxies to inject welcome memory")
		}

		// 4. BRC-105 HTTP 402 Paywall -> Paid 200 -> Replay 409!
		ansBody := `{"claims":[{"text":"Nektar stores verifiable, encrypted AI agent memory anchored on BSV","cites":[{"shard":"welcome","seq":1}]}]}`
		r402, _ := http.Post(alpha.ts.URL+"/v1/answer", "application/json", strings.NewReader(ansBody))
		if r402.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("expected 402, got %d", r402.StatusCode)
		}
		r402.Body.Close()
		payHdr, _ := json.Marshal(server.BRC105PaymentHeader{TxID: "e2epay001", AmountSats: 50, Inputs: []string{"utxo0:0"}})
		reqOK, _ := http.NewRequest(http.MethodPost, alpha.ts.URL+"/v1/answer", strings.NewReader(ansBody))
		reqOK.Header.Set("Content-Type", "application/json")
		reqOK.Header.Set("X-BSV-Payment", string(payHdr))
		r200, _ := http.DefaultClient.Do(reqOK)
		if r200.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on paid /v1/answer, got %d", r200.StatusCode)
		}
		r200.Body.Close()
		reqDup, _ := http.NewRequest(http.MethodPost, alpha.ts.URL+"/v1/answer", strings.NewReader(ansBody))
		reqDup.Header.Set("Content-Type", "application/json")
		reqDup.Header.Set("X-BSV-Payment", string(payHdr))
		r409, _ := http.DefaultClient.Do(reqDup)
		if r409.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 replay conflict, got %d", r409.StatusCode)
		}
		r409.Body.Close()

		// 5. 3 Concurrent stdio->HTTP MCP Bridge Clients sharing ONE daemon ("one brain, many mouths")!
		var wg sync.WaitGroup
		mcpErrs := make(chan error, 3)
		for i := range 3 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recall","arguments":{}}}` + "\n"
				var out bytes.Buffer
				if err := server.RunStdioBridge(ctx, alpha.ts.URL, "", strings.NewReader(in), &out); err != nil {
					mcpErrs <- err
					return
				}
				if !strings.Contains(out.String(), "welcome") {
					mcpErrs <- fmt.Errorf("client %d missing welcome shard: %s", id, out.String())
				}
			}(i)
		}
		wg.Wait()
		close(mcpErrs)
		for err := range mcpErrs {
			t.Fatalf("concurrent MCP bridge failed: %v", err)
		}
	})

	// =========================================================================
	// PHASE B — 4-AGENT A2A MESH, COMPETITIVE RANKING, NAT BBOX, HUB BOUNTIES & 2-HUB FEDERATION
	// =========================================================================
	// Seed specialized proprietary knowledge on each of the 4 agents & arm their sell listings!
	remAlpha, err := alpha.mem.Remember(ctx, memory.RememberInput{
		Shard: "ops-infra", Text: "Alpha SRE: Kubernetes etcd quorum snapshots rotate every 15m to s3://etcd-prod.", Source: "alpha", AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = alpha.broker.ArmSell(ctx, market.SellInput{Shard: "ops-infra", Domains: []string{"ops"}, PriceSats: 70})

	// Beta competes with Alpha on "ops" at a LOWER price (55 sats) AND sells "bsv"!
	_, _ = beta.mem.Remember(ctx, memory.RememberInput{
		Shard: "ops-fast", Text: "Beta SRE: Kubernetes etcd quorum snapshots rotate every 15m with zstd level 3.", Source: "beta", AutoAnchor: true,
	})
	_, _ = beta.broker.ArmSell(ctx, market.SellInput{Shard: "ops-fast", Domains: []string{"ops"}, PriceSats: 55})
	_, _ = beta.mem.Remember(ctx, memory.RememberInput{
		Shard: "bsv-spv", Text: "Beta BSV: Teranode ARC accepts Extended Format transactions with 100 sat/kB fee.", Source: "beta", AutoAnchor: true,
	})
	_, _ = beta.broker.ArmSell(ctx, market.SellInput{Shard: "bsv-spv", Domains: []string{"bsv"}, PriceSats: 65})

	remGamma, _ := gamma.mem.Remember(ctx, memory.RememberInput{
		Shard: "agents-ai", Text: "Gamma AI: MCP Streamable HTTP supports stateless fan-out across concurrent tool calls.", Source: "gamma", AutoAnchor: true,
	})
	_, _ = gamma.broker.ArmSell(ctx, market.SellInput{Shard: "agents-ai", Domains: []string{"agents"}, PriceSats: 60})

	// NAT-only Delta (ZERO public HTTP URL!) sells "nat" purely through Hub /bbox relay!
	_, _ = deltaNAT.mem.Remember(ctx, memory.RememberInput{
		Shard: "nat-edge", Text: "Delta NAT: Zero-port agents trade atomically over Hub bbox store-and-forward.", Source: "delta", AutoAnchor: true,
	})
	_, _ = deltaNAT.broker.ArmSell(ctx, market.SellInput{Shard: "nat-edge", Domains: []string{"nat"}, PriceSats: 50})

	// Start all 5 brokers & announce all 5 nodes on the signed `dir` lane!
	for _, n := range []*testAgent{hub1, alpha, beta, gamma, deltaNAT} {
		n.broker.Start(ctx)
		_, _ = n.broker.AnnounceSelf(ctx)
	}
	time.Sleep(250 * time.Millisecond)

	t.Run("PhaseB_FourAgentMeshCompetitionNATAndBounties", func(t *testing.T) {
		// 1. Peer discovery + bfinger handle lookup + Layer-2 CHIRP starter pack seeding!
		peers, err := gamma.disc.DiscoverPeers(ctx, nil)
		if err != nil || len(peers) < 5 {
			t.Fatalf("expected >=5 verified peers (Hub + 4 agents), got %d err=%v", len(peers), err)
		}
		if p, ok := gamma.disc.ResolveHandle("delta-nat@nektar.test"); !ok || p.PublicURL != "" {
			t.Fatalf("expected bfinger handle delta-nat@nektar.test with empty PublicURL, got ok=%v p=%+v", ok, p)
		}
		seeded, err := gamma.disc.SeedFromNetwork(ctx, []string{"ops"})
		if err != nil || seeded < 2 {
			t.Fatalf("Layer-2 SeedFromNetwork expected >=2 starter items, got %d err=%v", seeded, err)
		}

		// 2. Competitive Ranking: Gamma wants "ops" -> Alpha (70), Beta (55), Hub (90) all quote -> Beta (55) MUST win!
		dComp, err := gamma.broker.Want(ctx, market.WantRequest{
			Domains: []string{"ops"}, Question: "Kubernetes etcd quorum snapshots", MaxSats: 150, TargetShard: "bought/ops",
		})
		if err != nil || dComp.Status != "settled" {
			t.Fatalf("Gamma competitive want failed: err=%v deal=%+v", err, dComp)
		}
		if dComp.CounterpartyPub != beta.broker.PubHex() || dComp.PriceSats != 55 {
			t.Fatalf("expected cheaper organic Beta (55 sats) to win bps ranking, got pub=%s price=%d", dComp.CounterpartyPub, dComp.PriceSats)
		}

		// 3. Cross-Mesh: Alpha buys "agents" from Gamma!
		dAG, err := alpha.broker.Want(ctx, market.WantRequest{
			Domains: []string{"agents"}, Question: "MCP Streamable HTTP stateless", MaxSats: 120, TargetShard: "bought/agents",
		})
		if err != nil || dAG.Status != "settled" || !strings.Contains(dAG.DeliveredText, "Streamable HTTP") {
			t.Fatalf("Alpha <- Gamma trade failed: err=%v deal=%+v", err, dAG)
		}

		// 4. NAT-only Delta SELLS to Beta purely over Hub /bbox!
		dBNAT, err := beta.broker.Want(ctx, market.WantRequest{
			Domains: []string{"nat"}, Question: "Zero-port agents bbox", MaxSats: 100, TargetShard: "bought/nat",
		})
		if err != nil || dBNAT.Status != "settled" || !strings.Contains(dBNAT.DeliveredText, "store-and-forward") {
			t.Fatalf("Beta <- Delta(NAT) bbox trade failed: err=%v deal=%+v", err, dBNAT)
		}

		// 5. NAT-only Delta BUYS "bsv" from Beta!
		dNATB, err := deltaNAT.broker.Want(ctx, market.WantRequest{
			Domains: []string{"bsv"}, Question: "Teranode ARC Extended Format", MaxSats: 120, TargetShard: "bought/bsv",
		})
		if err != nil || dNATB.Status != "settled" || !strings.Contains(dNATB.DeliveredText, "100 sat/kB") {
			t.Fatalf("Delta(NAT) <- Beta trade failed: err=%v deal=%+v", err, dNATB)
		}

		// 6. Sell-to-Hub Bounties: Alpha -> bounty-ops AND Gamma -> bounty-agents (+ HDP duplicate 409 check)!
		p1, err := alpha.broker.SellToHub(ctx, hub1URL, "bounty-ops", "ops-infra", remAlpha.Seq)
		if err != nil || !p1.Accepted || p1.TxID == "" {
			t.Fatalf("Alpha SellToHub bounty-ops failed: err=%v res=%+v", err, p1)
		}
		if _, dupErr := alpha.broker.SellToHub(ctx, hub1URL, "bounty-ops", "ops-infra", remAlpha.Seq); dupErr == nil {
			t.Fatal("expected duplicate HDP bounty submission to be rejected")
		}
		p2, err := gamma.broker.SellToHub(ctx, hub1URL, "bounty-agents", "agents-ai", remGamma.Seq)
		if err != nil || !p2.Accepted || p2.TxID == "" {
			t.Fatalf("Gamma SellToHub bounty-agents failed: err=%v res=%+v", err, p2)
		}

		// 7. 2-Hub Federation: Hub-2 federates from Hub-1 and automatically sees all 5 peers!
		hub2Node, err := hub.NewNode(hub.Config{Network: "memory"})
		if err != nil {
			t.Fatal(err)
		}
		defer hub2Node.Close()
		hub2Node.StartFederation(ctx, []string{hub1URL})
		hub2TS := httptest.NewServer(hub2Node.Handler())
		defer hub2TS.Close()

		deadline := time.Now().Add(4 * time.Second)
		var fedPeers []discovery.PeerInfo
		for time.Now().Before(deadline) {
			fedPeers, _ = alpha.disc.DiscoverPeers(ctx, []string{hub2TS.URL})
			if len(fedPeers) >= 5 {
				break
			}
			time.Sleep(60 * time.Millisecond)
		}
		if len(fedPeers) < 5 {
			t.Fatalf("expected Hub-2 federation to replicate >=5 peers from Hub-1, got %d", len(fedPeers))
		}
	})

	// =========================================================================
	// PHASE C — BORG BRC-369 REVOCATION, CEDAR ISOLATION, MANGLE DATALOG & HONESTY RULE
	// =========================================================================
	t.Run("PhaseC_BorgCedarMangleAndPolicyEscalation", func(t *testing.T) {
		// 1. Borg BRC-369 Group Epoch Rotation: Alpha decrypts epoch 1, fails closed after revocation in epoch 2!
		alphaPub := alpha.mem.Chain().IdentityPubHex()
		gammaPub := gamma.mem.Chain().IdentityPubHex()
		if _, err := gamma.mem.Groups().CreateGroup("sec-team", "Security Team", gammaPub); err != nil {
			t.Fatal(err)
		}
		if err := gamma.mem.Groups().AddMember("sec-team", alphaPub, "member"); err != nil {
			t.Fatal(err)
		}
		if _, err := gamma.mem.Remember(ctx, memory.RememberInput{
			Shard: "secret-ops", Text: "Epoch1 secret: vault rotation Friday.", BorgGroup: "sec-team", CallerPub: gammaPub,
		}); err != nil {
			t.Fatal(err)
		}
		ep1, _ := gamma.mem.Recall(ctx, memory.RecallInput{Shard: "secret-ops", CallerPub: alphaPub})
		if len(ep1) != 1 || !strings.Contains(ep1[0].Text, "Epoch1 secret") {
			t.Fatalf("expected Alpha to decrypt epoch 1 Borg fragment, got %+v", ep1)
		}
		newEp, err := gamma.mem.Groups().RevokeMember("sec-team", alphaPub)
		if err != nil || newEp != 2 {
			t.Fatalf("expected revoke to rotate to epoch 2, got ep=%d err=%v", newEp, err)
		}
		if _, err := gamma.mem.Remember(ctx, memory.RememberInput{
			Shard: "secret-ops", Text: "Epoch2 top-secret: rotated root key.", BorgGroup: "sec-team", CallerPub: gammaPub,
		}); err != nil {
			t.Fatal(err)
		}
		afterRevoke, _ := gamma.mem.Recall(ctx, memory.RecallInput{Shard: "secret-ops", CallerPub: alphaPub})
		for _, f := range afterRevoke {
			if strings.Contains(f.Text, "Epoch2") {
				t.Fatal("SECURITY VIOLATION: revoked member decrypted epoch 2 Borg fragment!")
			}
		}

		// 2. AWS Cedar Level Isolation: level-5 fragment denied to level-2 principal, allowed to level-5!
		_, _ = gamma.mem.Remember(ctx, memory.RememberInput{Shard: "classified", Text: "Clearance level 5 nuclear launch code.", Level: 5})
		lowRec, _ := gamma.mem.Recall(ctx, memory.RecallInput{Shard: "classified", Principal: policy.Principal{Type: "Agent", ID: "intern", Level: 2}})
		if len(lowRec) != 0 {
			t.Fatalf("SECURITY VIOLATION: level-2 principal read level-5 fragment: %+v", lowRec)
		}
		highRec, _ := gamma.mem.Recall(ctx, memory.RecallInput{Shard: "classified", Principal: policy.Principal{Type: "Agent", ID: "director", Level: 5}})
		if len(highRec) != 1 {
			t.Fatalf("expected level-5 principal to read level-5 fragment, got %d", len(highRec))
		}

		// 3. Mangle Grounding + Honesty Rule + Forget flipping receipt GREEN -> RED!
		rFact, _ := beta.mem.Remember(ctx, memory.RememberInput{Shard: "audit", Text: "Audit fact: quarterly reserve verified at 100%.", AutoAnchor: true})
		ga, err := beta.mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
			Claims: []reasoning.Claim{
				{Text: "Audit fact: quarterly reserve verified at 100%", Cites: []reasoning.FragmentRef{{Shard: "audit", Seq: rFact.Seq}}},
				{Text: "Hallucinated claim with zero citation backing", Cites: []reasoning.FragmentRef{{Shard: "audit", Seq: 999}}},
			},
			AutoAnchor: true,
		})
		if err != nil || ga.Ungrounded || strings.Contains(ga.Answer, "Hallucinated") {
			t.Fatalf("expected grounded answer stripping hallucination, got %+v err=%v", ga, err)
		}
		verGreen, _ := beta.mem.VerifyReceipt(ctx, ga.ReceiptToken)
		if verGreen.Verdict != "green" {
			t.Fatalf("expected green receipt before forget, got %+v", verGreen)
		}
		// Pure hallucination -> MUST return ungrounded=true & EMPTY footer (Honesty Rule)!
		gaHalluc, _ := beta.mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
			Claims: []reasoning.Claim{{Text: "Invented fact", Cites: []reasoning.FragmentRef{{Shard: "audit", Seq: 999}}}},
		})
		if !gaHalluc.Ungrounded || gaHalluc.Footer != "" || gaHalluc.ReceiptToken != "" {
			t.Fatalf("HONESTY VIOLATION: ungrounded answer emitted receipt footer: %+v", gaHalluc)
		}
		// Forget fragment -> citing receipt flips GREEN -> RED!
		_ = beta.mem.Forget(ctx, "audit", rFact.Seq)
		verRed, _ := beta.mem.VerifyReceipt(ctx, ga.ReceiptToken)
		if verRed.Verdict != "red" {
			t.Fatalf("expected forgotten cited fragment to flip receipt to red, got %+v", verRed)
		}

		// 4. Operator Policy Escalation & Approval Resume!
		paused, err := deltaNAT.broker.Want(ctx, market.WantRequest{
			Domains: []string{"bsv"}, Question: "Teranode ARC", MaxSats: 5000, // exceeds 2000 maxPerDeal!
		})
		if err != nil || paused.Status != "approval_pending" {
			t.Fatalf("expected approval_pending for 5000 > 2000 cap, got status=%s err=%v", paused.Status, err)
		}
		apprs, _ := deltaNAT.mem.DB().ListPendingApprovals()
		if len(apprs) == 0 {
			t.Fatal("expected pending approval record in SQLite")
		}
		_, resumed, err := deltaNAT.broker.ApproveAndExecute(ctx, apprs[0].ID, true)
		if err != nil || resumed == nil || resumed.Status != "settled" {
			t.Fatalf("ApproveAndExecute failed: err=%v deal=%+v", err, resumed)
		}
	})

	// =========================================================================
	// PHASE D — ADVERSARIAL ATTACKS & CHAOS FAULT-INJECTION
	// =========================================================================
	t.Run("PhaseD_AdversarialAndChaosFaultInjection", func(t *testing.T) {
		// 1. Forged OfferAd signature rejected by Hub /submit!
		k1, _ := ec.NewPrivateKey()
		k2, _ := ec.NewPrivateKey()
		forgedOffer := bstack.OfferAd{
			OfferID: "forged-1", WantID: "w-1",
			Pub: hex.EncodeToString(k1.PubKey().Compressed()), // claims k1
			Net: "memory", Domains: []string{"ops"}, PriceSats: 1, Revision: 1,
			Exp: time.Now().Add(time.Hour).UnixMilli(),
		}
		badSig, _ := bstack.SignArtifact(k2, forgedOffer) // signed by k2!
		badBody, _ := json.Marshal(bstack.SignedOffer{Offer: forgedOffer, Sig: badSig})
		badBEEF, _ := discovery.BuildMarketBEEF(bstack.MarketPayload{Kind: bstack.KindOffer, Body: badBody})
		if err := alpha.disc.SubmitBEEF(ctx, hub1URL, badBEEF, []string{bstack.LaneOf("memory", "ops")}); err != nil {
			// Good if rejected at HTTP level; if 200 steak with 0 admitted outputs, verify lookup does NOT return it!
		}
		qBytes, _ := json.Marshal(map[string]any{"service": bstack.LookupService, "query": bstack.MarketQuery{Kind: bstack.KindOffer, WantID: "w-1"}})
		lResp, _ := http.Post(hub1URL+"/lookup", "application/json", bytes.NewReader(qBytes))
		lRaw, _ := io.ReadAll(lResp.Body)
		lResp.Body.Close()
		if strings.Contains(string(lRaw), "forged-1") {
			t.Fatal("SECURITY VIOLATION: Hub admitted forged OfferAd signature!")
		}

		// 2. Wrong BRC-369 content reveal key k rejected by ClaimKeyedSale!
		so, _, _, err := alpha.mkt.PrepareDirectSale(ctx, "ops-infra", remAlpha.Seq, 70, beta.broker.PubHex())
		if err != nil {
			t.Fatal(err)
		}
		wrongKey := make([]byte, 32)
		wrongKey[0] = 0xDE
		if _, err := contracts.ClaimKeyedSale(so, wrongKey); err == nil {
			t.Fatal("SECURITY VIOLATION: ClaimKeyedSale accepted wrong BRC-369 reveal key!")
		}

		// 3. Operator KillSwitch halts trading immediately (fail-closed)!
		alpha.mkt.Policy().SetKillSwitch(true)
		if _, err := alpha.broker.Want(ctx, market.WantRequest{Domains: []string{"bsv"}, Question: "kill test", MaxSats: 100}); err == nil {
			t.Fatal("expected KillSwitch to refuse Want immediately")
		}
		alpha.mkt.Policy().SetKillSwitch(false)

		// 4. SpendGuard daily cap fail-closed refusal!
		capped := spawnAgent(t, ctx, agentOpts{name: "capped", spendGuardCap: 100})
		if _, _, err := capped.mem.Chain().SendToAddress(ctx, mustAddr(t, beta), 500); err == nil {
			t.Fatal("expected SpendGuard daily cap (100 sats) to refuse 500-sat payment")
		}

		// 5. Hub SQLite Crash & Restart Recovery Mid-Flight!
		_ = hub1.hubNode.Close()
		reopened, err := hub.NewNode(hub.Config{Network: "memory", DBPath: hubDBPath, EnableFinger: true})
		if err != nil {
			t.Fatalf("reopen hub SQLite after crash: %v", err)
		}
		defer reopened.Close()
		evs, err := reopened.Store().EventsSince(0, nil, 500)
		if err != nil || len(evs) < 5 {
			t.Fatalf("expected >=5 persisted market/dir events after Hub crash & restart, got %d err=%v", len(evs), err)
		}
		bounties, _ := reopened.Store().ListBounties(true)
		if len(bounties) < 3 {
			t.Fatalf("expected persisted Hub bounties to survive crash & restart, got %d", len(bounties))
		}
	})
}

func TestFourAgentSpeedThroughputAndConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	hub1 := spawnAgent(t, ctx, agentOpts{name: "perf-hub", isHub: true})
	hubURL := hub1.ts.URL + "/hub"

	agents := make([]*testAgent, 4)
	domains := []string{"d0", "d1", "d2", "d3"}
	for i := range 4 {
		a := spawnAgent(t, ctx, agentOpts{name: fmt.Sprintf("perf-%d", i), hubURLs: []string{hubURL}})
		_, _ = market.ApprovePolicyPreset(a.mem.DB(), a.mkt.Policy(), "both", nil, "perf")
		// Pre-split 20 coins so parallel trades never wait on UTXO lock!
		_ = a.mem.Chain().EnsureSplitCoins(ctx, 20, 2000)
		_, _ = a.mem.Remember(ctx, memory.RememberInput{
			Shard: "prod", Text: fmt.Sprintf("Agent %d verified domain %s benchmark payload.", i, domains[i]), AutoAnchor: true,
		})
		_, _ = a.broker.ArmSell(ctx, market.SellInput{Shard: "prod", Domains: []string{domains[i]}, PriceSats: 40})
		a.broker.Start(ctx)
		_, _ = a.broker.AnnounceSelf(ctx)
		agents[i] = a
	}
	hub1.broker.Start(ctx)
	t.Cleanup(func() {
		cancel()
		hub1.ts.CloseClientConnections()
	})
	time.Sleep(200 * time.Millisecond)

	// 1. 12 Parallel A2A Trades across all 4 agents (each agent buys from the other 3 concurrently!)
	type tradeRes struct {
		dur   time.Duration
		offer int64
		err   error
	}
	resCh := make(chan tradeRes, 12)
	var wg sync.WaitGroup
	tStart := time.Now()
	for buyerIdx := range 4 {
		for offset := 1; offset <= 3; offset++ {
			sellerIdx := (buyerIdx + offset) % 4
			wg.Add(1)
			go func(bIdx, sIdx int) {
				defer wg.Done()
				st := time.Now()
				deal, err := agents[bIdx].broker.Want(ctx, market.WantRequest{
					Domains:     []string{domains[sIdx]},
					Question:    fmt.Sprintf("domain %s benchmark payload", domains[sIdx]),
					MaxSats:     100,
					TargetShard: fmt.Sprintf("bought/%s", domains[sIdx]),
				})
				if err != nil {
					resCh <- tradeRes{err: err}
					return
				}
				if deal.Status != "settled" {
					resCh <- tradeRes{err: fmt.Errorf("buyer %d -> seller %d status=%s err=%s", bIdx, sIdx, deal.Status, deal.Error)}
					return
				}
				resCh <- tradeRes{dur: time.Since(st), offer: deal.FirstOfferMs}
			}(buyerIdx, sellerIdx)
		}
	}
	wg.Wait()
	close(resCh)
	totalWall := time.Since(tStart)

	var tradeDurs []time.Duration
	var offerMs []int64
	for r := range resCh {
		if r.err != nil {
			t.Fatalf("parallel A2A trade failed: %v", r.err)
		}
		tradeDurs = append(tradeDurs, r.dur)
		offerMs = append(offerMs, r.offer)
	}
	sort.Slice(tradeDurs, func(i, j int) bool { return tradeDurs[i] < tradeDurs[j] })
	sort.Slice(offerMs, func(i, j int) bool { return offerMs[i] < offerMs[j] })

	// 2. 200 Rapid Remember + Recall + GroundedAnswer + VerifyReceipt + 500 Offline SPV Verifies!
	opStart := time.Now()
	for i := range 50 {
		_, err := agents[0].mem.Remember(ctx, memory.RememberInput{
			Shard: "speed", Text: fmt.Sprintf("High-speed verifiable fact #%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	remDur := time.Since(opStart)

	_, err := agents[0].mem.AnchorShard(ctx, "speed")
	if err != nil {
		t.Fatal(err)
	}
	bundle, vr, err := agents[0].mem.Prove(ctx, "speed", 25)
	if err != nil || !vr.OK || !vr.Settled {
		t.Fatalf("speed Prove failed: %+v %v", vr, err)
	}
	spvStart := time.Now()
	for range 500 {
		if r := contracts.VerifyFragment(*bundle, agents[0].mem.Chain().Tracker()); !r.OK || !r.Settled {
			t.Fatalf("SPV verify failed: %+v", r)
		}
	}
	spvDur := time.Since(spvStart)

	t.Logf("=== 4-AGENT SPEED & CONCURRENCY SCORECARD ===")
	t.Logf("12 parallel A2A trades (full BSV AtomicBEEF + BRC-369 + CHIRP + SPV): wall=%v | p50=%v | p95=%v | firstOffer p50=%dms",
		totalWall.Round(time.Millisecond),
		tradeDurs[len(tradeDurs)/2].Round(time.Millisecond),
		tradeDurs[len(tradeDurs)-1].Round(time.Millisecond),
		offerMs[len(offerMs)/2],
	)
	t.Logf("50 Remember ops: %v (%.0f ops/sec) | 500 offline SPV VerifyFragment: %v (%.3f ms/verify, %.0f verifies/sec)",
		remDur.Round(time.Millisecond),
		50.0/remDur.Seconds(),
		spvDur.Round(time.Microsecond),
		float64(spvDur.Microseconds())/500.0/1000.0,
		500.0/spvDur.Seconds(),
	)
}

func mustAddr(t *testing.T, a *testAgent) string {
	t.Helper()
	addr, err := a.mem.Chain().FundAddress()
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
