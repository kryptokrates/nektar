package policy

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	cedar "github.com/cedar-policy/cedar-go"

	"github.com/kryptokrates/nektar/internal/core"
)

const maxCachedPolicySets = 64

var (
	policyCacheMu sync.RWMutex
	policyCache   = make(map[string]*cedar.PolicySet, 8)
)

func parsePolicySetCached(policyText string) (*cedar.PolicySet, error) {
	policyCacheMu.RLock()
	if ps, ok := policyCache[policyText]; ok {
		policyCacheMu.RUnlock()
		return ps, nil
	}
	policyCacheMu.RUnlock()

	ps, err := cedar.NewPolicySetFromBytes("policy.cedar", []byte(policyText))
	if err != nil {
		return nil, fmt.Errorf("policy: parse cedar: %w", err)
	}
	policyCacheMu.Lock()
	if len(policyCache) >= maxCachedPolicySets {
		policyCache = make(map[string]*cedar.PolicySet, 8)
	}
	policyCache[policyText] = ps
	policyCacheMu.Unlock()
	return ps, nil
}

// DefaultPolicySet grants agents access to fragments at or below their proven clearance level,
// and enforces fail-closed distillation on Action::"sell" unless explicit operator allowRawOverride is set.
const DefaultPolicySet = `
permit (
  principal,
  action in [Action::"read", Action::"write", Action::"disclose", Action::"rotate", Action::"revoke"],
  resource
) when {
  principal.level >= resource.level
};
permit (
  principal,
  action == Action::"sell",
  resource
) when {
  principal.level >= resource.level &&
  (resource.distilled || (context has allowRawOverride && context.allowRawOverride == true))
};
forbid (
  principal,
  action == Action::"sell",
  resource
) when {
  !resource.distilled && !(context has allowRawOverride && context.allowRawOverride == true)
};
`

type Principal struct {
	Type  string `json:"type"` // Agent | User
	ID    string `json:"id"`
	Level int64  `json:"level"`
}

// JevMatcherPrincipal is the dedicated Level-0 Cedar principal evaluated before any
// fragment preview may leave the node to the external Jev System One matching API.
// Operators can write targeted `forbid (principal == Agent::"jev-matcher", ...)` rules
// to keep specific shards/classes strictly local without disabling local market selling.
var JevMatcherPrincipal = Principal{
	Type:  "Agent",
	ID:    "jev-matcher",
	Level: 0,
}

// MatchContext carries deterministic integer-bps match signals into Cedar Request.Context
// for optional operator-authored post-match market policies (monotonic tightening only).
type MatchContext struct {
	Role          string `json:"role"` // buyer | seller
	RelevanceBP   int64  `json:"relevanceBp"`
	ConfidenceBP  int64  `json:"confidenceBp"`
	AbuseProbBP   int64  `json:"abuseProbBp"`
	PolicyFitBP   int64  `json:"policyFitBp"`
	PriceSats     int64  `json:"priceSats"`
	MaxSats       int64  `json:"maxSats"`
	SettledDeals  int64  `json:"settledDeals"`
	FailedDeals   int64  `json:"failedDeals"`
	DomainTrustBP int64  `json:"domainTrustBp"`
	AccBP         int64  `json:"accBp"`
	DispBP        int64  `json:"dispBp"`
	EigenTrustBP  int64  `json:"eigenTrustBp"`
	IsHub         bool   `json:"isHub"`
	Direct        bool   `json:"direct"`
	RiskFlag      string `json:"riskFlag"`
	Domain        string `json:"domain"`
}

type Resource struct {
	Type      string `json:"type"` // Fragment
	ID        string `json:"id"`
	Level     int64  `json:"level"`
	Shard     string `json:"shard"`
	Class     string `json:"class"`
	Distilled bool   `json:"distilled,omitempty"`
}

type PolicyCommitment struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Hash string `json:"hash"`
}

type Decision struct {
	Allow   bool     `json:"allow"`
	Reasons []string `json:"reasons,omitempty"`
}

type DecisionReceipt struct {
	Request       string `json:"request"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
	Action        string `json:"action,omitempty"`
	ResourceType  string `json:"resourceType,omitempty"`
	ResourceShard string `json:"resourceShard,omitempty"`
	ResourceID    string `json:"resourceId,omitempty"`
	PolicySetHash string `json:"policySetHash"`
	Decision      string `json:"decision"` // Allow | Deny
	Time          int64  `json:"time"`
	IssuerPub     string `json:"issuerPub,omitempty"`
	Signature     string `json:"signature,omitempty"`
}

// Canonicalize parses and canonically formats a Cedar policy document using native cedar-go
// and computes its domain-separated SHA-256d commitment hash.
func Canonicalize(name, text string) (*PolicyCommitment, error) {
	ps, err := cedar.NewPolicySetFromBytes(name+".cedar", []byte(text))
	if err != nil {
		return nil, fmt.Errorf("policy: parse cedar: %w", err)
	}
	canon := string(ps.MarshalCedar())
	h := hex.EncodeToString(core.DSHA256(core.Concat([]byte("nektar/policyset/v1"), []byte(canon))))
	return &PolicyCommitment{Name: name, Text: canon, Hash: h}, nil
}

// Authorize evaluates a Cedar policy set in native Go (zero WASM overhead).
func Authorize(policyText string, principal Principal, action string, resource Resource) (Decision, error) {
	return authorizeWithContext(policyText, principal, action, resource, cedar.NewRecord(nil))
}

// AuthorizeSell evaluates Action::"sell" under fail-closed Cedar rules enforcing resource.distilled == true
// unless explicit operator allowRawOverride is supplied.
func AuthorizeSell(policyText string, principal Principal, resource Resource, allowRawOverride bool) (Decision, error) {
	ctxRec := cedar.NewRecord(cedar.RecordMap{
		"allowRawOverride": cedar.Boolean(allowRawOverride),
	})
	return authorizeWithContext(policyText, principal, "sell", resource, ctxRec)
}

// AuthorizeMatch evaluates an operator-authored Cedar market policy with deterministic
// integer-bps MatchContext attributes (zero float non-determinism; fail-closed on diag.Errors).
func AuthorizeMatch(policyText string, principal Principal, action string, resource Resource, mctx MatchContext) (Decision, error) {
	ctxRec := cedar.NewRecord(cedar.RecordMap{
		"role":          cedar.String(mctx.Role),
		"relevanceBp":   cedar.Long(mctx.RelevanceBP),
		"confidenceBp":  cedar.Long(mctx.ConfidenceBP),
		"abuseProbBp":   cedar.Long(mctx.AbuseProbBP),
		"policyFitBp":   cedar.Long(mctx.PolicyFitBP),
		"priceSats":     cedar.Long(mctx.PriceSats),
		"maxSats":       cedar.Long(mctx.MaxSats),
		"settledDeals":  cedar.Long(mctx.SettledDeals),
		"failedDeals":   cedar.Long(mctx.FailedDeals),
		"domainTrustBp": cedar.Long(mctx.DomainTrustBP),
		"accBp":         cedar.Long(mctx.AccBP),
		"dispBp":        cedar.Long(mctx.DispBP),
		"eigenTrustBp":  cedar.Long(mctx.EigenTrustBP),
		"isHub":         cedar.Boolean(mctx.IsHub),
		"direct":        cedar.Boolean(mctx.Direct),
		"riskFlag":      cedar.String(mctx.RiskFlag),
		"domain":        cedar.String(mctx.Domain),
	})
	return authorizeWithContext(policyText, principal, action, resource, ctxRec)
}

func authorizeWithContext(policyText string, principal Principal, action string, resource Resource, reqCtx cedar.Record) (Decision, error) {
	ps, err := parsePolicySetCached(policyText)
	if err != nil {
		return Decision{}, err
	}
	pType := principal.Type
	if pType == "" {
		pType = "Agent"
	}
	rType := resource.Type
	if rType == "" {
		rType = "Fragment"
	}
	pUID := cedar.NewEntityUID(cedar.EntityType(pType), cedar.String(principal.ID))
	aUID := cedar.NewEntityUID(cedar.EntityType("Action"), cedar.String(action))
	rUID := cedar.NewEntityUID(cedar.EntityType(rType), cedar.String(resource.ID))

	entities := cedar.EntityMap{
		pUID: cedar.Entity{
			UID: pUID,
			Attributes: cedar.NewRecord(cedar.RecordMap{
				"level": cedar.Long(principal.Level),
			}),
		},
		rUID: cedar.Entity{
			UID: rUID,
			Attributes: cedar.NewRecord(cedar.RecordMap{
				"level":     cedar.Long(resource.Level),
				"shard":     cedar.String(resource.Shard),
				"class":     cedar.String(resource.Class),
				"distilled": cedar.Boolean(resource.Distilled),
			}),
		},
	}

	dec, diag := ps.IsAuthorized(entities, cedar.Request{
		Principal: pUID,
		Action:    aUID,
		Resource:  rUID,
		Context:   reqCtx,
	})
	// Fail closed on any policy evaluation error (e.g. missing attribute, type mismatch, overflow).
	if len(diag.Errors) > 0 {
		return Decision{Allow: false}, fmt.Errorf("policy: cedar evaluation error (%d): %s", len(diag.Errors), diag.Errors[0].Message)
	}
	reasons := make([]string, 0, len(diag.Reasons))
	for _, r := range diag.Reasons {
		reasons = append(reasons, string(r.PolicyID))
	}
	return Decision{
		Allow:   dec == cedar.Allow,
		Reasons: reasons,
	}, nil
}

// MakeReceipt binds an authorization decision to an anchored PolicyCommitment with
// unambiguous structured principal/action/resource fields (F-32), normalizing default
// entity types ("Agent" / "Fragment") identically to authorizeWithContext.
func MakeReceipt(principal Principal, action string, resource Resource, pc *PolicyCommitment, allow bool) DecisionReceipt {
	dec := "Deny"
	if allow {
		dec = "Allow"
	}
	pType := principal.Type
	if pType == "" {
		pType = "Agent"
	}
	rType := resource.Type
	if rType == "" {
		rType = "Fragment"
	}
	return DecisionReceipt{
		Request:       fmt.Sprintf(`%s::"%s"::%s::"%s/%s"`, pType, principal.ID, action, resource.Shard, resource.ID),
		PrincipalType: pType,
		PrincipalID:   principal.ID,
		Action:        action,
		ResourceType:  rType,
		ResourceShard: resource.Shard,
		ResourceID:    resource.ID,
		PolicySetHash: pc.Hash,
		Decision:      dec,
		Time:          time.Now().Unix(),
	}
}

// DecisionReceiptDigest computes the domain-separated, length-framed digest over a DecisionReceipt's structured fields.
func DecisionReceiptDigest(dr DecisionReceipt) []byte {
	return core.DSHA256(core.Concat(
		[]byte("nektar:policy-decision:v2"),
		core.Field([]byte(dr.PrincipalType)),
		core.Field([]byte(dr.PrincipalID)),
		core.Field([]byte(dr.Action)),
		core.Field([]byte(dr.ResourceType)),
		core.Field([]byte(dr.ResourceShard)),
		core.Field([]byte(dr.ResourceID)),
		core.Field([]byte(dr.PolicySetHash)),
		core.Field([]byte(dr.Decision)),
		core.U64LE(uint64(dr.Time)),
	))
}

// SignDecisionReceipt signs dr using issuerPubHex and a digest signer callback (preserving BRC-43 key custody).
func SignDecisionReceipt(issuerPubHex string, signDigest func(digest []byte) ([]byte, error), dr *DecisionReceipt) error {
	if issuerPubHex == "" || signDigest == nil || dr == nil {
		return errors.New("policy: missing issuerPub, signer, or decision receipt")
	}
	if _, _, err := core.ParseStrictCompressedPubKey(issuerPubHex); err != nil {
		return fmt.Errorf("policy: invalid issuerPub: %w", err)
	}
	dr.IssuerPub = issuerPubHex
	sigBytes, err := signDigest(DecisionReceiptDigest(*dr))
	if err != nil {
		return err
	}
	dr.Signature = hex.EncodeToString(sigBytes)
	return nil
}

// VerifyDecisionReceipt validates IssuerPub and Signature on a signed DecisionReceipt.
func VerifyDecisionReceipt(dr DecisionReceipt) error {
	if dr.IssuerPub == "" || dr.Signature == "" {
		return errors.New("policy: unsigned decision receipt")
	}
	pub, _, err := core.ParseStrictCompressedPubKey(dr.IssuerPub)
	if err != nil {
		return fmt.Errorf("policy: invalid issuerPub: %w", err)
	}
	sig, err := core.ParseStrictLowSDER(dr.Signature)
	if err != nil {
		return fmt.Errorf("policy: invalid signature: %w", err)
	}
	if !sig.Verify(DecisionReceiptDigest(dr), pub) {
		return errors.New("policy: invalid decision receipt signature")
	}
	return nil
}

const maxLevelIsolationSpan = 10000

// ProveLevelIsolation performs bounded exhaustive verification confirming no principal
// below fragmentLevel can read or disclose a fragment at fragmentLevel.
func ProveLevelIsolation(policyText string, minLevel, fragmentLevel int64) (ok bool, checked, violations int, err error) {
	if minLevel >= fragmentLevel {
		return false, 0, 0, fmt.Errorf("policy: invalid level isolation range [%d, %d)", minLevel, fragmentLevel)
	}
	if fragmentLevel-minLevel > maxLevelIsolationSpan {
		return false, 0, 0, fmt.Errorf("policy: level isolation span %d exceeds max %d", fragmentLevel-minLevel, maxLevelIsolationSpan)
	}
	ps, err := parsePolicySetCached(policyText)
	if err != nil {
		return false, 0, 0, err
	}
	pUID := cedar.NewEntityUID(cedar.EntityType("Agent"), cedar.String("probe"))
	rUID := cedar.NewEntityUID(cedar.EntityType("Fragment"), cedar.String("f"))
	rEnt := cedar.Entity{
		UID: rUID,
		Attributes: cedar.NewRecord(cedar.RecordMap{
			"level":     cedar.Long(fragmentLevel),
			"shard":     cedar.String("s"),
			"class":     cedar.String("c"),
			"distilled": cedar.Boolean(true),
		}),
	}
	for lvl := minLevel; lvl < fragmentLevel; lvl++ {
		entities := cedar.EntityMap{
			pUID: cedar.Entity{
				UID: pUID,
				Attributes: cedar.NewRecord(cedar.RecordMap{
					"level": cedar.Long(lvl),
				}),
			},
			rUID: rEnt,
		}
		for _, act := range []string{"read", "disclose"} {
			aUID := cedar.NewEntityUID(cedar.EntityType("Action"), cedar.String(act))
			dec, diag := ps.IsAuthorized(entities, cedar.Request{
				Principal: pUID,
				Action:    aUID,
				Resource:  rUID,
				Context:   cedar.NewRecord(nil),
			})
			if len(diag.Errors) > 0 {
				return false, checked, violations, fmt.Errorf("policy: cedar evaluation error at level %d: %s", lvl, diag.Errors[0].Message)
			}
			checked++
			if dec == cedar.Allow {
				violations++
			}
		}
	}
	return violations == 0, checked, violations, nil
}
