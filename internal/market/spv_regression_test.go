package market

import (
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
)

func TestSPVMarketPolicyDefaultsLeafEnforcementAndReorgTransitions(t *testing.T) {
	// 1. DefaultPolicy fails closed on counterparty 0-conf (ZeroConfMaxSats == 0) (#7).
	pol := DefaultPolicy()
	perDeal, agg, maxAnc := pol.ZeroConfLimits()
	if perDeal != 0 || agg != 0 || maxAnc <= 0 {
		t.Fatalf("expected DefaultPolicy ZeroConfMaxSats=0 (fail-closed) and positive maxAncestors, got perDeal=%d agg=%d maxAnc=%d", perDeal, agg, maxAnc)
	}

	// 2. RankOffersWithRep rejects OfferAds missing 32-byte hex Leaf on live networks (#3).
	wantLive := bstack.WantAd{WantID: "w-live", Net: "main", MaxSats: 1000}
	ranked := RankOffersWithRep(wantLive, []bstack.OfferAd{
		{OfferID: "bad-empty-leaf", Pub: "02aa", PriceSats: 100, Leaf: ""},
		{OfferID: "good-leaf", Pub: "02bb", PriceSats: 200, Leaf: strings.Repeat("ab", 32)},
	}, nil, nil)
	if len(ranked) != 1 || ranked[0].Offer.OfferID != "good-leaf" {
		t.Fatalf("expected live RankOffersWithRep to filter out empty-Leaf offer, got %+v", ranked)
	}

	// 3. Restate Tx VO supports mined -> reorged rollback AND reorged -> mined revival (#13).
	if !shouldAdvanceTxStatus("mined", "reorged") {
		t.Fatal("expected mined -> reorged rollback edge to be permitted")
	}
	if !shouldAdvanceTxStatus("reorged", "mined") {
		t.Fatal("expected reorged -> mined revival edge to be permitted")
	}
	if shouldAdvanceTxStatus("mined", "broadcast") {
		t.Fatal("expected mined -> broadcast non-reorg regression to remain blocked")
	}
}
