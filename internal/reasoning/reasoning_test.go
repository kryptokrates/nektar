package reasoning

import (
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func TestMangleGroundingAORGAndReceipts(t *testing.T) {
	// 1. Mangle Datalog entailment.
	entailed, err := Entails(
		[]string{`trusts("alice", "bob")`, `trusts("bob", "carol")`},
		[]string{`reachable(X, Y) :- trusts(X, Y).`, `reachable(X, Z) :- trusts(X, Y), reachable(Y, Z).`},
		`reachable("alice", "carol")`,
	)
	if err != nil || !entailed {
		t.Fatalf("expected Mangle transitive entailment, entailed=%v err=%v", entailed, err)
	}

	// 2. Grounding: quote + Mangle entailment + uncited claim stripping!
	frags := map[FragmentRef]string{
		{Shard: "ops", Seq: 1}: "BSV Chronicle activated at block 943816.",
		{Shard: "ops", Seq: 2}: `trusts("alice", "bob")` + "\n" + `trusts("bob", "carol")`,
	}
	fetch := func(ref FragmentRef) (bool, string) {
		txt, ok := frags[ref]
		return ok, txt
	}
	report, err := Ground([]Claim{
		{Text: "BSV Chronicle activated at block 943816", Cites: []FragmentRef{{Shard: "ops", Seq: 1}}},
		{Text: "Alice transitively reaches Carol", Literal: `reachable("alice", "carol")`, Cites: []FragmentRef{{Shard: "ops", Seq: 2}}},
		{Text: "Hallucinated unsupported claim", Cites: nil},
	}, fetch, "remove", []string{
		`reachable(X, Y) :- trusts(X, Y).`,
		`reachable(X, Z) :- trusts(X, Y), reachable(Y, Z).`,
	})
	if err != nil {
		t.Fatalf("Ground: %v", err)
	}
	if len(report.Kept) != 2 || len(report.Flagged) != 1 {
		t.Fatalf("expected 2 kept / 1 stripped, got kept=%d flagged=%d", len(report.Kept), len(report.Flagged))
	}

	// 3. Receipt token roundtrip + §16 delegation verification.
	key, _ := ec.NewPrivateKey()
	del, err := MakeDelegation(key, "verified ops", "BSV Chronicle activated at block 943816")
	if err != nil {
		t.Fatal(err)
	}
	rcp := Receipt{
		Answer:              "BSV Chronicle activated at block 943816",
		FragmentRefs:        []FragmentRef{{Shard: "ops", Seq: 1}},
		CheckResult:         "verified",
		ModelID:             ModelIdentity{Kind: "api", Provider: "anthropic", Release: "claude"},
		Timestamp:           1730000000,
		GroundingReportHash: report.ReportHash,
		TruthNote:           TruthNote,
		Delegations:         []Delegation{del},
	}
	if err := VerifyDelegations(rcp); err != nil {
		t.Fatalf("VerifyDelegations: %v", err)
	}
	h, err := ReceiptHash(rcp)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ReceiptToken(h)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseReceiptToken(tok)
	if err != nil || parsed != h {
		t.Fatalf("ParseReceiptToken mismatch: got %s want %s err=%v", parsed, h, err)
	}

	// 4. Bi-temporal AORG with Mangle justifiability.
	g := NewAORGraph([]string{`derived_fact(X) :- episode_fact(X).`})
	ep, err := g.Append(Vertex{Kind: "episode", Phi: `episode_fact("v1")`, LogPos: 1, Context: []string{"ops"}}, nil, "observe")
	if err != nil {
		t.Fatalf("append episode: %v", err)
	}
	der, err := g.Append(Vertex{Kind: "derived", Phi: `derived_fact("v1")`, LogPos: 2, Context: []string{"ops"}}, []string{ep.H}, "r1")
	if err != nil {
		t.Fatalf("append derived: %v", err)
	}
	if _, err := g.Append(Vertex{Kind: "refinement", Phi: `episode_fact("v2")`, LogPos: 3, Refines: ep.H, Context: []string{"ops"}}, []string{ep.H}, "refine"); err != nil {
		t.Fatalf("append refinement: %v", err)
	}
	if len(g.CurrentView(2)) != 2 {
		t.Fatalf("at logPos 2 expected 2 active vertices (ep + der), got %d", len(g.CurrentView(2)))
	}
	if len(g.CurrentView(3)) != 2 {
		t.Fatalf("at logPos 3 expected ep invalidated (der + refinement active), got %d (der=%s)", len(g.CurrentView(3)), der.H)
	}
}
