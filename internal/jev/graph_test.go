package jev

import (
	"context"
	"testing"

	"github.com/kryptokrates/nektar/internal/reasoning"
)

type stubGraphEval struct {
	resp *Response
	err  error
}

func (s *stubGraphEval) Evaluate(_ context.Context, req Request) (*Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	if err := ValidateResponse(req, s.resp); err != nil {
		return nil, err
	}
	return s.resp.Clone(), nil
}

func TestBlockEntities_ExactByteSpans(t *testing.T) {
	man := reasoning.DefaultOntologyManifest("ops")
	text := "Go 1.27 with modernc.org/sqlite requires busy_timeout=5000 to prevent lock errors under Git."
	mentions := BlockEntities(text, man)
	if len(mentions) < 3 {
		t.Fatalf("expected >= 3 mentions, got %+v", mentions)
	}
	for _, m := range mentions {
		got := text[m.StartByte:m.EndByte]
		if got != m.Surface {
			t.Fatalf("byte span mismatch for %s: text[%d:%d]=%q want %q", m.EntityID, m.StartByte, m.EndByte, got, m.Surface)
		}
	}
}

func TestExtractGraph_JevChoiceNoulAndFallback(t *testing.T) {
	man := reasoning.DefaultOntologyManifest("ops")
	text := "Go 1.27 requires modernc.org/sqlite WAL mode to prevent concurrent lock failures."

	// 1. Jev Choice/Noul path.
	p := 0.92
	c := 0.88
	eval := &stubGraphEval{
		resp: &Response{
			Model: "kev-0.8b",
			Answers: map[string]Answer{
				"rel_0":   {Type: TypeChoice, Choice: "requires", Confidence: &c},
				"valid_0": {Type: TypeNoul, Noul: &p},
			},
			Usage: Usage{InputTokens: 64},
		},
	}
	res, err := ExtractGraph(context.Background(), eval, ExtractGraphInput{
		Text:     text,
		Manifest: man,
	})
	if err != nil {
		t.Fatalf("ExtractGraph(jev): %v", err)
	}
	if res.Mode != "jev" || !res.Consistent || len(res.Triples) != 1 {
		t.Fatalf("unexpected jev result: %+v", res)
	}
	if res.Triples[0].Triple.Subject != "ops:go" || res.Triples[0].Triple.Predicate != "requires" || res.Triples[0].Triple.Object != "ops:sqlite" {
		t.Fatalf("unexpected triple: %+v", res.Triples[0].Triple)
	}
	if res.TripleSetHash == "" {
		t.Fatalf("expected non-empty deterministic TripleSetHash")
	}

	// 2. Deterministic offline fallback when eval == nil.
	detRes, err := ExtractGraph(context.Background(), nil, ExtractGraphInput{
		Text:     text,
		Manifest: man,
	})
	if err != nil {
		t.Fatalf("ExtractGraph(deterministic): %v", err)
	}
	if detRes.Mode != "deterministic" || len(detRes.Triples) != 1 {
		t.Fatalf("unexpected deterministic result: %+v", detRes)
	}
	if detRes.TripleSetHash != res.TripleSetHash {
		t.Fatalf("cross-extractor triple set hash mismatch: det=%s jev=%s", detRes.TripleSetHash, res.TripleSetHash)
	}
}

func TestExtractGraph_RejectsDisjointnessConflict(t *testing.T) {
	man := reasoning.OntologyManifest{
		Version: 1,
		Domain:  "test",
		Entities: []reasoning.OntologyEntity{
			{ID: "svc:alpha", Type: "Deprecated", Aliases: []string{"Alpha"}},
			{ID: "svc:beta", Type: "Recommended", Aliases: []string{"Beta"}},
		},
		Predicates: []reasoning.OntologyPredicate{
			{ID: "requires", DomainClass: "Recommended"}, // conflicts with svc:alpha being Deprecated!
		},
		Axioms: []reasoning.OntologyAxiom{
			{ID: "disj", Kind: reasoning.AxiomDisjointWith, Subject: "Deprecated", Object: "Recommended"},
		},
	}
	text := "Alpha requires Beta in production."
	res, err := ExtractGraph(context.Background(), nil, ExtractGraphInput{
		Text:     text,
		Manifest: man,
	})
	if err != nil {
		t.Fatalf("ExtractGraph: %v", err)
	}
	if len(res.Triples) != 0 || res.RejectedTriples == 0 {
		t.Fatalf("expected conflicting triple to be rejected by Mangle TBox check, got %+v", res)
	}
}
