package market

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
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
	BuyerApproved    bool            `json:"buyerApproved"`
	SellerApproved   bool            `json:"sellerApproved"`
}

func DefaultPolicy() *Policy {
	return &Policy{
		MaxSatsPerDeal:   50_000,
		MaxSatsPerDay:    500_000,
		ZeroConfMaxSats:  10_000,
		BlockedSellers:   make(map[string]bool),
		PreferredSellers: make(map[string]bool),
	}
}

func (p *Policy) SetKillSwitch(on bool) {
	p.mu.Lock()
	p.KillSwitch = on
	p.mu.Unlock()
}

func (p *Policy) Snapshot() (kill bool, maxDeal, maxDay, zeroConf, spent uint64, buyerOK, sellerOK bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.KillSwitch, p.MaxSatsPerDeal, p.MaxSatsPerDay, p.ZeroConfMaxSats, p.DailySpentSats, p.BuyerApproved, p.SellerApproved
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

// ApprovePolicyPreset creates, hashes, persists, and activates a safe operator policy preset
// ("buyer" | "seller" | "both") so autonomous buying/selling passes explicit operator consent.
func ApprovePolicyPreset(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string) (store.OperatorPolicyRecord, error) {
	preset = strings.ToLower(strings.TrimSpace(preset))
	var roles []string
	switch preset {
	case "buyer":
		roles = []string{"buyer"}
	case "seller":
		roles = []string{"seller"}
	case "both":
		roles = []string{"buyer", "seller"}
	default:
		return store.OperatorPolicyRecord{}, fmt.Errorf("market: unknown policy preset %q (use buyer, seller, or both)", preset)
	}
	if approvedBy == "" {
		approvedBy = "operator"
	}
	rec := store.OperatorPolicyRecord{
		PolicyID:    fmt.Sprintf("pol-%s-%d", preset, time.Now().UnixNano()%1_000_000),
		Roles:       roles,
		Domains:     domains,
		MaxPerDeal:  2_000,
		MaxPerDay:   20_000,
		ZeroConfMax: 1_000,
		Active:      true,
		ApprovedBy:  approvedBy,
		ApprovedAt:  time.Now().Unix(),
	}
	canon, err := core.CanonicalJSON(map[string]any{
		"roles":       rec.Roles,
		"domains":     rec.Domains,
		"maxPerDeal":  rec.MaxPerDeal,
		"maxPerDay":   rec.MaxPerDay,
		"zeroConfMax": rec.ZeroConfMax,
	})
	if err != nil {
		return store.OperatorPolicyRecord{}, err
	}
	rec.PolicyHash = hex.EncodeToString(core.SHA256(canon))
	if db != nil {
		if err := db.SaveOperatorPolicy(rec); err != nil {
			return store.OperatorPolicyRecord{}, err
		}
	}
	if pol != nil && db != nil {
		_ = SyncPolicyFromDB(db, pol)
	} else if pol != nil {
		applyPolicyRecord(pol, rec)
	}
	return rec, nil
}

func applyPolicyRecord(pol *Policy, rec store.OperatorPolicyRecord) {
	pol.mu.Lock()
	defer pol.mu.Unlock()
	for _, r := range rec.Roles {
		if r == "buyer" {
			pol.BuyerApproved = true
		}
		if r == "seller" {
			pol.SellerApproved = true
		}
	}
	if rec.MaxPerDeal > 0 {
		pol.MaxSatsPerDeal = rec.MaxPerDeal
	}
	if rec.MaxPerDay > 0 {
		pol.MaxSatsPerDay = rec.MaxPerDay
	}
	if rec.ZeroConfMax > 0 {
		pol.ZeroConfMaxSats = rec.ZeroConfMax
	}
	if len(rec.Domains) > 0 {
		if pol.AllowedDomains == nil {
			pol.AllowedDomains = make(map[string]bool)
		}
		for _, d := range rec.Domains {
			pol.AllowedDomains[strings.ToLower(strings.TrimSpace(d))] = true
		}
	}
}

// SyncPolicyFromDB loads active persisted operator_policies into pol.
func SyncPolicyFromDB(db *store.DB, pol *Policy) error {
	if db == nil || pol == nil {
		return nil
	}
	list, err := db.ListOperatorPolicies()
	if err != nil {
		return err
	}
	pol.mu.Lock()
	pol.BuyerApproved = false
	pol.SellerApproved = false
	var maxDeal, maxDay, zeroConf uint64
	doms := make(map[string]bool)
	for _, rec := range list {
		if !rec.Active {
			continue
		}
		for _, r := range rec.Roles {
			if r == "buyer" {
				pol.BuyerApproved = true
			}
			if r == "seller" {
				pol.SellerApproved = true
			}
		}
		if rec.MaxPerDeal > maxDeal {
			maxDeal = rec.MaxPerDeal
		}
		if rec.MaxPerDay > maxDay {
			maxDay = rec.MaxPerDay
		}
		if rec.ZeroConfMax > zeroConf {
			zeroConf = rec.ZeroConfMax
		}
		for _, d := range rec.Domains {
			if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
				doms[d] = true
			}
		}
	}
	if maxDeal > 0 {
		pol.MaxSatsPerDeal = maxDeal
	}
	if maxDay > 0 {
		pol.MaxSatsPerDay = maxDay
	}
	if zeroConf > 0 {
		pol.ZeroConfMaxSats = zeroConf
	}
	if len(doms) > 0 {
		pol.AllowedDomains = doms
	} else {
		pol.AllowedDomains = nil
	}
	pol.mu.Unlock()
	return nil
}

// ScoredOffer ranks a candidate OfferAd deterministically using integer basis points (0..11000).
type ScoredOffer struct {
	Offer   bstack.OfferAd `json:"offer"`
	ScoreBP int64          `json:"scoreBp"`
	Reason  string         `json:"reason"`
}

// RankOffers scores and sorts offers deterministically in integer basis points.
func RankOffers(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy) []ScoredOffer {
	return RankOffersWithRep(want, offers, pol, nil)
}

// RankOffersWithRep adds a verified seller-reputation integer bps bonus (up to +1000 bp)
// derived strictly from locally verified settled vs failed trades in SQLite.
func RankOffersWithRep(want bstack.WantAd, offers []bstack.OfferAd, pol *Policy, reps map[string]store.SellerReputation) []ScoredOffer {
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
		// Organic peer slightly preferred over hub fallback on exact tie so the network graduates off the hub!
		if !o.IsHub {
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
