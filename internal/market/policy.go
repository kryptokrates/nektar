package market

import (
	"errors"
	"sort"
	"sync"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// Policy constrains automated agent market spending and selling.
// Fail-closed: if KillSwitch is set or price exceeds caps, deals are refused.
type Policy struct {
	mu               sync.RWMutex
	KillSwitch       bool            `json:"killSwitch"`
	MaxSatsPerDeal   uint64          `json:"maxSatsPerDeal"`
	MaxSatsPerDay    uint64          `json:"maxSatsPerDay"`
	ZeroConfMaxSats  uint64          `json:"zeroConfMaxSats"`
	AllowedDomains   map[string]bool `json:"allowedDomains,omitempty"`
	BlockedSellers   map[string]bool `json:"blockedSellers,omitempty"`
	PreferredSellers map[string]bool `json:"preferredSellers,omitempty"`
	DailySpentSats   uint64          `json:"dailySpentSats"`
}

func DefaultPolicy() *Policy {
	return &Policy{
		MaxSatsPerDeal:  50_000,
		MaxSatsPerDay:   500_000,
		ZeroConfMaxSats: 10_000,
		BlockedSellers:  make(map[string]bool),
		PreferredSellers: make(map[string]bool),
	}
}

func (p *Policy) SetKillSwitch(on bool) {
	p.mu.Lock()
	p.KillSwitch = on
	p.mu.Unlock()
}

func (p *Policy) CanSpend(sellerPub string, sats uint64) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.KillSwitch {
		return errors.New("market: operator kill switch is engaged")
	}
	if p.BlockedSellers[sellerPub] {
		return errors.New("market: seller is blocked by policy")
	}
	if p.MaxSatsPerDeal > 0 && sats > p.MaxSatsPerDeal {
		return errors.New("market: deal price exceeds maxSatsPerDeal")
	}
	if p.MaxSatsPerDay > 0 && p.DailySpentSats+sats > p.MaxSatsPerDay {
		return errors.New("market: deal exceeds daily spending cap")
	}
	return nil
}

func (p *Policy) RecordSpend(sats uint64) {
	p.mu.Lock()
	p.DailySpentSats += sats
	p.mu.Unlock()
}

// ScoredOffer ranks a candidate OfferAd deterministically using integer basis points (0..10000).
type ScoredOffer struct {
	Offer   bstack.OfferAd `json:"offer"`
	ScoreBP int64          `json:"scoreBp"`
	Reason  string         `json:"reason"`
}

// RankOffers scores and sorts offers deterministically in integer basis points:
// price efficiency (up to 6000 bp) + direct delivery bonus (1500 bp) + verified leaf/CHIRP commitment (1500 bp) + preferred seller bonus (1000 bp).
func RankOffers(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy) []ScoredOffer {
	var ranked []ScoredOffer
	for _, o := range offers {
		if want.MaxSats > 0 && o.PriceSats > want.MaxSats {
			continue
		}
		if pol != nil && pol.CanSpend(o.Pub, o.PriceSats) != nil {
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
		if pol != nil {
			pol.mu.RLock()
			if pol.PreferredSellers[o.Pub] {
				score += 1000
			}
			pol.mu.RUnlock()
		}
		ranked = append(ranked, ScoredOffer{Offer: o, ScoreBP: score, Reason: "eligible"})
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
