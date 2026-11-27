package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

type stubGraphEval struct {
	fn func(ctx context.Context, req jev.Request) (*jev.Response, error)
}

func (s stubGraphEval) Evaluate(ctx context.Context, req jev.Request) (*jev.Response, error) {
	return s.fn(ctx, req)
}

func f64p(v float64) *float64 { return &v }

const testCorpOntologyYAML = `
version: 1
name: corp
namespace: "https://nektar.run/ont/corp#"
description: "Corporate entity and employment TBox"
classes:
  - id: Agent
    label: Agent
    definition: "An actor capable of employment or ownership."
  - id: Person
    label: Person
    parent: Agent
    disjointWith: [Organization]
    definition: "A human individual."
  - id: Organization
    label: Organization
    parent: Agent
    disjointWith: [Person]
    definition: "A legal corporation or company."
  - id: Location
    label: Location
    definition: "A city or geographic jurisdiction."
relations:
  - id: works_at
    label: "works at"
    definition: "Person is employed by Organization."
    domain: Person
    range: Organization
  - id: headquartered_in
    label: "headquartered in"
    definition: "Organization has its primary headquarters in Location."
    domain: Organization
    range: Location
    functional: true
entities:
  - id: "Alice"
    type: Person
    aliases: ["Alice Chen"]
  - id: "Acme"
    type: Organization
    aliases: ["Acme Corp"]
  - id: "Zurich"
    type: Location
  - id: "Berlin"
    type: Location
thresholds:
  acceptMinConfidenceBp: 7000
  acceptMinMarginBp: 1500
  acceptMinSupportBp: 7000
`

const testPartnerOntologyYAML = `
version: 1
name: partner
namespace: "https://nektar.run/ont/partner#"
description: "External partner taxonomy for cross-ontology bridges"
classes:
  - id: Individual
    label: Individual
    definition: "A human individual person."
  - id: Company
    label: Company
    definition: "A corporate organization or firm."
relations:
  - id: employed_by
    label: "employed by"
    definition: "Individual is employed by Company."
    domain: Individual
    range: Company
`

func newTestOntologyService(t *testing.T, eval jev.Evaluator) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory"})
	if err != nil {
		t.Fatalf("open port: %v", err)
	}
	t.Cleanup(func() { _ = port.Close() })
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{
		JevEvaluator: eval,
	})
	if err != nil {
		t.Fatalf("new memory service: %v", err)
	}
	return svc
}

func corpExtractEval() jev.Evaluator {
	return stubGraphEval{fn: func(_ context.Context, req jev.Request) (*jev.Response, error) {
		st, _ := req.State.(map[string]any)
		if cands, ok := st["candidates"].([]map[string]any); ok {
			answers := make(map[string]jev.Answer, len(cands)*2)
			for _, c := range cands {
				pid := c["id"].(string)
				sub := c["subjectId"].(string)
				obj := c["objectId"].(string)
				switch {
				case sub == "Alice" && obj == "Acme":
					answers["rel_"+pid] = jev.Answer{
						Type:          jev.TypeChoice,
						Choice:        "works_at",
						Confidence:    f64p(0.93),
						Probabilities: jev.ProbabilityMap{"works_at": 0.93, "NONE": 0.07},
					}
					answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: f64p(0.95)}
				case sub == "Acme" && obj == "Zurich":
					answers["rel_"+pid] = jev.Answer{
						Type:          jev.TypeChoice,
						Choice:        "headquartered_in",
						Confidence:    f64p(0.91),
						Probabilities: jev.ProbabilityMap{"headquartered_in": 0.91, "NONE": 0.09},
					}
					answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: f64p(0.94)}
				default:
					answers["rel_"+pid] = jev.Answer{Type: jev.TypeChoice, Choice: "NONE", Confidence: f64p(0.92)}
					answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: f64p(0.05)}
				}
			}
			return &jev.Response{Model: "kev-0.8b", Answers: answers}, nil
		}
		if sc, ok := st["sourceClass"].(map[string]any); ok {
			switch sc["id"].(string) {
			case "Person":
				return &jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"target_class": {Type: jev.TypeChoice, Choice: "Individual", Confidence: f64p(0.95), Probabilities: jev.ProbabilityMap{"Individual": 0.95, "NONE": 0.05}},
						"match_kind":   {Type: jev.TypeChoice, Choice: jevgraph.PredEquivalentClass, Confidence: f64p(0.94)},
						"is_valid":     {Type: jev.TypeNoul, Noul: f64p(0.96)},
					},
				}, nil
			case "Organization":
				return &jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"target_class": {Type: jev.TypeChoice, Choice: "Company", Confidence: f64p(0.93), Probabilities: jev.ProbabilityMap{"Company": 0.93, "NONE": 0.07}},
						"match_kind":   {Type: jev.TypeChoice, Choice: jevgraph.PredEquivalentClass, Confidence: f64p(0.92)},
						"is_valid":     {Type: jev.TypeNoul, Noul: f64p(0.95)},
					},
				}, nil
			default:
				return &jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"target_class": {Type: jev.TypeChoice, Choice: "NONE", Confidence: f64p(0.95)},
						"match_kind":   {Type: jev.TypeChoice, Choice: jevgraph.PredCloseMatch, Confidence: f64p(0.95)},
						"is_valid":     {Type: jev.TypeNoul, Noul: f64p(0.05)},
					},
				}, nil
			}
		}
		if sr, ok := st["sourceRelation"].(map[string]any); ok {
			if sr["id"].(string) == "works_at" {
				return &jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"target_relation": {Type: jev.TypeChoice, Choice: "employed_by", Confidence: f64p(0.94), Probabilities: jev.ProbabilityMap{"employed_by": 0.94, "NONE": 0.06}},
						"match_kind":      {Type: jev.TypeChoice, Choice: jevgraph.PredEquivalentProperty, Confidence: f64p(0.93)},
						"is_valid":        {Type: jev.TypeNoul, Noul: f64p(0.96)},
					},
				}, nil
			}
			return &jev.Response{
				Model: "kev-0.8b",
				Answers: map[string]jev.Answer{
					"target_relation": {Type: jev.TypeChoice, Choice: "NONE", Confidence: f64p(0.95)},
					"match_kind":      {Type: jev.TypeChoice, Choice: jevgraph.PredCloseMatch, Confidence: f64p(0.95)},
					"is_valid":        {Type: jev.TypeNoul, Noul: f64p(0.05)},
				},
			}, nil
		}
		return &jev.Response{Model: "kev-0.8b", Answers: map[string]jev.Answer{}}, nil
	}}
}

func TestOntologyEndToEnd_RememberMPECapsuleGroundingAndForget(t *testing.T) {
	ctx := context.Background()
	svc := newTestOntologyService(t, corpExtractEval())

	// 1. Commit first-class TBox Memory Repo (ont-corp).
	ontInfo, compiled, err := svc.CommitDomainOntology(ctx, "ont-corp", []byte(testCorpOntologyYAML), true)
	if err != nil {
		t.Fatalf("CommitDomainOntology: %v", err)
	}
	if ontInfo.Repo != "ont-corp" || ontInfo.OntologyID != compiled.ID || ontInfo.AnchorTxID == "" {
		t.Fatalf("unexpected ontology info: %+v", ontInfo)
	}

	// 2. Remember a public prose fragment pinned to ont-corp and anchored on BSV (memory chain).
	rem, err := svc.Remember(ctx, RememberInput{
		Shard:      "corp-notes",
		Path:       "notes/alice.md",
		Text:       "Alice Chen works at Acme Corp in Zurich.",
		Public:     true,
		Ontology:   "ont-corp",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("Remember with ontology: %v", err)
	}
	if rem.OntologyID != ontInfo.OntologyID || rem.EdgesRoot == "" {
		t.Fatalf("missing ontologyId/edgesRoot on RememberResult: %+v", rem)
	}
	if rem.Extraction == nil || rem.Extraction.AcceptedCount != 2 {
		t.Fatalf("expected 2 accepted edges, got %+v", rem.Extraction)
	}

	// 3. Verify companion graph sidecar blob exists in the exact same commit TreeObject.
	_, tr, blobs, err := svc.RepoTree(ctx, "corp-notes", "", rem.CommitHash)
	if err != nil {
		t.Fatalf("RepoTree: %v", err)
	}
	var sidecarBlob *memrepo.BlobObject
	for _, e := range tr.Entries {
		if e.Path == "graph/notes/alice.md.edges.json" {
			b := blobs[e.BlobHash]
			sidecarBlob = &b
		}
	}
	if sidecarBlob == nil {
		t.Fatalf("expected graph/notes/alice.md.edges.json in commit tree, got entries: %+v", tr.Entries)
	}
	var parsedSidecar jevgraph.EdgeSidecar
	if err := json.Unmarshal(sidecarBlob.Inline, &parsedSidecar); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if parsedSidecar.EdgesRoot != rem.EdgesRoot {
		t.Fatalf("sidecar edgesRoot %s != result %s", parsedSidecar.EdgesRoot, rem.EdgesRoot)
	}

	// 4. Selective-field L1 MPE disclosure of ontologyId & edgesRoot.
	fp, err := svc.ProveFragmentFields(ctx, "corp-notes", rem.Seq, []string{"ontologyId", "edgesRoot"})
	if err != nil {
		t.Fatalf("ProveFragmentFields(ontologyId, edgesRoot): %v", err)
	}
	if err := core.VerifySelectiveDisclosure(fp.FieldsRoot, fp.Proof); err != nil {
		t.Fatalf("VerifySelectiveDisclosure: %v", err)
	}
	revealed := make(map[string]string, len(fp.Proof.Fields))
	for _, fld := range fp.Proof.Fields {
		revealed[fld.Key] = string(fld.Value)
	}
	if revealed["ontologyId"] != ontInfo.OntologyID || revealed["edgesRoot"] != rem.EdgesRoot {
		t.Fatalf("unexpected disclosed MPE fields: %+v", revealed)
	}

	// 5. Export .nkt Memory Capsule & verify offline including EdgesVerified == 2!
	_, capObj, _, err := svc.ExportCapsule(ctx, "corp-notes", "main", "")
	if err != nil {
		t.Fatalf("ExportCapsule: %v", err)
	}
	verRep, err := memrepo.VerifyCapsule(ctx, capObj, svc.Objects(), svc.Chain().Tracker(), true)
	if err != nil || !verRep.OK {
		t.Fatalf("VerifyCapsule failed: err=%v rep=%+v", err, verRep)
	}
	if verRep.EdgesVerified != 2 {
		t.Fatalf("expected EdgesVerified=2, got %d", verRep.EdgesVerified)
	}

	// 6. GroundedAnswer using prose fragment + TBox subclass axiom (Person -> Agent) & VerifyReceipt!
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:    "Alice is an Agent.",
			Literal: `rdf_type("Alice", "Agent")`,
			Cites:   []reasoning.FragmentRef{{Shard: "corp-notes", Seq: rem.Seq}},
		}},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("GroundedAnswer: %v", err)
	}
	if ga.Ungrounded || ga.ReceiptToken == "" {
		t.Fatalf("expected grounded answer with receipt, got %+v", ga)
	}
	rv, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil || !rv.OK || rv.Verdict == "red" {
		t.Fatalf("VerifyReceipt failed: err=%v rv=%+v", err, rv)
	}
	if rv.DerivationsVerified != 1 {
		t.Fatalf("expected DerivationsVerified=1, got %d (rv=%+v)", rv.DerivationsVerified, rv)
	}

	// 7. Forget fragment and confirm SQLite edges AND sidecar blob are shredded atomically.
	if err := svc.Forget(ctx, "corp-notes", rem.Seq); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	edgesAfter, err := svc.DB().ListKnowledgeEdges(store.KnowledgeEdgeFilter{Shard: "corp-notes", IncludeInactive: true})
	if err != nil {
		t.Fatalf("ListKnowledgeEdges after forget: %v", err)
	}
	if len(edgesAfter) != 0 {
		t.Fatalf("expected 0 knowledge_edges after forget, got %d", len(edgesAfter))
	}
	_, trAfter, _, err := svc.RepoTree(ctx, "corp-notes", "", rem.CommitHash)
	if err != nil {
		t.Fatalf("RepoTree after forget: %v", err)
	}
	for _, e := range trAfter.Entries {
		if e.Path == "graph/notes/alice.md.edges.json" {
			t.Fatalf("expected forgotten graph sidecar to be filtered out of RepoTree")
		}
	}
}

func TestCrossOntologyBridgeGroundingAndReviewGate(t *testing.T) {
	ctx := context.Background()
	svc := newTestOntologyService(t, corpExtractEval())

	if _, _, err := svc.CommitDomainOntology(ctx, "ont-corp", []byte(testCorpOntologyYAML), false); err != nil {
		t.Fatalf("commit ont-corp: %v", err)
	}
	if _, _, err := svc.CommitDomainOntology(ctx, "ont-partner", []byte(testPartnerOntologyYAML), false); err != nil {
		t.Fatalf("commit ont-partner: %v", err)
	}

	// Align ont-corp -> ont-partner and commit Bridge Shard.
	brInfo, manifest, err := svc.AlignAndCommitBridge(ctx, "ont-corp", "ont-partner", "", false)
	if err != nil {
		t.Fatalf("AlignAndCommitBridge: %v", err)
	}
	if brInfo.AcceptedCount < 2 || manifest.AcceptedCount < 2 {
		t.Fatalf("expected >=2 accepted bridge mappings, got info=%+v manifest=%+v", brInfo, manifest)
	}

	// Commit prose fragment under ont-corp (default private CLKA — no public flag!).
	rem, err := svc.Remember(ctx, RememberInput{
		Shard:    "private-intel",
		Text:     "Alice Chen works at Acme Corp.",
		Ontology: "ont-corp",
	})
	if err != nil {
		t.Fatalf("Remember private CLKA with ontology: %v", err)
	}
	if !rem.Encrypted {
		t.Fatalf("expected default CLKA encryption for private fragment")
	}
	// Confirm NO plaintext sidecar blob leaked into mem_objects tree for encrypted fragment!
	_, tr, _, err := svc.RepoTree(ctx, "private-intel", "", rem.CommitHash)
	if err != nil {
		t.Fatalf("RepoTree private-intel: %v", err)
	}
	for _, e := range tr.Entries {
		if strings.HasPrefix(e.Path, "graph/") {
			t.Fatalf("private encrypted fragment must not emit plaintext graph sidecar blob: %s", e.Path)
		}
	}

	// Ground a claim in TARGET ontology vocabulary ("employed_by") via committed Bridge Shard!
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{{
			Text:    "Alice is employed_by Acme.",
			Literal: `triple("Alice", "employed_by", "Acme")`,
			Cites:   []reasoning.FragmentRef{{Shard: "private-intel", Seq: rem.Seq}},
		}},
	})
	if err != nil {
		t.Fatalf("GroundedAnswer across bridge: %v", err)
	}
	if ga.Ungrounded {
		t.Fatalf("expected bridged claim to ground via ont-corp -> ont-partner bridge, report=%+v", ga.Report)
	}
	rv, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil || !rv.OK || rv.Verdict == "red" {
		t.Fatalf("VerifyReceipt across bridge failed: err=%v rv=%+v", err, rv)
	}
}

func TestBorgPrivacyAndReviewPromotionMangleGate(t *testing.T) {
	ctx := context.Background()
	// Start offline (nil evaluator) so blocking produces review candidates cleanly.
	svc := newTestOntologyService(t, nil)
	ontInfo, _, err := svc.CommitDomainOntology(ctx, "ont-corp", []byte(testCorpOntologyYAML), false)
	if err != nil {
		t.Fatalf("commit ont-corp: %v", err)
	}

	// 1. Borg-encrypted fragment: ZERO rows in knowledge_edges and ZERO plaintext sidecar blobs!
	borgRem, err := svc.Remember(ctx, RememberInput{
		Shard:     "secret-corp",
		Text:      "Acme Corp is headquartered in Zurich.",
		BorgGroup: "exec-team",
		Ontology:  "ont-corp",
	})
	if err != nil {
		t.Fatalf("Remember borg with ontology: %v", err)
	}
	if borgRem.OntologyID != ontInfo.OntologyID {
		t.Fatalf("expected ontologyId bound on Borg fragment")
	}
	borgEdges, err := svc.DB().ListKnowledgeEdges(store.KnowledgeEdgeFilter{Shard: "secret-corp", IncludeInactive: true})
	if err != nil {
		t.Fatalf("ListKnowledgeEdges borg: %v", err)
	}
	if len(borgEdges) != 0 {
		t.Fatalf("Borg fragment must NEVER write plaintext rows to knowledge_edges, got %d", len(borgEdges))
	}

	// 2. Non-Borg fragment offline -> review candidate -> accept via ReviewKnowledgeEdge -> conflicting promotion rejected!
	r1, err := svc.Remember(ctx, RememberInput{
		Shard:    "hq-facts",
		Text:     "Acme Corp is headquartered in Zurich.",
		Public:   true,
		Ontology: "ont-corp",
	})
	if err != nil {
		t.Fatalf("Remember r1 offline: %v", err)
	}
	if r1.Extraction == nil || r1.Extraction.ExtractorStatus != "jev_offline" || r1.Extraction.ReviewCount == 0 {
		t.Fatalf("expected jev_offline review candidate, got %+v", r1.Extraction)
	}
	var zurichHash string
	for _, e := range r1.Extraction.Edges {
		if e.Status == jevgraph.EdgeReview && e.Triple.Subject == "Acme" && e.Triple.Predicate == "headquartered_in" && e.Triple.Object == "Zurich" {
			zurichHash = e.Triple.Hash
		}
	}
	if zurichHash == "" {
		t.Fatalf("expected Acme headquartered_in Zurich review candidate, got %+v", r1.Extraction.Edges)
	}
	promoted, err := svc.ReviewKnowledgeEdge(ctx, "hq-facts", r1.Seq, zurichHash, true, "human_verified")
	if err != nil || promoted.Status != "accepted" {
		t.Fatalf("ReviewKnowledgeEdge accept Zurich failed: err=%v row=%+v", err, promoted)
	}

	// Now add second offline fragment with conflicting headquarters (Berlin) -> Mangle functional gate must reject promotion!
	r2, err := svc.Remember(ctx, RememberInput{
		Shard:    "hq-facts",
		Text:     "Acme Corp is headquartered in Berlin.",
		Public:   true,
		Ontology: "ont-corp",
	})
	if err != nil {
		t.Fatalf("Remember r2 offline: %v", err)
	}
	var berlinHash string
	for _, e := range r2.Extraction.Edges {
		if e.Status == jevgraph.EdgeReview && e.Triple.Subject == "Acme" && e.Triple.Predicate == "headquartered_in" && e.Triple.Object == "Berlin" {
			berlinHash = e.Triple.Hash
		}
	}
	if berlinHash == "" {
		t.Fatalf("expected Acme headquartered_in Berlin review candidate, got %+v", r2.Extraction.Edges)
	}
	if _, err := svc.ReviewKnowledgeEdge(ctx, "hq-facts", r2.Seq, berlinHash, true, "bad_promo"); err == nil {
		t.Fatalf("expected Mangle functional conflict gate to reject promoting second headquarters for Acme")
	}
}
