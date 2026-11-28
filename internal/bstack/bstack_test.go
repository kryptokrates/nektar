package bstack

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func TestMarketWireBorgVaultAndChirp(t *testing.T) {
	// 1. Canonical CBOR nkt1 market payload roundtrip + signing.
	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	want := WantAd{
		WantID:   "w-1",
		Pub:      pubHex,
		Net:      "testnet",
		Domains:  []string{"finance", "bsv"},
		Query:    "latest BSV chronicle opcode fees",
		MaxSats:  1000,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, err := SignArtifact(priv, want)
	if err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	if err := VerifyArtifact(pubHex, sig, want); err != nil {
		t.Fatalf("VerifyArtifact: %v", err)
	}
	payloadWant, err := NewWantPayload(SignedWant{Want: want, Sig: sig})
	if err != nil {
		t.Fatalf("NewWantPayload: %v", err)
	}
	wire, err := EncodePayload(payloadWant)
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	dec, err := DecodePayload(wire)
	if err != nil || dec.Kind != KindWant {
		t.Fatalf("DecodePayload: dec=%+v err=%v", dec, err)
	}

	// 2. Borg group + BRC-369 bsecret-style epoch sealing & revocation forward secrecy.
	reg := NewGroupRegistry()
	if _, err := reg.CreateGroup("team-alpha", "Alpha Research", pubHex); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	member2, _ := ec.NewPrivateKey()
	m2Pub := hex.EncodeToString(member2.PubKey().Compressed())
	if err := reg.AddMember("team-alpha", m2Pub, "reader"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	sym1, err := reg.EpochKey("team-alpha", m2Pub, 1)
	if err != nil {
		t.Fatalf("EpochKey(m2, ep1): %v", err)
	}
	cid1 := sha256.Sum256([]byte("frag-1"))
	env1, _, err := SealForEpoch("team-alpha", 1, sym1, cid1, []byte("secret epoch 1 memory"))
	if err != nil {
		t.Fatalf("SealForEpoch: %v", err)
	}
	plain1, err := OpenWithEpoch(env1, sym1)
	if err != nil || string(plain1) != "secret epoch 1 memory" {
		t.Fatalf("OpenWithEpoch: got %q err=%v", plain1, err)
	}

	// Revoke member2 -> forces epoch 2 rotation; member2 CANNOT access epoch 2!
	ep2, err := reg.RevokeMember("team-alpha", m2Pub)
	if err != nil || ep2 != 2 {
		t.Fatalf("RevokeMember: ep=%d err=%v", ep2, err)
	}
	if _, err := reg.EpochKey("team-alpha", m2Pub, 2); err == nil {
		t.Fatal("revoked member must NOT obtain epoch 2 key")
	}
	sym2, err := reg.EpochKey("team-alpha", pubHex, 2)
	if err != nil {
		t.Fatalf("owner EpochKey(ep2): %v", err)
	}
	cid2 := sha256.Sum256([]byte("frag-2"))
	env2, _, err := SealForEpoch("team-alpha", 2, sym2, cid2, []byte("post-revocation secret"))
	if err != nil {
		t.Fatalf("SealForEpoch(ep2): %v", err)
	}
	// Attempting to open epoch-2 envelope with old epoch-1 key must fail!
	if _, err := OpenWithEpoch(env2, sym1); err == nil {
		t.Fatal("old epoch-1 key must NOT open epoch-2 BRC-369 envelope")
	}
	plain2, err := OpenWithEpoch(env2, sym2)
	if err != nil || string(plain2) != "post-revocation secret" {
		t.Fatalf("OpenWithEpoch(ep2): got %q err=%v", plain2, err)
	}

	// 3. BRC-167 CHIRP content-addressed store & closure verification.
	objStore := NewMemoryObjectStore()
	payload := []byte("verified shard pack bytes packed via BRC-167 CHIRP")
	url, _, _, err := StoreChirpWithHashes(objStore, payload)
	if err != nil {
		t.Fatalf("StoreChirpWithHashes: %v", err)
	}
	got, err := ResolveChirp(objStore, url)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("ResolveChirp: got %q err=%v", got, err)
	}
}
