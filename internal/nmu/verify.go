package nmu

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/reasoning"
)

// TripleCountOptions configures anti-stuffing rules for canonical verified triple counting.
type TripleCountOptions struct {
	// AllowedPredicates, when non-empty, restricts counted triples strictly to predicates
	// declared in the pinned OntologyManifest so sellers cannot pad UnitCount with noise predicates.
	AllowedPredicates map[string]struct{}

	// MaxPerSubjectPredicate caps how many distinct triples sharing one (subject, predicate) pair
	// are counted toward UnitCount. Zero or negative defaults to DefaultMaxTriplesPerSubjectPredicate (8).
	MaxPerSubjectPredicate int
}

// TripleCountResult reports canonical verified triple count and anti-stuffing drop telemetry.
type TripleCountResult struct {
	UnitCount            uint32   `json:"unitCount"`
	UniqueHashes         []string `json:"uniqueHashes,omitempty"`
	TotalInput           int      `json:"totalInput"`
	InvalidDropped       int      `json:"invalidDropped,omitempty"`
	DuplicateDropped     int      `json:"duplicateDropped,omitempty"`
	UnknownPredDropped   int      `json:"unknownPredDropped,omitempty"`
	PerPairCappedDropped int      `json:"perPairCappedDropped,omitempty"`
}

// CountVerifiedTriples counts unique, valid, ontology-scoped triples using canonical RFC 8949 CBOR
// TagTripleV1 hashes while preventing both exact duplicate stuffing and semantic predicate/pair padding.
func CountVerifiedTriples(triples []reasoning.Triple, opts TripleCountOptions) TripleCountResult {
	res := TripleCountResult{TotalInput: len(triples)}
	if len(triples) == 0 {
		return res
	}
	maxPerPair := opts.MaxPerSubjectPredicate
	if maxPerPair <= 0 {
		maxPerPair = DefaultMaxTriplesPerSubjectPredicate
	}
	seenHash := make(map[string]struct{}, len(triples))
	pairCount := make(map[string]int, len(triples))
	hasAllowlist := len(opts.AllowedPredicates) > 0

	for _, t := range triples {
		fin, err := reasoning.FinalizeTriple(t)
		if err != nil {
			res.InvalidDropped++
			continue
		}
		if _, dup := seenHash[fin.Hash]; dup {
			res.DuplicateDropped++
			continue
		}
		seenHash[fin.Hash] = struct{}{}

		if hasAllowlist {
			if _, ok := opts.AllowedPredicates[fin.Predicate]; !ok {
				res.UnknownPredDropped++
				continue
			}
		}

		pairKey := fin.Subject + "\x00" + fin.Predicate
		if pairCount[pairKey] >= maxPerPair {
			res.PerPairCappedDropped++
			continue
		}
		pairCount[pairKey]++

		if uint32(len(res.UniqueHashes)) >= MaxUnitCount {
			continue
		}
		res.UniqueHashes = append(res.UniqueHashes, fin.Hash)
	}
	res.UnitCount = uint32(len(res.UniqueHashes))
	return res
}

// FragmentGradeInput carries deterministic local evidence for computing the maximum
// NMU-1 Grade a seller may legally advertise for a fragment.
type FragmentGradeInput struct {
	HasCleanText           bool
	Distilled              bool
	OntologyID             string
	Triples                []reasoning.Triple
	AllowedPredicates      map[string]struct{}
	MaxPerSubjectPredicate int
	MangleConsistent       bool
	EdgesRoot              string
	MPEHasOntologyAndEdges bool
	ActiveBond             bool
	LiveFeedURL            string
}

// GradeClassification is the deterministic output of ClassifyFragmentGrade.
type GradeClassification struct {
	Grade       Grade             `json:"grade"`
	UnitCount   uint32            `json:"unitCount"`
	TripleCount TripleCountResult `json:"tripleCount"`
}

// ClassifyFragmentGrade deterministically computes the highest provable NMU-1 Grade (C/B/A/S)
// and canonical UnitCount for a fragment:
//   - C: PII-clean non-empty text (UnitCount = 1 when < 1 verified triple).
//   - B: C + distilled/JevGraph + valid 32-byte hex OntologyID + Mangle consistent + >= 1 verified unique triple.
//   - A: B + valid 32-byte hex EdgesRoot committed in L1 MPE + active un-seized NektarCheckpointBond.
//   - S: A + live SSE supersession FeedURL supporting bi-temporal core.RefinementSupersedes updates.
func ClassifyFragmentGrade(in FragmentGradeInput) GradeClassification {
	if !in.HasCleanText {
		return GradeClassification{}
	}
	tc := CountVerifiedTriples(in.Triples, TripleCountOptions{
		AllowedPredicates:      in.AllowedPredicates,
		MaxPerSubjectPredicate: in.MaxPerSubjectPredicate,
	})
	out := GradeClassification{
		Grade:       GradeC,
		UnitCount:   1,
		TripleCount: tc,
	}
	if in.Distilled && isHash32Hex(in.OntologyID) && in.MangleConsistent && tc.UnitCount >= 1 {
		out.Grade = GradeB
		out.UnitCount = tc.UnitCount
		if isHash32Hex(in.EdgesRoot) && in.MPEHasOntologyAndEdges && in.ActiveBond {
			out.Grade = GradeA
			if strings.TrimSpace(in.LiveFeedURL) != "" {
				out.Grade = GradeS
			}
		}
	}
	return out
}

// DeliveryObservation captures the buyer's locally verified cryptographic & semantic facts
// immediately after decrypting a purchased fragment.
type DeliveryObservation struct {
	HasCleanText           bool
	DistilledOrStructured  bool
	OntologyID             string
	OntologyIDMismatch     bool
	Triples                []reasoning.Triple
	AllowedPredicates      map[string]struct{}
	MaxPerSubjectPredicate int
	MangleConsistent       bool
	EdgesRoot              string
	SPVVerified            bool
	MPEHasOntologyAndEdges bool
	ActiveBondVerified     bool
	NowUnix                int64
}

// GradeVerification is the buyer's deterministic post-reveal NMU-1 audit verdict.
type GradeVerification struct {
	OK              bool              `json:"ok"`
	ClaimedGrade    Grade             `json:"claimedGrade,omitempty"`
	AchievedGrade   Grade             `json:"achievedGrade"`
	ClaimedUnits    uint32            `json:"claimedUnits,omitempty"`
	ActualUnits     uint32            `json:"actualUnits"`
	GradeMismatch   bool              `json:"gradeMismatch"`
	QuarantineEdges bool              `json:"quarantineEdges"`
	Violations      []string          `json:"violations,omitempty"`
	TripleCount     TripleCountResult `json:"tripleCount"`
	// Recommended deterministic 6D reputation caps when GradeMismatch is true.
	SlashAccCapBP  int64 `json:"slashAccCapBp,omitempty"`
	SlashDispCapBP int64 `json:"slashDispCapBp,omitempty"`
	SlashProvCapBP int64 `json:"slashProvCapBp,omitempty"`
}

// VerifyDeliveredGrade runs after buyer decryption/reveal and verifies whether the delivered
// fragment cryptographically and semantically meets the seller's advertised NMU-1 Spec.
// On any grade/unit/expiry violation, GradeMismatch and QuarantineEdges are both set true by default
// so dishonest seller triples never pollute the buyer's local knowledge_edges / Mangle reasoning graph.
func VerifyDeliveredGrade(claimed Spec, obs DeliveryObservation) GradeVerification {
	claimedGrade := NormalizeGrade(string(claimed.Grade))
	cls := ClassifyFragmentGrade(FragmentGradeInput{
		HasCleanText:           obs.HasCleanText,
		Distilled:              obs.DistilledOrStructured,
		OntologyID:             obs.OntologyID,
		Triples:                obs.Triples,
		AllowedPredicates:      obs.AllowedPredicates,
		MaxPerSubjectPredicate: obs.MaxPerSubjectPredicate,
		MangleConsistent:       obs.MangleConsistent,
		EdgesRoot:              obs.EdgesRoot,
		MPEHasOntologyAndEdges: obs.MPEHasOntologyAndEdges && obs.SPVVerified,
		ActiveBond:             obs.ActiveBondVerified,
		LiveFeedURL:            claimed.FeedURL,
	})

	out := GradeVerification{
		OK:            true,
		ClaimedGrade:  claimedGrade,
		AchievedGrade: cls.Grade,
		ClaimedUnits:  claimed.UnitCount,
		ActualUnits:   cls.UnitCount,
		TripleCount:   cls.TripleCount,
	}

	// Legacy ungraded offer: no claimed grade/unit assertions to violate (still check explicit expiry).
	if claimedGrade == "" && claimed.UnitCount == 0 {
		if IsExpired(claimed.FreshnessClass, claimed.ValidUntilUnix, obs.NowUnix) {
			out.Violations = append(out.Violations, "expired_ephemeral_unit")
		}
	} else {
		if claimedGrade != "" && GradeRank(cls.Grade) < GradeRank(claimedGrade) {
			out.Violations = append(out.Violations,
				fmt.Sprintf("grade_downgrade: claimed %s achieved %s", claimedGrade, cls.Grade))
		}
		if GradeRank(claimedGrade) >= GradeRank(GradeB) && claimed.UnitCount > 0 && cls.UnitCount < claimed.UnitCount {
			out.Violations = append(out.Violations,
				fmt.Sprintf("unit_count_shortfall: claimed %d verified %d", claimed.UnitCount, cls.UnitCount))
		}
		if GradeRank(claimedGrade) >= GradeRank(GradeB) && obs.OntologyIDMismatch {
			out.Violations = append(out.Violations, "ontology_id_mismatch")
		}
		if GradeRank(claimedGrade) >= GradeRank(GradeA) && !obs.SPVVerified {
			out.Violations = append(out.Violations, "missing_spv_proof_for_grade_a_or_s")
		}
		if claimedGrade == GradeS && strings.TrimSpace(claimed.FeedURL) == "" {
			out.Violations = append(out.Violations, "missing_feed_url_for_grade_s")
		}
		if IsExpired(claimed.FreshnessClass, claimed.ValidUntilUnix, obs.NowUnix) {
			out.Violations = append(out.Violations, "expired_ephemeral_unit")
		}
	}

	if len(out.Violations) > 0 {
		out.OK = false
		out.GradeMismatch = true
		out.QuarantineEdges = true
		out.SlashAccCapBP = 2000
		out.SlashDispCapBP = 2500
		out.SlashProvCapBP = 2500
	}
	return out
}

func isHash32Hex(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) != 64 {
		return false
	}
	b, err := hex.DecodeString(trimmed)
	return err == nil && len(b) == 32
}
