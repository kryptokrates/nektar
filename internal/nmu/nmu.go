package nmu

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

const (
	// SpecVersion identifies the canonical Nektar Memory Unit Standard v1.
	SpecVersion = "nmu-1"

	// MaxUnitCount bounds verified triples counted in one FactPack / fragment to prevent
	// unbounded wire amplification or pathological pricing division.
	MaxUnitCount uint32 = 4096

	// MaxSubjectVersionLen bounds optional version pins (e.g. "go-1.27.4", "bsv-sdk-v1.7.8").
	MaxSubjectVersionLen = 64

	// MaxFeedURLLen bounds optional live SSE supersession / query endpoint URLs.
	MaxFeedURLLen = 256

	// DefaultMaxTriplesPerSubjectPredicate caps counted triples sharing the same (subject, predicate)
	// pair so sellers cannot pad one entity with dozens of low-value attributes to inflate UnitCount.
	DefaultMaxTriplesPerSubjectPredicate = 8

	// AdvisoryNMUNote enforces Nektar's Honesty Rule across all NMU-1 graded commodities.
	AdvisoryNMUNote = "NMU-1 grade proves recorded, unaltered, schema-conformant, evidence-cited, bonded, and live-maintained status — never real-world factual truth."
)

// Grade classifies a tradable memory unit's machine-verifiable commodity tier (C < B < A < S).
type Grade string

const (
	// GradeC is raw PII/secret-scrubbed text (no ontology triples required).
	GradeC Grade = "C"
	// GradeB is distilled and ontology-pinned with >= 1 verified unique triple and Mangle consistency.
	GradeB Grade = "B"
	// GradeA is Grade B + verbatim source spans bound in L1 MPE (EdgesRoot) + active NektarCheckpointBond.
	GradeA Grade = "A"
	// GradeS is Grade A + live SSE supersession feed (bi-temporal core.RefinementSupersedes updates).
	GradeS Grade = "S"
)

// NormalizeGrade canonicalizes a grade string to uppercase trimmed Grade.
func NormalizeGrade(raw string) Grade {
	return Grade(strings.ToUpper(strings.TrimSpace(raw)))
}

// ValidGrade reports whether g is one of the four canonical NMU-1 grades (C, B, A, S).
func ValidGrade(g Grade) bool {
	switch g {
	case GradeC, GradeB, GradeA, GradeS:
		return true
	default:
		return false
	}
}

// GradeRank maps a Grade to its deterministic total order (C=1, B=2, A=3, S=4; empty/invalid=0).
func GradeRank(g Grade) int {
	switch NormalizeGrade(string(g)) {
	case GradeC:
		return 1
	case GradeB:
		return 2
	case GradeA:
		return 3
	case GradeS:
		return 4
	default:
		return 0
	}
}

// MeetsMinGrade reports whether actual satisfies the required minimum grade min.
// An empty min accepts any grade (including legacy ungraded offers).
func MeetsMinGrade(actual, min Grade) bool {
	minNorm := NormalizeGrade(string(min))
	if minNorm == "" {
		return true
	}
	if !ValidGrade(minNorm) {
		return false
	}
	return GradeRank(actual) >= GradeRank(minNorm)
}

// FreshnessClass describes a tradable memory unit's temporal decay regime.
type FreshnessClass string

const (
	// FreshnessEphemeral decays rapidly (e.g. live outage / incident workaround) and carries a TTL.
	FreshnessEphemeral FreshnessClass = "ephemeral"
	// FreshnessVersioned is pinned to a software/protocol version and valid until superseded.
	FreshnessVersioned FreshnessClass = "versioned"
	// FreshnessEvergreen captures durable invariants (math, RFCs, protocol rules).
	FreshnessEvergreen FreshnessClass = "evergreen"
)

// NormalizeFreshness canonicalizes a freshness class string to lowercase trimmed FreshnessClass.
func NormalizeFreshness(raw string) FreshnessClass {
	return FreshnessClass(strings.ToLower(strings.TrimSpace(raw)))
}

// ValidFreshness reports whether f is one of ephemeral, versioned, or evergreen.
func ValidFreshness(f FreshnessClass) bool {
	switch f {
	case FreshnessEphemeral, FreshnessVersioned, FreshnessEvergreen:
		return true
	default:
		return false
	}
}

// IsExpired reports whether a unit with the given freshness class and validUntilUnix (unix seconds)
// has expired as of nowUnix. Any non-zero validUntilUnix <= nowUnix is treated as expired (fail closed).
func IsExpired(fc FreshnessClass, validUntilUnix, nowUnix int64) bool {
	if validUntilUnix <= 0 {
		return false
	}
	if nowUnix <= 0 {
		return false
	}
	_ = fc
	return nowUnix >= validUntilUnix
}

// UsageRights specifies whether purchased memory may only be used locally or distilled & resold.
type UsageRights string

const (
	// RightsInferenceOnly permits local reasoning/recall only; strictly forbids resale or distillation into market shards.
	RightsInferenceOnly UsageRights = "inference_only"
	// RightsDistillResellable permits merging, distilling (with provenance edge), and reselling in derived shards.
	RightsDistillResellable UsageRights = "distill_resellable"
)

// NormalizeRights canonicalizes usage rights to lowercase trimmed UsageRights.
func NormalizeRights(raw string) UsageRights {
	return UsageRights(strings.ToLower(strings.TrimSpace(raw)))
}

// ValidRights reports whether r is inference_only or distill_resellable.
func ValidRights(r UsageRights) bool {
	switch r {
	case RightsInferenceOnly, RightsDistillResellable:
		return true
	default:
		return false
	}
}

// EffectiveRights resolves omitted or unknown usage rights fail-closed to RightsInferenceOnly.
func EffectiveRights(r UsageRights) UsageRights {
	norm := NormalizeRights(string(r))
	if norm == RightsDistillResellable {
		return RightsDistillResellable
	}
	return RightsInferenceOnly
}

// MeetsRequiredRights reports whether actual usage rights satisfy buyer's required rights.
// Empty or inference_only requirements accept any valid/legacy offer; distill_resellable requires an exact grant.
func MeetsRequiredRights(actual, required UsageRights) bool {
	reqNorm := NormalizeRights(string(required))
	if reqNorm == "" || reqNorm == RightsInferenceOnly {
		return true
	}
	if reqNorm != RightsDistillResellable {
		return false
	}
	return EffectiveRights(actual) == RightsDistillResellable
}

// PricingMode identifies the standardized NMU-1 market contract mode.
type PricingMode string

const (
	// ModeSpot is a one-time atomic DisclosureSale / BRC-29+BRC-369 purchase of a FactPack.
	ModeSpot PricingMode = "spot"
	// ModePerQuery is metered pay-per-lookup recall over BRC-105 HTTP 402.
	ModePerQuery PricingMode = "per_query"
	// ModeSubscription is a time-windowed Shard Feed with live Grade S supersession SSE push.
	ModeSubscription PricingMode = "subscription"
)

// NormalizePricingMode canonicalizes a pricing mode string to lowercase trimmed PricingMode.
func NormalizePricingMode(raw string) PricingMode {
	return PricingMode(strings.ToLower(strings.TrimSpace(raw)))
}

// ValidPricingMode reports whether m is spot, per_query, or subscription.
func ValidPricingMode(m PricingMode) bool {
	switch m {
	case ModeSpot, ModePerQuery, ModeSubscription:
		return true
	default:
		return false
	}
}

// EffectivePricingMode defaults empty pricing mode to ModeSpot for backward compatibility.
func EffectivePricingMode(m PricingMode) PricingMode {
	norm := NormalizePricingMode(string(m))
	if ValidPricingMode(norm) {
		return norm
	}
	return ModeSpot
}

// Spec carries the canonical NMU-1 commodity metadata for a fragment, offer, or deal receipt.
type Spec struct {
	Version         string         `json:"version,omitempty"`
	Grade           Grade          `json:"grade,omitempty"`
	UnitCount       uint32         `json:"unitCount,omitempty"`
	SatsPer1kUnits  uint64         `json:"satsPer1kUnits,omitempty"`
	FreshnessClass  FreshnessClass `json:"freshnessClass,omitempty"`
	ValidUntilUnix  int64          `json:"validUntilUnix,omitempty"`
	SubjectVersion  string         `json:"subjectVersion,omitempty"`
	UsageRights     UsageRights    `json:"usageRights,omitempty"`
	PricingMode     PricingMode    `json:"pricingMode,omitempty"`
	SubscriptionSec int64          `json:"subscriptionSec,omitempty"`
	FeedURL         string         `json:"feedUrl,omitempty"`
	AdvisoryNote    string         `json:"advisoryNote,omitempty"`
}

// IsEmpty reports whether no NMU-1 fields are populated (legacy ungraded item).
func (s Spec) IsEmpty() bool {
	return s.Grade == "" &&
		s.UnitCount == 0 &&
		s.SatsPer1kUnits == 0 &&
		s.FreshnessClass == "" &&
		s.ValidUntilUnix == 0 &&
		s.SubjectVersion == "" &&
		s.UsageRights == "" &&
		s.PricingMode == "" &&
		s.SubscriptionSec == 0 &&
		s.FeedURL == ""
}

// ComputeSatsPer1kUnits computes exact integer milli-sats per canonical knowledge unit:
// floor((priceSats * 1000) / max(1, unitCount)) using 128-bit overflow-safe arithmetic.
// Non-zero prices with large unit counts floor to at least 1 milli-sat so paid units never appear free.
func ComputeSatsPer1kUnits(priceSats uint64, unitCount uint32) uint64 {
	if priceSats == 0 {
		return 0
	}
	denom := uint64(unitCount)
	if denom == 0 {
		denom = 1
	}
	hi, lo := bits.Mul64(priceSats, 1000)
	if hi >= denom {
		return math.MaxUint64
	}
	q, _ := bits.Div64(hi, lo, denom)
	if q == 0 {
		return 1
	}
	return q
}

// ComputeTotalSatsForUnits computes the total satoshi ask from a per-1k-unit rate (satsPer1kUnits)
// and unitCount, rounding up to at least 1 sat when rate > 0.
func ComputeTotalSatsForUnits(satsPer1kUnits uint64, unitCount uint32) uint64 {
	if satsPer1kUnits == 0 {
		return 0
	}
	units := uint64(unitCount)
	if units == 0 {
		units = 1
	}
	hi, lo := bits.Mul64(satsPer1kUnits, units)
	// Add 999 for ceiling division by 1000.
	lo, carry := bits.Add64(lo, 999, 0)
	hi += carry
	if hi >= 1000 {
		return math.MaxUint64
	}
	q, _ := bits.Div64(hi, lo, 1000)
	if q == 0 {
		return 1
	}
	return q
}

// ValidateOfferSpec strictly validates optional NMU-1 fields carried on an OfferAd or DealReceipt.
// Legacy offers with all NMU-1 fields empty pass cleanly.
func ValidateOfferSpec(s Spec, priceSats uint64) error {
	if s.Version != "" && s.Version != SpecVersion {
		return fmt.Errorf("nmu: unsupported version %q", s.Version)
	}
	if s.Grade != "" {
		g := NormalizeGrade(string(s.Grade))
		if !ValidGrade(g) {
			return fmt.Errorf("nmu: invalid grade %q", s.Grade)
		}
		if g == GradeS && strings.TrimSpace(s.FeedURL) == "" {
			return errors.New("nmu: grade S requires non-empty feedUrl")
		}
	}
	if s.UnitCount > MaxUnitCount {
		return fmt.Errorf("nmu: unitCount %d exceeds max %d", s.UnitCount, MaxUnitCount)
	}
	if s.FreshnessClass != "" && !ValidFreshness(NormalizeFreshness(string(s.FreshnessClass))) {
		return fmt.Errorf("nmu: invalid freshnessClass %q", s.FreshnessClass)
	}
	if s.ValidUntilUnix < 0 {
		return errors.New("nmu: validUntilUnix cannot be negative")
	}
	if len(s.SubjectVersion) > MaxSubjectVersionLen {
		return fmt.Errorf("nmu: subjectVersion exceeds %d bytes", MaxSubjectVersionLen)
	}
	if s.UsageRights != "" && !ValidRights(NormalizeRights(string(s.UsageRights))) {
		return fmt.Errorf("nmu: invalid usageRights %q", s.UsageRights)
	}
	if s.PricingMode != "" && !ValidPricingMode(NormalizePricingMode(string(s.PricingMode))) {
		return fmt.Errorf("nmu: invalid pricingMode %q", s.PricingMode)
	}
	if s.SubscriptionSec < 0 {
		return errors.New("nmu: subscriptionSec cannot be negative")
	}
	if len(s.FeedURL) > MaxFeedURLLen {
		return fmt.Errorf("nmu: feedUrl exceeds %d bytes", MaxFeedURLLen)
	}
	// Fail closed if seller publishes a spoofed SatsPer1kUnits inconsistent with PriceSats / UnitCount.
	if s.SatsPer1kUnits > 0 && priceSats > 0 {
		want := ComputeSatsPer1kUnits(priceSats, s.UnitCount)
		if s.SatsPer1kUnits != want {
			return fmt.Errorf("nmu: satsPer1kUnits %d does not match priceSats=%d unitCount=%d (want %d)",
				s.SatsPer1kUnits, priceSats, s.UnitCount, want)
		}
	}
	return nil
}

// ValidateWantFilters strictly validates optional NMU-1 buyer filters on a WantAd or StandingOrder.
func ValidateWantFilters(minGrade, freshness, requiredRights, pricingMode string) error {
	if strings.TrimSpace(minGrade) != "" && !ValidGrade(NormalizeGrade(minGrade)) {
		return fmt.Errorf("nmu: invalid minGrade %q", minGrade)
	}
	if strings.TrimSpace(freshness) != "" && !ValidFreshness(NormalizeFreshness(freshness)) {
		return fmt.Errorf("nmu: invalid freshnessClass %q", freshness)
	}
	if strings.TrimSpace(requiredRights) != "" && !ValidRights(NormalizeRights(requiredRights)) {
		return fmt.Errorf("nmu: invalid requiredRights %q", requiredRights)
	}
	if strings.TrimSpace(pricingMode) != "" && !ValidPricingMode(NormalizePricingMode(pricingMode)) {
		return fmt.Errorf("nmu: invalid pricingMode %q", pricingMode)
	}
	return nil
}

// FormatSatsPerTriple renders integer satsPer1kUnits as a human-readable decimal string
// (e.g. 500 -> "0.500", 12500 -> "12.500") without using floating point arithmetic.
func FormatSatsPerTriple(satsPer1kUnits uint64) string {
	whole := satsPer1kUnits / 1000
	frac := satsPer1kUnits % 1000
	return fmt.Sprintf("%d.%03d", whole, frac)
}
