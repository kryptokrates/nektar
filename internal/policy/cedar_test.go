package policy

import "testing"

func TestCedarGoPolicyAndLevelIsolation(t *testing.T) {
	pc, err := Canonicalize("default", DefaultPolicySet)
	if err != nil || len(pc.Hash) != 64 {
		t.Fatalf("Canonicalize: pc=%+v err=%v", pc, err)
	}
	allow, err := Authorize(DefaultPolicySet, Principal{Type: "Agent", ID: "a1", Level: 2}, "read", Resource{
		Type: "Fragment", ID: "f1", Level: 2, Shard: "alpha", Class: "secret",
	})
	if err != nil || !allow.Allow {
		t.Fatalf("expected allow at equal level, got %+v err=%v", allow, err)
	}
	deny, err := Authorize(DefaultPolicySet, Principal{Type: "Agent", ID: "a1", Level: 1}, "read", Resource{
		Type: "Fragment", ID: "f1", Level: 2, Shard: "alpha", Class: "secret",
	})
	if err != nil || deny.Allow {
		t.Fatalf("expected deny below level, got %+v err=%v", deny, err)
	}
	ok, checked, violations, err := ProveLevelIsolation(DefaultPolicySet, 0, 5)
	if err != nil || !ok || checked != 10 || violations != 0 {
		t.Fatalf("ProveLevelIsolation: ok=%v checked=%d violations=%d err=%v", ok, checked, violations, err)
	}

	// Targeted forbid on JevMatcherPrincipal blocks AI egress for internal-only fragments
	// while still permitting local market-seller disclosure.
	egressPol := DefaultPolicySet + `
forbid (
  principal == Agent::"jev-matcher",
  action == Action::"disclose",
  resource
) when {
  resource.class == "internal-only"
};
`
	intRes := Resource{Type: "Fragment", ID: "ops#1", Level: 0, Shard: "ops", Class: "internal-only"}
	if dec, err := Authorize(egressPol, JevMatcherPrincipal, "disclose", intRes); err != nil || dec.Allow {
		t.Fatalf("expected JevMatcherPrincipal forbidden on internal-only, got %+v err=%v", dec, err)
	}
	if dec, err := Authorize(egressPol, Principal{Type: "Agent", ID: "market-seller", Level: 0}, "disclose", intRes); err != nil || !dec.Allow {
		t.Fatalf("expected local market-seller allowed on internal-only, got %+v err=%v", dec, err)
	}

	// Post-match Cedar policy using integer-bps MatchContext attributes.
	marketCedar := `
permit (
  principal == Agent::"jev-matcher",
  action == Action::"buy",
  resource
) when {
  context.relevanceBp >= 7000 && context.confidenceBp >= 7500 && context.riskFlag == "none"
};
forbid (
  principal == Agent::"jev-matcher",
  action == Action::"buy",
  resource
) when {
  context.priceSats > 500 && context.settledDeals == 0
};
`
	offerRes := Resource{Type: "Offer", ID: "off-1", Level: 0, Shard: "ops", Class: "ops"}
	if dec, err := AuthorizeMatch(marketCedar, JevMatcherPrincipal, "buy", offerRes, MatchContext{
		Role: "buyer", RelevanceBP: 8500, ConfidenceBP: 9000, PriceSats: 200, SettledDeals: 0, RiskFlag: "none",
	}); err != nil || !dec.Allow {
		t.Fatalf("expected AuthorizeMatch allow on strong low-price offer, got %+v err=%v", dec, err)
	}
	if dec, err := AuthorizeMatch(marketCedar, JevMatcherPrincipal, "buy", offerRes, MatchContext{
		Role: "buyer", RelevanceBP: 8500, ConfidenceBP: 9000, PriceSats: 600, SettledDeals: 0, RiskFlag: "none",
	}); err != nil || dec.Allow {
		t.Fatalf("expected AuthorizeMatch forbid on >500 sat unproven seller, got %+v err=%v", dec, err)
	}
}
