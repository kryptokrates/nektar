package market

import (
	"encoding/hex"
	"sort"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
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

// RankOffers scores and sorts offers deterministically in integer basis points.
func RankOffers(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy) []ScoredOffer {
	return RankOffersWithRep(want, offers, pol, nil)
}

// RankOffersWithRep adds a verified seller-reputation integer bps bonus (up to +1000 bp)
// derived strictly from locally verified settled vs failed trades in SQLite.
func RankOffersWithRep(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy, reps map[string]store.SellerReputation) []ScoredOffer {
	return RankOffersWithRepAndBonds(want, offers, pol, reps, nil)
}

// RankOffersWithRepAndBonds extends RankOffersWithRep with a deterministic SPV-verified
// NektarCheckpointBond stake bonus (up to +750 bp for active un-seized bonds >= 1000 sats)
// to defeat free Sybil quote spam.
func RankOffersWithRepAndBonds(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy, reps map[string]store.SellerReputation, verifiedBondSats map[string]uint64) []ScoredOffer {
	var ranked []ScoredOffer
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
		var score int64
		if want.MaxSats > 0 {
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
		if verifiedBondSats != nil {
			if bondSats := verifiedBondSats[o.OfferID]; bondSats >= 1000 {
				bondBonus := int64(bondSats / 100)
				if bondBonus > 750 {
					bondBonus = 750
				}
				score += bondBonus
			}
		}
		ranked = append(ranked, ScoredOffer{Offer: o, ScoreBP: score, BaseScoreBP: score, Reason: "eligible"})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].ScoreBP != ranked[j].ScoreBP {
			return ranked[i].ScoreBP > ranked[j].ScoreBP
		}
		if ranked[i].Offer.PriceSats != ranked[j].Offer.PriceSats {
			return ranked[i].Offer.PriceSats < ranked[j].Offer.PriceSats
		}
		return ranked[i].Offer.OfferID < ranked[j].Offer.OfferID
	})
	return ranked
}
