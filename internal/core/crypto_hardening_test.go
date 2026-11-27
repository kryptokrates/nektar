package core

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// TestWycheproofECVerifyHardening verifies F-30 strict pubkey & low-S DER validation.
func TestWycheproofECVerifyHardening(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	pub := priv.PubKey()
	compHex := hex.EncodeToString(pub.Compressed())

	// Valid canonical lowercase 33-byte compressed pubkey passes.
	if _, raw, err := ParseStrictCompressedPubKey(compHex); err != nil || len(raw) != 33 {
		t.Fatalf("valid compressed pubkey rejected: err=%v len=%d", err, len(raw))
	}
	// Reject uppercase hex.
	if _, _, err := ParseStrictCompressedPubKey(strings.ToUpper(compHex)); err == nil {
		t.Fatal("expected uppercase pubkey hex rejection")
	}
	// Reject 65-byte uncompressed pubkey (0x04...).
	if _, _, err := ParseStrictCompressedPubKey(hex.EncodeToString(pub.Uncompressed())); err == nil {
		t.Fatal("expected uncompressed 65B pubkey rejection")
	}
	// Reject hybrid prefix (0x06/0x07).
	hybrid := append([]byte(nil), pub.Uncompressed()...)
	hybrid[0] = 0x06
	if _, _, err := ParseStrictCompressedPubKey(hex.EncodeToString(hybrid)); err == nil {
		t.Fatal("expected hybrid pubkey rejection")
	}
	// Reject off-curve / malformed 33-byte point (x = p + 1 > field prime).
	offCurve := "02" + strings.Repeat("ff", 32)
	if _, _, err := ParseStrictCompressedPubKey(offCurve); err == nil {
		t.Fatal("expected off-curve pubkey rejection")
	}

	// Sign a message and verify canonical low-S passes.
	msg := DSHA256([]byte("wycheproof-test-msg"))
	sig, err := priv.Sign(msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sigBytes := sig.Serialize()
	sigHex := hex.EncodeToString(sigBytes)
	if _, err := ParseStrictLowSDER(sigHex); err != nil {
		t.Fatalf("valid canonical low-S DER rejected: %v", err)
	}

	// Reject trailing garbage byte after valid DER (CVE-class malleability).
	if _, err := ParseStrictLowSDER(sigHex + "00"); err == nil {
		t.Fatal("expected trailing DER byte rejection")
	}

	// Construct raw malleable high-S DER bytes directly (ec.Signature.Serialize auto-normalizes to low-S):
	// s' = N - s (> N/2) and confirm ParseStrictLowSDER rejects it.
	curveN := ec.S256().N
	highS := new(big.Int).Sub(curveN, sig.S)
	derInt := func(n *big.Int) []byte {
		b := n.Bytes()
		if len(b) == 0 || b[0]&0x80 != 0 {
			b = append([]byte{0x00}, b...)
		}
		return append([]byte{0x02, byte(len(b))}, b...)
	}
	rDER := derInt(sig.R)
	sDER := derInt(highS)
	highSDER := append([]byte{0x30, byte(len(rDER) + len(sDER))}, append(rDER, sDER...)...)
	if _, err := ParseStrictLowSDER(hex.EncodeToString(highSDER)); err == nil {
		t.Fatal("expected malleable high-S DER signature rejection")
	}

	// Reject zero R / zero S DER encoding.
	zeroRS := "3006020100020100"
	if _, err := ParseStrictLowSDER(zeroRS); err == nil {
		t.Fatal("expected zero R/S DER signature rejection")
	}
}

// TestHDPV2CollisionResistance verifies F-10 framed H(d|p) prevents boundary collisions.
func TestHDPV2CollisionResistance(t *testing.T) {
	p1 := Provenance{Source: "cd", Time: 1700000000, ModelID: "m"}
	p2 := Provenance{Source: "bcd", Time: 1700000000, ModelID: "m"}
	h1 := ComputeHDPV2([]byte("ab"), p1)
	h2 := ComputeHDPV2([]byte("a"), p2)
	if bytes.Equal(h1, h2) {
		t.Fatal("F-10: ComputeHDPV2 collided across data/provenance boundary")
	}
	if !VerifyHDP([]byte("ab"), SerializeProvenance(p1), h1) {
		t.Fatal("VerifyHDP failed on valid v2 HDP")
	}
	// Legacy unframed v1 HDP (nektar:prov:v1) MUST be rejected now that framed v2 is the sole format.
	legacyUnframed := SHA256(Concat([]byte("nektar:prov:v1"), []byte("ab"), SerializeProvenance(p1)))
	if VerifyHDP([]byte("ab"), SerializeProvenance(p1), legacyUnframed) {
		t.Fatal("VerifyHDP must reject legacy unframed v1 HDP")
	}
	if VerifyHDP([]byte("tampered"), SerializeProvenance(p1), h1) {
		t.Fatal("VerifyHDP accepted tampered data")
	}
}

// TestCanonicalJSONRFC8785 verifies F-31 unescaped <, >, & and number normalization.
func TestCanonicalJSONRFC8785(t *testing.T) {
	b, err := CanonicalJSON(map[string]any{
		"z": "a < b && c > d",
		"a": 1.0,
		"b": -0.0,
		"c": 1e2,
	})
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := `{"a":1,"b":0,"c":100,"z":"a < b && c > d"}`
	if string(b) != want {
		t.Fatalf("CanonicalJSON mismatch:\n got: %s\nwant: %s", string(b), want)
	}
}

// TestMerkleHardening verifies F-07 strict 32-byte lengths and F-08 non-looping HeightForLeafCount.
func TestMerkleHardening(t *testing.T) {
	// F-08: previously looped forever when n > 1<<62 due to int64 wrap.
	if h := HeightForLeafCount((1 << 62) + 1); h != 63 {
		t.Fatalf("HeightForLeafCount(2^62+1) = %d, want 63", h)
	}
	if h := HeightForLeafCount(0); h != 0 {
		t.Fatalf("HeightForLeafCount(0) = %d, want 0", h)
	}
	// F-07: reject non-32B leaf/sibling or negative index.
	root := SHA256([]byte("root"))
	if VerifyProof([]byte("short"), &MerkleProof{Index: 0, Siblings: nil}, root) {
		t.Fatal("VerifyProof accepted non-32B leaf")
	}
	if VerifyProof(root, &MerkleProof{Index: -1, Siblings: nil}, root) {
		t.Fatal("VerifyProof accepted negative index")
	}
	if VerifyProof(root, &MerkleProof{Index: 0, Siblings: [][]byte{[]byte("short-sib")}}, root) {
		t.Fatal("VerifyProof accepted non-32B sibling")
	}
}

// FuzzParseProvenance exercises binary provenance decoding against malformed/overflow lengths (F-35).
func FuzzParseProvenance(f *testing.F) {
	valid := SerializeProvenance(Provenance{
		Source:  "agent",
		Time:    1700000000,
		ModelID: "test",
		Modality: &Modality{
			Kind:      "epistemic",
			Qualifier: "x < y & z",
		},
	})
	f.Add(valid)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x01})
	f.Add([]byte{0x00, 0x00, 0x00, 0x80})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseProvenance(data)
		if err != nil {
			return
		}
		// Round-trip must never panic.
		_ = SerializeProvenance(p)
	})
}
