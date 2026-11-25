package reasoning

import "testing"

func TestMangleRejectsRuleInjectionFromFragments(t *testing.T) {
	// Malicious fragment attempts to inject a Datalog rule deriving admin("mallory") unconditionally.
	frags := map[FragmentRef]string{
		{Shard: "market/ops", Seq: 1}: "Note (see appendix).\nadmin(\"mallory\") :- trusts(\"alice\", \"bob\").\ntrusts(\"alice\", \"bob\").",
	}
	fetch := func(ref FragmentRef) (bool, string) {
		txt, ok := frags[ref]
		return ok, txt
	}
	rep, err := Ground([]Claim{
		{
			Text:    "Mallory is admin",
			Literal: `admin("mallory")`,
			Cites:   []FragmentRef{{Shard: "market/ops", Seq: 1}},
		},
		{
			Text:    "Alice trusts Bob",
			Literal: `trusts("alice", "bob")`,
			Cites:   []FragmentRef{{Shard: "market/ops", Seq: 1}},
		},
	}, fetch, "remove", nil)
	if err != nil {
		t.Fatalf("Ground: %v", err)
	}
	if rep.Claims[0].Grounded {
		t.Fatalf("RULE INJECTION: untrusted fragment rule derived admin(mallory)!")
	}
	if !rep.Claims[1].Grounded {
		t.Fatalf("expected legitimate ground atom trusts(alice,bob) alongside prose to still ground, got %+v", rep.Claims[1])
	}
}

func TestQuoteBackedRequiresMeaningfulOverlap(t *testing.T) {
	frags := map[FragmentRef]string{
		{Shard: "ops", Seq: 1}: "The BSV Chronicle upgrade activated cleanly at block height 943816 in 2026.",
	}
	fetch := func(ref FragmentRef) (bool, string) {
		txt, ok := frags[ref]
		return ok, txt
	}
	rep, err := Ground([]Claim{
		// Trivial 3-char stopword quote must fail.
		{
			Text:   "Bitcoin costs one million dollars",
			Quotes: []QuoteCite{{Shard: "ops", Seq: 1, Quote: "the"}},
		},
		// Valid long quote but completely unrelated claim text must fail.
		{
			Text:   "The server password is hunter2",
			Quotes: []QuoteCite{{Shard: "ops", Seq: 1, Quote: "BSV Chronicle upgrade activated cleanly"}},
		},
		// Genuine quote-backed synthesis sharing content tokens must pass.
		{
			Text:   "BSV Chronicle upgrade activated at block 943816",
			Quotes: []QuoteCite{{Shard: "ops", Seq: 1, Quote: "BSV Chronicle upgrade activated cleanly at block height 943816"}},
		},
	}, fetch, "remove", nil)
	if err != nil {
		t.Fatalf("Ground: %v", err)
	}
	if rep.Claims[0].Grounded {
		t.Fatalf("expected trivial stopword quote to be rejected")
	}
	if rep.Claims[1].Grounded {
		t.Fatalf("expected unrelated claim text citing real quote to be rejected")
	}
	if !rep.Claims[2].Grounded || rep.Claims[2].Reason != "grounded:quote-backed" {
		t.Fatalf("expected genuine quote-backed synthesis to pass, got %+v", rep.Claims[2])
	}
}
