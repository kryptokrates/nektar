package jevgraph

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultMaxSentenceWindow allows same-sentence (0) and adjacent-sentence (1) pairing.
	DefaultMaxSentenceWindow = 1
	// DefaultMaxCharDistance bounds character distance between blocked mentions.
	DefaultMaxCharDistance = 400
	// DefaultMaxCandidatePairs caps blocked pairs per fragment to enforce O(nk) bounds.
	DefaultMaxCandidatePairs = 64
	// HardMaxCandidatePairs is the absolute upper bound regardless of caller override.
	HardMaxCandidatePairs = 256
)

// BlockingConfig tunes Stage 1 deterministic local candidate blocking.
type BlockingConfig struct {
	MaxSentenceWindow int
	MaxCharDistance   int
	MaxCandidatePairs int
}

func (c BlockingConfig) normalized() BlockingConfig {
	out := c
	if out.MaxSentenceWindow < 0 {
		out.MaxSentenceWindow = 0
	}
	if out.MaxSentenceWindow == 0 && c.MaxSentenceWindow == 0 {
		// Distinguish unset (0) only when caller left whole struct zero; callers wanting
		// strictly same-sentence can pass MaxSentenceWindow = 0 with MaxCharDistance > 0.
		if c.MaxCharDistance == 0 && c.MaxCandidatePairs == 0 {
			out.MaxSentenceWindow = DefaultMaxSentenceWindow
		}
	}
	if out.MaxCharDistance <= 0 {
		out.MaxCharDistance = DefaultMaxCharDistance
	}
	if out.MaxCandidatePairs <= 0 {
		out.MaxCandidatePairs = DefaultMaxCandidatePairs
	}
	if out.MaxCandidatePairs > HardMaxCandidatePairs {
		out.MaxCandidatePairs = HardMaxCandidatePairs
	}
	return out
}

// SentenceSpan records one sentence's exact UTF-8 byte range [Start, End) in the original text.
type SentenceSpan struct {
	Index int
	Start int
	End   int
	Text  string
}

// Mention records one deterministically matched entity occurrence in canonical text.
type Mention struct {
	EntityID    string `json:"entityId"`
	Type        string `json:"type"`
	Surface     string `json:"surface"`
	StartOffset int    `json:"startOffset"`
	EndOffset   int    `json:"endOffset"`
	SentenceIdx int    `json:"sentenceIdx"`
}

// EvidenceSpan pins an extracted edge to an exact verbatim byte slice text[StartOffset:EndOffset].
type EvidenceSpan struct {
	Quote       string `json:"quote"`
	StartOffset int    `json:"startOffset"`
	EndOffset   int    `json:"endOffset"`
}

// CandidatePair is one O(nk)-blocked directed (Subject, Object) pair eligible for Jev Choice+Noul.
type CandidatePair struct {
	Subject          Mention
	Object           Mention
	AllowedRelations []RelationDef
	Evidence         EvidenceSpan
}

// SegmentSentences splits text into deterministic sentences while preserving exact UTF-8 byte offsets
// so text[s.Start:s.End] == s.Text always holds verbatim.
func SegmentSentences(text string) []SentenceSpan {
	if len(text) == 0 {
		return nil
	}
	var out []SentenceSpan
	n := len(text)
	segStart := 0

	flush := func(end int) {
		// Trim leading and trailing whitespace while preserving exact byte indices in text.
		s, e := segStart, end
		for s < e {
			r, sz := utf8.DecodeRuneInString(text[s:e])
			if !unicode.IsSpace(r) {
				break
			}
			s += sz
		}
		for e > s {
			r, sz := utf8.DecodeLastRuneInString(text[s:e])
			if !unicode.IsSpace(r) {
				break
			}
			e -= sz
		}
		if s < e {
			out = append(out, SentenceSpan{
				Index: len(out),
				Start: s,
				End:   e,
				Text:  text[s:e],
			})
		}
	}

	for i := 0; i < n; {
		r, sz := utf8.DecodeRuneInString(text[i:])
		nextIdx := i + sz

		// Paragraph/line break split on newline.
		if r == '\n' {
			flush(i)
			segStart = nextIdx
			i = nextIdx
			continue
		}
		// Sentence-ending punctuation followed by whitespace or EOF (avoids splitting versions like v1.2 or URLs).
		if r == '.' || r == '!' || r == '?' {
			if nextIdx >= n {
				flush(nextIdx)
				segStart = nextIdx
				i = nextIdx
				continue
			}
			nr, _ := utf8.DecodeRuneInString(text[nextIdx:])
			if unicode.IsSpace(nr) {
				flush(nextIdx)
				segStart = nextIdx
				i = nextIdx
				continue
			}
		}
		i = nextIdx
	}
	if segStart < n {
		flush(n)
	}
	return out
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func hasWordBoundaries(text string, start, end int) bool {
	if start > 0 {
		prev, _ := utf8.DecodeLastRuneInString(text[:start])
		first, _ := utf8.DecodeRuneInString(text[start:end])
		if isWordRune(prev) && isWordRune(first) {
			return false
		}
	}
	if end < len(text) {
		last, _ := utf8.DecodeLastRuneInString(text[start:end])
		next, _ := utf8.DecodeRuneInString(text[end:])
		if isWordRune(last) && isWordRune(next) {
			return false
		}
	}
	return true
}

func sentenceIndexForOffset(sentences []SentenceSpan, offset int) int {
	// Binary search over non-overlapping ordered sentence spans.
	lo, hi := 0, len(sentences)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		s := sentences[mid]
		if offset < s.Start {
			hi = mid - 1
		} else if offset >= s.End {
			lo = mid + 1
		} else {
			return s.Index
		}
	}
	if len(sentences) == 0 {
		return 0
	}
	if lo >= len(sentences) {
		return sentences[len(sentences)-1].Index
	}
	return sentences[lo].Index
}

type aliasRule struct {
	entityID string
	typ      string
	lower    string
	byteLen  int
}

// ExtractMentions scans text deterministically using the compiled ontology's entity gazetteer
// plus optional caller-provided extraHints. Uses longest-match-first non-overlapping resolution
// with Unicode word-boundary enforcement and exact UTF-8 byte offsets.
func ExtractMentions(text string, sentences []SentenceSpan, comp *CompiledOntology, extraHints []EntityEntry) []Mention {
	if len(text) == 0 || comp == nil {
		return nil
	}
	if len(sentences) == 0 {
		sentences = SegmentSentences(text)
	}

	// Merge ontology catalog + extraHints (validating extraHints against compiled TBox classes).
	allEntities := make([]EntityEntry, 0, len(comp.Ontology.Entities)+len(extraHints))
	allEntities = append(allEntities, comp.Ontology.Entities...)
	for _, h := range extraHints {
		h.ID = strings.TrimSpace(h.ID)
		h.Type = strings.TrimSpace(h.Type)
		if h.ID == "" || h.Type == "" {
			continue
		}
		if _, ok := comp.ClassesByID[h.Type]; !ok {
			continue
		}
		allEntities = append(allEntities, h)
	}
	if len(allEntities) == 0 {
		return nil
	}

	// Build case-folded alias table sorted longest-first for greedy non-overlapping matching.
	var aliases []aliasRule
	for _, ent := range allEntities {
		seenAlias := make(map[string]bool)
		rawAliases := ent.Aliases
		// When an entity uses a plain canonical label (no URI/CURIE colon) or has no explicit aliases/pattern,
		// also include its canonical ID as a matchable surface form.
		if (len(rawAliases) == 0 && ent.Pattern == "") || !strings.Contains(ent.ID, ":") {
			rawAliases = append([]string{ent.ID}, rawAliases...)
		}
		for _, rawAlias := range rawAliases {
			a := strings.TrimSpace(rawAlias)
			if len(a) < 2 {
				continue
			}
			low := strings.ToLower(a)
			if seenAlias[low] {
				continue
			}
			seenAlias[low] = true
			aliases = append(aliases, aliasRule{
				entityID: ent.ID,
				typ:      ent.Type,
				lower:    low,
				byteLen:  len(low),
			})
		}
	}
	sort.Slice(aliases, func(i, j int) bool {
		if aliases[i].byteLen != aliases[j].byteLen {
			return aliases[i].byteLen > aliases[j].byteLen
		}
		if aliases[i].entityID != aliases[j].entityID {
			return aliases[i].entityID < aliases[j].entityID
		}
		return aliases[i].lower < aliases[j].lower
	})

	// Build lowercase mapping preserving original byte offsets for ASCII-dominant & UTF-8 text.
	// Using rune-aligned offset table guarantees text[origStart:origEnd] stays exact even when
	// Unicode case folding changes UTF-8 byte lengths.
	var lowerBuf strings.Builder
	lowerBuf.Grow(len(text))
	lowerToOrig := make([]int, 0, len(text)+1)
	for origIdx, r := range text {
		lr := unicode.ToLower(r)
		startLower := lowerBuf.Len()
		lowerBuf.WriteRune(lr)
		endLower := lowerBuf.Len()
		for k := startLower; k < endLower; k++ {
			lowerToOrig = append(lowerToOrig, origIdx)
		}
	}
	lowerToOrig = append(lowerToOrig, len(text))
	lowerText := lowerBuf.String()

	type rawHit struct {
		entityID string
		typ      string
		start    int
		end      int
	}
	var hits []rawHit

	// 1. Gazetteer alias matches.
	for _, ar := range aliases {
		pos := 0
		for pos < len(lowerText) {
			idx := strings.Index(lowerText[pos:], ar.lower)
			if idx < 0 {
				break
			}
			lStart := pos + idx
			lEnd := lStart + ar.byteLen
			origStart := lowerToOrig[lStart]
			origEnd := lowerToOrig[lEnd]
			if origStart < origEnd && origEnd <= len(text) && hasWordBoundaries(text, origStart, origEnd) {
				hits = append(hits, rawHit{
					entityID: ar.entityID,
					typ:      ar.typ,
					start:    origStart,
					end:      origEnd,
				})
			}
			pos = lStart + 1
		}
	}

	// 2. Bounded RE2 pattern matches (for IDs like CVEs, TXIDs, handles, versions).
	for _, ent := range allEntities {
		if ent.Pattern == "" {
			continue
		}
		re := comp.compiledPatterns[ent.ID]
		if re == nil {
			compiled, err := regexp.Compile(ent.Pattern)
			if err != nil {
				continue
			}
			re = compiled
		}
		locs := re.FindAllStringIndex(text, 32)
		for _, loc := range locs {
			if loc[0] < loc[1] && hasWordBoundaries(text, loc[0], loc[1]) {
				surface := text[loc[0]:loc[1]]
				// Pattern entities use "<entityID>:<surface>" when ID ends with ":" (template prefix),
				// or exact entityID otherwise.
				eid := ent.ID
				if strings.HasSuffix(eid, ":") {
					eid = eid + surface
				}
				hits = append(hits, rawHit{
					entityID: eid,
					typ:      ent.Type,
					start:    loc[0],
					end:      loc[1],
				})
			}
		}
	}

	if len(hits) == 0 {
		return nil
	}

	// Resolve overlapping spans deterministically: longest span first, earliest start, lexical entityID.
	sort.Slice(hits, func(i, j int) bool {
		li := hits[i].end - hits[i].start
		lj := hits[j].end - hits[j].start
		if li != lj {
			return li > lj
		}
		if hits[i].start != hits[j].start {
			return hits[i].start < hits[j].start
		}
		return hits[i].entityID < hits[j].entityID
	})

	occupied := make([]bool, len(text))
	var mentions []Mention
	for _, h := range hits {
		overlap := false
		for p := h.start; p < h.end; p++ {
			if occupied[p] {
				overlap = true
				break
			}
		}
		if overlap {
			continue
		}
		for p := h.start; p < h.end; p++ {
			occupied[p] = true
		}
		mentions = append(mentions, Mention{
			EntityID:    h.entityID,
			Type:        h.typ,
			Surface:     text[h.start:h.end],
			StartOffset: h.start,
			EndOffset:   h.end,
			SentenceIdx: sentenceIndexForOffset(sentences, h.start),
		})
	}

	sort.Slice(mentions, func(i, j int) bool {
		if mentions[i].StartOffset != mentions[j].StartOffset {
			return mentions[i].StartOffset < mentions[j].StartOffset
		}
		return mentions[i].EndOffset < mentions[j].EndOffset
	})
	return mentions
}

// BlockCandidates constructs bounded O(nk) directed (Subject, Object) candidate pairs
// whose mentions appear within MaxSentenceWindow & MaxCharDistance and satisfy at least
// one committed TBox relation's (Domain, Range) under subClassOf closure.
func BlockCandidates(text string, sentences []SentenceSpan, mentions []Mention, comp *CompiledOntology, cfg BlockingConfig) []CandidatePair {
	if len(mentions) < 2 || comp == nil {
		return nil
	}
	if len(sentences) == 0 {
		sentences = SegmentSentences(text)
	}
	bc := cfg.normalized()

	type pairKey struct {
		subID   string
		objID   string
		sentMin int
		sentMax int
	}
	seen := make(map[pairKey]bool)
	var out []CandidatePair

	buildEvidence := func(a, b Mention) EvidenceSpan {
		sMin, sMax := a.SentenceIdx, b.SentenceIdx
		if sMin > sMax {
			sMin, sMax = sMax, sMin
		}
		if sMin >= 0 && sMax < len(sentences) {
			start := sentences[sMin].Start
			end := sentences[sMax].End
			if start >= 0 && end <= len(text) && start < end {
				return EvidenceSpan{
					Quote:       text[start:end],
					StartOffset: start,
					EndOffset:   end,
				}
			}
		}
		start := min(a.StartOffset, b.StartOffset)
		end := max(a.EndOffset, b.EndOffset)
		return EvidenceSpan{Quote: text[start:end], StartOffset: start, EndOffset: end}
	}

	for i := 0; i < len(mentions); i++ {
		for j := i + 1; j < len(mentions); j++ {
			m1, m2 := mentions[i], mentions[j]
			if m2.SentenceIdx-m1.SentenceIdx > bc.MaxSentenceWindow {
				// Mentions are ordered by offset/sentence; break inner loop for O(nk) behavior.
				break
			}
			charGap := m2.StartOffset - m1.EndOffset
			if charGap > bc.MaxCharDistance {
				break
			}
			if m1.EntityID == m2.EntityID {
				continue
			}

			// Evaluate both directed orientations (m1 -> m2 and m2 -> m1) against TBox domain/range.
			orientations := [][2]Mention{{m1, m2}, {m2, m1}}
			for _, pair := range orientations {
				sub, obj := pair[0], pair[1]
				rels := comp.AllowedRelations(sub.Type, obj.Type)
				if len(rels) == 0 {
					continue
				}
				pk := pairKey{
					subID:   sub.EntityID,
					objID:   obj.EntityID,
					sentMin: min(sub.SentenceIdx, obj.SentenceIdx),
					sentMax: max(sub.SentenceIdx, obj.SentenceIdx),
				}
				if seen[pk] {
					continue
				}
				seen[pk] = true
				out = append(out, CandidatePair{
					Subject:          sub,
					Object:           obj,
					AllowedRelations: rels,
					Evidence:         buildEvidence(sub, obj),
				})
				if len(out) >= bc.MaxCandidatePairs {
					return out
				}
			}
		}
	}
	return out
}
