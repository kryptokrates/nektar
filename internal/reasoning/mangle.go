package reasoning

import (
	"fmt"
	"strings"

	"codeberg.org/TauCeti/mangle-go/analysis"
	"codeberg.org/TauCeti/mangle-go/ast"
	"codeberg.org/TauCeti/mangle-go/engine"
	"codeberg.org/TauCeti/mangle-go/factstore"
	"codeberg.org/TauCeti/mangle-go/parse"
)

const (
	// MaxMangleFacts caps input ground facts per evaluation to prevent DoS.
	MaxMangleFacts = 4096
	// MaxMangleRules caps trusted operator rules per evaluation.
	MaxMangleRules = 512
	// MaxMangleProgramBytes caps total Mangle source fed into EvalDatalog.
	MaxMangleProgramBytes = 256 << 10
	// MaxMangleDerivedFacts caps newly derived facts inside EvalProgram to prevent blowup DoS.
	MaxMangleDerivedFacts = 16384
)

func isGroundBaseTerm(t ast.BaseTerm) bool {
	switch v := t.(type) {
	case ast.Constant:
		return true
	case ast.Variable:
		return false
	case ast.ApplyFn:
		for _, arg := range v.Args {
			if !isGroundBaseTerm(arg) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// ParseGroundFact parses a single candidate fact string into a ground Mangle atom.
// Rejects any rule syntax (:-), transforms (|>), or non-ground/malformed statements (including
// variables nested inside ApplyFn terms) so untrusted memory fragments can never inject
// executable Datalog rules or break analysis.
func ParseGroundFact(raw string) (ast.Atom, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.Contains(s, ":-") || strings.Contains(s, "|>") || strings.ContainsAny(s, "\r\n") {
		return ast.Atom{}, false
	}
	s = strings.TrimSuffix(s, ".")
	atom, err := parse.Atom(s)
	if err != nil {
		return ast.Atom{}, false
	}
	// Ensure every argument is recursively ground (no top-level or nested ApplyFn variables).
	for _, arg := range atom.Args {
		if !isGroundBaseTerm(arg) {
			return ast.Atom{}, false
		}
	}
	return atom, true
}

// EvalDatalog runs Google Mangle (mangle-go) semi-naive stratified evaluation over
// ground facts and rules in deterministic order, returning all derived fact strings.
func EvalDatalog(programSource string) (map[string]bool, error) {
	if len(programSource) > MaxMangleProgramBytes {
		return nil, fmt.Errorf("mangle: program size %d exceeds max %d bytes", len(programSource), MaxMangleProgramBytes)
	}
	unit, err := parse.Unit(strings.NewReader(programSource))
	if err != nil {
		return nil, fmt.Errorf("mangle: parse: %w", err)
	}
	info, err := analysis.AnalyzeOneUnit(unit, nil)
	if err != nil {
		return nil, fmt.Errorf("mangle: analyze: %w", err)
	}
	fs := factstore.NewMultiIndexedInMemoryStore()
	for _, cl := range unit.Clauses {
		if len(cl.Premises) == 0 {
			fs.Add(cl.Head)
		}
	}
	if err := engine.EvalProgram(info, fs, engine.WithCreatedFactLimit(MaxMangleDerivedFacts)); err != nil {
		return nil, fmt.Errorf("mangle: eval: %w", err)
	}
	out := make(map[string]bool)
	_ = factstore.GetAllFacts(fs, func(a ast.Atom) error {
		out[a.String()] = true
		return nil
	})
	return out, nil
}

// Entails checks whether facts + rules entail goalLiteral under Mangle stratified Datalog.
// Every element of facts is strictly validated as a ground atom via ParseGroundFact so
// untrusted fragment text can never inject Datalog rules or break parsing.
func Entails(facts []string, rules []string, goalLiteral string) (bool, error) {
	if len(rules) > MaxMangleRules {
		return false, fmt.Errorf("mangle: rule count %d exceeds max %d", len(rules), MaxMangleRules)
	}
	var b strings.Builder
	factCount := 0
	for _, f := range facts {
		atom, ok := ParseGroundFact(f)
		if !ok {
			continue
		}
		factCount++
		if factCount > MaxMangleFacts {
			return false, fmt.Errorf("mangle: ground fact count exceeds max %d", MaxMangleFacts)
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
	goalAtom, err := parse.Atom(strings.TrimSuffix(strings.TrimSpace(goalLiteral), "."))
	if err != nil {
		return false, fmt.Errorf("mangle: invalid goal %q: %w", goalLiteral, err)
	}
	derived, err := EvalDatalog(b.String())
	if err != nil {
		return false, err
	}
	return derived[goalAtom.String()], nil
}
