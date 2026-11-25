package market

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

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

	EscrowModeAuto      = "auto"
	EscrowModeAlways    = "always"
	EscrowModeOff       = "off"
	DefaultEscrowMinSat = uint64(500)
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
	mu                       sync.RWMutex
	db                       *store.DB
	KillSwitch               bool              `json:"killSwitch"`
	MaxSatsPerDeal           uint64            `json:"maxSatsPerDeal"`
	MaxSatsPerDay            uint64            `json:"maxSatsPerDay"`
	ZeroConfMaxSats          uint64            `json:"zeroConfMaxSats"`
	ZeroConfMaxAggregateSats uint64            `json:"zeroConfMaxAggregateSats,omitempty"`
	MaxUnconfirmedAncestors  int               `json:"maxUnconfirmedAncestors,omitempty"`
	AllowedDomains           map[string]bool   `json:"allowedDomains,omitempty"`
	BlockedSellers           map[string]bool   `json:"blockedSellers,omitempty"`
	PreferredSellers         map[string]bool   `json:"preferredSellers,omitempty"`
	DailySpentSats           uint64            `json:"dailySpentSats"`
	MaxHubSubmitFeeSats      uint64            `json:"maxHubSubmitFeeSats,omitempty"`
	MaxHubBboxFeeSats        uint64            `json:"maxHubBboxFeeSats,omitempty"`
	MaxHubEgressTopupSats    uint64            `json:"maxHubEgressTopupSats,omitempty"`
	MaxHubFeeSatsPerDay      uint64            `json:"maxHubFeeSatsPerDay,omitempty"`
	DailyHubFeesSpentSats    uint64            `json:"dailyHubFeesSpentSats,omitempty"`
	BuyerApproved            bool              `json:"buyerApproved"`
	SellerApproved           bool              `json:"sellerApproved"`
	EscrowMode               string            `json:"escrowMode,omitempty"`    // auto (default) | always | off
	EscrowMinSats            uint64            `json:"escrowMinSats,omitempty"` // default 500 sats in auto mode
	AIMatching               *AIMatchingPolicy `json:"aiMatching,omitempty"`
	pinnedOperatorPub        string
	activePolicyHash         string
	tamperedPolicies         int
}

// SetPinnedOperatorPub pins an external Operator compressed secp256k1 pubkey (NEKTAR_OPERATOR_PUBKEY)
// so SyncPolicyFromDB rejects unsigned or forged operator_policies rows (separation of powers).
func (p *Policy) SetPinnedOperatorPub(pubHex string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.pinnedOperatorPub = strings.ToLower(strings.TrimSpace(pubHex))
	p.mu.Unlock()
}

// PinnedOperatorPub returns the configured external operator public key hex (empty if unpinned).
func (p *Policy) PinnedOperatorPub() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pinnedOperatorPub
}

// ActivePolicyHash returns the canonical PolicyHash of the newest verified active operator policy.
func (p *Policy) ActivePolicyHash() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activePolicyHash
}

// TamperedPoliciesCount returns the number of active DB policy rows rejected during the last sync.
func (p *Policy) TamperedPoliciesCount() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tamperedPolicies
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
		MaxSatsPerDeal:           50_000,
		MaxSatsPerDay:            500_000,
		ZeroConfMaxSats:          0, // fail-closed: 0-conf disabled until explicitly approved by operator policy
		ZeroConfMaxAggregateSats: 0,
		MaxUnconfirmedAncestors:  15,
		MaxHubSubmitFeeSats:      25,
		MaxHubBboxFeeSats:        10,
		MaxHubEgressTopupSats:    100,
		MaxHubFeeSatsPerDay:      500,
		EscrowMode:               EscrowModeAuto,
		EscrowMinSats:            DefaultEscrowMinSat,
		BlockedSellers:           make(map[string]bool),
		PreferredSellers:         make(map[string]bool),
	}
}

// SetHubFeeLimits updates the operator's Hub fee caps (fail-closed against gouging hubs).
func (p *Policy) SetHubFeeLimits(submitMax, bboxMax, egressTopupMax, dailyMax uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.MaxHubSubmitFeeSats = submitMax
	p.MaxHubBboxFeeSats = bboxMax
	p.MaxHubEgressTopupSats = egressTopupMax
	p.MaxHubFeeSatsPerDay = dailyMax
	p.mu.Unlock()
}

// CanPayHubFee enforces operator Hub fee caps fail-closed before an agent or Leaf Hub auto-pays
// a Hub 402 challenge or metered egress credit top-up.
func (p *Policy) CanPayHubFee(action string, sats uint64) error {
	if sats == 0 {
		return nil
	}
	if p == nil {
		return errors.New("market: no policy configured for hub fee payment")
	}
	p.mu.RLock()
	db := p.db
	kill := p.KillSwitch
	maxSubmit := p.MaxHubSubmitFeeSats
	maxBbox := p.MaxHubBboxFeeSats
	maxTopup := p.MaxHubEgressTopupSats
	maxDaily := p.MaxHubFeeSatsPerDay
	spent := p.DailyHubFeesSpentSats
	p.mu.RUnlock()
	if db != nil {
		if v, err := db.DailySpend("market_hub_fees_daily"); err == nil {
			spent = v
		}
	}
	if kill {
		return errors.New("market: operator kill switch is engaged")
	}
	var perActionCap uint64
	switch {
	case strings.HasPrefix(action, "submit:"):
		perActionCap = maxSubmit
	case action == "bbox:send":
		perActionCap = maxBbox
	case action == "egress:topup":
		perActionCap = maxTopup
	case strings.HasPrefix(action, "lookup:"):
		perActionCap = maxSubmit
	default:
		perActionCap = maxSubmit
	}
	if perActionCap == 0 || sats > perActionCap {
		return fmt.Errorf("market: hub fee %d sats for %s exceeds policy cap %d sats", sats, action, perActionCap)
	}
	if maxDaily == 0 || spent+sats > maxDaily {
		return fmt.Errorf("market: hub fee %d sats would exceed daily hub fee cap %d sats (spent=%d)", sats, maxDaily, spent)
	}
	return nil
}

// RecordHubFeeSpend records a completed Hub fee payment against the daily hub fee budget.
func (p *Policy) RecordHubFeeSpend(sats uint64) {
	if p == nil || sats == 0 {
		return
	}
	p.mu.Lock()
	p.DailyHubFeesSpentSats += sats
	db := p.db
	p.mu.Unlock()
	if db != nil {
		if next, err := db.AddDailySpend("market_hub_fees_daily", sats); err == nil {
			p.mu.Lock()
			p.DailyHubFeesSpentSats = next
			p.mu.Unlock()
		}
	}
}

// SetEscrowMode configures the buyer's DisclosureSale on-chain escrow policy ("auto" | "always" | "off").
func (p *Policy) SetEscrowMode(mode string, minSats uint64) {
	if p == nil {
		return
	}
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = EscrowModeAuto
	}
	p.mu.Lock()
	p.EscrowMode = m
	if minSats > 0 {
		p.EscrowMinSats = minSats
	}
	p.mu.Unlock()
}

// ShouldUseEscrow reports whether a purchase of priceSats should settle via trustless
// DisclosureSale on-chain atomic escrow instead of direct BRC-29 payment.
func (p *Policy) ShouldUseEscrow(priceSats uint64) bool {
	if priceSats == 0 {
		return false
	}
	if p == nil {
		return priceSats >= DefaultEscrowMinSat
	}
	p.mu.RLock()
	mode := strings.ToLower(strings.TrimSpace(p.EscrowMode))
	minSats := p.EscrowMinSats
	zeroConfMax := p.ZeroConfMaxSats
	p.mu.RUnlock()
	if mode == "" {
		mode = EscrowModeAuto
	}
	if minSats == 0 {
		minSats = DefaultEscrowMinSat
	}
	switch mode {
	case EscrowModeAlways:
		return true
	case EscrowModeOff:
		return false
	default: // auto
		if priceSats >= minSats {
			return true
		}
		if zeroConfMax > 0 && priceSats > zeroConfMax {
			return true
		}
		return false
	}
}

// ZeroConfLimits returns the enforced per-deal cap, aggregate unconfirmed cap, and max unconfirmed ancestor depth.
func (p *Policy) ZeroConfLimits() (perDeal uint64, aggregate uint64, maxAncestors int) {
	if p == nil {
		return 0, 0, 15
	}
	p.mu.RLock()
	perDeal = p.ZeroConfMaxSats
	aggregate = p.ZeroConfMaxAggregateSats
	maxAncestors = p.MaxUnconfirmedAncestors
	p.mu.RUnlock()
	if aggregate == 0 && perDeal > 0 {
		aggregate = perDeal * 4
	}
	if maxAncestors <= 0 {
		maxAncestors = 15
	}
	return perDeal, aggregate, maxAncestors
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
