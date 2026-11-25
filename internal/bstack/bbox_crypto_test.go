package bstack

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

func TestBboxE2ESealAndOpen(t *testing.T) {
	senderKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	recipientKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	senderPub := hex.EncodeToString(senderKey.PubKey().Compressed())
	recipientPub := hex.EncodeToString(recipientKey.PubKey().Compressed())
	aad := BboxAAD("default", senderPub, recipientPub, "deal-e2e-1", "key_reveal")

	secretPayload := []byte(`{"contentKeyHex":"deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	sealed, err := SealBboxBody(recipientPub, aad, secretPayload)
	if err != nil {
		t.Fatalf("SealBboxBody: %v", err)
	}
	// Hub relay must never see plaintext key material.
	if strings.Contains(string(sealed), "deadbeefcafebabe") {
		t.Fatalf("sealed bbox payload leaked plaintext: %s", string(sealed))
	}

	// Recipient opens successfully with matching AAD.
	opened, err := OpenBboxBody(recipientKey, aad, sealed)
	if err != nil {
		t.Fatalf("OpenBboxBody: %v", err)
	}
	if string(opened) != string(secretPayload) {
		t.Fatalf("opened mismatch: got %q want %q", string(opened), string(secretPayload))
	}

	// Wrong recipient key must fail.
	if _, err := OpenBboxBody(wrongKey, aad, sealed); err == nil {
		t.Fatal("expected wrong recipient key to fail decryption")
	}

	// Tampered AAD (different dealID) must fail AEAD authentication.
	badAAD := BboxAAD("default", senderPub, recipientPub, "deal-e2e-tampered", "key_reveal")
	if _, err := OpenBboxBody(recipientKey, badAAD, sealed); err == nil {
		t.Fatal("expected tampered AAD to fail authentication")
	}

	// Plaintext JSON without sealed envelope must be rejected.
	if _, err := OpenBboxBody(recipientKey, aad, json.RawMessage(secretPayload)); err == nil {
		t.Fatal("expected unencrypted plaintext bbox body to be rejected")
	}
}
