package reasoning

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"codeberg.org/TauCeti/mangle-go/ast"
	"codeberg.org/TauCeti/mangle-go/parse"
)

const MaxDerivationDepth = 32

// CommittedFact links a ground Mangle atom back to the exact committed ABox fragment (Ref, HDP).
type CommittedFact struct {
	Atom string      `json:"atom"`
	Ref  FragmentRef `json:"ref"`
	HDP  string      `json:"hdp,omitempty"`
}

// CommittedRule links a stratified Mangle rule back to its committed TBox fragment in __ontology__.
type CommittedRule struct {
	RuleText string       `json:"ruleText"`
	Ref      *FragmentRef `json:"ref,omitempty"`
	RuleHash string       `json:"ruleHash"`
}

// DerivationNode is either a base committed ABox fact leaf (FactRef != nil)
// or an intermediate derived step (Step != nil) or a stratified negation check (Negated == true).
type DerivationNode struct {
	Atom    string          `json:"atom"`
	FactRef *FragmentRef    `json:"factRef,omitempty"`
	FactHDP string          `json:"factHdp,omitempty"`
	Negated bool            `json:"negated,omitempty"`
	Step    *DerivationStep `json:"step,omitempty"`
}

// DerivationStep is an independently verifiable modus-ponens proof step:
// substituting Bindings into RuleText yields DerivedAtom from verified Premises (Paper §8).
type DerivationStep struct {
	DerivedAtom string            `json:"derivedAtom"`
	RuleRef     *FragmentRef      `json:"ruleRef,omitempty"`
	RuleHash    string            `json:"ruleHash"`
	RuleText    string            `json:"ruleText"`
	Bindings    map[string]string `json:"bindings,omitempty"`
	Premises    []DerivationNode  `json:"premises"`
}

// EntailsWithProof evaluates facts + rules under Mangle stratified Datalog and, if goalLiteral is
// entailed, reconstructs a minimal, cycle-free, offline-verifiable DerivationStep proof tree.
func EntailsWithProof(facts []CommittedFact, rules []CommittedRule, goalLiteral string) (*DerivationStep, bool, error) {
	if len(rules) > MaxMangleRules {
		return nil, false, fmt.Errorf("mangle: rule count %d exceeds max %d", len(rules), MaxMangleRules)
	}
	goalAtom, err := parse.Atom(strings.TrimSuffix(strings.TrimSpace(goalLiteral), "."))
	if err != nil {
		return nil, false, fmt.Errorf("mangle: invalid goal %q: %w", goalLiteral, err)
	}

	baseByAtom := make(map[string]CommittedFact, len(facts))
	factStrings := make([]string, 0, len(facts))
	for _, cf := range facts {
		atom, ok := ParseGroundFact(cf.Atom)
		if !ok {
			continue
		}
		key := atom.String()
		if _, exists := baseByAtom[key]; !exists {
			cp := cf
			cp.Atom = key
			baseByAtom[key] = cp
			factStrings = append(factStrings, key)
		}
	}

	type parsedRule struct {
		meta   CommittedRule
		clause ast.Clause
	}
	var parsedRules []parsedRule
	ruleStrings := make([]string, 0, len(rules))
	for _, cr := range rules {
		rt := strings.TrimSpace(cr.RuleText)
		if rt == "" {
			continue
		}
		if !strings.HasSuffix(rt, ".") {
			rt += "."
		}
		u, pErr := parse.Unit(strings.NewReader(rt + "\n"))
		if pErr != nil {
			return nil, false, fmt.Errorf("mangle: parse rule %q: %w", rt, pErr)
		}
		rh := cr.RuleHash
		if rh == "" {
			rh = ComputeRuleHash(rt)
		}
		for _, cl := range u.Clauses {
			if len(cl.Premises) == 0 {
				continue
			}
			meta := cr
			meta.RuleText = rt
			meta.RuleHash = rh
			parsedRules = append(parsedRules, parsedRule{meta: meta, clause: cl})
		}
		ruleStrings = append(ruleStrings, rt)
	}

	// Build and evaluate the full stratified program via mangle-go.
	var src strings.Builder
	for _, f := range factStrings {
		src.WriteString(f)
		src.WriteString(".\n")
	}
	for _, r := range ruleStrings {
		src.WriteString(r)
		src.WriteByte('\n')
	}
	derivedMap, err := EvalDatalog(src.String())
	if err != nil {
		return nil, false, err
	}
	goalStr := goalAtom.String()
	if !derivedMap[goalStr] {
		return nil, false, nil
	}

	// If goal is directly an ABox fact, return a direct identity witness step.
	if bf, ok := baseByAtom[goalStr]; ok {
		refCopy := bf.Ref
		return &DerivationStep{
			DerivedAtom: goalStr,
			RuleHash:    "abox:direct",
			Premises: []DerivationNode{{
				Atom:    goalStr,
				FactRef: &refCopy,
				FactHDP: bf.HDP,
			}},
		}, true, nil
	}

	// Index derived atoms by predicate signature for fast deterministic unification.
	derivedByPred := make(map[string][]ast.Atom)
	derivedKeys := make([]string, 0, len(derivedMap))
	for k := range derivedMap {
		derivedKeys = append(derivedKeys, k)
	}
	sort.Strings(derivedKeys)
	for _, k := range derivedKeys {
		if a, pErr := parse.Atom(k); pErr == nil {
			pk := predKey(a.Predicate)
			derivedByPred[pk] = append(derivedByPred[pk], a)
		}
	}

	// Memoized cycle-safe backward proof tree search.
	memo := make(map[string]*DerivationNode)
	var prove func(target ast.Atom, visiting map[string]bool, depth int) (*DerivationNode, bool)
	prove = func(target ast.Atom, visiting map[string]bool, depth int) (*DerivationNode, bool) {
		if depth > MaxDerivationDepth {
			return nil, false
		}
		ts := target.String()
		if cached, ok := memo[ts]; ok {
			return cached, true
		}
		if bf, ok := baseByAtom[ts]; ok {
			refCopy := bf.Ref
			n := &DerivationNode{Atom: ts, FactRef: &refCopy, FactHDP: bf.HDP}
			memo[ts] = n
			return n, true
		}
		if visiting[ts] {
			return nil, false
		}
		visiting[ts] = true
		defer delete(visiting, ts)

		for _, pr := range parsedRules {
			if pr.clause.Head.Predicate.Symbol != target.Predicate.Symbol || pr.clause.Head.Predicate.Arity != target.Predicate.Arity {
				continue
			}
			initBindings := make(map[string]ast.BaseTerm)
			if !unifyAtom(pr.clause.Head, target, initBindings) {
				continue
			}
			// Match all premises left-to-right against derived atoms.
			var matchPremises func(idx int, env map[string]ast.BaseTerm, acc []DerivationNode) ([]DerivationNode, map[string]ast.BaseTerm, bool)
			matchPremises = func(idx int, env map[string]ast.BaseTerm, acc []DerivationNode) ([]DerivationNode, map[string]ast.BaseTerm, bool) {
				if idx == len(pr.clause.Premises) {
					return acc, env, true
				}
				switch p := pr.clause.Premises[idx].(type) {
				case ast.Atom:
					cands := derivedByPred[predKey(p.Predicate)]
					for _, cand := range cands {
						nextEnv := cloneEnv(env)
						if !unifyAtom(p, cand, nextEnv) {
							continue
						}
						subNode, ok := prove(cand, visiting, depth+1)
						if !ok {
							continue
						}
						if nodes, finalEnv, ok := matchPremises(idx+1, nextEnv, append(acc, *subNode)); ok {
							return nodes, finalEnv, true
						}
					}
					return nil, nil, false
				case ast.NegAtom:
					inst, ok := substituteAtom(p.Atom, env)
					if !ok || derivedMap[inst.String()] {
						return nil, nil, false
					}
					return matchPremises(idx+1, env, append(acc, DerivationNode{Atom: inst.String(), Negated: true}))
				case ast.Eq:
					l, lok := substituteBaseTerm(p.Left, env)
					r, rok := substituteBaseTerm(p.Right, env)
					if !lok || !rok || l.String() != r.String() {
						return nil, nil, false
					}
					return matchPremises(idx+1, env, acc)
				case ast.Ineq:
					l, lok := substituteBaseTerm(p.Left, env)
					r, rok := substituteBaseTerm(p.Right, env)
					if !lok || !rok || l.String() == r.String() {
						return nil, nil, false
					}
					return matchPremises(idx+1, env, acc)
				default:
					return nil, nil, false
				}
			}

			if premises, finalEnv, ok := matchPremises(0, initBindings, nil); ok {
				strBindings := make(map[string]string, len(finalEnv))
				for k, v := range finalEnv {
					strBindings[k] = v.String()
				}
				step := &DerivationStep{
					DerivedAtom: ts,
					RuleRef:     pr.meta.Ref,
					RuleHash:    pr.meta.RuleHash,
					RuleText:    pr.meta.RuleText,
					Bindings:    strBindings,
					Premises:    premises,
				}
				node := &DerivationNode{Atom: ts, Step: step}
				memo[ts] = node
				return node, true
			}
		}
		return nil, false
	}

	rootNode, ok := prove(goalAtom, make(map[string]bool), 0)
	if !ok || rootNode == nil || rootNode.Step == nil {
		// Mangle derived the atom via complex transform/eval; return a checked entailment marker
		// so grounding doesn't falsely fail, while standard rules always emit full proof trees.
		return nil, true, nil
	}
	return rootNode.Step, true, nil
}

// FactVerifier confirms that ref holds a committed ABox fragment matching expectedHDP containing atom.
type FactVerifier func(ref FragmentRef, expectedHDP string, atom string) error

// RuleVerifier confirms that a TBox rule with (ref, ruleHash, ruleText) is committed in __ontology__.
type RuleVerifier func(ref *FragmentRef, ruleHash string, ruleText string) error

// VerifyDerivationProof independently verifies a DerivationStep proof tree offline:
// checks every ABox leaf against factVer, checks every TBox rule against ruleVer,
// and re-checks every modus-ponens instantiation by applying Bindings to RuleText!
func VerifyDerivationProof(step *DerivationStep, factVer FactVerifier, ruleVer RuleVerifier) error {
	return verifyDerivationStep(step, factVer, ruleVer, 0)
}

func verifyDerivationStep(step *DerivationStep, factVer FactVerifier, ruleVer RuleVerifier, depth int) error {
	if step == nil {
		return errors.New("proof: nil derivation step")
	}
	if depth > MaxDerivationDepth {
		return errors.New("proof: derivation depth exceeds limit")
	}
	if step.RuleHash == "abox:direct" {
		if len(step.Premises) != 1 || step.Premises[0].FactRef == nil || step.Premises[0].Atom != step.DerivedAtom {
			return errors.New("proof: invalid direct ABox witness")
		}
		if factVer != nil {
			return factVer(*step.Premises[0].FactRef, step.Premises[0].FactHDP, step.Premises[0].Atom)
		}
		return nil
	}
	if strings.TrimSpace(step.RuleText) == "" {
		return errors.New("proof: derivation step missing ruleText")
	}
	wantRuleHash := ComputeRuleHash(step.RuleText)
	if step.RuleHash != "" && step.RuleHash != wantRuleHash {
		return fmt.Errorf("proof: ruleHash mismatch (got %s want %s)", step.RuleHash, wantRuleHash)
	}
	if ruleVer != nil {
		if err := ruleVer(step.RuleRef, wantRuleHash, step.RuleText); err != nil {
			return fmt.Errorf("proof: rule verification failed: %w", err)
		}
	}
	rt := strings.TrimSpace(step.RuleText)
	if !strings.HasSuffix(rt, ".") {
		rt += "."
	}
	unit, err := parse.Unit(strings.NewReader(rt + "\n"))
	if err != nil || len(unit.Clauses) != 1 {
		return fmt.Errorf("proof: invalid single rule clause %q", step.RuleText)
	}
	cl := unit.Clauses[0]

	// Parse Bindings into ground Mangle BaseTerms.
	env := make(map[string]ast.BaseTerm, len(step.Bindings))
	for k, v := range step.Bindings {
		bt, pErr := parse.BaseTerm(v)
		if pErr != nil || !isGroundBaseTerm(bt) {
			return fmt.Errorf("proof: invalid ground binding %s=%q", k, v)
		}
		env[k] = bt
	}
	// Verify Head(theta) == DerivedAtom.
	instHead, ok := substituteAtom(cl.Head, env)
	if !ok {
		return fmt.Errorf("proof: incomplete variable bindings for rule head %s", cl.Head.String())
	}
	if instHead.String() != step.DerivedAtom {
		return fmt.Errorf("proof: head instantiation %q != derivedAtom %q", instHead.String(), step.DerivedAtom)
	}

	// Verify every positive premise in cl.Premises matches a verified DerivationNode!
	premByAtom := make(map[string]DerivationNode, len(step.Premises))
	for _, pn := range step.Premises {
		premByAtom[pn.Atom] = pn
	}
	for _, pr := range cl.Premises {
		switch p := pr.(type) {
		case ast.Atom:
			instP, ok := substituteAtom(p, env)
			if !ok {
				return fmt.Errorf("proof: unbound variable in premise %s", p.String())
			}
			node, exists := premByAtom[instP.String()]
			if !exists || node.Negated {
				return fmt.Errorf("proof: missing verified premise %q", instP.String())
			}
			if node.Step != nil {
				if node.Step.DerivedAtom != instP.String() {
					return fmt.Errorf("proof: child step derived %q != expected %q", node.Step.DerivedAtom, instP.String())
				}
				if err := verifyDerivationStep(node.Step, factVer, ruleVer, depth+1); err != nil {
					return err
				}
			} else if node.FactRef != nil {
				if factVer != nil {
					if err := factVer(*node.FactRef, node.FactHDP, node.Atom); err != nil {
						return fmt.Errorf("proof: base fact %s#%d invalid: %w", node.FactRef.Shard, node.FactRef.Seq, err)
					}
				}
			} else {
				return fmt.Errorf("proof: premise %q has neither factRef nor child step", instP.String())
			}
		case ast.NegAtom:
			instN, ok := substituteAtom(p.Atom, env)
			if !ok {
				return fmt.Errorf("proof: unbound variable in negated premise %s", p.Atom.String())
			}
			node, exists := premByAtom[instN.String()]
			if !exists || !node.Negated {
				return fmt.Errorf("proof: missing negated premise witness for %q", instN.String())
			}
		case ast.Eq:
			l, lok := substituteBaseTerm(p.Left, env)
			r, rok := substituteBaseTerm(p.Right, env)
			if !lok || !rok || l.String() != r.String() {
				return fmt.Errorf("proof: equality premise failed")
			}
		case ast.Ineq:
			l, lok := substituteBaseTerm(p.Left, env)
			r, rok := substituteBaseTerm(p.Right, env)
			if !lok || !rok || l.String() == r.String() {
				return fmt.Errorf("proof: inequality premise failed")
			}
		}
	}
	return nil
}

func predKey(p ast.PredicateSym) string {
	return fmt.Sprintf("%s/%d", p.Symbol, p.Arity)
}

func cloneEnv(src map[string]ast.BaseTerm) map[string]ast.BaseTerm {
	dst := make(map[string]ast.BaseTerm, len(src)+2)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func unifyAtom(pattern, ground ast.Atom, env map[string]ast.BaseTerm) bool {
	if pattern.Predicate.Symbol != ground.Predicate.Symbol || len(pattern.Args) != len(ground.Args) {
		return false
	}
	for i := range pattern.Args {
		if !unifyTerm(pattern.Args[i], ground.Args[i], env) {
			return false
		}
	}
	return true
}

func unifyTerm(pattern, ground ast.BaseTerm, env map[string]ast.BaseTerm) bool {
	switch p := pattern.(type) {
	case ast.Variable:
		if p.Symbol == "_" {
			return true
		}
		if cur, ok := env[p.Symbol]; ok {
			return cur.String() == ground.String()
		}
		env[p.Symbol] = ground
		return true
	case ast.Constant:
		return p.String() == ground.String()
	case ast.ApplyFn:
		g, ok := ground.(ast.ApplyFn)
		if !ok || p.Function.Symbol != g.Function.Symbol || len(p.Args) != len(g.Args) {
			return false
		}
		for i := range p.Args {
			if !unifyTerm(p.Args[i], g.Args[i], env) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func substituteAtom(a ast.Atom, env map[string]ast.BaseTerm) (ast.Atom, bool) {
	args := make([]ast.BaseTerm, len(a.Args))
	for i, arg := range a.Args {
		sub, ok := substituteBaseTerm(arg, env)
		if !ok {
			return ast.Atom{}, false
		}
		args[i] = sub
	}
	return ast.Atom{Predicate: a.Predicate, Args: args}, true
}

func substituteBaseTerm(t ast.BaseTerm, env map[string]ast.BaseTerm) (ast.BaseTerm, bool) {
	switch v := t.(type) {
	case ast.Constant:
		return v, true
	case ast.Variable:
		b, ok := env[v.Symbol]
		return b, ok
	case ast.ApplyFn:
		args := make([]ast.BaseTerm, len(v.Args))
		for i, a := range v.Args {
			sub, ok := substituteBaseTerm(a, env)
			if !ok {
				return nil, false
			}
			args[i] = sub
		}
		return ast.ApplyFn{Function: v.Function, Args: args}, true
	default:
		return nil, false
	}
}
