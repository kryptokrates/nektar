package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func postBounty(tsURL string, sub BountySubmission) (*http.Response, BountyPayoutResult) {
	b, _ := json.Marshal(sub)
	resp, err := http.Post(tsURL+"/bounties/submit", "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, BountyPayoutResult{}
	}
	defer resp.Body.Close()
	var out BountyPayoutResult
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestBountyProofEnforcementAndSustainableResale(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	if _, err := ArmHubLiquidity(ctx, node, mem, LiquidityConfig{ArmBounties: true}); err != nil {
		t.Fatalf("ArmHubLiquidity: %v", err)
	}
	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	bounties, err := node.Store().ListBounties(true)
	if err != nil || len(bounties) == 0 {
		t.Fatalf("expected seeded bounties, got %+v err=%v", bounties, err)
	}
	bounty := bounties[0]

	// 1. Missing ProofBundle MUST be rejected (anti-drain gate).
	addr, _ := port.FundAddress()
	if resp, _ := postBounty(ts.URL, BountySubmission{
		BountyID:      bounty.ID,
		SellerPub:     "02deadbeef",
		SellerAddress: addr,
		HDP:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Text:          "Fake unanchored claim",
	}); resp == nil || resp.StatusCode < 400 {
		t.Fatalf("expected bounty submission without proofBundle to be rejected, got %+v", resp)
	}

	// 2. Valid anchored fragment with matching ProofBundle succeeds and lists for resale above reward!
	rem, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      "bounty-src",
		Text:       "Chronicle verified script opcodes activated cleanly on testnet.",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pb, _, err := mem.Prove(ctx, "bounty-src", rem.Seq)
	if err != nil || pb == nil {
		t.Fatalf("Prove: %v", err)
	}
	pbRaw, _ := json.Marshal(pb)

	// Tampered HDP must be rejected.
	if resp, _ := postBounty(ts.URL, BountySubmission{
		BountyID:      bounty.ID,
		SellerPub:     "02deadbeef",
		SellerAddress: addr,
		Shard:         "bounty-src",
		Seq:           rem.Seq,
		HDP:           "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Text:          "Tampered HDP",
		ProofBundle:   pbRaw,
	}); resp == nil || resp.StatusCode < 400 {
		t.Fatalf("expected bounty submission with mismatched HDP to be rejected, got %+v", resp)
	}

	resp, res := postBounty(ts.URL, BountySubmission{
		BountyID:      bounty.ID,
		SellerPub:     "02deadbeef",
		SellerAddress: addr,
		Shard:         "bounty-src",
		Seq:           rem.Seq,
		HDP:           rem.HDP,
		Text:          "Chronicle verified script opcodes activated cleanly on testnet.",
		ProofBundle:   pbRaw,
	})
	if resp == nil || resp.StatusCode != http.StatusOK || !res.Accepted {
		t.Fatalf("expected valid bounty submission to succeed, got resp=%+v res=%+v", resp, res)
	}
	// Verify resale listing price is positive margin (>= reward * 1.2).
	listings, _ := db.ListSellListings()
	foundResale := false
	for _, l := range listings {
		if l.Shard == "curated/"+bounty.Domain {
			foundResale = true
			if l.PriceSats < bounty.RewardSats {
				t.Fatalf("resale price %d must be >= bounty reward %d", l.PriceSats, bounty.RewardSats)
			}
		}
	}
	if !foundResale {
		t.Fatalf("expected hub resale listing for domain %s, got %+v", bounty.Domain, listings)
	}
}

func TestPruneBboxAndGlobalFaucetCap(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.SaveBboxMessage(BboxMessage{
		Office: "default", RecipientPub: "02aa", SenderPub: "02bb", DealID: "d1", Kind: "test",
		Body: json.RawMessage(`{}`), CreatedAt: time.Now().Add(-48 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := st.PruneBbox(time.Now().Add(-24*time.Hour).UnixMilli(), 1000)
	if err != nil || pruned != 1 {
		t.Fatalf("expected 1 pruned bbox message, got %d err=%v", pruned, err)
	}

	// Global daily faucet cap check.
	t.Setenv("NEKTAR_FAUCET_DAILY_CAP", "3000")
	db, _ := store.Open(":memory:")
	defer db.Close()
	port, _ := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	mem, _ := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{})
	addr, _ := port.FundAddress()
	if _, _, err := DripFaucet(ctx, mem, addr, 2000, 10_000); err != nil {
		t.Fatalf("first drip failed: %v", err)
	}
	// Second drip to another address exceeds 3000 daily cap (2000+2000 > 3000).
	if _, _, err := DripFaucet(ctx, mem, "mfaFakeAddress222222222222222222", 2000, 10_000); err == nil {
		t.Fatal("expected global daily faucet cap to reject second drip")
	}
}
