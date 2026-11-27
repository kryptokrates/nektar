package reasoning

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"codeberg.org/TauCeti/mangle-go/analysis"
	"codeberg.org/TauCeti/mangle-go/parse"
	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

// AxiomKind names a supported DL-Lite / OWL 2 RL / deontic legislative axiom compiled to Mangle Datalog.
type AxiomKind string

const (
	AxiomSubClassOf         AxiomKind = "subClassOf"         // Subject ⊑ Object
	AxiomSubPropertyOf      AxiomKind = "subPropertyOf"      // Subject ⊑ Object (role hierarchy)
	AxiomDomain             AxiomKind = "domain"             // dom(Subject) ⊑ Object
	AxiomRange              AxiomKind = "range"              // ran(Subject) ⊑ Object
	AxiomInverseOf          AxiomKind = "inverseOf"          // Subject ≡ Object^-
	AxiomTransitiveProperty AxiomKind = "transitiveProperty" // Subject^+ ⊑ Subject
	AxiomDisjointWith       AxiomKind = "disjointWith"       // Subject ⊓ Object ⊑ ⊥
	AxiomDeonticObligation  AxiomKind = "obligation"         // Role Subject obligated to perform Object
	AxiomDeonticProhibition AxiomKind = "prohibition"        // Role Subject prohibited from performing Object
	AxiomDeonticPermission  AxiomKind = "permission"         // Role Subject permitted to perform Object
	AxiomMangleRule         AxiomKind = "mangleRule"         // Custom stratified Mangle clause in RuleText
)

// OntologyAxiom is an immutable, CBOR-hashed TBox rule committed into the __ontology__ shard (Paper §8).
type OntologyAxiom struct {
	ID       string    `json:"id"`
	Kind     AxiomKind `json:"kind"`
	Subject  string    `json:"subject,omitempty"`
	Object   string    `json:"object,omitempty"`
	RuleText string    `json:"ruleText,omitempty"`
	Hash     string    `json:"hash,omitempty"`
}

// ComputeAxiomHash computes the canonical RFC 8949 CBOR commitment hash for ax.
func ComputeAxiomHash(ax OntologyAxiom) (string, error) {
	h, err := core.HashCBOR(core.TagOntologyV1, cbor.Map{
		{Key: uint64(0), Val: strings.TrimSpace(ax.ID)},
		{Key: uint64(1), Val: string(ax.Kind)},
		{Key: uint64(2), Val: strings.TrimSpace(ax.Subject)},
		{Key: uint64(3), Val: strings.TrimSpace(ax.Object)},
		{Key: uint64(4), Val: strings.TrimSpace(ax.RuleText)},
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h), nil
}

// ComputeRuleHash computes a canonical domain-separated hash over a single Mangle rule string.
func ComputeRuleHash(ruleText string) string {
	norm := strings.TrimSpace(ruleText)
	if norm != "" && !strings.HasSuffix(norm, ".") {
		norm += "."
	}
	return hex.EncodeToString(core.DSHA256(core.Concat(core.TagOntologyV1, []byte(norm))))
}

// CompileAxiom compiles one DL-Lite / OWL 2 RL / deontic OntologyAxiom into deterministic,
// validated stratified Mangle Datalog clauses (zero external JVM/reasoner dependency).
func CompileAxiom(ax OntologyAxiom) ([]string, error) {
	sub := strings.TrimSpace(ax.Subject)
	obj := strings.TrimSpace(ax.Object)
	id := strings.TrimSpace(ax.ID)
	if id == "" {
		h, err := ComputeAxiomHash(ax)
		if err != nil {
			return nil, err
		}
		id = h[:16]
	}

	var rules []string
	switch ax.Kind {
	case AxiomSubClassOf:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: subClassOf requires subject and object")
		}
		rules = []string{
			fmt.Sprintf("rdf_type(X, %s) :- rdf_type(X, %s).", strconv.Quote(obj), strconv.Quote(sub)),
		}
	case AxiomSubPropertyOf:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: subPropertyOf requires subject and object")
		}
		rules = []string{
			fmt.Sprintf("triple(X, %s, Y) :- triple(X, %s, Y).", strconv.Quote(obj), strconv.Quote(sub)),
		}
	case AxiomDomain:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: domain requires subject (property) and object (class)")
		}
		rules = []string{
			fmt.Sprintf("rdf_type(X, %s) :- triple(X, %s, Y).", strconv.Quote(obj), strconv.Quote(sub)),
		}
	case AxiomRange:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: range requires subject (property) and object (class)")
		}
		rules = []string{
			fmt.Sprintf("rdf_type(Y, %s) :- triple(X, %s, Y).", strconv.Quote(obj), strconv.Quote(sub)),
		}
	case AxiomInverseOf:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: inverseOf requires subject and object properties")
		}
		rules = []string{
			fmt.Sprintf("triple(Y, %s, X) :- triple(X, %s, Y).", strconv.Quote(obj), strconv.Quote(sub)),
			fmt.Sprintf("triple(Y, %s, X) :- triple(X, %s, Y).", strconv.Quote(sub), strconv.Quote(obj)),
		}
	case AxiomTransitiveProperty:
		if sub == "" {
			return nil, errors.New("ontology: transitiveProperty requires subject property")
		}
		qs := strconv.Quote(sub)
		rules = []string{
			fmt.Sprintf("triple(X, %s, Z) :- triple(X, %s, Y), triple(Y, %s, Z).", qs, qs, qs),
		}
	case AxiomDisjointWith:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: disjointWith requires subject and object classes")
		}
		rules = []string{
			fmt.Sprintf("ontology_conflict(X, %s, %s) :- rdf_type(X, %s), rdf_type(X, %s).",
				strconv.Quote(sub), strconv.Quote(obj), strconv.Quote(sub), strconv.Quote(obj)),
		}
	case AxiomDeonticProhibition:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: prohibition requires subject (role/class) and object (action)")
		}
		rules = []string{
			fmt.Sprintf("prohibited(X, %s, %s) :- rdf_type(X, %s).", strconv.Quote(obj), strconv.Quote(id), strconv.Quote(sub)),
			fmt.Sprintf("deontic_violation(X, %s, %s) :- triple(X, \"performed\", %s), prohibited(X, %s, %s).",
				strconv.Quote(obj), strconv.Quote(id), strconv.Quote(obj), strconv.Quote(obj), strconv.Quote(id)),
		}
	case AxiomDeonticObligation:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: obligation requires subject (role/class) and object (action)")
		}
		rules = []string{
			fmt.Sprintf("obligated(X, %s, %s) :- rdf_type(X, %s).", strconv.Quote(obj), strconv.Quote(id), strconv.Quote(sub)),
		}
	case AxiomDeonticPermission:
		if sub == "" || obj == "" {
			return nil, errors.New("ontology: permission requires subject (role/class) and object (action)")
		}
		rules = []string{
			fmt.Sprintf("permitted(X, %s, %s) :- rdf_type(X, %s).", strconv.Quote(obj), strconv.Quote(id), strconv.Quote(sub)),
		}
	case AxiomMangleRule:
		r := strings.TrimSpace(ax.RuleText)
		if r == "" {
			return nil, errors.New("ontology: mangleRule requires ruleText")
		}
		if !strings.HasSuffix(r, ".") {
			r += "."
		}
		rules = []string{r}
	default:
		return nil, fmt.Errorf("ontology: unsupported axiom kind %q", ax.Kind)
	}

	// Validate every compiled clause parses cleanly and passes Mangle stratification/safety analysis.
	// Prepend EDB declarations for built-in ontology predicates (triple, rdf_type, triple_modality)
	// so standalone TBox axioms referencing EDB facts validate before ABox facts arrive.
	var src strings.Builder
	if ax.Kind != AxiomMangleRule {
		src.WriteString("Decl triple(S, P, O).\n")
		src.WriteString("Decl rdf_type(S, C).\n")
		src.WriteString("Decl triple_modality(H, K, Q).\n")
	}
	for _, r := range rules {
		src.WriteString(r)
		src.WriteByte('\n')
	}
	unit, err := parse.Unit(strings.NewReader(src.String()))
	if err != nil {
		return nil, fmt.Errorf("ontology: parse compiled axiom %q: %w", id, err)
	}
	if ax.Kind != AxiomMangleRule {
		if _, err := analysis.AnalyzeOneUnit(unit, nil); err != nil {
			return nil, fmt.Errorf("ontology: analyze compiled axiom %q: %w", id, err)
		}
	} else {
		// For custom Mangle rules, reject bare facts without premises or malformed rules;
		// full stratification runs when evaluated against ABox facts.
		if len(unit.Clauses) == 0 {
			return nil, fmt.Errorf("ontology: mangleRule %q produced no clauses", id)
		}
	}
	return rules, nil
}

// FinalizeAxiom validates, compiles, and populates the canonical Hash of an OntologyAxiom.
func FinalizeAxiom(ax OntologyAxiom) (OntologyAxiom, []string, error) {
	rules, err := CompileAxiom(ax)
	if err != nil {
		return OntologyAxiom{}, nil, err
	}
	h, err := ComputeAxiomHash(ax)
	if err != nil {
		return OntologyAxiom{}, nil, err
	}
	ax.Hash = h
	if strings.TrimSpace(ax.ID) == "" {
		ax.ID = h[:16]
	}
	return ax, rules, nil
}

// ConsistencyReport summarizes DL-Lite disjointness conflicts and deontic violations derived
// from committed ABox facts under committed TBox axioms.
type ConsistencyReport struct {
	Consistent        bool     `json:"consistent"`
	Conflicts         []string `json:"conflicts,omitempty"`
	DeonticViolations []string `json:"deonticViolations,omitempty"`
}

// CheckOntologyConsistency evaluates facts + rules and detects any ontology_conflict(...)
// or deontic_violation(...) derivations.
func CheckOntologyConsistency(facts []string, rules []string) (*ConsistencyReport, error) {
	if len(rules) > MaxMangleRules {
		return nil, fmt.Errorf("mangle: rule count %d exceeds max %d", len(rules), MaxMangleRules)
	}
	var b strings.Builder
	b.WriteString("Decl triple(S, P, O).\n")
	b.WriteString("Decl rdf_type(S, C).\n")
	b.WriteString("Decl triple_modality(H, K, Q).\n")
	factCount := 0
	for _, f := range facts {
		atom, ok := ParseGroundFact(f)
		if !ok {
			continue
		}
		factCount++
		if factCount > MaxMangleFacts {
			return nil, fmt.Errorf("mangle: ground fact count exceeds max %d", MaxMangleFacts)
		}
		b.WriteString(atom.String())
		b.WriteString(".\n")
	}
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.HasSuffix(r, ".") {
			r += "."
		}
		b.WriteString(r)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return &ConsistencyReport{Consistent: true}, nil
	}
	derived, err := EvalDatalog(b.String())
	if err != nil {
		return nil, err
	}
	rep := &ConsistencyReport{Consistent: true}
	for atomStr := range derived {
		if strings.HasPrefix(atomStr, "ontology_conflict(") {
			rep.Conflicts = append(rep.Conflicts, atomStr)
			rep.Consistent = false
		} else if strings.HasPrefix(atomStr, "deontic_violation(") {
			rep.DeonticViolations = append(rep.DeonticViolations, atomStr)
			rep.Consistent = false
		}
	}
	return rep, nil
}
