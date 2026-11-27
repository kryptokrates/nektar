package market

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

type stubMarketGraphEval struct{}

func (stubMarketGraphEval) Evaluate(_ context.Context, req jev.Request) (*jev.Response, error) {
	st, _ := req.State.(map[string]any)
	f := func(v float64) *float64 { return &v }
	if cands, ok := st["candidates"].([]map[string]any); ok {
		ans := make(map[string]jev.Answer, len(cands)*2)
		for _, c := range cands {
			pid := c["id"].(string)
			ans["rel_"+pid] = jev.Answer{
				Type:          jev.TypeChoice,
				Choice:        "works_at",
				Confidence:    f(0.94),
				Probabilities: jev.ProbabilityMap{"works_at": 0.94, "NONE": 0.06},
			}
			ans["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: f(0.96)}
		}
		return &jev.Response{Model: "kev-0.8b", Answers: ans}, nil
	}
	return &jev.Response{Model: "kev-0.8b", Answers: map[string]jev.Answer{}}, nil
}

const marketOntAYAML = `
version: 1
name: mkt-alpha
namespace: "nektar:ont:mkt-alpha"
classes:
  - id: Person
    definition: "A human individual."
  - id: Org
    definition: "An organization."
relations:
  - id: works_at
    domain: Person
    range: Org
    definition: "Person works at Org."
entities:
  - id: "Alice"
    type: Person
  - id: "Acme"
    type: Org
`

const marketOntBYAML = `
version: 1
name: mkt-beta
namespace: "nektar:ont:mkt-beta"
classes:
  - id: Human
    definition: "A human individual."
  - id: Firm
    definition: "A corporate firm."
relations:
  - id: employed_by
    domain: Human
    range: Firm
    definition: "Human employed by Firm."
`

func newMarketOntologyMem(t *testing.T) *memory.Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = port.Close() })
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{
		JevEvaluator: stubMarketGraphEval{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mem
}

func TestStandingOrderOntologyMatchingAndBridge(t *testing.T) {
	ctx := context.Background()
	mem := newMarketOntologyMem(t)

	infoA, _, err := mem.CommitDomainOntology(ctx, "ont-mkt-alpha", []byte(marketOntAYAML), false)
	if err != nil {
		t.Fatalf("commit ontA: %v", err)
	}
	infoB, _, err := mem.CommitDomainOntology(ctx, "ont-mkt-beta", []byte(marketOntBYAML), false)
	if err != nil {
		t.Fatalf("commit ontB: %v", err)
	}

	// 1. Unpinned StandingOrder matches any offer (empty or typed).
	if !standingOrderOntologyMatches(ctx, mem, "", false, "") {
		t.Fatal("unpinned standing order must match untyped offer")
	}
	if !standingOrderOntologyMatches(ctx, mem, "", false, infoA.OntologyID) {
		t.Fatal("unpinned standing order must match typed offer")
	}

	// 2. Pinned StandingOrder rejects untyped and foreign offers without a bridge.
	if standingOrderOntologyMatches(ctx, mem, infoB.OntologyID, false, "") {
		t.Fatal("pinned standing order must reject untyped offer")
	}
	if standingOrderOntologyMatches(ctx, mem, infoB.OntologyID, true, infoA.OntologyID) {
		t.Fatal("pinned standing order must reject foreign ontology before bridge is committed")
	}
	if !standingOrderOntologyMatches(ctx, mem, infoB.OntologyID, false, infoB.OntologyID) {
		t.Fatal("pinned standing order must accept exact ontologyId match")
	}

	// 3. Commit a Bridge Shard ontA -> ontB; now AllowBridge=true accepts ontA offers while AllowBridge=false still rejects!
	tr, _ := reasoning.FinalizeTriple(reasoning.Triple{
		Subject:   "works_at",
		Predicate: jevgraph.PredEquivalentProperty,
		Object:    "employed_by",
	})
	if _, err := mem.CommitBridgeManifest(ctx, "", &jevgraph.BridgeManifest{
		Version:          1,
		SourceOntologyID: infoA.OntologyID,
		TargetOntologyID: infoB.OntologyID,
		Mappings: []jevgraph.BridgeMapping{{
			Kind:   "relation",
			Triple: tr,
			Status: jevgraph.EdgeAccepted,
		}},
	}, false); err != nil {
		t.Fatalf("CommitBridgeManifest: %v", err)
	}

	if standingOrderOntologyMatches(ctx, mem, infoB.OntologyID, false, infoA.OntologyID) {
		t.Fatal("AllowBridge=false must still reject foreign ontology even when bridge exists")
	}
	if !standingOrderOntologyMatches(ctx, mem, infoB.OntologyID, true, infoA.OntologyID) {
		t.Fatal("AllowBridge=true must accept foreign ontology when committed Bridge Shard exists")
	}
}

func TestSellerAutoQuoteOntologyFilteringAndOfferStamping(t *testing.T) {
	ctx := context.Background()
	mem := newMarketOntologyMem(t)

	infoA, _, err := mem.CommitDomainOntology(ctx, "ont-mkt-alpha", []byte(marketOntAYAML), false)
	if err != nil {
		t.Fatalf("commit ontA: %v", err)
	}
	infoB, _, err := mem.CommitDomainOntology(ctx, "ont-mkt-beta", []byte(marketOntBYAML), false)
	if err != nil {
		t.Fatalf("commit ontB: %v", err)
	}

	// Commit a typed fragment under ontA and anchor it on the memory chain.
	rem, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      "alpha-facts",
		Text:       "Alice works at Acme.",
		Public:     true,
		Ontology:   "ont-mkt-alpha",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("Remember alpha-facts: %v", err)
	}
	if rem.EdgesRoot == "" || rem.OntologyID != infoA.OntologyID {
		t.Fatalf("expected edgesRoot & ontologyId, got %+v", rem)
	}

	eng := NewEngine(mem, DefaultPolicy())
	disc := discovery.New(mem, nil, nil)
	br := NewBroker(mem, eng, disc, BrokerConfig{IsHub: true})
	defer br.Stop()

	if _, err := br.ArmSell(ctx, SellInput{
		Shard:       "alpha-facts",
		Domains:     []string{"corp"},
		PriceSats:   50,
		Description: "Alpha typed corporate facts",
		AllowRaw:    true,
	}); err != nil {
		t.Fatalf("ArmSell: %v", err)
	}

	// 1. Want pinned to ontB (no bridge) -> seller MUST NOT quote (0 pendingSales)!
	wantB := bstack.WantAd{
		WantID:     "want-ont-b",
		Pub:        "02" + "1111111111111111111111111111111111111111111111111111111111111111",
		Net:        "memory",
		Domains:    []string{"corp"},
		Query:      "Alice works at Acme",
		OntologyID: infoB.OntologyID,
		MaxSats:    100,
		Exp:        time.Now().Add(10 * time.Minute).UnixMilli(),
	}
	br.autoQuoteWant(ctx, wantB)
	br.mu.RLock()
	nAfterB := len(br.pendingSales)
	br.mu.RUnlock()
	if nAfterB != 0 {
		t.Fatalf("expected seller to skip quoting ontA fragment for ontB want, got %d pendingSales", nAfterB)
	}

	// 2. Want pinned to ontA -> seller quotes & stamps OfferAd.OntologyID + OfferAd.EdgesRoot!
	wantA := wantB
	wantA.WantID = "want-ont-a"
	wantA.OntologyID = infoA.OntologyID
	br.autoQuoteWant(ctx, wantA)
	br.mu.RLock()
	nAfterA := len(br.pendingSales)
	br.mu.RUnlock()
	if nAfterA != 1 {
		t.Fatalf("expected seller to quote 1 pendingSale for matching ontA want, got %d", nAfterA)
	}

	// Also verify GetFragmentGraphMeta preserved exact committed ontologyId & edgesRoot.
	gotOnt, gotRoot, err := mem.DB().GetFragmentGraphMeta("alpha-facts", rem.Seq)
	if err != nil || gotOnt != infoA.OntologyID || gotRoot != rem.EdgesRoot {
		t.Fatalf("GetFragmentGraphMeta mismatch: ont=%s root=%s err=%v", gotOnt, gotRoot, err)
	}
	_ = json.Marshal
}
