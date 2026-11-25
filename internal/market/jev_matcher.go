package market

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/jev"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	// AdvisoryMatchNote preserves Nektar's Honesty Rule: open-weight System-1 outputs are probabilistic
	// match telemetry, never authoritative truth proofs or hard policy overrides.
	AdvisoryMatchNote = "Probabilistic System-1 match signal (self-hosted open-weight matcher); deterministic policy, Cedar, SpendGuard, and cryptographic proofs remain authoritative."

	maxSellerJevCandidates = 8
	maxBuyerJevOffers      = 12
	maxWantQueryChars      = 512
	maxOfferDescChars      = 280
	maxFragPreviewChars    = 600
)

// MatchAudit captures auditable, non-secret telemetry for a semantic offer/want matching decision.
type MatchAudit struct {
	Engine          string `json:"engine"`                    // "jev" | "deterministic"
	Model           string `json:"model,omitempty"`           // exact snapshot returned by Jev
	Mode            string `json:"mode,omitempty"`            // off | advisory | gated | strict
	Role            string `json:"role"`                      // buyer | seller
	Verdict         string `json:"verdict"`                   // allow | reject | escalate | fallback
	Reason          string `json:"reason,omitempty"`          // human-readable verdict explanation
	ChosenID        string `json:"chosenId,omitempty"`        // offerId or shard#seq
	BaseScoreBP     int64  `json:"baseScoreBp,omitempty"`     // deterministic base bps
	SemanticScoreBP int64  `json:"semanticScoreBp,omitempty"` // bounded Jev semantic bps delta
	RelevanceBP     int64  `json:"relevanceBp,omitempty"`     // 0..10000 quantized relevance
	ConfidenceBP    int64  `json:"confidenceBp,omitempty"`    // 0..10000 quantized confidence
	PolicyFitBP     int64  `json:"policyFitBp,omitempty"`     // 0..10000 semantic policy scope fit
	AbuseProbBP     int64  `json:"abuseProbBp,omitempty"`     // 0..10000 suspicion/injection risk
	RiskFlag        string `json:"riskFlag,omitempty"`        // none | prompt_injection | domain_spoof | pii_or_secret | spam_low_value | ambiguous_mismatch
	CedarAllowed    *bool  `json:"cedarAllowed,omitempty"`    // optional post-match Cedar policy verdict
	LatencyMs       int64  `json:"latencyMs,omitempty"`
	InputTokens     int    `json:"inputTokens,omitempty"`
	AdvisoryNote    string `json:"advisoryNote,omitempty"`
}

// MatcherMetrics reports atomic Jev matching counters for Prometheus /v1/metrics and `nektar doctor`.
type MatcherMetrics struct {
	Enabled      bool                `json:"enabled"`
	DefaultMode  string              `json:"defaultMode"`
	Requests     int64               `json:"requests"`
	Rejections   int64               `json:"rejections"`
	Escalations  int64               `json:"escalations"`
	Fallbacks    int64               `json:"fallbacks"`
	Errors       int64               `json:"errors"`
	InputTokens  int64               `json:"inputTokens"`
	AvgLatencyMs int64               `json:"avgLatencyMs"`
	ClientHealth *jev.HealthSnapshot `json:"clientHealth,omitempty"`
}

// SellerCandidate pairs an active SellListing with one Cedar-permitted Level-0 fragment candidate.
type SellerCandidate struct {
	ID           string
	Listing      store.SellListing
	Fragment     memory.RecalledFragment
	LexicalScore int
}

// SellerMatchDecision is the policy-bounded output of seller-side want matching.
type SellerMatchDecision struct {
	Approved bool
	Chosen   *SellerCandidate
	Audit    MatchAudit
}

// BuyerMatchDecision is the policy-bounded output of buyer-side offer evaluation & ranking.
type BuyerMatchDecision struct {
	Verdict string // allow | reject | escalate | fallback
	Reason  string
	Ranked  []ScoredOffer
	Chosen  *bstack.OfferAd
	Audit   MatchAudit
}

// Matcher abstracts semantic offer/want matching so Broker and Restate workflows stay testable
// and deterministic on replay.
type Matcher interface {
	DefaultMode() string
	MatchWantForSeller(ctx context.Context, want bstack.WantAd, candidates []SellerCandidate, pol *Policy) SellerMatchDecision
	MatchOffersForBuyer(ctx context.Context, want bstack.WantAd, baseRanked []ScoredOffer, reps map[string]store.SellerReputation, pol *Policy) BuyerMatchDecision
	MetricsSnapshot() MatcherMetrics
}

// JevMatcherOptions configures a JevMatcher wrapping a jev.Evaluator.
type JevMatcherOptions struct {
	DefaultMode        string // off | advisory | gated | strict (overridden by active operator policy when set)
	DefaultPrivacyMode string // metadata_only | disclosable_preview
}

// JevMatcher implements policy-bounded single-pass System One matching via Jev AI.
type JevMatcher struct {
	eval           jev.Evaluator
	defaultMode    string
	defaultPrivacy string

	requests       atomic.Int64
	rejections     atomic.Int64
	escalations    atomic.Int64
	fallbacks      atomic.Int64
	errors         atomic.Int64
	inputTokens    atomic.Int64
	totalLatencyMs atomic.Int64
}

// NewJevMatcher constructs a JevMatcher around eval (typically *jev.Client).
func NewJevMatcher(eval jev.Evaluator, opts JevMatcherOptions) *JevMatcher {
	mode := strings.ToLower(strings.TrimSpace(opts.DefaultMode))
	if mode == "" {
		mode = AIModeGated
	}
	priv := strings.ToLower(strings.TrimSpace(opts.DefaultPrivacyMode))
	if priv == "" {
		priv = PrivacyDisclosablePreview
	}
	return &JevMatcher{
		eval:           eval,
		defaultMode:    mode,
		defaultPrivacy: priv,
	}
}

func (m *JevMatcher) DefaultMode() string {
	if m == nil {
		return AIModeOff
	}
	return m.defaultMode
}

func (m *JevMatcher) effectivePolicy(pol *Policy) AIMatchingPolicy {
	var base AIMatchingPolicy
	if pol != nil {
		if p := pol.AIMatchingPolicy(); p != nil {
			base = *p
		}
	}
	if base.SellerPrivacyScope == "" {
		base.SellerPrivacyScope = m.defaultPrivacy
	}
	return base.Normalized(m.defaultMode)
}

func (m *JevMatcher) syncAllowedHosts(aiPol AIMatchingPolicy) {
	if setter, ok := m.eval.(interface{ SetAllowedHosts([]string) }); ok {
		setter.SetAllowedHosts(aiPol.AllowedHosts)
	}
}

// MetricsSnapshot returns atomic matcher counters and optional underlying Jev client health.
func (m *JevMatcher) MetricsSnapshot() MatcherMetrics {
	if m == nil {
		return MatcherMetrics{}
	}
	reqs := m.requests.Load()
	var avgMs int64
	if reqs > 0 {
		avgMs = m.totalLatencyMs.Load() / reqs
	}
	snap := MatcherMetrics{
		Enabled:      m.eval != nil && m.defaultMode != AIModeOff,
		DefaultMode:  m.defaultMode,
		Requests:     reqs,
		Rejections:   m.rejections.Load(),
		Escalations:  m.escalations.Load(),
		Fallbacks:    m.fallbacks.Load(),
		Errors:       m.errors.Load(),
		InputTokens:  m.inputTokens.Load(),
		AvgLatencyMs: avgMs,
	}
	if hc, ok := m.eval.(interface{ Health() jev.HealthSnapshot }); ok {
		h := hc.Health()
		snap.ClientHealth = &h
	}
	return snap
}

func buildScopeRubric(pol *Policy, aiPol AIMatchingPolicy) string {
	var parts []string
	if doms := pol.AllowedDomainsList(); len(doms) > 0 {
		parts = append(parts, "allowedDomains="+strings.Join(doms, ","))
	}
	if intent := strings.TrimSpace(aiPol.SemanticIntent); intent != "" {
		parts = append(parts, "intent="+sanitizeUntrusted(intent, 240))
	}
	if len(aiPol.ProhibitedTopics) > 0 {
		parts = append(parts, "prohibited="+strings.Join(aiPol.ProhibitedTopics, ","))
	}
	if len(parts) == 0 {
		return "general knowledge matching declared domains"
	}
	return strings.Join(parts, "; ")
}

func sanitizeUntrusted(s string, maxChars int) string {
	var b strings.Builder
	b.Grow(min(len(s), maxChars))
	lastSpace := false
	written := 0
	for _, r := range s {
		if written >= maxChars {
			break
		}
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r) {
			if !lastSpace && written > 0 {
				b.WriteByte(' ')
				written++
				lastSpace = true
			}
			continue
		}
		if r < 0x20 || r == 0x7f || !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
		written++
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

func quantizeBP(ratio float64) int64 {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return 0
	}
	if ratio >= 1 {
		return 10000
	}
	return int64(math.Round(ratio * 10000))
}

func answerConfidenceBP(ans jev.Answer) int64 {
	if c, ok := ans.ConfidenceValue(); ok {
		return quantizeBP(c)
	}
	return 10000
}

func combinedConfidenceBP(a, b jev.Answer) int64 {
	ca := answerConfidenceBP(a)
	cb := answerConfidenceBP(b)
	if ca < cb {
		return ca
	}
	return cb
}

func riskAbuseProbBP(ans jev.Answer) int64 {
	if pNone, ok := ans.Probabilities["none"]; ok && !math.IsNaN(pNone) && !math.IsInf(pNone, 0) {
		return quantizeBP(1.0 - pNone)
	}
	if ans.Choice != "" && ans.Choice != "none" {
		return 10000
	}
	return 0
}

func extractQueryTokens(query string) []string {
	var tokens []string
	for _, raw := range strings.Fields(strings.ToLower(query)) {
		tok := strings.Trim(raw, ".,?!:;\"'()[]{}")
		if len(tok) >= 3 {
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

func lexicalFragmentScore(text string, tokens []string) int {
	lower := strings.ToLower(text)
	score := 0
	for _, tok := range tokens {
		if strings.Contains(lower, tok) {
			score++
		}
	}
	return score
}
