package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/server"
)

// TestCrossHubSixHubFederationE2E validates a full 6-Hub (1 Spine + 5 Leaf Hubs) federated
// topology with 5 distinct agents (1 per Leaf Hub, including a NAT-only agent on Hub 5):
//   - Cross-hub gossip propagation across Spine + Hub 1..5 via /hub/events and /hub/sync
//   - Competitive policy-filtered cross-hub trade (Hub 1 seller 110s beats Hub 2 seller 220s for Hub 3 buyer <=150s)
//   - Cross-hub NAT-only trade via Hub /bbox store-and-forward relay (Hub 4 <-> Hub 5)
//   - Hub bounty fulfillment + duplicate HDP replay rejection
//   - Full protocol surface (MCP stdio bridge, REST, online & offline receipt verification)
//   - Adversarial fail-closed checks (tampered receipt, 401 auth gate, SpendGuard cap)
func TestCrossHubSixHubFederationE2E(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// 1. Spawn Spine Hub + 5 Leaf Hubs (hub1..hub5), all with isolated SQLite vaults.
	spine := spawnAgent(t, ctx, agentOpts{
		name:          "spine",
		isHub:         true,
		hubDBPath:     filepath.Join(t.TempDir(), "spine.sqlite"),
		apiToken:      "spine-tok",
		operatorToken: "spine-op",
	})
	spineURL := spine.ts.URL + "/hub"

	leaves := make([]*testAgent, 5)
	leafURLs := make([]string, 5)
	for i := range 5 {
		name := fmt.Sprintf("hub%d", i+1)
		leaves[i] = spawnAgent(t, ctx, agentOpts{
			name:          name,
			isHub:         true,
			hubDBPath:     filepath.Join(t.TempDir(), name+".sqlite"),
			apiToken:      name + "-tok",
			operatorToken: name + "-op",
		})
		leafURLs[i] = leaves[i].ts.URL + "/hub"
	}

	// Wire bilateral Spine <-> Leaf federation across all 5 Leaf Hubs!
	spine.hubNode.StartFederation(ctx, leafURLs)
	for i := range 5 {
		leaves[i].hubNode.StartFederation(ctx, []string{spineURL})
	}

	// 2. Spawn 5 Agents — each docked on a DIFFERENT Leaf Hub!
	//    - agent1 on hub1: Seller ("travel" @ 110 sats — policy winner)
	//    - agent2 on hub2: Overpriced Seller ("travel" @ 220 sats — filtered out!) + Bounty Hunter
	//    - agent3 on hub3: Policy Buyer ("travel" <= 150 sats)
	//    - agent4 on hub4: Seller ("prosumer" @ 85 sats)
	//    - agent5 on hub5: NAT-Only Buyer (ZERO inbound HTTP URL -> Hub /bbox relay)
	agent1 := spawnAgent(t, ctx, agentOpts{name: "agent1-h1", hubURLs: []string{leafURLs[0]}})
	agent2 := spawnAgent(t, ctx, agentOpts{name: "agent2-h2", hubURLs: []string{leafURLs[1]}})
	agent3 := spawnAgent(t, ctx, agentOpts{name: "agent3-h3", hubURLs: []string{leafURLs[2]}})
	agent4 := spawnAgent(t, ctx, agentOpts{name: "agent4-h4", hubURLs: []string{leafURLs[3]}})
	agent5NAT := spawnAgent(t, ctx, agentOpts{name: "agent5-h5-nat", hubURLs: []string{leafURLs[4]}, natOnly: true})

	// Cancel SSE/federation goroutines BEFORE httptest servers tear down (t.Cleanup is LIFO).
	t.Cleanup(func() {
		cancel()
		spine.hubNode.CloseSSE()
		spine.ts.CloseClientConnections()
		for _, lf := range leaves {
			lf.hubNode.CloseSSE()
			lf.ts.CloseClientConnections()
		}
	})

	allAgents := []*testAgent{agent1, agent2, agent3, agent4, agent5NAT}
	for _, a := range allAgents {
		if _, err := market.ApprovePolicyPreset(a.mem.DB(), a.mkt.Policy(), "both", nil, "cross-hub-e2e"); err != nil {
			t.Fatalf("%s approve policy: %v", a.name, err)
		}
	}

	// Seed specialized shards & arm sellers on their respective Hubs.
	_, err := agent1.mem.Remember(ctx, memory.RememberInput{
		Shard:      "travel/rebook",
		Text:       "Verified EU261 flight rebook playbook: invoke Article 8 rerouting desk before accepting voucher.",
		Source:     "agent1",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("agent1 remember: %v", err)
	}
	if _, err := agent1.broker.ArmSell(ctx, market.SellInput{Shard: "travel/rebook", Domains: []string{"travel"}, PriceSats: 110}); err != nil {
		t.Fatalf("agent1 ArmSell: %v", err)
	}

	remA2, err := agent2.mem.Remember(ctx, memory.RememberInput{
		Shard:      "travel/expensive",
		Text:       "Overpriced travel summary that exceeds the 150 sat preset buyer policy.",
		Source:     "agent2",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("agent2 remember: %v", err)
	}
	if _, err := agent2.broker.ArmSell(ctx, market.SellInput{Shard: "travel/expensive", Domains: []string{"travel"}, PriceSats: 220}); err != nil {
		t.Fatalf("agent2 ArmSell: %v", err)
	}

	_, err = agent4.mem.Remember(ctx, memory.RememberInput{
		Shard:      "prosumer/deals",
		Text:       "Verified local price-drop playbook: off-peak EV charging windows drop 42% between 01:00 and 05:00.",
		Source:     "agent4",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("agent4 remember: %v", err)
	}
	if _, err := agent4.broker.ArmSell(ctx, market.SellInput{Shard: "prosumer/deals", Domains: []string{"prosumer"}, PriceSats: 85}); err != nil {
		t.Fatalf("agent4 ArmSell: %v", err)
	}

	// Start all 6 Hub brokers + 5 Agent brokers & publish signed `dir` announcements.
	for _, h := range append([]*testAgent{spine}, leaves...) {
		h.broker.Start(ctx)
		_, _ = h.broker.AnnounceSelf(ctx)
	}
	for _, a := range allAgents {
		a.broker.Start(ctx)
		if _, err := a.broker.AnnounceSelf(ctx); err != nil {
			t.Fatalf("%s AnnounceSelf: %v", a.name, err)
		}
	}

	// =========================================================================
	// PHASE 1: CROSS-HUB GOSSIP CONVERGENCE ACROSS ALL 6 HUBS
	// =========================================================================
	t.Run("Phase1_SixHubGossipConvergence", func(t *testing.T) {
		wantPubs := map[string]string{
			agent1.broker.PubHex():    "agent1-h1",
			agent2.broker.PubHex():    "agent2-h2",
			agent3.broker.PubHex():    "agent3-h3",
			agent4.broker.PubHex():    "agent4-h4",
			agent5NAT.broker.PubHex(): "agent5-h5-nat",
		}
		allHubURLs := append([]string{spineURL}, leafURLs...)
		deadline := time.Now().Add(10 * time.Second)
		for idx, hURL := range allHubURLs {
			var found int
			for time.Now().Before(deadline) {
				peers, _ := agent3.disc.DiscoverPeers(ctx, []string{hURL})
				seen := map[string]bool{}
				for _, p := range peers {
					if _, ok := wantPubs[p.Pub]; ok {
						seen[p.Pub] = true
					}
				}
				found = len(seen)
				if found == len(wantPubs) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if found != len(wantPubs) {
				t.Fatalf("hub[%d] (%s) converged only %d/%d cross-hub agents", idx, hURL, found, len(wantPubs))
			}
		}
		// Verify 6 distinct Hub identity pubkeys via /.well-known/agent.json.
		hubPubs := map[string]bool{}
		for _, h := range append([]*testAgent{spine}, leaves...) {
			resp, err := http.Get(h.ts.URL + "/.well-known/agent.json")
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("%s agent.json failed: %v", h.name, err)
			}
			var card map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&card)
			resp.Body.Close()
			pub, _ := card["identityPub"].(string)
			if pub == "" || hubPubs[pub] {
				t.Fatalf("%s missing or duplicate identityPub=%q", h.name, pub)
			}
			hubPubs[pub] = true
		}
		if len(hubPubs) != 6 {
			t.Fatalf("expected 6 distinct Hub identityPubs, got %d", len(hubPubs))
		}
	})

	// =========================================================================
	// PHASE 2: CROSS-HUB POLICY-FILTERED COMPETITIVE TRADE (Hub 3 <- Hub 1, skipping Hub 2)
	// =========================================================================
	t.Run("Phase2_CrossHubPolicyFilteredTrade", func(t *testing.T) {
		deal, err := agent3.broker.Want(ctx, market.WantRequest{
			Domains:     []string{"travel"},
			Question:    "Flight cancelled — fastest EU261 rebook and refund path",
			MaxSats:     150, // MUST filter out agent2 (220s on hub2) and auto-pick agent1 (110s on hub1)!
			TargetShard: "bought/travel",
		})
		if err != nil || deal.Status != "settled" {
			t.Fatalf("cross-hub trade (hub3 <- hub1) failed: err=%v deal=%+v", err, deal)
		}
		if deal.CounterpartyPub != agent1.broker.PubHex() || deal.PriceSats != 110 {
			t.Fatalf("expected hub1 seller agent1 (110 sats) to beat hub2 seller (220 sats), got pub=%s price=%d", deal.CounterpartyPub, deal.PriceSats)
		}
		if !strings.Contains(deal.DeliveredText, "Article 8 rerouting desk") {
			t.Fatalf("unexpected decrypted plaintext: %q", deal.DeliveredText)
		}

		// Ground an answer from the purchased cross-hub shard and verify the nektarr1.* receipt both offline & online!
		ans, err := agent3.mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
			Claims: []reasoning.Claim{{
				Text:  "Verified EU261 flight rebook playbook: invoke Article 8 rerouting desk before accepting voucher.",
				Cites: []reasoning.FragmentRef{{Shard: "bought/travel", Seq: 1}},
			}},
		})
		if err != nil || ans.ReceiptToken == "" {
			t.Fatalf("GroundedAnswer on bought cross-hub shard failed: err=%v ans=%+v", err, ans)
		}
		vr, err := agent3.mem.VerifyReceipt(ctx, ans.ReceiptToken)
		if err != nil || !vr.OK {
			t.Fatalf("offline VerifyReceipt failed: err=%v vr=%+v", err, vr)
		}
		httpVer, err := http.Get(agent3.ts.URL + "/v1/verify/" + ans.ReceiptToken)
		if err != nil || httpVer.StatusCode != http.StatusOK {
			t.Fatalf("HTTP /v1/verify failed: err=%v resp=%+v", err, httpVer)
		}
		var verJSON map[string]any
		_ = json.NewDecoder(httpVer.Body).Decode(&verJSON)
		httpVer.Body.Close()
		if verJSON["ok"] != true {
			t.Fatalf("expected HTTP /v1/verify ok=true, got %+v", verJSON)
		}

		// Adversarial check: tampered receipt MUST fail closed!
		tampered := ans.ReceiptToken[:len(ans.ReceiptToken)-4] + "ZZZZ"
		if tv, _ := agent3.mem.VerifyReceipt(ctx, tampered); tv != nil && tv.OK {
			t.Fatalf("tampered receipt MUST fail closed, got ok=true")
		}
	})

	// =========================================================================
	// PHASE 3: CROSS-HUB NAT-ONLY TRADE VIA /hub/bbox RELAY (Hub 5 NAT <- Hub 4)
	// =========================================================================
	t.Run("Phase3_CrossHubNATBboxRelayTrade", func(t *testing.T) {
		// Agent-5 (docked on Hub 5 with ZERO inbound HTTP URL) buys from Agent-4 (docked on Hub 4)!
		deal, err := agent5NAT.broker.Want(ctx, market.WantRequest{
			Domains:     []string{"prosumer"},
			Question:    "off-peak EV charging windows price drop",
			MaxSats:     120,
			TargetShard: "bought/prosumer",
		})
		if err != nil || deal.Status != "settled" {
			t.Fatalf("cross-hub NAT trade (hub5 <- hub4) failed: err=%v deal=%+v", err, deal)
		}
		if deal.CounterpartyPub != agent4.broker.PubHex() || deal.PriceSats != 85 {
			t.Fatalf("expected hub4 seller agent4 (85 sats), got pub=%s price=%d", deal.CounterpartyPub, deal.PriceSats)
		}
		if !strings.Contains(deal.DeliveredText, "42%") {
			t.Fatalf("unexpected NAT decrypted plaintext: %q", deal.DeliveredText)
		}
	})

	// =========================================================================
	// PHASE 4: HUB BOUNTY PAYOUT + REPLAY REJECTION + MCP STDIO + FAIL-CLOSED AUTH & SPENDGUARD
	// =========================================================================
	t.Run("Phase4_BountiesMCPAndFailClosedSecurity", func(t *testing.T) {
		// 1. Agent-2 claims live Hub bounty on Hub 2 + duplicate replay rejected!
		paid, err := agent2.broker.SellToHub(ctx, leafURLs[1], "bounty-ops", "travel/expensive", remA2.Seq)
		if err != nil || !paid.Accepted || paid.TxID == "" {
			t.Fatalf("agent2 SellToHub on hub2 failed: err=%v res=%+v", err, paid)
		}
		if _, dupErr := agent2.broker.SellToHub(ctx, leafURLs[1], "bounty-ops", "travel/expensive", remA2.Seq); dupErr == nil {
			t.Fatal("expected duplicate bounty replay on hub2 to fail closed")
		}

		// 2. MCP stdio bridge against Agent-3 recalls the cross-hub purchased shard!
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recall","arguments":{"shard":"bought/travel"}}}` + "\n"
		var out bytes.Buffer
		if err := server.RunStdioBridge(ctx, agent3.ts.URL, "", strings.NewReader(in), &out); err != nil {
			t.Fatalf("MCP stdio bridge failed: %v", err)
		}
		if !strings.Contains(out.String(), "Article 8 rerouting desk") {
			t.Fatalf("MCP stdio recall missing cross-hub bought shard: %s", out.String())
		}

		// 3. Unauthenticated operator endpoints on all 6 Hubs MUST return 401!
		for _, h := range append([]*testAgent{spine}, leaves...) {
			r401, err := http.Get(h.ts.URL + "/v1/policies")
			if err != nil {
				t.Fatalf("%s /v1/policies req err: %v", h.name, err)
			}
			r401.Body.Close()
			if r401.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s expected 401 on unauthenticated /v1/policies, got %d", h.name, r401.StatusCode)
			}
		}

		// 4. SpendGuard fail-closed daily cap enforcement!
		capped := spawnAgent(t, ctx, agentOpts{name: "capped", hubURLs: []string{leafURLs[0]}, spendGuardCap: 200})
		_, err = capped.mem.Chain().SpendGuard().Reserve(500, 10)
		if err == nil {
			t.Fatal("expected SpendGuard daily cap breach to fail closed")
		}
		var _ = discovery.PeerInfo{}
		var _ = core.SpendGuardOpts{}
	})
}
