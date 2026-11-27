package jev

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	ahocorasick "github.com/BobuSumisu/aho-corasick"

	"github.com/kryptokrates/nektar/internal/reasoning"
)

const (
	DefaultMaxGraphPairs       = 12
	DefaultGraphMinConfidence  = 0.70
	DefaultGraphPairWindowByte = 280
)

// EntityMention is one exact O(n) Aho-Corasick entity hit with byte-accurate evidence offsets.
type EntityMention struct {
	EntityID  string `json:"entityId"`
	Type      string `json:"type,omitempty"`
	Surface   string `json:"surface"`
	StartByte int    `json:"startByte"`
	EndByte   int    `json:"endByte"`
}

// EvidenceSpan anchors an extracted triple back to the exact byte substring in the sanitized source text.
type EvidenceSpan struct {
	StartByte int    `json:"startByte"`
	EndByte   int    `json:"endByte"`
	Quote     string `json:"quote"`
}

// ExtractedTriple pairs a canonical CBOR-hashed reasoning.Triple with extraction confidence and source evidence.
type ExtractedTriple struct {
	Triple       reasoning.Triple `json:"triple"`
	ConfidenceBP int64            `json:"confidenceBp"` // 0..10000 deterministic integer basis points
	Evidence     EvidenceSpan     `json:"evidence"`
	Extractor    string           `json:"extractor"` // "jev" | "deterministic"
}

// ExtractGraphInput configures one bounded JevGraph extraction pass over sanitized text.
type ExtractGraphInput struct {
	Text          string
	Manifest      reasoning.OntologyManifest
	CompiledRules []string
	MaxPairs      int
	MinConfidence float64
	WindowBytes   int
}

// ExtractGraphResult summarizes entities blocked, triples extracted, and Mangle TBox consistency verification.
type ExtractGraphResult struct {
	OntologyHash     string            `json:"ontologyHash"`
	Mode             string            `json:"mode"` // "jev" | "deterministic"
	Mentions         []EntityMention   `json:"mentions,omitempty"`
	Triples          []ExtractedTriple `json:"triples,omitempty"`
	RejectedTriples  int               `json:"rejectedTriples,omitempty"`
	TripleSetHash    string            `json:"tripleSetHash,omitempty"`
	Consistent       bool              `json:"consistent"`
	ConflictReasons  []string          `json:"conflictReasons,omitempty"`
	InputTokens      int               `json:"inputTokens,omitempty"`
}

type patternBinding struct {
	entityID string
	typ      string
	length   int
}

// asciiFoldBytes folds ASCII A-Z to a-z in-place preserving exact UTF-8 byte offsets 1:1.
func asciiFoldBytes(s string) []byte {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return b
}

func isWordBoundary(b []byte, start, end int) bool {
	if start > 0 && isIdentByte(b[start-1]) && isIdentByte(b[start]) {
		return false
	}
	if end < len(b) && isIdentByte(b[end]) && isIdentByte(b[end-1]) {
		return false
	}
	return true
}

func isIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// BlockEntities runs a single O(n) Aho-Corasick scan over text using manifest.Entities (IDs + aliases)
// returning non-overlapping, longest-first EntityMentions with exact byte offsets into text.
func BlockEntities(text string, manifest reasoning.OntologyManifest) []EntityMention {
	if strings.TrimSpace(text) == "" || len(manifest.Entities) == 0 {
		return nil
	}
	tb := ahocorasick.NewTrieBuilder()
	var bindings []patternBinding
	seenPat := make(map[string]bool)

	addPat := func(raw, entID, typ string) {
		p := strings.TrimSpace(raw)
		if len(p) < 2 {
			return
		}
		folded := string(asciiFoldBytes(p))
		if seenPat[folded] {
			return
		}
		seenPat[folded] = true
		tb.AddString(folded)
		bindings = append(bindings, patternBinding{
			entityID: entID,
			typ:      typ,
			length:   len(folded),
		})
	}

	norm := reasoning.NormalizeOntologyManifest(manifest)
	for _, e := range norm.Entities {
		addPat(e.ID, e.ID, e.Type)
		// Also match local name after prefix (e.g. "ops:sqlite" -> "sqlite").
		if idx := strings.IndexByte(e.ID, ':'); idx >= 0 && idx+1 < len(e.ID) {
			addPat(e.ID[idx+1:], e.ID, e.Type)
		}
		for _, al := range e.Aliases {
			addPat(al, e.ID, e.Type)
		}
	}
	if len(bindings) == 0 {
		return nil
	}

	trie := tb.Build()
	foldedText := asciiFoldBytes(text)
	rawMatches := trie.Match(foldedText)
	if len(rawMatches) == 0 {
		return nil
	}

	rawMentions := make([]EntityMention, 0, len(rawMatches))
	for _, m := range rawMatches {
		patIdx := int(m.Pattern())
		if patIdx < 0 || patIdx >= len(bindings) {
			continue
		}
		start := int(m.Pos())
		end := start + len(m.Match())
		if start < 0 || end > len(text) || start >= end {
			continue
		}
		if !isWordBoundary(foldedText, start, end) {
			continue
		}
		bind := bindings[patIdx]
		rawMentions = append(rawMentions, EntityMention{
			EntityID:  bind.entityID,
			Type:      bind.typ,
			Surface:   text[start:end],
			StartByte: start,
			EndByte:   end,
		})
	}
	// Deterministic longest-span-first deduplication for overlapping matches.
	sort.Slice(rawMentions, func(i, j int) bool {
		if rawMentions[i].StartByte != rawMentions[j].StartByte {
			return rawMentions[i].StartByte < rawMentions[j].StartByte
		}
		li := rawMentions[i].EndByte - rawMentions[i].StartByte
		lj := rawMentions[j].EndByte - rawMentions[j].StartByte
		if li != lj {
			return li > lj
		}
		return rawMentions[i].EntityID < rawMentions[j].EntityID
	})
	var out []EntityMention
	lastEnd := -1
	for _, m := range rawMentions {
		if m.StartByte < lastEnd {
			continue
		}
		out = append(out, m)
		lastEnd = m.EndByte
	}
	return out
}

type candidatePair struct {
	subject EntityMention
	object  EntityMention
	span    EvidenceSpan
}

func buildCandidatePairs(text string, mentions []EntityMention, windowBytes, maxPairs int) []candidatePair {
	if len(mentions) < 2 {
		return nil
	}
	if windowBytes <= 0 {
		windowBytes = DefaultGraphPairWindowByte
	}
	if maxPairs <= 0 {
		maxPairs = DefaultMaxGraphPairs
	}
	seen := make(map[string]bool)
	var pairs []candidatePair
	for i := 0; i < len(mentions); i++ {
		for j := i + 1; j < len(mentions); j++ {
			a, b := mentions[i], mentions[j]
			if b.StartByte-a.EndByte > windowBytes {
				break
			}
			if a.EntityID == b.EntityID {
				continue
			}
			key := a.EntityID + "|" + b.EntityID
			if seen[key] {
				continue
			}
			seen[key] = true
			spanStart, spanEnd := expandSentenceSpan(text, a.StartByte, b.EndByte)
			pairs = append(pairs, candidatePair{
				subject: a,
				object:  b,
				span: EvidenceSpan{
					StartByte: spanStart,
					EndByte:   spanEnd,
					Quote:     strings.TrimSpace(text[spanStart:spanEnd]),
				},
			})
			if len(pairs) >= maxPairs {
				return pairs
			}
		}
	}
	return pairs
}

func expandSentenceSpan(text string, start, end int) (int, int) {
	s := start
	for s > 0 && start-s < 120 {
		c := text[s-1]
		if c == '\n' || c == '.' || c == '!' || c == '?' {
			break
		}
		s--
	}
	for s < start && unicode.IsSpace(rune(text[s])) {
		s++
	}
	e := end
	for e < len(text) && e-end < 120 {
		c := text[e]
		e++
		if c == '\n' || c == '.' || c == '!' || c == '?' {
			break
		}
	}
	return s, e
}

// ExtractGraph executes the bounded JevGraph pipeline:
// 1. O(n) Aho-Corasick candidate blocking with exact byte spans.
// 2. Single batched Jev System One forward pass (Choice + Noul per candidate pair) when eval != nil,
//    automatically falling back to deterministic pure-Go extraction when eval == nil or unavailable.
// 3. Mangle TBox consistency verification (reasoning.CheckOntologyConsistency).
func ExtractGraph(ctx context.Context, eval Evaluator, in ExtractGraphInput) (*ExtractGraphResult, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, errors.New("jev: graph extraction requires non-empty text")
	}
	finMan, compiledRules, _, err := reasoning.FinalizeOntologyManifest(in.Manifest)
	if err != nil {
		return nil, err
	}
	if len(in.CompiledRules) > 0 {
		compiledRules = in.CompiledRules
	}
	minConf := in.MinConfidence
	if minConf <= 0 || minConf > 1 {
		minConf = DefaultGraphMinConfidence
	}
	maxPairs := in.MaxPairs
	if maxPairs <= 0 {
		maxPairs = DefaultMaxGraphPairs
	}

	mentions := BlockEntities(in.Text, *finMan)
	pairs := buildCandidatePairs(in.Text, mentions, in.WindowBytes, maxPairs)

	res := &ExtractGraphResult{
		OntologyHash: finMan.Hash,
		Mode:         "deterministic",
		Mentions:     mentions,
		Consistent:   true,
	}

	var candidates []ExtractedTriple
	// Always honor explicit canonical N-Triples embedded in the text first.
	for _, line := range strings.Split(in.Text, "\n") {
		if tr, ok := reasoning.ParseNTripleLine(line); ok {
			idx := strings.Index(in.Text, strings.TrimSpace(line))
			start, end := 0, len(in.Text)
			if idx >= 0 {
				start = idx
				end = idx + len(strings.TrimSpace(line))
			}
			candidates = append(candidates, ExtractedTriple{
				Triple:       tr,
				ConfidenceBP: 10000,
				Evidence:     EvidenceSpan{StartByte: start, EndByte: end, Quote: strings.TrimSpace(line)},
				Extractor:    "deterministic",
			})
		}
	}

	usedJev := false
	if eval != nil && len(pairs) > 0 && len(finMan.Predicates) > 0 {
		jevTriples, toks, jErr := extractPairsWithJev(ctx, eval, in.Text, *finMan, pairs, minConf)
		if jErr == nil {
			usedJev = true
			res.Mode = "jev"
			res.InputTokens = toks
			candidates = append(candidates, jevTriples...)
		}
	}
	if !usedJev {
		candidates = append(candidates, extractPairsDeterministic(in.Text, *finMan, pairs)...)
	}

	// Add entity rdf:type facts for Mangle domain/range & disjointness checking.
	baseAtoms := make([]string, 0, len(mentions)+len(candidates)*2)
	seenEntType := make(map[string]bool)
	for _, m := range mentions {
		if m.Type == "" || seenEntType[m.EntityID+"|"+m.Type] {
			continue
		}
		seenEntType[m.EntityID+"|"+m.Type] = true
		if t, tErr := reasoning.FinalizeTriple(reasoning.Triple{Subject: m.EntityID, Predicate: "rdf:type", Object: m.Type}); tErr == nil {
			if atoms, aErr := t.ToMangleAtoms(); aErr == nil {
				baseAtoms = append(baseAtoms, atoms...)
			}
		}
	}

	// Validate each candidate triple canonically + check Mangle TBox consistency.
	dedupTriple := make(map[string]bool)
	var accepted []ExtractedTriple
	var rawTriples []reasoning.Triple
	for _, cand := range candidates {
		fin, tErr := reasoning.FinalizeTriple(cand.Triple)
		if tErr != nil {
			res.RejectedTriples++
			continue
		}
		if dedupTriple[fin.Hash] {
			continue
		}
		atoms, aErr := fin.ToMangleAtoms()
		if aErr != nil {
			res.RejectedTriples++
			continue
		}
		if len(compiledRules) > 0 {
			checkFacts := append(append([]string(nil), baseAtoms...), atoms...)
			rep, cErr := reasoning.CheckOntologyConsistency(checkFacts, compiledRules)
			if cErr != nil || (rep != nil && !rep.Consistent) {
				res.RejectedTriples++
				if rep != nil {
					res.ConflictReasons = append(res.ConflictReasons, rep.Conflicts...)
					res.ConflictReasons = append(res.ConflictReasons, rep.DeonticViolations...)
				}
				continue
			}
		}
		dedupTriple[fin.Hash] = true
		cand.Triple = fin
		accepted = append(accepted, cand)
		rawTriples = append(rawTriples, fin)
		baseAtoms = append(baseAtoms, atoms...)
	}

	// Final whole-graph consistency check across all accepted triples combined.
	if len(compiledRules) > 0 && len(baseAtoms) > 0 {
		if rep, cErr := reasoning.CheckOntologyConsistency(baseAtoms, compiledRules); cErr != nil {
			res.Consistent = false
			res.ConflictReasons = append(res.ConflictReasons, cErr.Error())
		} else if rep != nil && !rep.Consistent {
			res.Consistent = false
			res.ConflictReasons = append(res.ConflictReasons, rep.Conflicts...)
			res.ConflictReasons = append(res.ConflictReasons, rep.DeonticViolations...)
		}
	}

	res.Triples = accepted
	if len(rawTriples) > 0 {
		if setHash, hErr := reasoning.ComputeTripleSetHash(rawTriples); hErr == nil {
			res.TripleSetHash = setHash
		}
	}
	return res, nil
}

func extractPairsWithJev(
	ctx context.Context,
	eval Evaluator,
	text string,
	manifest reasoning.OntologyManifest,
	pairs []candidatePair,
	minConf float64,
) ([]ExtractedTriple, int, error) {
	criteria := make(map[string]string, len(manifest.Predicates)+1)
	criteria["none"] = "No direct ontology-supported relationship stated between subject and object in the text"
	for _, p := range manifest.Predicates {
		desc := p.Description
		if desc == "" {
			desc = "Relation " + p.ID
		}
		criteria[p.ID] = desc
	}
	questions := make(map[string]Question, len(pairs)*2)
	pairItems := make([]map[string]any, len(pairs))
	for i, cp := range pairs {
		pairItems[i] = map[string]any{
			"index":   i,
			"subject": cp.subject.EntityID,
			"object":  cp.object.EntityID,
			"quote":   cp.span.Quote,
		}
		questions[fmt.Sprintf("rel_%d", i)] = Question{
			Type:         TypeChoice,
			Instructions: fmt.Sprintf("Select the exact ontology predicate linking subject %q to object %q based ONLY on the quote, or \"none\".", cp.subject.EntityID, cp.object.EntityID),
			Criteria:     criteria,
		}
		questions[fmt.Sprintf("valid_%d", i)] = Question{
			Type:         TypeNoul,
			Instructions: fmt.Sprintf("Probability [0,1] that the quote explicitly asserts a factual relation between %q and %q.", cp.subject.EntityID, cp.object.EntityID),
		}
	}
	snippet := text
	if len(snippet) > 2048 {
		snippet = snippet[:2048]
	}
	req := Request{
		State: map[string]any{
			"ontology": manifest.Domain,
			"text":     snippet,
			"pairs":    pairItems,
		},
		Questions: questions,
	}
	resp, err := eval.Evaluate(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	var out []ExtractedTriple
	for i, cp := range pairs {
		relAns := resp.Answers[fmt.Sprintf("rel_%d", i)]
		valAns := resp.Answers[fmt.Sprintf("valid_%d", i)]
		if relAns.Choice == "" || relAns.Choice == "none" {
			continue
		}
		prob, ok := valAns.NoulProb()
		if !ok || prob < minConf {
			continue
		}
		conf := prob
		if c, ok := relAns.ConfidenceValue(); ok && c < conf {
			conf = c
		}
		confBP := int64(conf*10000 + 0.5)
		if confBP > 10000 {
			confBP = 10000
		}
		out = append(out, ExtractedTriple{
			Triple: reasoning.Triple{
				Subject:   cp.subject.EntityID,
				Predicate: relAns.Choice,
				Object:    cp.object.EntityID,
			},
			ConfidenceBP: confBP,
			Evidence:     cp.span,
			Extractor:    "jev",
		})
	}
	return out, resp.Usage.InputTokens, nil
}

type detRule struct {
	predicate string
	re        *regexp.Regexp
}

var deterministicRelPatterns = []detRule{
	{predicate: "requires", re: regexp.MustCompile(`\b(requires?|needs?|must\s+(?:use|set|enable)|depends?\s+on)\b`)},
	{predicate: "fixes", re: regexp.MustCompile(`\b(fixes?|resolves?|workaround\s+for|patches?|solves?)\b`)},
	{predicate: "supersedes", re: regexp.MustCompile(`\b(supersedes?|replaces?|deprecates?|migrated?\s+from)\b`)},
	{predicate: "verifies", re: regexp.MustCompile(`\b(verifies?|validates?|proves?|checks?)\b`)},
	{predicate: "mitigates", re: regexp.MustCompile(`\b(mitigates?|prevents?|blocks?|defends?\s+against)\b`)},
	{predicate: "breaks_in", re: regexp.MustCompile(`\b(breaks?|breaking\s+change|regression\s+in|fails?\s+in)\b`)},
	{predicate: "causes", re: regexp.MustCompile(`\b(causes?|triggers?|leads?\s+to)\b`)},
	{predicate: "integrates_with", re: regexp.MustCompile(`\b(integrates?\s+with|docks?\s+with|wires?\s+into|connects?\s+to)\b`)},
	{predicate: "enforces", re: regexp.MustCompile(`\b(enforces?|guarantees?|guards?)\b`)},
	{predicate: "incompatible_with", re: regexp.MustCompile(`\b(incompatible\s+with|conflicts?\s+with)\b`)},
}

func extractPairsDeterministic(text string, manifest reasoning.OntologyManifest, pairs []candidatePair) []ExtractedTriple {
	allowed := make(map[string]bool, len(manifest.Predicates))
	for _, p := range manifest.Predicates {
		allowed[p.ID] = true
	}
	var out []ExtractedTriple
	for _, cp := range pairs {
		betweenStart := cp.subject.EndByte
		betweenEnd := cp.object.StartByte
		if betweenStart > betweenEnd || betweenEnd > len(text) {
			continue
		}
		between := strings.ToLower(text[betweenStart:betweenEnd])
		quoteLower := strings.ToLower(cp.span.Quote)
		matchedPred := ""
		for _, r := range deterministicRelPatterns {
			if len(allowed) > 0 && !allowed[r.predicate] {
				continue
			}
			if r.re.MatchString(between) || r.re.MatchString(quoteLower) {
				matchedPred = r.predicate
				break
			}
		}
		if matchedPred == "" {
			continue
		}
		out = append(out, ExtractedTriple{
			Triple: reasoning.Triple{
				Subject:   cp.subject.EntityID,
				Predicate: matchedPred,
				Object:    cp.object.EntityID,
			},
			ConfidenceBP: 7800,
			Evidence:     cp.span,
			Extractor:    "deterministic",
		})
	}
	return out
}
