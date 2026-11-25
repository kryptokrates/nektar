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

// EvalDatalog runs Google Mangle (mangle-go) semi-naive stratified evaluation over
// ground facts and rules in deterministic order, returning all derived fact strings.
func EvalDatalog(programSource string) (map[string]bool, error) {
	unit, err := parse.Unit(strings.NewReader(programSource))
	if err != nil {
		return nil, fmt.Errorf("mangle: parse: %w", err)
	}
	info, err := analysis.AnalyzeOneUnit(unit, nil)
	if err != nil {
		return nil, fmt.Errorf("mangle: analyze: %w", err)
	}
	fs := factstore.NewSimpleInMemoryStore()
	for _, fact := range info.EdbPredicates {
		_ = fact
	}
	for _, cl := range unit.Clauses {
		if len(cl.Premises) == 0 {
			fs.Add(cl.Head)
		}
	}
	if err := engine.EvalProgram(info, fs, engine.WithDeterministicOrder()); err != nil {
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
func Entails(facts []string, rules []string, goalLiteral string) (bool, error) {
	var b strings.Builder
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.HasSuffix(f, ".") {
			f += "."
		}
		b.WriteString(f)
		b.WriteByte('\n')
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
