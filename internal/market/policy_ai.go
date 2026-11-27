package market

import (
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/policy"
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
