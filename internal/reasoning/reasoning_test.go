package reasoning

import (
	"encoding/hex"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func makeTestDelegation(key *ec.PrivateKey, decision, answer string) (Delegation, error) {
	delegator := hex.EncodeToString(key.PubKey().Compressed())
	msg := delegationDigestV2(delegator, decision, answer)
	sig, err := key.Sign(msg)
	if err != nil {
		return Delegation{}, err
	}
	return Delegation{
		Version:   2,
		Delegator: delegator,
		Decision:  decision,
		Signature: hex.EncodeToString(sig.Serialize()),
	}, nil
}

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
	del, err := makeTestDelegation(key, "verified ops", "BSV Chronicle activated at block 943816")
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
}
