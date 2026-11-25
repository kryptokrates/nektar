package policy

import (
	"encoding/hex"
	"fmt"
	"time"

	cedar "github.com/cedar-policy/cedar-go"

	"github.com/kryptokrates/nektar/internal/core"
)

// DefaultPolicySet grants agents access to fragments at or below their proven clearance level.
const DefaultPolicySet = `
permit (
  principal,
  action in [Action::"read", Action::"write", Action::"disclose", Action::"rotate", Action::"revoke"],
  resource
) when {
  principal.level >= resource.level
};
`

type Principal struct {
	Type  string `json:"type"` // Agent | User
	ID    string `json:"id"`
	Level int64  `json:"level"`
}

type Resource struct {
	Type  string `json:"type"` // Fragment
	ID    string `json:"id"`
	Level int64  `json:"level"`
	Shard string `json:"shard"`
	Class string `json:"class"`
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
	PolicySetHash string `json:"policySetHash"`
	Decision      string `json:"decision"` // Allow | Deny
	Time          int64  `json:"time"`
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
	ps, err := cedar.NewPolicySetFromBytes("policy.cedar", []byte(policyText))
	if err != nil {
		return Decision{}, fmt.Errorf("policy: parse cedar: %w", err)
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
				"level": cedar.Long(resource.Level),
				"shard": cedar.String(resource.Shard),
				"class": cedar.String(resource.Class),
			}),
		},
	}

	dec, diag := ps.IsAuthorized(entities, cedar.Request{
		Principal: pUID,
		Action:    aUID,
		Resource:  rUID,
		Context:   cedar.NewRecord(nil),
	})
	reasons := make([]string, 0, len(diag.Reasons))
	for _, r := range diag.Reasons {
		reasons = append(reasons, string(r.PolicyID))
	}
	return Decision{
		Allow:   dec == cedar.Allow,
		Reasons: reasons,
	}, nil
}

// MakeReceipt binds an authorization decision to an anchored PolicyCommitment.
func MakeReceipt(principal Principal, action string, resource Resource, pc *PolicyCommitment, allow bool) DecisionReceipt {
	dec := "Deny"
	if allow {
		dec = "Allow"
	}
	return DecisionReceipt{
		Request:       fmt.Sprintf(`%s::"%s"::%s::"%s/%s"`, principal.Type, principal.ID, action, resource.Shard, resource.ID),
		PolicySetHash: pc.Hash,
		Decision:      dec,
		Time:          time.Now().Unix(),
	}
}

// ProveLevelIsolation performs bounded exhaustive verification confirming no principal
// below fragmentLevel can read or disclose a fragment at fragmentLevel.
func ProveLevelIsolation(policyText string, minLevel, fragmentLevel int64) (ok bool, checked, violations int, err error) {
	for lvl := minLevel; lvl < fragmentLevel; lvl++ {
		for _, act := range []string{"read", "disclose"} {
			d, aErr := Authorize(policyText, Principal{Type: "Agent", ID: "probe", Level: lvl}, act, Resource{
				Type: "Fragment", ID: "f", Level: fragmentLevel, Shard: "s", Class: "c",
			})
			if aErr != nil {
				return false, checked, violations, aErr
			}
			checked++
			if d.Allow {
				violations++
			}
		}
	}
	return violations == 0, checked, violations, nil
}
