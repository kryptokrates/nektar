package jevgraph

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

const (
	// MaxOntologyClasses caps TBox class declarations to bound memory & Jev Choice criteria.
	MaxOntologyClasses = 256
	// MaxOntologyRelations caps TBox relation declarations to bound Jev Choice criteria.
	MaxOntologyRelations = 256
	// MaxOntologyEntities caps inline gazetteer entries per ontology document.
	MaxOntologyEntities = 4096
	// MaxOntologyBytes caps raw YAML/JSON ontology payloads (1 MiB).
	MaxOntologyBytes = 1 << 20

	// DefaultAcceptMinConfidenceBP is the default minimum calibrated Jev confidence (0.75) for auto-accept.
	DefaultAcceptMinConfidenceBP int64 = 7500
	// DefaultAcceptMinMarginBP is the default minimum top1-top2 probability margin (0.15) for auto-accept.
	DefaultAcceptMinMarginBP int64 = 1500
	// DefaultAcceptMinSupportBP is the default minimum Noul affirmative-support probability (0.70) for auto-accept.
	DefaultAcceptMinSupportBP int64 = 7000
	// DefaultReviewMinConfidenceBP is the default minimum confidence (0.45) to retain a candidate for review.
	DefaultReviewMinConfidenceBP int64 = 4500
)

var (
	ErrOntologyTooLarge = errors.New("jevgraph: ontology exceeds size or element limits")
	ErrOntologyInvalid  = errors.New("jevgraph: invalid ontology schema")
	ErrOntologyCycle    = errors.New("jevgraph: cycle detected in ontology hierarchy")
)

// Thresholds configures deterministic integer-bps (0..10000) acceptance and review bounds.
// Excluded from canonical OntologyID so agents sharing one TBox can tune local risk tolerance.
type Thresholds struct {
	AcceptMinConfidenceBP int64 `yaml:"acceptMinConfidenceBp,omitempty" json:"acceptMinConfidenceBp,omitempty"`
	AcceptMinMarginBP     int64 `yaml:"acceptMinMarginBp,omitempty" json:"acceptMinMarginBp,omitempty"`
	AcceptMinSupportBP    int64 `yaml:"acceptMinSupportBp,omitempty" json:"acceptMinSupportBp,omitempty"`
	ReviewMinConfidenceBP int64 `yaml:"reviewMinConfidenceBp,omitempty" json:"reviewMinConfidenceBp,omitempty"`
}

// Normalized fills zero fields with safe defaults and validates [0, 10000] bounds.
func (t *Thresholds) Normalized() (Thresholds, error) {
	var out Thresholds
	if t != nil {
		out = *t
	}
	if out.AcceptMinConfidenceBP == 0 {
		out.AcceptMinConfidenceBP = DefaultAcceptMinConfidenceBP
	}
	if out.AcceptMinMarginBP == 0 {
		out.AcceptMinMarginBP = DefaultAcceptMinMarginBP
	}
	if out.AcceptMinSupportBP == 0 {
		out.AcceptMinSupportBP = DefaultAcceptMinSupportBP
	}
	if out.ReviewMinConfidenceBP == 0 {
		out.ReviewMinConfidenceBP = DefaultReviewMinConfidenceBP
	}
	for name, v := range map[string]int64{
		"acceptMinConfidenceBp": out.AcceptMinConfidenceBP,
		"acceptMinMarginBp":     out.AcceptMinMarginBP,
		"acceptMinSupportBp":    out.AcceptMinSupportBP,
		"reviewMinConfidenceBp": out.ReviewMinConfidenceBP,
	} {
		if v < 0 || v > 10000 {
			return Thresholds{}, fmt.Errorf("%w: threshold %s=%d out of [0,10000]", ErrOntologyInvalid, name, v)
		}
	}
	if out.ReviewMinConfidenceBP > out.AcceptMinConfidenceBP {
		out.ReviewMinConfidenceBP = out.AcceptMinConfidenceBP
	}
	return out, nil
}

// ClassDef declares one TBox entity class with skos:definition rubric and DL-Lite relations.
type ClassDef struct {
	ID           string   `yaml:"id" json:"id"`
	Label        string   `yaml:"label,omitempty" json:"label,omitempty"`
	Definition   string   `yaml:"definition" json:"definition"`
	Parent       string   `yaml:"parent,omitempty" json:"parent,omitempty"`
	DisjointWith []string `yaml:"disjointWith,omitempty" json:"disjointWith,omitempty"`
}

// RelationDef declares one directed TBox relation with strict domain/range and algebraic flags.
type RelationDef struct {
	ID         string `yaml:"id" json:"id"`
	Label      string `yaml:"label,omitempty" json:"label,omitempty"`
	Domain     string `yaml:"domain" json:"domain"`
	Range      string `yaml:"range" json:"range"`
	Definition string `yaml:"definition" json:"definition"`
	Parent     string `yaml:"parent,omitempty" json:"parent,omitempty"`
	InverseOf  string `yaml:"inverseOf,omitempty" json:"inverseOf,omitempty"`
	Transitive bool   `yaml:"transitive,omitempty" json:"transitive,omitempty"`
	Functional bool   `yaml:"functional,omitempty" json:"functional,omitempty"`
	Symmetric  bool   `yaml:"symmetric,omitempty" json:"symmetric,omitempty"`
}

// EntityEntry binds a canonical entity URI/ID to a TBox class and surface matching forms.
type EntityEntry struct {
	ID      string   `yaml:"id" json:"id"`
	Type    string   `yaml:"type" json:"type"`
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Pattern string   `yaml:"pattern,omitempty" json:"pattern,omitempty"` // optional bounded RE2 regex
}

// Ontology is a JevGraph-compatible domain TBox anchored in a Nektar Memory Repo (e.g. ont-<domain>).
type Ontology struct {
	Version     int                       `yaml:"version" json:"version"`
	Name        string                    `yaml:"name" json:"name"`
	Namespace   string                    `yaml:"namespace" json:"namespace"`
	Description string                    `yaml:"description,omitempty" json:"description,omitempty"`
	Classes     []ClassDef                `yaml:"classes" json:"classes"`
	Relations   []RelationDef             `yaml:"relations" json:"relations"`
	Axioms      []reasoning.OntologyAxiom `yaml:"axioms,omitempty" json:"axioms,omitempty"`
	Entities    []EntityEntry             `yaml:"entities,omitempty" json:"entities,omitempty"`
	Thresholds  *Thresholds               `yaml:"thresholds,omitempty" json:"thresholds,omitempty"`
}

// CompiledOntology is the validated, indexed, Mangle-compiled runtime form of an Ontology.
type CompiledOntology struct {
	ID          string
	Ontology    Ontology
	Thresholds  Thresholds
	ClassesByID map[string]ClassDef
	RelsByID    map[string]RelationDef
	// ancestors[c] contains c and all transitive superclasses of c.
	ancestors map[string]map[string]bool
	// allowedRels[subType][objType] holds sorted RelationDefs valid for (subType, objType).
	allowedRels     map[string]map[string][]RelationDef
	Axioms          []reasoning.OntologyAxiom
	MangleRules     []string
	compiledPatterns map[string]*regexp.Regexp
}

// ParseOntologyYAML parses and validates a YAML TBox ontology document.
func ParseOntologyYAML(raw []byte) (*Ontology, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty ontology document", ErrOntologyInvalid)
	}
	if len(raw) > MaxOntologyBytes {
		return nil, fmt.Errorf("%w: size %d exceeds %d bytes", ErrOntologyTooLarge, len(raw), MaxOntologyBytes)
	}
	var ont Ontology
	if err := yaml.Unmarshal(raw, &ont); err != nil {
		return nil, fmt.Errorf("%w: parse yaml: %w", ErrOntologyInvalid, err)
	}
	if err := ValidateOntology(&ont); err != nil {
		return nil, err
	}
	return &ont, nil
}

// ParseOntologyJSON parses and validates a JSON TBox ontology document.
func ParseOntologyJSON(raw []byte) (*Ontology, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty ontology document", ErrOntologyInvalid)
	}
	if len(raw) > MaxOntologyBytes {
		return nil, fmt.Errorf("%w: size %d exceeds %d bytes", ErrOntologyTooLarge, len(raw), MaxOntologyBytes)
	}
	var ont Ontology
	if err := json.Unmarshal(raw, &ont); err != nil {
		return nil, fmt.Errorf("%w: parse json: %w", ErrOntologyInvalid, err)
	}
	if err := ValidateOntology(&ont); err != nil {
		return nil, err
	}
	return &ont, nil
}

// ParseOntologyAuto detects JSON vs YAML and parses + validates an Ontology.
func ParseOntologyAuto(raw []byte) (*Ontology, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return ParseOntologyJSON(raw)
	}
	return ParseOntologyYAML(raw)
}

func isValidSchemaIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.EqualFold(s, "NONE") {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == ':' || r == '.' || r == '/' {
			continue
		}
		return false
	}
	return true
}

// ValidateOntology enforces structural integrity, boundedness, reference validity, and acyclicity.
func ValidateOntology(ont *Ontology) error {
	if ont == nil {
		return fmt.Errorf("%w: nil ontology", ErrOntologyInvalid)
	}
	if ont.Version == 0 {
		ont.Version = 1
	}
	if ont.Version != 1 {
		return fmt.Errorf("%w: unsupported version %d", ErrOntologyInvalid, ont.Version)
	}
	ont.Name = strings.TrimSpace(ont.Name)
	ont.Namespace = strings.TrimSpace(ont.Namespace)
	ont.Description = strings.TrimSpace(ont.Description)
	if ont.Name == "" {
		return fmt.Errorf("%w: name is required", ErrOntologyInvalid)
	}
	if ont.Namespace == "" {
		ont.Namespace = "nektar:ont:" + strings.ToLower(ont.Name) + ":v1"
	}
	if len(ont.Classes) == 0 {
		return fmt.Errorf("%w: at least one class is required", ErrOntologyInvalid)
	}
	if len(ont.Classes) > MaxOntologyClasses {
		return fmt.Errorf("%w: classes %d exceeds %d", ErrOntologyTooLarge, len(ont.Classes), MaxOntologyClasses)
	}
	if len(ont.Relations) > MaxOntologyRelations {
		return fmt.Errorf("%w: relations %d exceeds %d", ErrOntologyTooLarge, len(ont.Relations), MaxOntologyRelations)
	}
	if len(ont.Entities) > MaxOntologyEntities {
		return fmt.Errorf("%w: entities %d exceeds %d", ErrOntologyTooLarge, len(ont.Entities), MaxOntologyEntities)
	}

	classSet := make(map[string]ClassDef, len(ont.Classes))
	for i := range ont.Classes {
		c := &ont.Classes[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Label = strings.TrimSpace(c.Label)
		c.Definition = strings.TrimSpace(c.Definition)
		c.Parent = strings.TrimSpace(c.Parent)
		if !isValidSchemaIdent(c.ID) {
			return fmt.Errorf("%w: invalid class id %q", ErrOntologyInvalid, c.ID)
		}
		if c.Label == "" {
			c.Label = c.ID
		}
		if c.Definition == "" {
			c.Definition = c.Label
		}
		if _, dup := classSet[c.ID]; dup {
			return fmt.Errorf("%w: duplicate class %q", ErrOntologyInvalid, c.ID)
		}
		classSet[c.ID] = *c
	}
	for _, c := range ont.Classes {
		if c.Parent != "" {
			if _, ok := classSet[c.Parent]; !ok {
				return fmt.Errorf("%w: class %q references unknown parent %q", ErrOntologyInvalid, c.ID, c.Parent)
			}
		}
		for _, d := range c.DisjointWith {
			d = strings.TrimSpace(d)
			if d == c.ID {
				return fmt.Errorf("%w: class %q cannot be disjoint with itself", ErrOntologyInvalid, c.ID)
			}
			if _, ok := classSet[d]; !ok {
				return fmt.Errorf("%w: class %q disjointWith unknown class %q", ErrOntologyInvalid, c.ID, d)
			}
		}
	}
	// Check class hierarchy acyclicity.
	if err := checkParentAcyclic(len(classSet), func(yield func(id, parent string)) {
		for _, c := range ont.Classes {
			yield(c.ID, c.Parent)
		}
	}); err != nil {
		return err
	}

	relSet := make(map[string]RelationDef, len(ont.Relations))
	for i := range ont.Relations {
		r := &ont.Relations[i]
		r.ID = strings.TrimSpace(r.ID)
		r.Label = strings.TrimSpace(r.Label)
		r.Domain = strings.TrimSpace(r.Domain)
		r.Range = strings.TrimSpace(r.Range)
		r.Definition = strings.TrimSpace(r.Definition)
		r.Parent = strings.TrimSpace(r.Parent)
		r.InverseOf = strings.TrimSpace(r.InverseOf)
		if !isValidSchemaIdent(r.ID) {
			return fmt.Errorf("%w: invalid relation id %q", ErrOntologyInvalid, r.ID)
		}
		if r.Label == "" {
			r.Label = r.ID
		}
		if r.Definition == "" {
			return fmt.Errorf("%w: relation %q requires a definition (skos:definition rubric)", ErrOntologyInvalid, r.ID)
		}
		if _, ok := classSet[r.Domain]; !ok {
			return fmt.Errorf("%w: relation %q references unknown domain class %q", ErrOntologyInvalid, r.ID, r.Domain)
		}
		if _, ok := classSet[r.Range]; !ok {
			return fmt.Errorf("%w: relation %q references unknown range class %q", ErrOntologyInvalid, r.ID, r.Range)
		}
		if _, dup := relSet[r.ID]; dup {
			return fmt.Errorf("%w: duplicate relation %q", ErrOntologyInvalid, r.ID)
		}
		relSet[r.ID] = *r
	}
	for _, r := range ont.Relations {
		if r.Parent != "" {
			if _, ok := relSet[r.Parent]; !ok {
				return fmt.Errorf("%w: relation %q references unknown parent %q", ErrOntologyInvalid, r.ID, r.Parent)
			}
		}
		if r.InverseOf != "" {
			if _, ok := relSet[r.InverseOf]; !ok {
				return fmt.Errorf("%w: relation %q references unknown inverseOf %q", ErrOntologyInvalid, r.ID, r.InverseOf)
			}
		}
	}
	if err := checkParentAcyclic(len(relSet), func(yield func(id, parent string)) {
		for _, r := range ont.Relations {
			yield(r.ID, r.Parent)
		}
	}); err != nil {
		return err
	}

	seenEnt := make(map[string]bool, len(ont.Entities))
	for i := range ont.Entities {
		e := &ont.Entities[i]
		e.ID = strings.TrimSpace(e.ID)
		e.Type = strings.TrimSpace(e.Type)
		e.Pattern = strings.TrimSpace(e.Pattern)
		if e.ID == "" {
			return fmt.Errorf("%w: entity entry missing id", ErrOntologyInvalid)
		}
		if seenEnt[e.ID] {
			return fmt.Errorf("%w: duplicate entity id %q", ErrOntologyInvalid, e.ID)
		}
		seenEnt[e.ID] = true
		if _, ok := classSet[e.Type]; !ok {
			return fmt.Errorf("%w: entity %q references unknown class %q", ErrOntologyInvalid, e.ID, e.Type)
		}
		if e.Pattern != "" {
			if len(e.Pattern) > 256 {
				return fmt.Errorf("%w: entity %q regex pattern exceeds 256 chars", ErrOntologyInvalid, e.ID)
			}
			if _, err := regexp.Compile(e.Pattern); err != nil {
				return fmt.Errorf("%w: entity %q invalid RE2 pattern: %w", ErrOntologyInvalid, e.ID, err)
			}
		}
	}
	if _, err := ont.Thresholds.Normalized(); err != nil {
		return err
	}
	return nil
}

func checkParentAcyclic(n int, iter func(yield func(id, parent string))) error {
	parents := make(map[string]string, n)
	iter(func(id, parent string) {
		parents[id] = parent
	})
	state := make(map[string]int, n) // 0=unvisited, 1=visiting, 2=done
	var dfs func(id string) error
	dfs = func(id string) error {
		state[id] = 1
		if p := parents[id]; p != "" {
			if state[p] == 1 {
				return fmt.Errorf("%w: %s -> %s", ErrOntologyCycle, id, p)
			}
			if state[p] == 0 {
				if err := dfs(p); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		return nil
	}
	for id := range parents {
		if state[id] == 0 {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// ComputeOntologyID hashes strictly the shared TBox vocabulary (version, name, namespace,
// description, sorted classes, sorted relations, sorted axioms) using TagOntologyV1.
// Local entity gazetteers and thresholds are intentionally excluded so different agents
// sharing the same TBox always derive the exact same 64-hex OntologyID on the BSV market wire.
func ComputeOntologyID(ont *Ontology) (string, error) {
	if err := ValidateOntology(ont); err != nil {
		return "", err
	}
	classes := make([]ClassDef, len(ont.Classes))
	for i, c := range ont.Classes {
		cp := c
		if len(c.DisjointWith) > 0 {
			cp.DisjointWith = append([]string(nil), c.DisjointWith...)
			for j := range cp.DisjointWith {
				cp.DisjointWith[j] = strings.TrimSpace(cp.DisjointWith[j])
			}
			sort.Strings(cp.DisjointWith)
		}
		classes[i] = cp
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].ID < classes[j].ID })

	rels := make([]RelationDef, len(ont.Relations))
	copy(rels, ont.Relations)
	sort.Slice(rels, func(i, j int) bool { return rels[i].ID < rels[j].ID })

	axioms := make([]reasoning.OntologyAxiom, len(ont.Axioms))
	for i, ax := range ont.Axioms {
		fin, _, err := reasoning.FinalizeAxiom(ax)
		if err != nil {
			return "", fmt.Errorf("jevgraph: invalid axiom %q: %w", ax.ID, err)
		}
		axioms[i] = fin
	}
	sort.Slice(axioms, func(i, j int) bool {
		if axioms[i].Hash != axioms[j].Hash {
			return axioms[i].Hash < axioms[j].Hash
		}
		return axioms[i].ID < axioms[j].ID
	})

	tbox := struct {
		Version     int                       `json:"version"`
		Name        string                    `json:"name"`
		Namespace   string                    `json:"namespace"`
		Description string                    `json:"description,omitempty"`
		Classes     []ClassDef                `json:"classes"`
		Relations   []RelationDef             `json:"relations"`
		Axioms      []reasoning.OntologyAxiom `json:"axioms,omitempty"`
	}{
		Version:     ont.Version,
		Name:        ont.Name,
		Namespace:   ont.Namespace,
		Description: ont.Description,
		Classes:     classes,
		Relations:   rels,
		Axioms:      axioms,
	}
	canon, err := core.CanonicalJSON(tbox)
	if err != nil {
		return "", fmt.Errorf("jevgraph: canonicalize TBox: %w", err)
	}
	return hex.EncodeToString(core.DSHA256(core.Concat(core.TagOntologyV1, canon))), nil
}

// Compile validates ont, computes its deterministic OntologyID, precomputes type subsumption
// and directed allowed-relation tables, and compiles all TBox constraints into Mangle rules.
func Compile(ont *Ontology) (*CompiledOntology, error) {
	if err := ValidateOntology(ont); err != nil {
		return nil, err
	}
	id, err := ComputeOntologyID(ont)
	if err != nil {
		return nil, err
	}
	thresh, err := ont.Thresholds.Normalized()
	if err != nil {
		return nil, err
	}

	classesByID := make(map[string]ClassDef, len(ont.Classes))
	for _, c := range ont.Classes {
		classesByID[c.ID] = c
	}
	relsByID := make(map[string]RelationDef, len(ont.Relations))
	for _, r := range ont.Relations {
		relsByID[r.ID] = r
	}

	// Precompute reflexive-transitive superclass closure for every class.
	ancestors := make(map[string]map[string]bool, len(ont.Classes))
	for _, c := range ont.Classes {
		anc := map[string]bool{c.ID: true}
		cur := c.Parent
		for cur != "" {
			anc[cur] = true
			cur = classesByID[cur].Parent
		}
		ancestors[c.ID] = anc
	}

	// Precompute allowedRelations[subClass][objClass] in deterministic sorted order.
	allowed := make(map[string]map[string][]RelationDef, len(ont.Classes))
	for _, sc := range ont.Classes {
		allowed[sc.ID] = make(map[string][]RelationDef)
		for _, oc := range ont.Classes {
			var list []RelationDef
			for _, r := range ont.Relations {
				if ancestors[sc.ID][r.Domain] && ancestors[oc.ID][r.Range] {
					list = append(list, r)
				}
			}
			if len(list) > 0 {
				sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
				allowed[sc.ID][oc.ID] = list
			}
		}
	}

	// Compile TBox axioms into validated stratified Mangle Datalog clauses.
	var axioms []reasoning.OntologyAxiom
	var mangleRules []string
	seenRule := make(map[string]bool)
	addAxiom := func(ax reasoning.OntologyAxiom) error {
		fin, compiled, err := reasoning.FinalizeAxiom(ax)
		if err != nil {
			return err
		}
		axioms = append(axioms, fin)
		for _, r := range compiled {
			if !seenRule[r] {
				seenRule[r] = true
				mangleRules = append(mangleRules, r)
			}
		}
		return nil
	}

	// Deterministic compilation order: sorted classes, sorted relations, explicit axioms.
	sortedClassIDs := make([]string, 0, len(classesByID))
	for cid := range classesByID {
		sortedClassIDs = append(sortedClassIDs, cid)
	}
	sort.Strings(sortedClassIDs)
	seenDisj := make(map[string]bool)
	for _, cid := range sortedClassIDs {
		c := classesByID[cid]
		if c.Parent != "" {
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:      "sub:" + c.ID + ":" + c.Parent,
				Kind:    reasoning.AxiomSubClassOf,
				Subject: c.ID,
				Object:  c.Parent,
			}); err != nil {
				return nil, err
			}
		}
		disjs := append([]string(nil), c.DisjointWith...)
		sort.Strings(disjs)
		for _, d := range disjs {
			d = strings.TrimSpace(d)
			a, b := c.ID, d
			if a > b {
				a, b = b, a
			}
			key := a + "|" + b
			if seenDisj[key] {
				continue
			}
			seenDisj[key] = true
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:      "disj:" + a + ":" + b,
				Kind:    reasoning.AxiomDisjointWith,
				Subject: a,
				Object:  b,
			}); err != nil {
				return nil, err
			}
		}
	}

	sortedRelIDs := make([]string, 0, len(relsByID))
	for rid := range relsByID {
		sortedRelIDs = append(sortedRelIDs, rid)
	}
	sort.Strings(sortedRelIDs)
	for _, rid := range sortedRelIDs {
		r := relsByID[rid]
		if err := addAxiom(reasoning.OntologyAxiom{
			ID:      "dom:" + r.ID + ":" + r.Domain,
			Kind:    reasoning.AxiomDomain,
			Subject: r.ID,
			Object:  r.Domain,
		}); err != nil {
			return nil, err
		}
		if err := addAxiom(reasoning.OntologyAxiom{
			ID:      "ran:" + r.ID + ":" + r.Range,
			Kind:    reasoning.AxiomRange,
			Subject: r.ID,
			Object:  r.Range,
		}); err != nil {
			return nil, err
		}
		if r.Parent != "" {
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:      "subp:" + r.ID + ":" + r.Parent,
				Kind:    reasoning.AxiomSubPropertyOf,
				Subject: r.ID,
				Object:  r.Parent,
			}); err != nil {
				return nil, err
			}
		}
		if r.InverseOf != "" {
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:      "inv:" + r.ID + ":" + r.InverseOf,
				Kind:    reasoning.AxiomInverseOf,
				Subject: r.ID,
				Object:  r.InverseOf,
			}); err != nil {
				return nil, err
			}
		}
		if r.Transitive {
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:      "trans:" + r.ID,
				Kind:    reasoning.AxiomTransitiveProperty,
				Subject: r.ID,
			}); err != nil {
				return nil, err
			}
		}
		if r.Symmetric {
			qr := strconv.Quote(r.ID)
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:       "sym:" + r.ID,
				Kind:     reasoning.AxiomMangleRule,
				RuleText: fmt.Sprintf("triple(Y, %s, X) :- triple(X, %s, Y).", qr, qr),
			}); err != nil {
				return nil, err
			}
		}
		if r.Functional {
			qr := strconv.Quote(r.ID)
			qtag := strconv.Quote(r.ID + ":functional")
			if err := addAxiom(reasoning.OntologyAxiom{
				ID:       "func:" + r.ID,
				Kind:     reasoning.AxiomMangleRule,
				RuleText: fmt.Sprintf("ontology_conflict(X, %s, Y) :- triple(X, %s, Y), triple(X, %s, Z), Y != Z.", qtag, qr, qr),
			}); err != nil {
				return nil, err
			}
		}
	}
	for _, ax := range ont.Axioms {
		if err := addAxiom(ax); err != nil {
			return nil, err
		}
	}

	compiledPatterns := make(map[string]*regexp.Regexp)
	for _, e := range ont.Entities {
		if e.Pattern != "" {
			re, err := regexp.Compile(e.Pattern)
			if err != nil {
				return nil, err
			}
			compiledPatterns[e.ID] = re
		}
	}

	cpOnt := *ont
	cpOnt.Classes = append([]ClassDef(nil), ont.Classes...)
	cpOnt.Relations = append([]RelationDef(nil), ont.Relations...)
	cpOnt.Axioms = append([]reasoning.OntologyAxiom(nil), ont.Axioms...)
	cpOnt.Entities = append([]EntityEntry(nil), ont.Entities...)

	return &CompiledOntology{
		ID:               id,
		Ontology:         cpOnt,
		Thresholds:       thresh,
		ClassesByID:      classesByID,
		RelsByID:         relsByID,
		ancestors:        ancestors,
		allowedRels:      allowed,
		Axioms:           axioms,
		MangleRules:      mangleRules,
		compiledPatterns: compiledPatterns,
	}, nil
}

// IsSubtype reports whether subClass is equal to or a descendant of superClass.
func (c *CompiledOntology) IsSubtype(subClass, superClass string) bool {
	if c == nil {
		return false
	}
	return c.ancestors[subClass][superClass]
}

// AllowedRelations returns all TBox relations valid for a directed pair (subjectClass, objectClass).
func (c *CompiledOntology) AllowedRelations(subjectClass, objectClass string) []RelationDef {
	if c == nil {
		return nil
	}
	byObj := c.allowedRels[subjectClass]
	if byObj == nil {
		return nil
	}
	return byObj[objectClass]
}

// PredicateSet returns the exact set of TBox relation IDs declared in this compiled ontology,
// used by NMU-1 anti-stuffing triple counting so unknown noise predicates never inflate UnitCount.
func (c *CompiledOntology) PredicateSet() map[string]struct{} {
	if c == nil || len(c.RelsByID) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(c.RelsByID))
	for id := range c.RelsByID {
		out[id] = struct{}{}
	}
	return out
}
