package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

type stubServerGraphEval struct{}

func (stubServerGraphEval) Evaluate(_ context.Context, req jev.Request) (*jev.Response, error) {
	st, _ := req.State.(map[string]any)
	f := func(v float64) *float64 { return &v }
	if cands, ok := st["candidates"].([]map[string]any); ok {
		ans := make(map[string]jev.Answer, len(cands)*2)
		for _, c := range cands {
			pid := c["id"].(string)
			ans["rel_"+pid] = jev.Answer{
				Type:          jev.TypeChoice,
				Choice:        "operates",
				Confidence:    f(0.95),
				Probabilities: jev.ProbabilityMap{"operates": 0.95, "NONE": 0.05},
			}
			ans["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: f(0.96)}
		}
		return &jev.Response{Model: "kev-0.8b", Answers: ans}, nil
	}
	return &jev.Response{Model: "kev-0.8b", Answers: map[string]jev.Answer{}}, nil
}

const serverTestOntologyYAML = `
version: 1
name: infra
namespace: "nektar:ont:infra"
classes:
  - id: Team
    definition: "An operations team."
  - id: Node
    definition: "A blockchain infrastructure node."
relations:
  - id: operates
    domain: Team
    range: Node
    definition: "Team operates Node."
entities:
  - id: "team:sre"
    type: Team
    aliases: ["SRE"]
  - id: "node:hub1"
    type: Node
    aliases: ["Hub1"]
`

func TestToolboxOntologyUnifiedTool(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{
		JevEvaluator: stubServerGraphEval{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mkt := market.NewEngine(mem, market.DefaultPolicy())
	tb := NewToolbox(mem, mkt, nil, nil, nil, "agent")

	if !tb.IsAllowedTool("ontology") {
		t.Fatal("expected ontology tool to be enabled in agent profile")
	}

	// 1. validate
	valRaw, _ := json.Marshal(OntologyToolInput{Action: "validate", YAML: serverTestOntologyYAML})
	valOut, err := tb.Call(ctx, "ontology", valRaw)
	if err != nil {
		t.Fatalf("ontology validate: %v", err)
	}
	valMap := valOut.(map[string]any)
	if valMap["ok"] != true || valMap["ontologyId"] == "" {
		t.Fatalf("unexpected validate output: %+v", valMap)
	}

	// 2. init
	initRaw, _ := json.Marshal(OntologyToolInput{Action: "init", Repo: "ont-infra", YAML: serverTestOntologyYAML})
	if _, err := tb.Call(ctx, "ontology", initRaw); err != nil {
		t.Fatalf("ontology init: %v", err)
	}

	// 3. list & show
	listRaw, _ := json.Marshal(OntologyToolInput{Action: "list"})
	listOut, err := tb.Call(ctx, "ontology", listRaw)
	if err != nil {
		t.Fatalf("ontology list: %v", err)
	}
	if listOut.(map[string]any)["count"].(int) != 1 {
		t.Fatalf("expected 1 ontology in list, got %+v", listOut)
	}

	// 4. remember via Toolbox with ontology -> edges tool lists accepted triple!
	remRaw, _ := json.Marshal(RememberToolInput{
		Shard:    "ops-log",
		Text:     "SRE operates Hub1 in production.",
		Public:   true,
		Ontology: "ont-infra",
	})
	remOut, err := tb.Call(ctx, "remember", remRaw)
	if err != nil {
		t.Fatalf("remember with ontology: %v", err)
	}
	remRes := remOut.(*memory.RememberResult)
	if remRes.EdgesRoot == "" || remRes.Extraction == nil || remRes.Extraction.AcceptedCount != 1 {
		t.Fatalf("expected 1 accepted edge from remember tool, got %+v", remRes)
	}

	edgesRaw, _ := json.Marshal(OntologyToolInput{Action: "edges", Shard: "ops-log", Status: "accepted"})
	edgesOut, err := tb.Call(ctx, "ontology", edgesRaw)
	if err != nil {
		t.Fatalf("ontology edges: %v", err)
	}
	if edgesOut.(map[string]any)["count"].(int) != 1 {
		t.Fatalf("expected 1 accepted edge in ontology edges, got %+v", edgesOut)
	}

	// 5. extract dry-run
	extRaw, _ := json.Marshal(OntologyToolInput{Action: "extract", Ontology: "ont-infra", Text: "SRE operates Hub1."})
	if _, err := tb.Call(ctx, "ontology", extRaw); err != nil {
		t.Fatalf("ontology extract: %v", err)
	}
}
