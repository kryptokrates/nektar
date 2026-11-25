package market

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	AIModeOff      = "off"
	AIModeAdvisory = "advisory"
	AIModeGated    = "gated"
	AIModeStrict   = "strict"

	PrivacyMetadataOnly       = "metadata_only"
	PrivacyDisclosablePreview = "disclosable_preview"
)

// AIMatchingPolicy governs cloud AI matching egress, privacy scope, thresholds, and semantic intent.
// Committed into OperatorPolicyRecord.PolicyHash (operator-only; never mutable by agent tools).
// Monotonic tightening: AI matching may only rank, reject, or escalate within deterministic hard policy.
type AIMatchingPolicy struct {
	Mode                 string   `json:"mode,omitempty"`                 // off | advisory | gated | strict
	AllowedHosts         []string `json:"allowedHosts,omitempty"`         // pinned AI egress hostnames
	SellerPrivacyScope   string   `json:"sellerPrivacyScope,omitempty"`   // metadata_only | disclosable_preview
	MaxRelevanceWeightBP int64    `json:"maxRelevanceWeightBp,omitempty"` // default 3000 (max 5000)
	MinSellerRelevanceBP int64    `json:"minSellerRelevanceBp,omitempty"` // default 5000 (2.0/4.0)
	MinBuyerRelevanceBP  int64    `json:"minBuyerRelevanceBp,omitempty"`  // default 5000 (2.0/4.0)
	MinConfidenceBP      int64    `json:"minConfidenceBp,omitempty"`      // default 6500 (0.65)
	MaxAbuseProbBP       int64    `json:"maxAbuseProbBp,omitempty"`       // default 3500 (0.35)
	EscalateConfidenceBP int64    `json:"escalateConfidenceBp,omitempty"` // default 7500 (0.75)
	AutoApproveMaxSats   uint64   `json:"autoApproveMaxSats,omitempty"`   // default 500
	SemanticIntent       string   `json:"semanticIntent,omitempty"`       // optional operator intent rubric
	ProhibitedTopics     []string `json:"prohibitedTopics,omitempty"`     // optional semantic exclusions
	CedarMarketPolicy    string   `json:"cedarMarketPolicy,omitempty"`    // optional post-match Cedar rules
}

// IsEmpty reports whether no AI matching fields have been configured.
func (a *AIMatchingPolicy) IsEmpty() bool {
	if a == nil {
		return true
	}
	return strings.TrimSpace(a.Mode) == "" &&
		len(a.AllowedHosts) == 0 &&
		strings.TrimSpace(a.SellerPrivacyScope) == "" &&
		a.MaxRelevanceWeightBP == 0 &&
		a.MinSellerRelevanceBP == 0 &&
		a.MinBuyerRelevanceBP == 0 &&
		a.MinConfidenceBP == 0 &&
		a.MaxAbuseProbBP == 0 &&
		a.EscalateConfidenceBP == 0 &&
		a.AutoApproveMaxSats == 0 &&
		strings.TrimSpace(a.SemanticIntent) == "" &&
		len(a.ProhibitedTopics) == 0 &&
		strings.TrimSpace(a.CedarMarketPolicy) == ""
}

// Validate verifies operator AI matching policy fields fail-closed.
func (a *AIMatchingPolicy) Validate() error {
	if a == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(a.Mode))
	if mode != "" && mode != AIModeOff && mode != AIModeAdvisory && mode != AIModeGated && mode != AIModeStrict {
		return fmt.Errorf("market: invalid aiMatching.mode %q (use off, advisory, gated, or strict)", a.Mode)
	}
	priv := strings.ToLower(strings.TrimSpace(a.SellerPrivacyScope))
	if priv != "" && priv != PrivacyMetadataOnly && priv != PrivacyDisclosablePreview {
		return fmt.Errorf("market: invalid aiMatching.sellerPrivacyScope %q (use metadata_only or disclosable_preview)", a.SellerPrivacyScope)
	}
	if a.MaxRelevanceWeightBP < 0 || a.MaxRelevanceWeightBP > 5000 {
		return fmt.Errorf("market: aiMatching.maxRelevanceWeightBp %d out of range [0, 5000]", a.MaxRelevanceWeightBP)
	}
	for name, v := range map[string]int64{
		"minSellerRelevanceBp": a.MinSellerRelevanceBP,
		"minBuyerRelevanceBp":  a.MinBuyerRelevanceBP,
		"minConfidenceBp":      a.MinConfidenceBP,
		"maxAbuseProbBp":       a.MaxAbuseProbBP,
		"escalateConfidenceBp": a.EscalateConfidenceBP,
	} {
		if v < 0 || v > 10000 {
			return fmt.Errorf("market: aiMatching.%s %d out of range [0, 10000]", name, v)
		}
	}
	if strings.TrimSpace(a.CedarMarketPolicy) != "" {
		if _, err := policy.Canonicalize("market-ai", a.CedarMarketPolicy); err != nil {
			return fmt.Errorf("market: invalid aiMatching.cedarMarketPolicy: %w", err)
		}
	}
	return nil
}

// Normalized returns a copy with safe defaults applied for active AI matching.
func (a AIMatchingPolicy) Normalized(defaultMode string) AIMatchingPolicy {
	out := a
	out.Mode = strings.ToLower(strings.TrimSpace(out.Mode))
	if out.Mode == "" {
		out.Mode = strings.ToLower(strings.TrimSpace(defaultMode))
	}
	if out.Mode == "" {
		out.Mode = AIModeOff
	}
	out.SellerPrivacyScope = strings.ToLower(strings.TrimSpace(out.SellerPrivacyScope))
	if out.SellerPrivacyScope == "" {
		out.SellerPrivacyScope = PrivacyDisclosablePreview
	}
	if out.MaxRelevanceWeightBP <= 0 {
		out.MaxRelevanceWeightBP = 3000
	}
	if out.MaxRelevanceWeightBP > 5000 {
		out.MaxRelevanceWeightBP = 5000
	}
	if out.MinSellerRelevanceBP <= 0 {
		out.MinSellerRelevanceBP = 5000
	}
	if out.MinBuyerRelevanceBP <= 0 {
		out.MinBuyerRelevanceBP = 5000
	}
	if out.MinConfidenceBP <= 0 {
		out.MinConfidenceBP = 6500
	}
	if out.MaxAbuseProbBP <= 0 {
		out.MaxAbuseProbBP = 3500
	}
	if out.EscalateConfidenceBP <= 0 {
		out.EscalateConfidenceBP = 7500
	}
	if out.AutoApproveMaxSats == 0 {
		out.AutoApproveMaxSats = 500
	}
	return out
}

// Policy constrains automated agent market spending and selling.
// Fail-closed: if KillSwitch is set or price exceeds caps, deals are refused.
type Policy struct {
	mu               sync.RWMutex
	db               *store.DB
	KillSwitch       bool              `json:"killSwitch"`
	MaxSatsPerDeal   uint64            `json:"maxSatsPerDeal"`
	MaxSatsPerDay    uint64            `json:"maxSatsPerDay"`
	ZeroConfMaxSats  uint64            `json:"zeroConfMaxSats"`
	AllowedDomains   map[string]bool   `json:"allowedDomains,omitempty"`
	BlockedSellers   map[string]bool   `json:"blockedSellers,omitempty"`
	PreferredSellers map[string]bool   `json:"preferredSellers,omitempty"`
	DailySpentSats   uint64            `json:"dailySpentSats"`
	BuyerApproved    bool              `json:"buyerApproved"`
	SellerApproved   bool              `json:"sellerApproved"`
	AIMatching       *AIMatchingPolicy `json:"aiMatching,omitempty"`
}

// AIMatchingPolicy returns a defensive copy of the active operator AI matching policy (or nil if unset).
func (p *Policy) AIMatchingPolicy() *AIMatchingPolicy {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.AIMatching == nil {
		return nil
	}
	cp := *p.AIMatching
	cp.AllowedHosts = append([]string(nil), p.AIMatching.AllowedHosts...)
	cp.ProhibitedTopics = append([]string(nil), p.AIMatching.ProhibitedTopics...)
	return &cp
}

// SetAIMatchingPolicy updates the in-memory AI matching policy atomically.
func (p *Policy) SetAIMatchingPolicy(ai *AIMatchingPolicy) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if ai == nil {
		p.AIMatching = nil
	} else {
		cp := *ai
		cp.AllowedHosts = append([]string(nil), ai.AllowedHosts...)
		cp.ProhibitedTopics = append([]string(nil), ai.ProhibitedTopics...)
		p.AIMatching = &cp
	}
	p.mu.Unlock()
}

// AllowedDomainsList returns a sorted snapshot of the policy's allowed domains.
func (p *Policy) AllowedDomainsList() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	out := make([]string, 0, len(p.AllowedDomains))
	for d, ok := range p.AllowedDomains {
		if ok {
			out = append(out, d)
		}
	}
	p.mu.RUnlock()
	sort.Strings(out)
	return out
}

func (p *Policy) AttachDB(db *store.DB) {
	if db == nil {
		return
	}
	p.mu.Lock()
	p.db = db
	if v, err := db.DailySpend("market_daily_spend"); err == nil {
		p.DailySpentSats = v
	}
	var killed bool
	if ok, err := db.GetJSON("market_kill_switch", &killed); err == nil && ok && killed {
		p.KillSwitch = true
	}
	p.mu.Unlock()
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
	db := p.db
	p.mu.Unlock()
	if db != nil {
		_ = db.SetJSON("market_kill_switch", on)
	}
}

func (p *Policy) Snapshot() (kill bool, maxDeal, maxDay, zeroConf, spent uint64, buyerOK, sellerOK bool) {
	p.mu.RLock()
	db := p.db
	kill, maxDeal, maxDay, zeroConf, spent, buyerOK, sellerOK = p.KillSwitch, p.MaxSatsPerDeal, p.MaxSatsPerDay, p.ZeroConfMaxSats, p.DailySpentSats, p.BuyerApproved, p.SellerApproved
	p.mu.RUnlock()
	if db != nil {
		if v, err := db.DailySpend("market_daily_spend"); err == nil {
			spent = v
		}
	}
	return kill, maxDeal, maxDay, zeroConf, spent, buyerOK, sellerOK
}

func (p *Policy) CanSpend(sellerPub string, sats uint64) error {
	return p.CanSpendForDomains(sellerPub, nil, sats)
}

func (p *Policy) CanSpendForDomains(sellerPub string, domains []string, sats uint64) error {
	p.mu.RLock()
	db := p.db
	kill := p.KillSwitch
	blocked := p.BlockedSellers[sellerPub]
	maxDeal := p.MaxSatsPerDeal
	maxDay := p.MaxSatsPerDay
	spent := p.DailySpentSats
	var allowedCopy map[string]bool
	if len(p.AllowedDomains) > 0 {
		allowedCopy = make(map[string]bool, len(p.AllowedDomains))
		for k, v := range p.AllowedDomains {
			allowedCopy[k] = v
		}
	}
	p.mu.RUnlock()
	if db != nil {
		if v, err := db.DailySpend("market_daily_spend"); err == nil {
			spent = v
		}
	}
	if kill {
		return errors.New("market: operator kill switch is engaged")
	}
	if blocked {
		return errors.New("market: seller is blocked by policy")
	}
	if len(allowedCopy) > 0 && len(domains) > 0 && !allowedCopy["*"] {
		domOK := false
		for _, d := range domains {
			if allowedCopy[strings.ToLower(strings.TrimSpace(d))] {
				domOK = true
				break
			}
		}
		if !domOK {
			return fmt.Errorf("market: domain(s) %v not in policy allowedDomains", domains)
		}
	}
	if maxDeal == 0 || sats > maxDeal {
		return errors.New("market: deal price exceeds maxSatsPerDeal")
	}
	if maxDay == 0 || spent+sats > maxDay {
		return errors.New("market: deal exceeds daily spending cap")
	}
	return nil
}

func (p *Policy) RecordSpend(sats uint64) {
	p.mu.Lock()
	p.DailySpentSats += sats
	db := p.db
	p.mu.Unlock()
	if db != nil {
		if next, err := db.AddDailySpend("market_daily_spend", sats); err == nil {
			p.mu.Lock()
			p.DailySpentSats = next
			p.mu.Unlock()
		}
	}
}

// ApprovePolicyPreset creates, hashes, persists, and activates a safe operator policy preset
// ("buyer" | "seller" | "both") so autonomous buying/selling passes explicit operator consent.
func ApprovePolicyPreset(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string) (store.OperatorPolicyRecord, error) {
	return ApprovePolicyPresetWithAI(db, pol, preset, domains, approvedBy, nil)
}

// ApprovePolicyPresetWithAI creates, validates, hashes, persists, and activates an operator policy preset
// with optional Jev AI matching egress/privacy/threshold controls bound into PolicyHash.
func ApprovePolicyPresetWithAI(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string, aiPol *AIMatchingPolicy) (store.OperatorPolicyRecord, error) {
	if err := aiPol.Validate(); err != nil {
		return store.OperatorPolicyRecord{}, err
	}
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
	var idBytes [6]byte
	if _, err := crand.Read(idBytes[:]); err != nil {
		return store.OperatorPolicyRecord{}, fmt.Errorf("market: csprng failed: %w", err)
	}
	var aiJSON string
	if !aiPol.IsEmpty() {
		raw, err := core.CanonicalJSON(aiPol)
		if err != nil {
			return store.OperatorPolicyRecord{}, err
		}
		aiJSON = string(raw)
	}
	rec := store.OperatorPolicyRecord{
		PolicyID:     fmt.Sprintf("pol-%s-%s", preset, hex.EncodeToString(idBytes[:])),
		Roles:        roles,
		Domains:      domains,
		MaxPerDeal:   2_000,
		MaxPerDay:    20_000,
		ZeroConfMax:  1_000,
		AIPolicyJSON: aiJSON,
		Active:       true,
		ApprovedBy:   approvedBy,
		ApprovedAt:   time.Now().Unix(),
	}
	hashInput := map[string]any{
		"roles":       rec.Roles,
		"domains":     rec.Domains,
		"maxPerDeal":  rec.MaxPerDeal,
		"maxPerDay":   rec.MaxPerDay,
		"zeroConfMax": rec.ZeroConfMax,
	}
	if aiJSON != "" {
		hashInput["aiMatching"] = aiPol
	}
	canon, err := core.CanonicalJSON(hashInput)
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
	if strings.TrimSpace(rec.AIPolicyJSON) != "" {
		var ai AIMatchingPolicy
		if json.Unmarshal([]byte(rec.AIPolicyJSON), &ai) == nil {
			pol.AIMatching = &ai
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
	activeCount := 0
	doms := make(map[string]bool)
	var latestAI *AIMatchingPolicy
	for _, rec := range list {
		if !rec.Active {
			continue
		}
		activeCount++
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
		// ListOperatorPolicies returns newest first: take the newest active policy carrying AI config.
		if latestAI == nil && strings.TrimSpace(rec.AIPolicyJSON) != "" {
			var parsed AIMatchingPolicy
			if json.Unmarshal([]byte(rec.AIPolicyJSON), &parsed) == nil {
				latestAI = &parsed
			}
		}
	}
	if activeCount > 0 {
		pol.MaxSatsPerDeal = maxDeal
		pol.MaxSatsPerDay = maxDay
		pol.ZeroConfMaxSats = zeroConf
		pol.AIMatching = latestAI
	} else if len(list) > 0 {
		// All persisted policies were revoked: reset caps to 0 (fail-closed, never keep stale revoked caps).
		pol.MaxSatsPerDeal = 0
		pol.MaxSatsPerDay = 0
		pol.ZeroConfMaxSats = 0
		pol.AIMatching = nil
	}
	if len(doms) > 0 {
		pol.AllowedDomains = doms
	} else {
		pol.AllowedDomains = nil
	}
	var killed bool
	if ok, kErr := db.GetJSON("market_kill_switch", &killed); kErr == nil && ok {
		pol.KillSwitch = killed
	}
	pol.mu.Unlock()
	pol.AttachDB(db)
	return nil
}

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
	var ranked []ScoredOffer
	for _, o := range offers {
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
