package bstack

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// TestBboxE2EHKDFAndAADFraming verifies F-29 (HKDF-SHA256 v2, framed AAD collision resistance, v1 compat).
func TestBboxE2EHKDFAndAADFraming(t *testing.T) {
	recPriv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	recPubHex := hex.EncodeToString(recPriv.PubKey().Compressed())

	// F-29 AAD delimiter collision resistance: shifting "|" across fields MUST yield distinct AADs.
	aad1 := BboxAAD("off", "sender", recPubHex, "deal|a", "kind")
	aad2 := BboxAAD("off", "sender", recPubHex, "deal", "a|kind")
	if bytes.Equal(aad1, aad2) {
		t.Fatal("F-29: BboxAAD collided across delimiter shift")
	}

	plaintext := []byte(`{"contentKeyHex":"deadbeef"}`)
	sealed, err := SealBboxBody(recPubHex, aad1, plaintext)
	if err != nil {
		t.Fatalf("SealBboxBody: %v", err)
	}
	var env BboxSealedBody
	if err := json.Unmarshal(sealed, &env); err != nil {
		t.Fatalf("unmarshal sealed: %v", err)
	}
	if env.V != 2 {
		t.Fatalf("expected BboxSealedBody V=2 (HKDF-SHA256), got V=%d", env.V)
	}

	// Valid decrypt succeeds.
	got, err := OpenBboxBody(recPriv, aad1, sealed)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("OpenBboxBody v2 failed: err=%v got=%q", err, got)
	}
	// Wrong AAD (shifted delimiter) MUST fail authentication.
	if _, err := OpenBboxBody(recPriv, aad2, sealed); err == nil {
		t.Fatal("OpenBboxBody accepted mismatched AAD")
	}
	// Legacy V=1 envelope still decrypts for backward compatibility.
	ephPriv, _ := ec.NewPrivateKey()
	shared, _ := ephPriv.DeriveSharedSecret(recPriv.PubKey())
	aadV1 := BboxAADv1("off", "sender", recPubHex, "deal1", "kind")
	keyV1 := deriveBboxAEADKeyV1(shared.Compressed(), ephPriv.PubKey().Compressed(), recPriv.PubKey().Compressed(), aadV1)
	blk, _ := aes.NewCipher(keyV1[:])
	gcm, _ := cipher.NewGCM(blk)
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	ct := gcm.Seal(nil, nonce[:], plaintext, aadV1)
	v1Raw, _ := json.Marshal(BboxSealedBody{
		V:      1,
		EphPub: hex.EncodeToString(ephPriv.PubKey().Compressed()),
		Nonce:  hex.EncodeToString(nonce[:]),
		CT:     hex.EncodeToString(ct),
	})
	gotV1, err := OpenBboxBody(recPriv, aadV1, v1Raw)
	if err != nil || !bytes.Equal(gotV1, plaintext) {
		t.Fatalf("OpenBboxBody v1 compat failed: err=%v", err)
	}
}

// TestArtifactDomainSeparation verifies F-15 cross-type signature replay protection.
func TestArtifactDomainSeparation(t *testing.T) {
	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	want := WantAd{WantID: "w1", Pub: pubHex, Net: "testnet", Domains: []string{"test"}, Query: "q", MaxSats: 100, Exp: 9999999999999}
	sig, err := SignArtifact(priv, want)
	if err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	if err := VerifyArtifact(pubHex, sig, want); err != nil {
		t.Fatalf("VerifyArtifact valid WantAd failed: %v", err)
	}
	// Replaying the exact same JSON fields as a generic map (different domain tag) MUST fail!
	asMap := map[string]any{
		"wantId": "w1", "pub": pubHex, "net": "testnet",
		"domains": []string{"test"}, "query": "q", "maxSats": 100, "revision": 0, "exp": 9999999999999,
	}
	if err := VerifyArtifact(pubHex, sig, asMap); err == nil {
		t.Fatal("F-15: WantAd signature verified under generic artifact domain!")
	}
}

// TestBorgGroupRevocationAndRedaction verifies F-14 epoch secrecy & GetGroup redaction.
func TestBorgGroupRevocationAndRedaction(t *testing.T) {
	mgr := NewGroupRegistry()
	owner := "02" + hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32))
	member := "02" + hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32))
	if _, err := mgr.CreateGroup("g1", "sec", owner); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := mgr.AddMember("g1", member, "member"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if _, err := mgr.EpochKey("g1", member, 1); err != nil {
		t.Fatalf("active member should access epoch 1: %v", err)
	}
	if _, err := mgr.RevokeMember("g1", member); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	// Revoked member MUST NOT read epoch 2 OR historical epoch 1.
	if _, err := mgr.EpochKey("g1", member, 2); err == nil {
		t.Fatal("F-14: revoked member accessed new epoch 2")
	}
	if _, err := mgr.EpochKey("g1", member, 1); err == nil {
		t.Fatal("F-14: revoked member accessed historical epoch 1")
	}
	// GetGroup MUST redact Epochs map.
	g, ok := mgr.GetGroup("g1")
	if !ok || g.Epochs != nil {
		t.Fatalf("F-14: GetGroup leaked epoch keys: epochs=%v", g.Epochs)
	}
}

// FuzzOpenBboxBody exercises E2E bbox envelope decoding & decryption against arbitrary payloads.
func FuzzOpenBboxBody(f *testing.F) {
	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	aad := BboxAAD("off", pubHex, pubHex, "deal", "kind")
	valid, _ := SealBboxBody(pubHex, aad, []byte("secret"))
	f.Add([]byte(valid), aad)
	f.Add([]byte(`{"v":2,"ephPub":"04","nonce":"00","ct":"00"}`), aad)
	f.Add([]byte(`not json`), []byte{})
	f.Fuzz(func(t *testing.T, raw, fuzzAAD []byte) {
		_, _ = OpenBboxBody(priv, fuzzAAD, json.RawMessage(raw))
	})
}
