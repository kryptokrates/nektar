package jevgraph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

const sampleBSVOpsYAML = `
version: 1
name: bsv-ops
namespace: "nektar:ont:bsv-ops:v1"
description: "Operational facts for BSV nodes, covenants, incidents, and mitigations."
classes:
  - id: Service
    label: "Service"
    definition: "A running software daemon, overlay hub, or infrastructure node."
  - id: Covenant
    label: "Covenant"
    parent: Service
    definition: "An on-chain Runar Bitcoin Script contract template."
  - id: Incident
    label: "Incident"
    disjointWith: [Service]
    definition: "An outage, vulnerability, or operational failure."
  - id: Mitigation
    label: "Mitigation"
    disjointWith: [Incident]
    definition: "A patch, config change, or runbook step resolving an incident."
  - id: Region
    label: "Region"
    disjointWith: [Service, Incident]
    definition: "A physical or cloud deployment region."
relations:
  - id: mitigates
    label: "mitigates"
    domain: Mitigation
    range: Incident
    definition: "The subject mitigation resolves, patches, or prevents the object incident."
  - id: affects
    label: "affects"
    domain: Incident
    range: Service
    definition: "The subject incident degrades, breaks, or impacts the object service."
  - id: depends_on
    label: "depends_on"
    domain: Service
    range: Service
    transitive: true
    definition: "The subject service requires the object service to function."
  - id: hosted_in
    label: "hosted_in"
    domain: Service
    range: Region
    functional: true
    definition: "The subject service's primary deployment region."
entities:
  - id: "svc:teranode"
    type: Service
    aliases: ["Teranode", "teranode-node"]
  - id: "cov:nektar-anchor"
    type: Covenant
    aliases: ["NektarAnchor", "nektar-anchor"]
  - id: "inc:utxo-stall"
    type: Incident
    aliases: ["UTXO stall", "anchor stall"]
  - id: "mit:fee-bump"
    type: Mitigation
    aliases: ["fee-bump patch", "CPFP fee bump"]
  - id: "reg:eu-central"
    type: Region
    aliases: ["eu-central-1", "Frankfurt"]
  - id: "reg:us-east"
    type: Region
    aliases: ["us-east-1", "Virginia"]
`

type stubEval struct {
	fn func(ctx context.Context, req jev.Request) (*jev.Response, error)
}

func (s stubEval) Evaluate(ctx context.Context, req jev.Request) (*jev.Response, error) {
	return s.fn(ctx, req)
}

func fptr(v float64) *float64 { return &v }

func TestOntologyParseCompileAndIDInvariance(t *testing.T) {
	ont1, err := ParseOntologyYAML([]byte(sampleBSVOpsYAML))
	if err != nil {
		t.Fatalf("ParseOntologyYAML: %v", err)
	}
	comp1, err := Compile(ont1)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(comp1.ID) != 64 {
		t.Fatalf("expected 64-hex OntologyID, got %q", comp1.ID)
	}
	if !comp1.IsSubtype("Covenant", "Service") {
		t.Fatal("expected Covenant to be subtype of Service")
	}
	// Subtype closure: Incident -> Covenant must inherit Incident -> Service relation `affects`.
	rels := comp1.AllowedRelations("Incident", "Covenant")
	if len(rels) != 1 || rels[0].ID != "affects" {
		t.Fatalf("expected [affects] via Covenant <: Service, got %+v", rels)
	}

	// Changing local gazetteer entities or local thresholds MUST NOT change canonical OntologyID!
	ont2, _ := ParseOntologyYAML([]byte(sampleBSVOpsYAML))
	ont2.Entities = append(ont2.Entities, EntityEntry{ID: "svc:extra", Type: "Service", Aliases: []string{"ExtraSvc"}})
	ont2.Thresholds = &Thresholds{AcceptMinConfidenceBP: 8200, AcceptMinMarginBP: 2000}
	id2, err := ComputeOntologyID(ont2)
	if err != nil {
		t.Fatalf("ComputeOntologyID(ont2): %v", err)
	}
	if id2 != comp1.ID {
		t.Fatalf("OntologyID drifted on local gazetteer/threshold change: %s != %s", id2, comp1.ID)
	}

	// Cycle in class hierarchy MUST be rejected.
	cyclic := `
version: 1
name: cyclic
classes:
  - id: A
    parent: B
    definition: "A"
  - id: B
    parent: A
    definition: "B"
relations: []
`
	if _, err := ParseOntologyYAML([]byte(cyclic)); !errors.Is(err, ErrOntologyCycle) {
		t.Fatalf("expected ErrOntologyCycle, got %v", err)
	}
}

func TestBlockingAndFourStagePipelineWithMangleGate(t *testing.T) {
	ont, err := ParseOntologyYAML([]byte(sampleBSVOpsYAML))
	if err != nil {
		t.Fatal(err)
	}
	comp, err := Compile(ont)
	if err != nil {
		t.Fatal(err)
	}

	// Sentence 1: affirmative relation (accepted).
	// Sentence 2: negated modality trap (rejected by Noul).
	// Sentence 3: low-margin ambiguous relation (review + gap signal).
	text := "The fee-bump patch resolves the UTXO stall cleanly. " +
		"The UTXO stall does NOT affect NektarAnchor in production. " +
		"Teranode may depend on NektarAnchor during sync."

	eval := stubEval{fn: func(_ context.Context, req jev.Request) (*jev.Response, error) {
		st := req.State.(map[string]any)
		cands := st["candidates"].([]map[string]any)
		answers := make(map[string]jev.Answer, len(cands)*2)
		for _, c := range cands {
			pid := c["id"].(string)
			sub := c["subjectId"].(string)
			obj := c["objectId"].(string)
			quote := c["evidenceText"].(string)

			switch {
			case sub == "mit:fee-bump" && obj == "inc:utxo-stall":
				answers["rel_"+pid] = jev.Answer{
					Type:          jev.TypeChoice,
					Choice:        "mitigates",
					Confidence:    fptr(0.93),
					Probabilities: jev.ProbabilityMap{"mitigates": 0.93, "NONE": 0.07},
				}
				answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: fptr(0.95)}

			case sub == "inc:utxo-stall" && obj == "cov:nektar-anchor" && strings.Contains(quote, "NOT"):
				// Modality trap: Choice picks "affects", but Noul catches negation!
				answers["rel_"+pid] = jev.Answer{
					Type:          jev.TypeChoice,
					Choice:        "affects",
					Confidence:    fptr(0.85),
					Probabilities: jev.ProbabilityMap{"affects": 0.85, "NONE": 0.15},
				}
				answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: fptr(0.12)}

			case sub == "svc:teranode" && obj == "cov:nektar-anchor":
				// Low margin (0.56 vs 0.44 = 1200 bp < 1500 bp) -> review + gap signal!
				answers["rel_"+pid] = jev.Answer{
					Type:          jev.TypeChoice,
					Choice:        "depends_on",
					Confidence:    fptr(0.56),
					Probabilities: jev.ProbabilityMap{"depends_on": 0.56, "NONE": 0.44},
				}
				answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: fptr(0.74)}

			default:
				answers["rel_"+pid] = jev.Answer{
					Type:          jev.TypeChoice,
					Choice:        "NONE",
					Confidence:    fptr(0.92),
					Probabilities: jev.ProbabilityMap{"NONE": 0.92},
				}
				answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: fptr(0.05)}
			}
		}
		return &jev.Response{
			Model:   "kev-0.8b",
			Answers: answers,
			Usage:   jev.Usage{InputTokens: 320},
		}, nil
	}}

	res, err := Extract(context.Background(), eval, comp, text, nil, ExtractOptions{
		Blocking: BlockingConfig{MaxSentenceWindow: 0, MaxCharDistance: 300, MaxCandidatePairs: 32},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Fatalf("expected 1 accepted edge, got %d (edges=%+v)", res.AcceptedCount, res.Edges)
	}
	if res.RejectedCount != 1 {
		t.Fatalf("expected 1 rejected modality-trap edge, got %d (edges=%+v)", res.RejectedCount, res.Edges)
	}
	if res.ReviewCount != 1 {
		t.Fatalf("expected 1 review low-margin edge, got %d (edges=%+v)", res.ReviewCount, res.Edges)
	}
	if len(res.Gaps) == 0 {
		t.Fatal("expected OntologyGapSignal on low-margin candidate")
	}
	if res.EdgesRoot == "" || len(res.EdgesRoot) != 64 {
		t.Fatalf("expected 64-hex EdgesRoot, got %q", res.EdgesRoot)
	}

	// Verify sidecar offline AND verify tamper rejection!
	sidecar := BuildSidecar("facts/0001.md", []byte(text), res)
	nVer, err := VerifySidecar([]byte(text), sidecar)
	if err != nil || nVer != 1 {
		t.Fatalf("VerifySidecar failed: n=%d err=%v", nVer, err)
	}
	tampered := sidecar
	tampered.Edges = append([]ExtractedEdge(nil), sidecar.Edges...)
	for i := range tampered.Edges {
		if tampered.Edges[i].Status == EdgeAccepted {
			tampered.Edges[i].Evidence.Quote = "forged quote"
		}
	}
	if _, err := VerifySidecar([]byte(text), tampered); !errors.Is(err, ErrSidecarVerify) {
		t.Fatalf("expected ErrSidecarVerify on tampered quote, got %v", err)
	}
}

func TestMangleFunctionalCardinalityConflictAndOfflineDegradation(t *testing.T) {
	ont, err := ParseOntologyYAML([]byte(sampleBSVOpsYAML))
	if err != nil {
		t.Fatal(err)
	}
	comp, err := Compile(ont)
	if err != nil {
		t.Fatal(err)
	}

	// Existing ABox already pins svc:teranode hosted_in reg:eu-central.
	existing, _ := reasoning.FinalizeTriple(reasoning.Triple{
		Subject:   "svc:teranode",
		Predicate: "hosted_in",
		Object:    "reg:eu-central",
	})
	text := "Teranode is hosted in us-east-1 now."
	eval := stubEval{fn: func(_ context.Context, req jev.Request) (*jev.Response, error) {
		st := req.State.(map[string]any)
		cands := st["candidates"].([]map[string]any)
		answers := make(map[string]jev.Answer, len(cands)*2)
		for _, c := range cands {
			pid := c["id"].(string)
			answers["rel_"+pid] = jev.Answer{
				Type:          jev.TypeChoice,
				Choice:        "hosted_in",
				Confidence:    fptr(0.95),
				Probabilities: jev.ProbabilityMap{"hosted_in": 0.95, "NONE": 0.05},
			}
			answers["sup_"+pid] = jev.Answer{Type: jev.TypeNoul, Noul: fptr(0.95)}
		}
		return &jev.Response{Model: "kev-0.8b", Answers: answers}, nil
	}}

	res, err := Extract(context.Background(), eval, comp, text, []reasoning.Triple{existing}, ExtractOptions{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.AcceptedCount != 0 || res.RejectedCount != 1 {
		t.Fatalf("expected functional conflict rejection (accepted=0 rejected=1), got accepted=%d rejected=%d edges=%+v",
			res.AcceptedCount, res.RejectedCount, res.Edges)
	}
	if !strings.Contains(res.Edges[0].Reason, "mangle_conflict") {
		t.Fatalf("expected mangle_conflict reason, got %q", res.Edges[0].Reason)
	}

	// Offline Jev degradation: non-strict yields review + jev_offline; strict fails closed!
	offRes, err := Extract(context.Background(), nil, comp, text, nil, ExtractOptions{Strict: false})
	if err != nil {
		t.Fatalf("non-strict offline Extract error: %v", err)
	}
	if offRes.ExtractorStatus != "jev_offline" || offRes.ReviewCount == 0 {
		t.Fatalf("expected jev_offline with review candidates, got %+v", offRes)
	}
	if _, err := Extract(context.Background(), nil, comp, text, nil, ExtractOptions{Strict: true}); !errors.Is(err, ErrEvaluatorRequired) {
		t.Fatalf("expected ErrEvaluatorRequired in strict mode, got %v", err)
	}
}

func TestCrossOntologyAlignmentAndBridgeEntailment(t *testing.T) {
	ontA, err := ParseOntologyYAML([]byte(`
version: 1
name: sec-alpha
classes:
  - id: Outage
    definition: "A service outage or operational failure."
  - id: Daemon
    disjointWith: [Outage]
    definition: "A long-running backend process."
relations:
  - id: degrades
    domain: Outage
    range: Daemon
    definition: "The outage degrades the daemon."
`))
	if err != nil {
		t.Fatal(err)
	}
	ontB, err := ParseOntologyYAML([]byte(sampleBSVOpsYAML))
	if err != nil {
		t.Fatal(err)
	}
	compA, err := Compile(ontA)
	if err != nil {
		t.Fatal(err)
	}
	compB, err := Compile(ontB)
	if err != nil {
		t.Fatal(err)
	}

	eval := stubEval{fn: func(_ context.Context, req jev.Request) (*jev.Response, error) {
		st := req.State.(map[string]any)
		if sc, ok := st["sourceClass"].(map[string]any); ok {
			switch sc["id"].(string) {
			case "Outage":
				return &jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"target_class": {Type: jev.TypeChoice, Choice: "Incident", Confidence: fptr(0.94), Probabilities: jev.ProbabilityMap{"Incident": 0.94, "NONE": 0.06}},
						"match_kind":   {Type: jev.TypeChoice, Choice: PredEquivalentClass, Confidence: fptr(0.92)},
						"is_valid":     {Type: jev.TypeNoul, Noul: fptr(0.96)},
					},
				}, nil
			case "Daemon":
				return &jev.Response{
					Model: "kev-0.8b",
					Answers: map[string]jev.Answer{
						"target_class": {Type: jev.TypeChoice, Choice: "Service", Confidence: fptr(0.91), Probabilities: jev.ProbabilityMap{"Service": 0.91, "NONE": 0.09}},
						"match_kind":   {Type: jev.TypeChoice, Choice: PredSubClassOf, Confidence: fptr(0.89)},
						"is_valid":     {Type: jev.TypeNoul, Noul: fptr(0.94)},
					},
				}, nil
			}
		}
		if sr, ok := st["sourceRelation"].(map[string]any); ok && sr["id"].(string) == "degrades" {
			return &jev.Response{
				Model: "kev-0.8b",
				Answers: map[string]jev.Answer{
					"target_relation": {Type: jev.TypeChoice, Choice: "affects", Confidence: fptr(0.93), Probabilities: jev.ProbabilityMap{"affects": 0.93, "NONE": 0.07}},
					"match_kind":      {Type: jev.TypeChoice, Choice: PredEquivalentProperty, Confidence: fptr(0.91)},
					"is_valid":        {Type: jev.TypeNoul, Noul: fptr(0.95)},
				},
			}, nil
		}
		return &jev.Response{Model: "kev-0.8b", Answers: map[string]jev.Answer{}}, nil
	}}

	bridge, err := AlignOntologies(context.Background(), eval, compA, compB, AlignOptions{Strict: true})
	if err != nil {
		t.Fatalf("AlignOntologies: %v", err)
	}
	if bridge.AcceptedCount != 3 || len(bridge.BridgeID) != 64 {
		t.Fatalf("expected 3 accepted bridge mappings + 64-hex BridgeID, got %+v", bridge)
	}
	_, bridgeRules, err := CompileBridgeRules(bridge)
	if err != nil {
		t.Fatalf("CompileBridgeRules: %v", err)
	}

	// Prove that a fact in ontA (`triple("inc:1", "degrades", "svc:1")`) entails ontB's
	// (`triple("inc:1", "affects", "svc:1")`) and (`rdf_type("inc:1", "Incident")`) via Mangle!
	factA, _ := reasoning.FinalizeTriple(reasoning.Triple{Subject: "inc:1", Predicate: "degrades", Object: "svc:1"})
	atoms, _ := factA.ToMangleAtoms()
	rules := append(append(append([]string(nil), compA.MangleRules...), compB.MangleRules...), bridgeRules...)

	entailedRel, err := reasoning.Entails(atoms, rules, `triple("inc:1", "affects", "svc:1")`)
	if err != nil || !entailedRel {
		t.Fatalf("expected cross-ontology relation entailment, got ok=%v err=%v", entailedRel, err)
	}
	entailedClass, err := reasoning.Entails(atoms, rules, `rdf_type("inc:1", "Incident")`)
	if err != nil || !entailedClass {
		t.Fatalf("expected cross-ontology class entailment, got ok=%v err=%v", entailedClass, err)
	}

	// Adversarial: mapping a source class onto a DISJOINT target class MUST be rejected by Mangle!
	badEval := stubEval{fn: func(_ context.Context, req jev.Request) (*jev.Response, error) {
		st := req.State.(map[string]any)
		if sc, ok := st["sourceClass"].(map[string]any); ok {
			// Map both Outage and Daemon to Service -> Outage & Daemon are disjoint in ontA!
			_ = sc
			return &jev.Response{
				Model: "kev-0.8b",
				Answers: map[string]jev.Answer{
					"target_class": {Type: jev.TypeChoice, Choice: "Service", Confidence: fptr(0.95), Probabilities: jev.ProbabilityMap{"Service": 0.95, "NONE": 0.05}},
					"match_kind":   {Type: jev.TypeChoice, Choice: PredEquivalentClass, Confidence: fptr(0.95)},
					"is_valid":     {Type: jev.TypeNoul, Noul: fptr(0.95)},
				},
			}, nil
		}
		return &jev.Response{
			Model: "kev-0.8b",
			Answers: map[string]jev.Answer{
				"target_relation": {Type: jev.TypeChoice, Choice: "NONE", Confidence: fptr(0.99)},
				"match_kind":      {Type: jev.TypeChoice, Choice: PredCloseMatch, Confidence: fptr(0.99)},
				"is_valid":        {Type: jev.TypeNoul, Noul: fptr(0.01)},
			},
		}, nil
	}}
	badBridge, err := AlignOntologies(context.Background(), badEval, compA, compB, AlignOptions{})
	if err != nil {
		t.Fatalf("AlignOntologies badEval: %v", err)
	}
	if badBridge.RejectedCount == 0 {
		t.Fatalf("expected disjoint cross-ontology alignment conflict to be rejected, got %+v", badBridge)
	}
}
