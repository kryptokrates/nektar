package reasoning

import (
	"encoding/hex"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// TestDelegationAndReceiptHardening verifies F-12 (framed delegation v2, framed AORG hashes)
// and F-32 (signed Receipt under TagReceiptSig).
func TestDelegationAndReceiptHardening(t *testing.T) {
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	del, err := makeTestDelegation(key, "sub-a", "answer-bc")
	if err != nil {
		t.Fatalf("makeTestDelegation: %v", err)
	}
	if del.Version != 2 {
		t.Fatalf("expected Delegation Version=2, got %d", del.Version)
	}
	rcp := Receipt{
		Answer:       "answer-bc",
		FragmentRefs: []FragmentRef{{Shard: "s1", Seq: 1}},
		CheckResult:  "verified",
		Timestamp:    1700000000,
		TruthNote:    TruthNote,
		Delegations:  []Delegation{del},
	}
	if err := VerifyDelegations(rcp); err != nil {
		t.Fatalf("VerifyDelegations valid failed: %v", err)
	}

	// F-12 boundary-shift attack: shifting characters between Decision and Answer MUST fail.
	shifted := rcp
	shifted.Delegations = []Delegation{{
		Version:   2,
		Delegator: del.Delegator,
		Decision:  "sub-aanswer-",
		Signature: del.Signature,
	}}
	shifted.Answer = "bc"
	if err := VerifyDelegations(shifted); err == nil {
		t.Fatal("F-12: boundary-shifted delegation verified!")
	}

	// F-32: Receipt signing and tamper rejection.
	rcp.IssuerPub = hex.EncodeToString(key.PubKey().Compressed())
	digest, err := ReceiptSignDigest(rcp)
	if err != nil {
		t.Fatalf("ReceiptSignDigest: %v", err)
	}
	sig, err := key.Sign(digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	rcp.IssuerSig = hex.EncodeToString(sig.Serialize())
	if err := VerifyReceiptSignature(rcp); err != nil {
		t.Fatalf("VerifyReceiptSignature valid failed: %v", err)
	}
	tampered := rcp
	tampered.Answer = "forged answer"
	if err := VerifyReceiptSignature(tampered); err == nil {
		t.Fatal("F-32: tampered receipt passed VerifyReceiptSignature!")
	}
	unsigned := rcp
	unsigned.IssuerPub = ""
	unsigned.IssuerSig = ""
	if err := VerifyReceiptSignature(unsigned); err == nil {
		t.Fatal("F7: unsigned receipt passed VerifyReceiptSignature!")
	}
}
