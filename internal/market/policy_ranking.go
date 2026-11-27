package market

import (
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

// ScoredOffer ranks a candidate OfferAd deterministically using integer basis points.
type ScoredOffer struct {
	Offer           bstack.OfferAd `json:"offer"`
	ScoreBP         int64          `json:"scoreBp"`
	BaseScoreBP     int64          `json:"baseScoreBp,omitempty"`
	SemanticScoreBP int64          `json:"semanticScoreBp,omitempty"`
	ConfidenceBP    int64          `json:"confidenceBp,omitempty"`
	RiskFlag        string         `json:"riskFlag,omitempty"`
	Reason          string         `json:"reason"`
}

// RankOffersWithRepAndBonds scores and sorts offers deterministically in integer basis points,
// combining verified local seller reputation (up to +1000 bp) and SPV-verified NektarCheckpointBond
// stake bonus (up to +750 bp for active un-seized bonds >= 1000 sats) to defeat free Sybil quote spam.
func RankOffersWithRepAndBonds(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy, reps map[string]store.SellerReputation, verifiedBondSats map[string]uint64) []ScoredOffer {
	return RankOffersWith6DReputation(want, offers, pol, reps, nil, verifiedBondSats)
}

// RankOffersWith6DReputation scores and sorts offers using deterministic integer basis points,
// incorporating NMU-1 unit-price normalization (satsPer1kUnits), machine-verifiable commodity grades
// (with Grade S dead-feed anti-gaming guard), ontology-scoped 6D reputation, Personalized EigenTrust, and bonds.
func RankOffersWith6DReputation(
	want bstack.WantAd,
	offers []bstack.OfferAd,
	pol *Policy,
	reps map[string]store.SellerReputation,
	rep6D map[string]reputation.Vector6D,
	verifiedBondSats map[string]uint64,
) []ScoredOffer {
	return RankOffersWith6DReputationAt(want, offers, pol, reps, rep6D, verifiedBondSats, time.Now().Unix())
}

// RankOffersWith6DReputationAt is the deterministic clock-injected form of RankOffersWith6DReputation.
func RankOffersWith6DReputationAt(
	want bstack.WantAd,
	offers []bstack.OfferAd,
	pol *Policy,
	reps map[string]store.SellerReputation,
	rep6D map[string]reputation.Vector6D,
	verifiedBondSats map[string]uint64,
	nowUnix int64,
) []ScoredOffer {
	type eligible struct {
		offer    bstack.OfferAd
		unitRate uint64
		grade    nmu.Grade
	}
	wantMinGrade := nmu.NormalizeGrade(want.MinGrade)
	wantFresh := nmu.NormalizeFreshness(want.FreshnessClass)
	wantRights := nmu.NormalizeRights(want.RequiredRights)
	wantMode := nmu.NormalizePricingMode(want.PricingMode)

	var cands []eligible
	useUnitPricing := want.MaxSatsPer1kUnits > 0
	var maxObservedRate uint64

	for _, o := range offers {
		if want.Net == "main" || want.Net == "testnet" {
			leafRaw, lErr := hex.DecodeString(strings.TrimSpace(o.Leaf))
			if lErr != nil || len(leafRaw) != 32 {
				continue
			}
		}
		if want.MaxSats > 0 && o.PriceSats > want.MaxSats {
			continue
		}
		if pol != nil && pol.CanSpendForDomains(o.Pub, want.Domains, o.PriceSats) != nil {
			continue
		}
		// Hard NMU-1 commodity pre-filters.
		if nmu.IsExpired(nmu.FreshnessClass(o.FreshnessClass), o.ValidUntilUnix, nowUnix) {
			continue
		}
		grade := nmu.NormalizeGrade(o.Grade)
		if wantMinGrade != "" && !nmu.MeetsMinGrade(grade, wantMinGrade) {
			continue
		}
		if wantRights != "" && !nmu.MeetsRequiredRights(nmu.UsageRights(o.UsageRights), wantRights) {
			continue
		}
		if wantFresh != "" && nmu.NormalizeFreshness(o.FreshnessClass) != wantFresh {
			continue
		}
		if wantMode != "" && nmu.EffectivePricingMode(nmu.PricingMode(o.PricingMode)) != wantMode {
			continue
		}
		rate := effectiveOfferUnitRate(o)
		if want.MaxSatsPer1kUnits > 0 && rate > want.MaxSatsPer1kUnits {
			continue
		}
		if grade != "" || o.UnitCount > 0 || o.SatsPer1kUnits > 0 {
			useUnitPricing = true
		}
		if rate > maxObservedRate {
			maxObservedRate = rate
		}
		cands = append(cands, eligible{offer: o, unitRate: rate, grade: grade})
	}
	if len(cands) == 0 {
		return nil
	}

	rateCeiling := want.MaxSatsPer1kUnits
	if useUnitPricing && rateCeiling == 0 {
		rateCeiling = maxObservedRate
		if want.MaxSats > 0 {
			maxOneUnit := nmu.ComputeSatsPer1kUnits(want.MaxSats, 1)
			if maxOneUnit > rateCeiling {
				rateCeiling = maxOneUnit
			}
		}
	}

	ranked := make([]ScoredOffer, 0, len(cands))
	for _, c := range cands {
		o := c.offer
		var score int64
		if useUnitPricing && rateCeiling > 0 {
			r := c.unitRate
			if r > rateCeiling {
				r = rateCeiling
			}
			savedRate := rateCeiling - r
			score += int64((savedRate * 6000) / rateCeiling)
		} else if want.MaxSats > 0 {
			saved := want.MaxSats - o.PriceSats
			score += int64((saved * 6000) / want.MaxSats)
		} else {
			score += 3000
		}
		if o.Direct {
			score += 1500
		}
		if o.KeyCommitHex != "" && o.ChirpURL != "" {
			score += 1500
		}
		// Organic peer slightly preferred over hub fallback on exact tie so the network graduates off the hub!
		if !o.IsHub {
			score += 250
		}
		// Prefer exact user-topic overlap over cluster-only overlap on tie.
		if HasExactUserDomainOverlap(want.Domains, o.Domains) {
			score += 250
		}
		if pol != nil {
			pol.mu.RLock()
			if pol.PreferredSellers[o.Pub] {
				score += 1000
			}
			pol.mu.RUnlock()
		}
		if reps != nil {
			if r, ok := reps[o.Pub]; ok {
				total := r.SettledDeals + r.FailedDeals
				if total > 0 {
					// Deterministic integer bps: up to +1000 bp for 100% settled track record, penalized for failures.
					repBonus := (r.SettledDeals * 1000) / total
					if r.FailedDeals > 0 {
						repBonus -= (r.FailedDeals * 500) / total
					}
					score += repBonus
				}
			}
		}
		var (
			vec6D         reputation.Vector6D
			feedPenalized bool
		)
		if rep6D != nil {
			if v, ok := rep6D[strings.ToLower(strings.TrimSpace(o.Pub))]; ok {
				vec6D = v
				// Ontology-scoped 6D + Personalized EigenTrust bonus (up to +1200 bp); zero for disconnected Sybils!
				if v.EigenTrustBP > 0 && v.CompositeBP > 0 {
					weighted := (v.CompositeBP * v.EigenTrustBP) / reputation.MaxBP
					score += (weighted * 1200) / reputation.MaxBP
				}
				// Sharp penalty for verified low accuracy or dispute history.
				if v.AttestCount > 0 && (v.AccBP < 4000 || v.DispBP < 4000) {
					score -= 1500
				}
				if v.AttestCount > 0 && (v.DispBP < 5000 || v.CollabBP < 5000) {
					feedPenalized = true
				}
			}
		}
		hasActiveBond := false
		if verifiedBondSats != nil {
			if bondSats := verifiedBondSats[o.OfferID]; bondSats >= 1000 {
				hasActiveBond = true
				bondBonus := int64(bondSats / 100)
				if bondBonus > 750 {
					bondBonus = 750
				}
				score += bondBonus
			}
		} else if o.BondOutpoint != "" {
			hasActiveBond = true
		}

		// Deterministic NMU-1 Grade bonus with anti-gaming guards:
		// - Grade S full +1300 bp requires BOTH an active bond AND non-penalized DispBP/CollabBP >= 5000
		//   (blocks "dead feed" gaming where a seller spins up a dummy FeedURL without pushing updates).
		// - Grade A +900 bp requires an active bond (unbonded Grade A claims are capped at Grade B).
		_ = vec6D
		switch c.grade {
		case nmu.GradeS:
			if hasActiveBond && !feedPenalized && strings.TrimSpace(o.FeedURL) != "" {
				score += 1300
			} else if hasActiveBond && !feedPenalized {
				score += 900
			} else {
				score += 400
			}
		case nmu.GradeA:
			if hasActiveBond {
				score += 900
			} else {
				score += 400
			}
		case nmu.GradeB:
			score += 400
		}

		ranked = append(ranked, ScoredOffer{Offer: o, ScoreBP: score, BaseScoreBP: score, Reason: "eligible"})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].ScoreBP != ranked[j].ScoreBP {
			return ranked[i].ScoreBP > ranked[j].ScoreBP
		}
		ri := effectiveOfferUnitRate(ranked[i].Offer)
		rj := effectiveOfferUnitRate(ranked[j].Offer)
		if ri != rj {
			return ri < rj
		}
		if ranked[i].Offer.PriceSats != ranked[j].Offer.PriceSats {
			return ranked[i].Offer.PriceSats < ranked[j].Offer.PriceSats
		}
		return ranked[i].Offer.OfferID < ranked[j].Offer.OfferID
	})
	return ranked
}

func effectiveOfferUnitRate(o bstack.OfferAd) uint64 {
	if o.SatsPer1kUnits > 0 {
		return o.SatsPer1kUnits
	}
	return nmu.ComputeSatsPer1kUnits(o.PriceSats, o.UnitCount)
}
