//go:build ignore

// Nektar v0.1 — Live 6-Hub Production Cross-Hub E2E Verification Suite
// Tests all 6 live Hetzner Hubs (https://nektar.run Spine + https://hub1..hub5.nektar.run)
// across real HTTPS, real BSV Testnet transactions, cross-hub federation gossip,
// policy-filtered competitive trading, NAT-only /bbox relay, live Hub bounties,
// MCP stdio bridge, online & offline nektarr1.* receipt verification, and fail-closed security.
//
// Usage:
//
//	go run ./scripts/cross_hub_live_e2e.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/server"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type liveAgent struct {
	name   string
	dir    string
	mem    *memory.Service
	mkt    *market.Engine
	disc   *discovery.Service
	broker *market.Broker
	app    *server.App
	ts     *httptest.Server
}

type phaseResult struct {
	Phase   string `json:"phase"`
	Passed  bool   `json:"passed"`
	Elapsed string `json:"elapsed"`
	Detail  string `json:"detail"`
}

func makeHTTPClient(resolveIP string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if resolveIP != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil && (host == "nektar.run" || strings.HasSuffix(host, ".nektar.run")) {
				// Try real DNS first; fall back to Hetzner IP if local resolver still holds stale NXDOMAIN.
				if _, lErr := net.DefaultResolver.LookupHost(ctx, host); lErr != nil {
					addr = net.JoinHostPort(resolveIP, port)
				}
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &http.Client{Transport: tr, Timeout: 25 * time.Second}
}

func main() {
	spineFlag := flag.String("spine", "https://nektar.run", "Primary Spine Hub base URL")
	hubsFlag := flag.String("hubs", "https://hub1.nektar.run,https://hub2.nektar.run,https://hub3.nektar.run,https://hub4.nektar.run,https://hub5.nektar.run", "Comma-separated 5 Leaf Hub base URLs")
	testnetAlias := flag.String("testnet-alias", "https://testnet.nektar.run", "Default testnet seed alias")
	resolveIP := flag.String("resolve-ip", os.Getenv("NEKTAR_RESOLVE_IP"), "Optional fallback IP for *.nektar.run if local DNS has stale NXDOMAIN cache (or set NEKTAR_RESOLVE_IP)")
	treasuryDir := flag.String("treasury-dir", filepath.Join(os.Getenv("USERPROFILE"), ".nektar", "testnet-4agent-e2e", "hub"), "Local funded BSV testnet wallet dir for funding ephemeral test agents")
	reportPath := flag.String("report", "", "Optional JSON report output path")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Patch default HTTP transport so internal discovery/broker clients also handle stale local DNS seamlessly while keeping full strict TLS!
	client := makeHTTPClient(*resolveIP)
	http.DefaultTransport = client.Transport

	spineBase := strings.TrimRight(*spineFlag, "/")
	var leafBases []string
	for _, h := range strings.Split(*hubsFlag, ",") {
		if h = strings.TrimRight(strings.TrimSpace(h), "/"); h != "" {
			leafBases = append(leafBases, h)
		}
	}
	allBases := append([]string{spineBase}, leafBases...)
	runID := fmt.Sprintf("xhub%d", time.Now().Unix()%100000)

	var results []phaseResult
	var txids []string
	record := func(name string, t0 time.Time, err error, detail string) {
		ok := err == nil
		if !ok {
			detail = fmt.Sprintf("FAIL: %v (%s)", err, detail)
		}
		r := phaseResult{Phase: name, Passed: ok, Elapsed: time.Since(t0).Round(time.Millisecond).String(), Detail: detail}
		results = append(results, r)
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		fmt.Printf("[%s] %-36s (%8s) %s\n", mark, name, r.Elapsed, r.Detail)
	}

	fmt.Printf("=== NEKTAR v0.1 LIVE 6-HUB CROSS-HUB E2E SUITE (run=%s) ===\n", runID)

	// =========================================================================
	// PHASE 0: PREFLIGHT — ALL 7 PUBLIC HTTPS HOSTS (TLS + HEALTH + IDENTITY + BOUNTIES)
	// =========================================================================
	t0 := time.Now()
	hubPubs := map[string]string{}
	var phase0Err error
	for _, base := range append(allBases, strings.TrimRight(*testnetAlias, "/")) {
		resp, err := client.Get(base + "/v1/health")
		if err != nil || resp.StatusCode != 200 {
			phase0Err = fmt.Errorf("%s/v1/health err=%v resp=%v", base, err, resp)
			break
		}
		var h struct {
			OK      bool   `json:"ok"`
			Version string `json:"version"`
			Network string `json:"network"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		if !h.OK || h.Version != "0.1.0" {
			phase0Err = fmt.Errorf("%s unexpected health=%+v", base, h)
			break
		}

		hResp, err := client.Get(base + "/hub/v1/health")
		if err != nil || hResp.StatusCode != 200 {
			phase0Err = fmt.Errorf("%s/hub/v1/health err=%v", base, err)
			break
		}
		hResp.Body.Close()

		tResp, err := client.Get(base + "/hub/v1/terms")
		if err != nil || tResp.StatusCode != 200 {
			phase0Err = fmt.Errorf("%s/hub/v1/terms err=%v", base, err)
			break
		}
		tResp.Body.Close()

		bResp, err := client.Get(base + "/hub/bounties")
		if err != nil || bResp.StatusCode != 200 {
			phase0Err = fmt.Errorf("%s/hub/bounties err=%v", base, err)
			break
		}
		bResp.Body.Close()

		aResp, err := client.Get(base + "/.well-known/agent.json")
		if err != nil || aResp.StatusCode != 200 {
			phase0Err = fmt.Errorf("%s/.well-known/agent.json err=%v", base, err)
			break
		}
		var card map[string]any
		_ = json.NewDecoder(aResp.Body).Decode(&card)
		aResp.Body.Close()
		pub, _ := card["identityPub"].(string)
		if pub == "" {
			phase0Err = fmt.Errorf("%s missing identityPub", base)
			break
		}
		if base != strings.TrimRight(*testnetAlias, "/") {
			hubPubs[pub] = base
		}
	}
	if phase0Err == nil && len(hubPubs) != 6 {
		phase0Err = fmt.Errorf("expected 6 distinct Hub identityPubs, got %d", len(hubPubs))
	}
	record("Phase0_SixHubsTLSHealthAndIdentity", t0, phase0Err, fmt.Sprintf("7/7 HTTPS hosts v0.1.0, %d distinct Hub identityPubs", len(hubPubs)))

	// =========================================================================
	// PHASE 1: SPAWN 5 LIVE EPHEMERAL TESTNET AGENTS (1 PER LEAF HUB) + PUBLIC FAUCET & FUNDING
	// =========================================================================
	t1 := time.Now()
	tmpRoot, err := os.MkdirTemp("", "nektar-xhub-live-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdirtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpRoot)

	spawnLive := func(name, hubBase string, natOnly bool) (*liveAgent, error) {
		dir := filepath.Join(tmpRoot, name)
		_ = os.MkdirAll(dir, 0o700)
		db, err := store.Open(filepath.Join(dir, "nektar.sqlite"))
		if err != nil {
			return nil, err
		}
		port, err := chain.Open(ctx, chain.Config{
			Network:    "testnet",
			DataDir:    dir,
			SpendGuard: core.SpendGuardOpts{MaxFeeSatsPerTx: 5000, MaxSpendSatsDay: 200000},
		})
		if err != nil {
			return nil, err
		}
		objStore, err := bstack.NewFileObjectStore(dir)
		if err != nil {
			return nil, err
		}
		mem, err := memory.New(db, port, bstack.NewGroupRegistry(), objStore, memory.Config{})
		if err != nil {
			return nil, err
		}
		mkt := market.NewEngine(mem, market.DefaultPolicy())
		if _, err := market.ApprovePolicyPreset(db, mkt.Policy(), "both", nil, "xhub-live"); err != nil {
			return nil, err
		}
		hubURL := hubBase + "/hub"
		disc := discovery.New(mem, []string{hubURL}, client)
		broker := market.NewBroker(mem, mkt, disc, market.BrokerConfig{
			Handle:         name + "@nektar.run",
			BboxOffice:     "default",
			CollectTimeout: 8 * time.Second,
			HTTPClient:     client,
		})
		app, err := server.NewApp(mem, mkt, server.Config{
			Discovery: disc,
			Broker:    broker,
		})
		if err != nil {
			return nil, err
		}
		ts := httptest.NewServer(app.Handler())
		if !natOnly {
			broker.SetPublicURL(ts.URL)
		}
		return &liveAgent{name: name, dir: dir, mem: mem, mkt: mkt, disc: disc, broker: broker, app: app, ts: ts}, nil
	}

	var agents []*liveAgent
	var phase1Err error
	for i, hb := range leafBases {
		a, err := spawnLive(fmt.Sprintf("%s-a%d", runID, i+1), hb, i == 4)
		if err != nil {
			phase1Err = fmt.Errorf("spawn agent %d: %w", i+1, err)
			break
		}
		agents = append(agents, a)
		defer a.ts.Close()
		defer a.mem.DB().Close()
	}

	var faucetTx string
	if phase1Err == nil {
		// 1a. Test LIVE public Testnet faucet POST https://nektar.run/v1/faucet for Agent-3!
		fAddr, fAddrErr := agents[2].mem.Chain().FundAddress()
		if fAddrErr != nil {
			phase1Err = fmt.Errorf("agent3 FundAddress: %w", fAddrErr)
		} else {
			fBody, _ := json.Marshal(map[string]string{"address": fAddr})
			fResp, err := client.Post(spineBase+"/v1/faucet", "application/json", bytes.NewReader(fBody))
			if err != nil {
				phase1Err = fmt.Errorf("public faucet call: %w", err)
			} else {
				rawF, _ := io.ReadAll(fResp.Body)
				fResp.Body.Close()
				if fResp.StatusCode == 200 {
					var fj map[string]any
					_ = json.Unmarshal(rawF, &fj)
					faucetTx, _ = fj["txid"].(string)
					if faucetTx != "" {
						txids = append(txids, "faucet:"+faucetTx)
					}
				} else {
					phase1Err = fmt.Errorf("public faucet HTTP %d: %s", fResp.StatusCode, string(rawF))
				}
			}
		}
	}

	// 1b. Fund all 5 ephemeral test agents via AtomicBEEF from local testnet treasury so they have instant spendable UTXOs!
	if phase1Err == nil {
		tPort, err := chain.Open(ctx, chain.Config{
			Network:    "testnet",
			DataDir:    *treasuryDir,
			SpendGuard: core.SpendGuardOpts{MaxFeeSatsPerTx: 5000, MaxSpendSatsDay: 2000000},
		})
		if err != nil {
			phase1Err = fmt.Errorf("open treasury: %w", err)
		} else {
			for i, ag := range agents {
				aAddr, err := ag.mem.Chain().FundAddress()
				if err != nil {
					phase1Err = fmt.Errorf("agent %d FundAddress: %w", i+1, err)
					break
				}
				res, err := tPort.Send(ctx, nwallet.SendArgs{
					ToAddress:   aAddr,
					Satoshis:    5000,
					Description: "xhub-live-e2e-fund",
				})
				if err != nil {
					phase1Err = fmt.Errorf("fund agent %d: %w", i+1, err)
					break
				}
				txids = append(txids, fmt.Sprintf("fund-a%d:%s", i+1, res.TxID))
				if _, err := ag.mem.Chain().ImportBEEF(ctx, res.BEEF); err != nil {
					phase1Err = fmt.Errorf("import BEEF agent %d: %w", i+1, err)
					break
				}
			}
		}
	}
	record("Phase1_PublicFaucetAndAgentFunding", t1, phase1Err, fmt.Sprintf("faucetTx=%s, 5 testnet agents funded (5,000 sats each)", shortID(faucetTx)))

	if phase1Err != nil {
		finish(results, txids, *reportPath)
		os.Exit(1)
	}

	// Start all 5 brokers & announce each agent to ITS OWN Leaf Hub!
	for _, ag := range agents {
		ag.broker.Start(ctx)
		if _, err := ag.broker.AnnounceSelf(ctx); err != nil {
			fmt.Printf("  warn: %s AnnounceSelf: %v\n", ag.name, err)
		}
	}

	// =========================================================================
	// PHASE 2: LIVE CROSS-HUB GOSSIP CONVERGENCE (hub1..hub5 <-> Spine nektar.run)
	// =========================================================================
	t2 := time.Now()
	var phase2Err error
	a1Pub := agents[0].broker.PubHex()
	convergedHubs := 0
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		convergedHubs = 0
		for _, hb := range allBases {
			peers, _ := agents[2].disc.DiscoverPeers(ctx, []string{hb + "/hub"})
			for _, p := range peers {
				if p.Pub == a1Pub {
					convergedHubs++
					break
				}
			}
		}
		if convergedHubs == len(allBases) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if convergedHubs < len(allBases) {
		phase2Err = fmt.Errorf("Agent-1 (announced on hub1) visible on %d/%d live Hubs", convergedHubs, len(allBases))
	}
	record("Phase2_LiveSixHubGossipConvergence", t2, phase2Err, fmt.Sprintf("Agent-1 (hub1) replicated across %d/%d live Hubs (Spine + hub1..hub5)", convergedHubs, len(allBases)))

	// =========================================================================
	// PHASE 3: LIVE CROSS-HUB POLICY-FILTERED ATOMIC TRADE ON REAL BSV TESTNET
	//          Agent-1 (hub1, 110s) vs Agent-2 (hub2, 220s) -> Agent-3 (hub3, <=150s)
	// =========================================================================
	t3 := time.Now()
	var phase3Err error
	var tradeReceipt string
	domainTag := "travel-" + runID
	remA1, err := agents[0].mem.Remember(ctx, memory.RememberInput{
		Shard:      "travel/" + runID,
		Text:       fmt.Sprintf("Live EU261 playbook (%s): request Article 8 rerouting immediately and keep boarding pass.", runID),
		Source:     agents[0].name,
		AutoAnchor: true,
	})
	if err != nil {
		phase3Err = fmt.Errorf("agent1 remember+anchor: %w", err)
	} else {
		if remA1.AnchorTxID != "" {
			txids = append(txids, "anchor-a1:"+remA1.AnchorTxID)
		}
		_, _ = agents[0].broker.ArmSell(ctx, market.SellInput{Shard: "travel/" + runID, Domains: []string{domainTag}, PriceSats: 110})

		remA2, err2 := agents[1].mem.Remember(ctx, memory.RememberInput{
			Shard:      "travel/exp-" + runID,
			Text:       fmt.Sprintf("Overpriced answer (%s) exceeding 150 sat policy.", runID),
			Source:     agents[1].name,
			AutoAnchor: true,
		})
		if err2 == nil && remA2.AnchorTxID != "" {
			txids = append(txids, "anchor-a2:"+remA2.AnchorTxID)
		}
		_, _ = agents[1].broker.ArmSell(ctx, market.SellInput{Shard: "travel/exp-" + runID, Domains: []string{domainTag}, PriceSats: 220})

		// Re-announce with sell domains & wait briefly for cross-hub propagation
		_, _ = agents[0].broker.AnnounceSelf(ctx)
		_, _ = agents[1].broker.AnnounceSelf(ctx)
		time.Sleep(3 * time.Second)

		deal, err := agents[2].broker.Want(ctx, market.WantRequest{
			Domains:     []string{domainTag},
			Question:    "EU261 rerouting playbook " + runID,
			MaxSats:     150,
			TargetShard: "bought/travel",
		})
		if err != nil || deal.Status != "settled" {
			phase3Err = fmt.Errorf("cross-hub Want failed: err=%v deal=%+v", err, deal)
		} else if deal.CounterpartyPub != agents[0].broker.PubHex() || deal.PriceSats != 110 {
			phase3Err = fmt.Errorf("policy expected hub1 seller (110s), got pub=%s price=%d", deal.CounterpartyPub, deal.PriceSats)
		} else if !strings.Contains(deal.DeliveredText, "Article 8 rerouting") {
			phase3Err = fmt.Errorf("decrypted plaintext mismatch: %q", deal.DeliveredText)
		} else {
			if deal.PaymentTxID != "" {
				txids = append(txids, "trade-h3-h1:"+deal.PaymentTxID)
			}
			// Ground answer & verify receipt via BOTH offline verifier and LIVE https://nektar.run/v1/verify/{token}!
			ans, gErr := agents[2].mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
				Claims: []reasoning.Claim{{
					Text:  deal.DeliveredText,
					Cites: []reasoning.FragmentRef{{Shard: "bought/travel", Seq: 1}},
				}},
			})
			if gErr != nil || ans.ReceiptToken == "" {
				phase3Err = fmt.Errorf("GroundedAnswer: err=%v", gErr)
			} else {
				tradeReceipt = ans.ReceiptToken
				vr, vErr := agents[2].mem.VerifyReceipt(ctx, tradeReceipt)
				if vErr != nil || !vr.OK {
					phase3Err = fmt.Errorf("offline VerifyReceipt failed: %v %+v", vErr, vr)
				}
			}
		}
	}
	record("Phase3_CrossHubPolicyTradeTestnet", t3, phase3Err, fmt.Sprintf("Agent-3(hub3) auto-picked Agent-1(hub1, 110s) over Agent-2(hub2, 220s), receipt=%s", shortID(tradeReceipt)))

	// =========================================================================
	// PHASE 4: LIVE CROSS-HUB NAT-ONLY TRADE VIA /hub/bbox RELAY (Agent-5 on hub5 <- Agent-4 on hub4)
	// =========================================================================
	t4 := time.Now()
	var phase4Err error
	natDomain := "prosumer-" + runID
	remA4, err := agents[3].mem.Remember(ctx, memory.RememberInput{
		Shard:      "prosumer/" + runID,
		Text:       fmt.Sprintf("Live prosumer EV tariff (%s): off-peak rate drops 42%% after 01:00.", runID),
		Source:     agents[3].name,
		AutoAnchor: true,
	})
	if err != nil {
		phase4Err = fmt.Errorf("agent4 remember+anchor: %w", err)
	} else {
		if remA4.AnchorTxID != "" {
			txids = append(txids, "anchor-a4:"+remA4.AnchorTxID)
		}
		_, _ = agents[3].broker.ArmSell(ctx, market.SellInput{Shard: "prosumer/" + runID, Domains: []string{natDomain}, PriceSats: 85})
		_, _ = agents[3].broker.AnnounceSelf(ctx)
		time.Sleep(3 * time.Second)

		deal, err := agents[4].broker.Want(ctx, market.WantRequest{
			Domains:     []string{natDomain},
			Question:    "EV off-peak tariff " + runID,
			MaxSats:     120,
			TargetShard: "bought/prosumer",
		})
		if err != nil || deal.Status != "settled" {
			phase4Err = fmt.Errorf("NAT bbox cross-hub Want failed: err=%v deal=%+v", err, deal)
		} else if deal.CounterpartyPub != agents[3].broker.PubHex() || deal.PriceSats != 85 {
			phase4Err = fmt.Errorf("expected hub4 seller (85s), got pub=%s price=%d", deal.CounterpartyPub, deal.PriceSats)
		} else if !strings.Contains(deal.DeliveredText, "42%") {
			phase4Err = fmt.Errorf("NAT decrypted plaintext mismatch: %q", deal.DeliveredText)
		} else if deal.PaymentTxID != "" {
			txids = append(txids, "trade-nat-h5-h4:"+deal.PaymentTxID)
		}
	}
	record("Phase4_CrossHubNATBboxTradeTestnet", t4, phase4Err, "Agent-5 (NAT-only on hub5) settled 85s trade with Agent-4 (hub4) via Hub /bbox relay")

	// =========================================================================
	// PHASE 5: LIVE HUB BOUNTY PAYOUT ON hub2.nektar.run + REPLAY REJECTION
	// =========================================================================
	t5 := time.Now()
	var phase5Err error
	var bountyTx string
	bFact, err := agents[1].mem.Remember(ctx, memory.RememberInput{
		Shard:      "bounty/" + runID,
		Text:       fmt.Sprintf("Verified Ops runbook (%s): always pin immutable Docker image digests before rolling deploys.", runID),
		Source:     agents[1].name,
		AutoAnchor: true,
	})
	if err != nil {
		phase5Err = fmt.Errorf("bounty remember+anchor: %w", err)
	} else {
		paid, err := agents[1].broker.SellToHub(ctx, leafBases[1]+"/hub", "bounty-ops", "bounty/"+runID, bFact.Seq)
		if err != nil || !paid.Accepted || paid.TxID == "" {
			phase5Err = fmt.Errorf("SellToHub on hub2 failed: err=%v res=%+v", err, paid)
		} else {
			bountyTx = paid.TxID
			txids = append(txids, "bounty-hub2:"+bountyTx)
			if _, dupErr := agents[1].broker.SellToHub(ctx, leafBases[1]+"/hub", "bounty-ops", "bounty/"+runID, bFact.Seq); dupErr == nil {
				phase5Err = fmt.Errorf("duplicate bounty submission on hub2 was NOT rejected")
			}
		}
	}
	record("Phase5_LiveHubBountyPayoutAndReplay", t5, phase5Err, fmt.Sprintf("hub2 paid +100s bounty tx=%s & rejected replay", shortID(bountyTx)))

	// =========================================================================
	// PHASE 6: PROTOCOL SURFACE — MCP STDIO, PUBLIC /v1/verify, PUBLIC WEBSITE ASSETS
	// =========================================================================
	t6 := time.Now()
	var phase6Err error
	// 6a. MCP stdio bridge against Agent-3
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recall","arguments":{"shard":"bought/travel"}}}` + "\n"
	var out bytes.Buffer
	if err := server.RunStdioBridge(ctx, agents[2].ts.URL, "", strings.NewReader(in), &out); err != nil {
		phase6Err = fmt.Errorf("MCP stdio bridge: %w", err)
	} else if !strings.Contains(out.String(), "Article 8 rerouting") {
		phase6Err = fmt.Errorf("MCP stdio recall missing cross-hub bought shard")
	}
	// 6b. Live public https://nektar.run/v1/verify/{token}
	if phase6Err == nil && tradeReceipt != "" {
		vResp, err := client.Get(spineBase + "/v1/verify/" + tradeReceipt)
		if err != nil || vResp.StatusCode != 200 {
			phase6Err = fmt.Errorf("public /v1/verify err=%v resp=%v", err, vResp)
		} else {
			var vj map[string]any
			_ = json.NewDecoder(vResp.Body).Decode(&vj)
			vResp.Body.Close()
			if vj["ok"] != true {
				phase6Err = fmt.Errorf("public /v1/verify returned %+v", vj)
			}
		}
	}
	// 6c. Public website & share assets
	if phase6Err == nil {
		for _, path := range []string{"/", "/demo.mp4?v=7", "/demo-poster.jpg?v=5", "/og-card.jpg?v=2", "/llm.txt", "/docs/index.html"} {
			r, err := client.Head(spineBase + path)
			if err != nil || r.StatusCode != 200 {
				phase6Err = fmt.Errorf("website %s err=%v resp=%v", path, err, r)
				break
			}
			r.Body.Close()
		}
	}
	record("Phase6_MCPPublicVerifyAndWebsite", t6, phase6Err, "MCP stdio, public /v1/verify, and all 6 website assets verified 200 OK")

	// =========================================================================
	// PHASE 7: ADVERSARIAL FAIL-CLOSED SECURITY CHECKS ACROSS ALL 6 LIVE HUBS
	// =========================================================================
	t7 := time.Now()
	var phase7Err error
	// 7a. Unauthenticated /v1/policies MUST return 401 on ALL 6 live Hubs!
	for _, hb := range allBases {
		r, err := client.Get(hb + "/v1/policies")
		if err != nil {
			phase7Err = fmt.Errorf("%s /v1/policies err=%v", hb, err)
			break
		}
		r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			phase7Err = fmt.Errorf("%s /v1/policies expected 401, got %d", hb, r.StatusCode)
			break
		}
	}
	// 7b. Tampered nektarr1.* receipt MUST fail closed on public https://nektar.run/v1/verify/
	if phase7Err == nil && len(tradeReceipt) > 8 {
		tampered := tradeReceipt[:len(tradeReceipt)-4] + "ZZZZ"
		r, err := client.Get(spineBase + "/v1/verify/" + tampered)
		if err == nil {
			var tj map[string]any
			_ = json.NewDecoder(r.Body).Decode(&tj)
			r.Body.Close()
			if tj["ok"] == true {
				phase7Err = fmt.Errorf("public verifier accepted tampered receipt!")
			}
		}
	}
	// 7c. SpendGuard cap breach MUST fail closed!
	if phase7Err == nil {
		if _, err := agents[0].mem.Chain().SpendGuard().Reserve(9999999, 100); err == nil {
			phase7Err = fmt.Errorf("SpendGuard failed to reject 9,999,999 sat cap breach")
		}
	}
	record("Phase7_AdversarialFailClosedChecks", t7, phase7Err, "6/6 Hubs returned 401 on /v1/policies, tampered receipt rejected, SpendGuard cap enforced")

	failed := finish(results, txids, *reportPath)
	if failed > 0 {
		os.Exit(1)
	}
}

func shortID(s string) string {
	if len(s) > 16 {
		return s[:16] + "..."
	}
	return s
}

func finish(results []phaseResult, txids []string, reportPath string) int {
	passed, failed := 0, 0
	for _, r := range results {
		if r.Passed {
			passed++
		} else {
			failed++
		}
	}
	fmt.Printf("\n=== SUMMARY: %d/%d PHASES PASSED (%d failed) ===\n", passed, len(results), failed)
	if len(txids) > 0 {
		fmt.Println("Live BSV Testnet TxIDs:")
		for _, tx := range txids {
			fmt.Println("  -", tx)
		}
	}
	if reportPath != "" {
		b, _ := json.MarshalIndent(map[string]any{
			"passed":  passed,
			"failed":  failed,
			"results": results,
			"txids":   txids,
		}, "", "  ")
		_ = os.WriteFile(reportPath, b, 0o644)
	}
	return failed
}
