package reasoning

import (
	"errors"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestCommittedTriplesDLLiteOntologyAndProofCarryingInference(t *testing.T) {
	// 1. Canonical CBOR Triple roundtrip + tamper rejection.
	tr, err := FinalizeTriple(Triple{
		Subject:   "agent:alice",
		Predicate: "rdf:type",
		Object:    "HIPAAContractor",
		Modality:  &core.Modality{Kind: "epistemic", Qualifier: "verified-roster"},
	})
	if err != nil {
		t.Fatalf("FinalizeTriple: %v", err)
	}
	raw, err := EncodeTripleCBOR(tr)
	if err != nil {
		t.Fatalf("EncodeTripleCBOR: %v", err)
	}
	dec, err := DecodeTripleCBOR(raw)
	if err != nil || dec.Hash != tr.Hash || dec.Modality == nil || dec.Modality.Kind != "epistemic" {
		t.Fatalf("DecodeTripleCBOR mismatch: dec=%+v err=%v", dec, err)
	}
	badRaw := append([]byte(nil), raw...)
	badRaw[len(badRaw)-1] ^= 0xff
	if _, err := DecodeTripleCBOR(badRaw); err == nil {
		t.Fatal("expected tampered CBOR triple to fail verification")
	}

	// 2. Compile DL-Lite / OWL 2 RL + deontic axioms into stratified Mangle rules.
	axioms := []OntologyAxiom{
		{ID: "ax-sub1", Kind: AxiomSubClassOf, Subject: "HIPAAContractor", Object: "RegulatedActor"},
		{ID: "ax-sub2", Kind: AxiomSubClassOf, Subject: "RegulatedActor", Object: "AuditedEntity"},
		{ID: "ax-disj", Kind: AxiomDisjointWith, Subject: "RegulatedActor", Object: "PublicAnonymous"},
		{ID: "ax-proh", Kind: AxiomDeonticProhibition, Subject: "RegulatedActor", Object: "ExportPlaintextPHI"},
	}
	var committedRules []CommittedRule
	var ruleStrings []string
	ruleByHash := make(map[string]string)
	for i, ax := range axioms {
		_, compiled, err := FinalizeAxiom(ax)
		if err != nil {
			t.Fatalf("FinalizeAxiom(%s): %v", ax.ID, err)
		}
		for _, r := range compiled {
			rh := ComputeRuleHash(r)
			ref := FragmentRef{Shard: "__ontology__", Seq: int64(i + 1)}
			committedRules = append(committedRules, CommittedRule{RuleText: r, Ref: &ref, RuleHash: rh})
			ruleStrings = append(ruleStrings, r)
			ruleByHash[rh] = r
		}
	}

	// 3. Emit ABox triples and run Proof-Carrying Inference for 2-hop SubClassOf + Deontic Prohibition!
	atoms1, err := tr.ToMangleAtoms()
	if err != nil {
		t.Fatalf("ToMangleAtoms: %v", err)
	}
	trAct, _ := FinalizeTriple(Triple{Subject: "agent:alice", Predicate: "performed", Object: "ExportPlaintextPHI"})
	atoms2, _ := trAct.ToMangleAtoms()

	factByRef := map[FragmentRef]string{
		{Shard: "legal", Seq: 1}: strings.Join(atoms1, "\n"),
		{Shard: "legal", Seq: 2}: strings.Join(atoms2, "\n"),
	}
	hdpByRef := map[FragmentRef]string{
		{Shard: "legal", Seq: 1}: core.ToHex(core.SHA256([]byte("f1"))),
		{Shard: "legal", Seq: 2}: core.ToHex(core.SHA256([]byte("f2"))),
	}

	var facts []CommittedFact
	for _, a := range atoms1 {
		facts = append(facts, CommittedFact{Atom: a, Ref: FragmentRef{Shard: "legal", Seq: 1}, HDP: hdpByRef[FragmentRef{Shard: "legal", Seq: 1}]})
	}
	for _, a := range atoms2 {
		facts = append(facts, CommittedFact{Atom: a, Ref: FragmentRef{Shard: "legal", Seq: 2}, HDP: hdpByRef[FragmentRef{Shard: "legal", Seq: 2}]})
	}

	step, ok, err := EntailsWithProof(facts, committedRules, `deontic_violation("agent:alice", "ExportPlaintextPHI", "ax-proh")`)
	if err != nil || !ok || step == nil {
		t.Fatalf("expected proof-carrying deontic_violation entailment: ok=%v step=%+v err=%v", ok, step, err)
	}

	// 4. Independent offline verification of the entire DerivationStep proof tree!
	factVer := func(ref FragmentRef, expectedHDP string, atom string) error {
		if hdpByRef[ref] != expectedHDP {
			return errors.New("hdp mismatch")
		}
		if !strings.Contains(factByRef[ref], atom) {
			return errors.New("atom not in committed fragment")
		}
		return nil
	}
	ruleVer := func(ref *FragmentRef, ruleHash string, ruleText string) error {
		if ref == nil || ref.Shard != "__ontology__" {
			return errors.New("unanchored rule")
		}
		if _, exists := ruleByHash[ruleHash]; !exists {
			return errors.New("rule hash not committed in __ontology__")
		}
		return nil
	}
	if err := VerifyDerivationProof(step, factVer, ruleVer); err != nil {
		t.Fatalf("VerifyDerivationProof valid proof failed: %v", err)
	}

	// Adversarial: tampering with rule text, bindings, or deleting an ontology rule MUST fail!
	tampered := *step
	tampered.RuleText = `deontic_violation(X, "ExportPlaintextPHI", "ax-proh") :- fake(X).`
	if err := VerifyDerivationProof(&tampered, factVer, ruleVer); err == nil {
		t.Fatal("VerifyDerivationProof accepted tampered RuleText!")
	}
	delete(ruleByHash, step.RuleHash)
	if err := VerifyDerivationProof(step, factVer, ruleVer); err == nil {
		t.Fatal("VerifyDerivationProof accepted proof citing uncommitted/deleted ontology rule!")
	}

	// 5. Ontology consistency checker catches both disjointness conflict and deontic violation!
	trDisj, _ := FinalizeTriple(Triple{Subject: "agent:alice", Predicate: "rdf:type", Object: "PublicAnonymous"})
	atoms3, _ := trDisj.ToMangleAtoms()
	allFactStrs := append(append(append([]string(nil), atoms1...), atoms2...), atoms3...)
	cons, err := CheckOntologyConsistency(allFactStrs, ruleStrings)
	if err != nil {
		t.Fatalf("CheckOntologyConsistency: %v", err)
	}
	if cons.Consistent || len(cons.Conflicts) == 0 || len(cons.DeonticViolations) == 0 {
		t.Fatalf("expected disjointness conflict + deontic violation, got: %+v", cons)
	}
}
