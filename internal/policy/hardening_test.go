package policy

import (
	"encoding/hex"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// TestDecisionReceiptSigningAndFraming verifies F-32 structured DecisionReceipt signing & tamper rejection.
func TestDecisionReceiptSigningAndFraming(t *testing.T) {
	pc, err := Canonicalize("default", DefaultPolicySet)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	dr := MakeReceipt(
		Principal{Type: "Agent", ID: "alice::read", Level: 2},
		"read",
		Resource{Type: "Fragment", ID: "1", Shard: "s1", Level: 1},
		pc,
		true,
	)
	err = SignDecisionReceipt(pubHex, func(digest []byte) ([]byte, error) {
		sig, sErr := priv.Sign(digest)
		if sErr != nil {
			return nil, sErr
		}
		return sig.Serialize(), nil
	}, &dr)
	if err != nil {
		t.Fatalf("SignDecisionReceipt: %v", err)
	}
	if err := VerifyDecisionReceipt(dr); err != nil {
		t.Fatalf("VerifyDecisionReceipt valid failed: %v", err)
	}
	// Shifting fields across principal/action boundary MUST fail verification.
	tampered := dr
	tampered.Decision = "Deny"
	if err := VerifyDecisionReceipt(tampered); err == nil {
		t.Fatal("F-32: tampered DecisionReceipt verified")
	}
	shifted := dr
	shifted.PrincipalID = "alice"
	shifted.Action = "read::read"
	if err := VerifyDecisionReceipt(shifted); err == nil {
		t.Fatal("F-32: field-shifted DecisionReceipt verified")
	}
}
