package market

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/kryptokrates/nektar/internal/nmu"
)

const (
	// DefaultSatPer1kTokens is the default conversion rate (sats per 1,000 LLM reasoning tokens).
	// Example: ~$0.015 / 1k reasoning tokens at ~$50/BSV ≈ 30 sats / 1k tokens.
	DefaultSatPer1kTokens uint64 = 30

	// DefaultArbitrageAlphaBP is the buyer arbitrage capture factor (3000 bp = 30% of estimated savings).
	DefaultArbitrageAlphaBP int64 = 3000

	// DefaultEstimatedReasoningTokens is the baseline reasoning tokens saved by avoiding a failed loop.
	DefaultEstimatedReasoningTokens uint64 = 8000

	// Dynamic pricing bounds and step rates (integer basis points).
	DefaultPriceRatchetUpBP   int64 = 1000 // +10% on demand hit
	DefaultPriceDecayDownBP   int64 = 1000 // -10% on idle window without hits
	DefaultMaxCollabDiscountBP int64 = 1500 // up to -15% discount for priority buyers (high r_collab)
	DefaultDecayIntervalSec   int64 = 86400
)

// TokenCounter estimates or counts LLM tokens for a text string.
// The default implementation is zero-bloat pure Go (no embedded multi-MB BPE tables);
// callers may swap in a provider-specific tokenizer behind this interface.
type TokenCounter interface {
	CountTokens(text string) int
}

// CalibratedTokenCounter is a fast, deterministic pure-Go token estimator calibrated against
// cl100k_base/o200k_base over English prose, Go/TS source code, and JSON/YAML payloads.
type CalibratedTokenCounter struct{}

// CountTokens returns a deterministic token estimate in microseconds with zero allocations.
func (CalibratedTokenCounter) CountTokens(text string) int {
	s := strings.TrimSpace(text)
	if s == "" {
		return 0
	}
	words := 0
	punct := 0
	inWord := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			if !inWord {
				words++
				inWord = true
			}
		default:
			punct++
			inWord = false
		}
	}
	// Blend byte-length heuristic (len/4) and word+punctuation count for accurate code + prose estimates.
	charEst := (len(s) + 3) / 4
	wordEst := (words*13+9)/10 + (punct+1)/2
	est := (charEst + wordEst) / 2
	if est < 1 {
		est = 1
	}
	return est
}

// DefaultTokenCounter returns the default zero-bloat pure-Go TokenCounter.
func DefaultTokenCounter() TokenCounter {
	return CalibratedTokenCounter{}
}

// ArbitrageBidInput configures deterministic integer-bps buyer bid computation.
type ArbitrageBidInput struct {
	EstTokensSaved     uint64 // estimated LLM tokens saved by purchasing verified fact (0 -> DefaultEstimatedReasoningTokens)
	SatPer1kTokens     uint64 // satoshis equivalent per 1,000 tokens (0 -> DefaultSatPer1kTokens)
	AlphaBP            int64  // buyer savings share in bps [1..10000] (0 -> DefaultArbitrageAlphaBP)
	SellerCompositeBP  int64  // seller 6D CompositeBP in [0..10000] (0 -> neutral 5000 if unrated)
	SellerEigenTrustBP int64  // seller EigenTrustBP in [0..10000]
	SellerBonded       bool   // true if seller holds active NektarCheckpointBond
	MinBidSats         uint64 // optional floor (default 1)
	MaxPolicyDealSats  uint64 // hard cap from OperatorPolicy.MaxSatsPerDeal / SpendGuard (0 = uncapped here)
}

// ArbitrageBidResult details the deterministic integer-bps auto-bid decision.
type ArbitrageBidResult struct {
	MaxBidSats     uint64 `json:"maxBidSats"`
	EstTokensSaved uint64 `json:"estTokensSaved"`
	RepWeightBP    int64  `json:"repWeightBp"`
	RawSavingsSats uint64 `json:"rawSavingsSats"`
	CappedByPolicy bool   `json:"cappedByPolicy,omitempty"`
}

// ComputeAutoBidSats computes a deterministic integer-bps maximum bid for a buyer:
//
//	MaxBidSats = min(MaxPolicyDealSats, EstTokensSaved * SatPer1kTokens / 1000 * AlphaBP / 10000 * RepWeightBP / 10000)
//
// Zero floats; guaranteed <= MaxPolicyDealSats whenever MaxPolicyDealSats > 0.
func ComputeAutoBidSats(in ArbitrageBidInput) ArbitrageBidResult {
	estTok := in.EstTokensSaved
	if estTok == 0 {
		estTok = DefaultEstimatedReasoningTokens
	}
	satPer1k := in.SatPer1kTokens
	if satPer1k == 0 {
		satPer1k = DefaultSatPer1kTokens
	}
	alphaBP := in.AlphaBP
	if alphaBP <= 0 {
		alphaBP = DefaultArbitrageAlphaBP
	}
	if alphaBP > 10000 {
		alphaBP = 10000
	}

	// Reputation multiplier in [2500..10000] bp:
	// - Rated seller with strong CompositeBP + EigenTrust scales smoothly up to 10000 bp.
	// - Unrated bonded seller gets 6500 bp cold-start multiplier.
	// - Unrated unbonded seller gets conservative 3500 bp multiplier.
	var repWeightBP int64
	if in.SellerCompositeBP > 0 || in.SellerEigenTrustBP > 0 {
		comp := in.SellerCompositeBP
		if comp > 10000 {
			comp = 10000
		}
		eigen := in.SellerEigenTrustBP
		if eigen > 10000 {
			eigen = 10000
		}
		// 70% 6D composite + 30% personalized EigenTrust, floored at 2500 bp.
		repWeightBP = (comp*70 + eigen*30) / 100
		if in.SellerBonded && repWeightBP < 6500 {
			repWeightBP = 6500
		}
		if repWeightBP < 2500 {
			repWeightBP = 2500
		}
	} else if in.SellerBonded {
		repWeightBP = 6500
	} else {
		repWeightBP = 3500
	}

	// Raw value of saved tokens in sats: ceil(estTok * satPer1k / 1000).
	rawSavings := (estTok*satPer1k + 999) / 1000
	// Apply alphaBP and repWeightBP sequentially with safe uint64 math.
	bid := (rawSavings * uint64(alphaBP)) / 10000
	bid = (bid * uint64(repWeightBP)) / 10000

	minBid := in.MinBidSats
	if minBid == 0 {
		minBid = 1
	}
	if bid < minBid {
		bid = minBid
	}

	capped := false
	if in.MaxPolicyDealSats > 0 && bid > in.MaxPolicyDealSats {
		bid = in.MaxPolicyDealSats
		capped = true
	}

	return ArbitrageBidResult{
		MaxBidSats:     bid,
		EstTokensSaved: estTok,
		RepWeightBP:    repWeightBP,
		RawSavingsSats: rawSavings,
		CappedByPolicy: capped,
	}
}

// GradeWeightBP returns the deterministic integer-bps quality multiplier for an NMU-1 Grade.
func GradeWeightBP(g nmu.Grade) int64 {
	switch nmu.NormalizeGrade(string(g)) {
	case nmu.GradeS:
		return 10000
	case nmu.GradeA:
		return 9000
	case nmu.GradeB:
		return 7500
	default:
		return 5000
	}
}

// RightsMultiplierBP returns the deterministic integer-bps pricing multiplier for NMU-1 UsageRights.
func RightsMultiplierBP(r nmu.UsageRights) int64 {
	if nmu.EffectiveRights(r) == nmu.RightsDistillResellable {
		return 12500
	}
	return 10000
}

// FreshnessDecayBP returns a deterministic integer-bps value factor [0..10000] based on NMU-1 freshness.
// Expired units return 0; ephemeral units decay smoothly as they approach ValidUntilUnix over a 7-day window.
func FreshnessDecayBP(fc nmu.FreshnessClass, validUntilUnix, nowUnix int64) int64 {
	if nmu.IsExpired(fc, validUntilUnix, nowUnix) {
		return 0
	}
	if nmu.NormalizeFreshness(string(fc)) == nmu.FreshnessEphemeral && validUntilUnix > 0 && nowUnix > 0 {
		remaining := validUntilUnix - nowUnix
		const fullWindow int64 = 7 * 86400
		if remaining >= fullWindow {
			return 10000
		}
		bp := (remaining * 10000) / fullWindow
		if bp < 2000 {
			return 2000
		}
		return bp
	}
	return 10000
}

// UnitArbitrageBidInput extends ArbitrageBidInput with NMU-1 grade, rights, freshness, and expected unit count.
type UnitArbitrageBidInput struct {
	ArbitrageBidInput
	TargetGrade    nmu.Grade
	DesiredRights  nmu.UsageRights
	FreshnessClass nmu.FreshnessClass
	ValidUntilUnix int64
	NowUnix        int64
	ExpectedUnits  uint32 // expected verified triples in pack (default 1)
}

// UnitArbitrageBidResult reports both total satoshi bid cap and normalized milli-sats/triple bid cap.
type UnitArbitrageBidResult struct {
	ArbitrageBidResult
	MaxSatsPer1kUnits uint64 `json:"maxSatsPer1kUnits"`
	GradeWeightBP     int64  `json:"gradeWeightBp"`
	RightsMultBP      int64  `json:"rightsMultBp"`
	FreshnessDecayBP  int64  `json:"freshnessDecayBp"`
}

// ComputeUnitAutoBid computes a deterministic integer-bps total bid AND normalized satsPer1kUnits ceiling
// adjusted by NMU-1 Grade, UsageRights, and FreshnessClass (always <= MaxPolicyDealSats).
func ComputeUnitAutoBid(in UnitArbitrageBidInput) UnitArbitrageBidResult {
	base := ComputeAutoBidSats(in.ArbitrageBidInput)
	gw := GradeWeightBP(in.TargetGrade)
	rm := RightsMultiplierBP(in.DesiredRights)
	fd := FreshnessDecayBP(in.FreshnessClass, in.ValidUntilUnix, in.NowUnix)

	bid := base.MaxBidSats
	if fd == 0 {
		bid = 0
	} else {
		bid = (bid * uint64(gw)) / 10000
		bid = (bid * uint64(rm)) / 10000
		bid = (bid * uint64(fd)) / 10000
		minBid := in.MinBidSats
		if minBid == 0 {
			minBid = 1
		}
		if bid < minBid {
			bid = minBid
		}
	}
	capped := base.CappedByPolicy
	if in.MaxPolicyDealSats > 0 && bid > in.MaxPolicyDealSats {
		bid = in.MaxPolicyDealSats
		capped = true
	}
	units := in.ExpectedUnits
	if units == 0 {
		units = 1
	}
	res := base
	res.MaxBidSats = bid
	res.CappedByPolicy = capped
	return UnitArbitrageBidResult{
		ArbitrageBidResult: res,
		MaxSatsPer1kUnits:  nmu.ComputeSatsPer1kUnits(bid, units),
		GradeWeightBP:      gw,
		RightsMultBP:       rm,
		FreshnessDecayBP:   fd,
	}
}

// DynamicPriceState persists per-listing adaptive pricing parameters in sell_listings.dynamic_pricing_json.
type DynamicPriceState struct {
	Enabled          bool   `json:"enabled"`
	PerUnitMode      bool   `json:"perUnitMode,omitempty"` // when true, CurrentPriceSats is satsPer1kUnits (milli-sats/triple)
	FloorSats        uint64 `json:"floorSats"`
	CeilingSats      uint64 `json:"ceilingSats"`
	CurrentPriceSats uint64 `json:"currentPriceSats"`
	HitsCount        int64  `json:"hitsCount"`
	LastHitUnix      int64  `json:"lastHitUnix,omitempty"`
	LastDecayUnix    int64  `json:"lastDecayUnix,omitempty"`
	DecayIntervalSec int64  `json:"decayIntervalSec,omitempty"`
}

// ParseDynamicPriceState decodes dynamic_pricing_json (returning a sane disabled default when empty).
func ParseDynamicPriceState(raw []byte, basePrice uint64) DynamicPriceState {
	st := DynamicPriceState{
		Enabled:          false,
		FloorSats:        maxUint64(1, basePrice/2),
		CeilingSats:      maxUint64(basePrice, basePrice*4),
		CurrentPriceSats: maxUint64(1, basePrice),
		DecayIntervalSec: DefaultDecayIntervalSec,
	}
	if len(raw) == 0 {
		return st
	}
	_ = json.Unmarshal(raw, &st)
	st.normalize(basePrice)
	return st
}

func (s *DynamicPriceState) normalize(basePrice uint64) {
	if s.FloorSats == 0 {
		s.FloorSats = maxUint64(1, basePrice/2)
	}
	if s.CeilingSats < s.FloorSats {
		s.CeilingSats = maxUint64(s.FloorSats, basePrice*4)
	}
	if s.CurrentPriceSats == 0 {
		s.CurrentPriceSats = maxUint64(s.FloorSats, basePrice)
	}
	if s.CurrentPriceSats < s.FloorSats {
		s.CurrentPriceSats = s.FloorSats
	}
	if s.CurrentPriceSats > s.CeilingSats {
		s.CurrentPriceSats = s.CeilingSats
	}
	if s.DecayIntervalSec <= 0 {
		s.DecayIntervalSec = DefaultDecayIntervalSec
	}
}

// QuotePriceForBuyer returns the deterministic ask price for a specific buyer, applying up to
// -15% collaboration discount when the buyer is a Mangle priority_buyer with high r_collab,
// never dropping below FloorSats.
func (s DynamicPriceState) QuotePriceForBuyer(basePrice uint64, buyerPriority bool, buyerCollabBP int64) uint64 {
	if !s.Enabled {
		if buyerPriority && buyerCollabBP >= 8000 && basePrice > 1 {
			disc := (basePrice * uint64(DefaultMaxCollabDiscountBP)) / 10000
			if basePrice > disc {
				return maxUint64(1, basePrice-disc)
			}
		}
		return basePrice
	}
	s.normalize(basePrice)
	price := s.CurrentPriceSats
	if buyerPriority && buyerCollabBP >= 8000 {
		// Linearly scale discount from 5% at 8000 bp up to 15% at 10000 bp.
		excess := buyerCollabBP - 8000
		if excess > 2000 {
			excess = 2000
		}
		discBP := int64(500) + (excess*1000)/2000
		if discBP > DefaultMaxCollabDiscountBP {
			discBP = DefaultMaxCollabDiscountBP
		}
		disc := (price * uint64(discBP)) / 10000
		if price > disc {
			price -= disc
		}
		if price < s.FloorSats {
			price = s.FloorSats
		}
	}
	return price
}

// QuoteForFactPack computes both total sats and normalized satsPer1kUnits for a fragment containing unitCount verified units.
// When PerUnitMode is true, basePrice / CurrentPriceSats represents a satsPer1kUnits rate (milli-sats per triple);
// otherwise it represents a flat per-fragment price normalized by unitCount.
func (s DynamicPriceState) QuoteForFactPack(basePrice uint64, unitCount uint32, buyerPriority bool, buyerCollabBP int64) (totalSats uint64, satsPer1kUnits uint64) {
	units := unitCount
	if units == 0 {
		units = 1
	}
	if s.PerUnitMode {
		unitRate := s.QuotePriceForBuyer(basePrice, buyerPriority, buyerCollabBP)
		totalSats = nmu.ComputeTotalSatsForUnits(unitRate, units)
		return totalSats, nmu.ComputeSatsPer1kUnits(totalSats, units)
	}
	totalSats = s.QuotePriceForBuyer(basePrice, buyerPriority, buyerCollabBP)
	return totalSats, nmu.ComputeSatsPer1kUnits(totalSats, units)
}

// OnDemandHit ratchets CurrentPriceSats upward on a settled sale (+5%..+10% scaled by seller
// ontology composite reputation), strictly clamped to [FloorSats, CeilingSats].
func (s *DynamicPriceState) OnDemandHit(basePrice uint64, sellerCompositeBP int64, nowUnix int64) uint64 {
	s.normalize(basePrice)
	s.HitsCount++
	s.LastHitUnix = nowUnix
	if !s.Enabled {
		return basePrice
	}
	// Step between +5% (unrated) and +10% (high seller composite rep).
	stepBP := int64(500)
	if sellerCompositeBP > 5000 {
		extra := ((sellerCompositeBP - 5000) * 500) / 5000
		if extra > 500 {
			extra = 500
		}
		stepBP += extra
	}
	delta := (s.CurrentPriceSats * uint64(stepBP)) / 10000
	if delta == 0 {
		delta = 1
	}
	next := s.CurrentPriceSats + delta
	if next > s.CeilingSats {
		next = s.CeilingSats
	}
	s.CurrentPriceSats = next
	return s.CurrentPriceSats
}

// ApplyIdleDecay decays CurrentPriceSats downward by 10% for each elapsed idle DecayIntervalSec
// without demand hits, strictly floored at FloorSats. Returns true if price changed.
func (s *DynamicPriceState) ApplyIdleDecay(basePrice uint64, nowUnix int64) bool {
	s.normalize(basePrice)
	if !s.Enabled || s.CurrentPriceSats <= s.FloorSats {
		return false
	}
	anchor := s.LastHitUnix
	if s.LastDecayUnix > anchor {
		anchor = s.LastDecayUnix
	}
	if anchor <= 0 {
		s.LastDecayUnix = nowUnix
		return false
	}
	elapsed := nowUnix - anchor
	if elapsed < s.DecayIntervalSec {
		return false
	}
	steps := elapsed / s.DecayIntervalSec
	if steps > 20 {
		steps = 20
	}
	changed := false
	for i := int64(0); i < steps; i++ {
		if s.CurrentPriceSats <= s.FloorSats {
			break
		}
		drop := (s.CurrentPriceSats * uint64(DefaultPriceDecayDownBP)) / 10000
		if drop == 0 {
			drop = 1
		}
		if s.CurrentPriceSats <= s.FloorSats+drop {
			s.CurrentPriceSats = s.FloorSats
		} else {
			s.CurrentPriceSats -= drop
		}
		changed = true
	}
	s.LastDecayUnix = nowUnix
	return changed
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
