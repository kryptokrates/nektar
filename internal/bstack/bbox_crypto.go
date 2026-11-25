package bstack

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// BboxSealedBody is the authenticated E2E-encrypted envelope stored inside BboxMessage.Body.
// Intermediate NAT relay Hubs see only ciphertext and never learn BRC-369 content keys or payment BEEFs.
type BboxSealedBody struct {
	V      int    `json:"v"`      // protocol version (1)
	EphPub string `json:"ephPub"` // 33-byte compressed ephemeral secp256k1 pubkey hex
	Nonce  string `json:"nonce"`  // 12-byte AES-GCM nonce hex
	CT     string `json:"ct"`     // AES-256-GCM ciphertext+tag hex
}

// BboxAAD returns canonical associated data binding the encrypted payload to its exact envelope metadata.
func BboxAAD(office, senderPub, recipientPub, dealID, kind string) []byte {
	return []byte("nektar/bbox-e2e/v1|" + office + "|" + senderPub + "|" + recipientPub + "|" + dealID + "|" + kind)
}

func deriveBboxAEADKey(sharedX []byte, ephPub, recipientPub []byte, aad []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("nektar/bbox-kdf/v1"))
	_, _ = h.Write(sharedX)
	_, _ = h.Write(ephPub)
	_, _ = h.Write(recipientPub)
	_, _ = h.Write(aad)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// SealBboxBody encrypts plaintext end-to-end to recipientPubHex using ephemeral secp256k1 ECDH + AES-256-GCM.
func SealBboxBody(recipientPubHex string, aad, plaintext []byte) (json.RawMessage, error) {
	recBytes, err := hex.DecodeString(recipientPubHex)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: decode recipientPub: %w", err)
	}
	recPub, err := ec.ParsePubKey(recBytes)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: parse recipientPub: %w", err)
	}
	ephPriv, err := ec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: generate ephemeral key: %w", err)
	}
	shared, err := ephPriv.DeriveSharedSecret(recPub)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: ecdh: %w", err)
	}
	ephPubBytes := ephPriv.PubKey().Compressed()
	key := deriveBboxAEADKey(shared.Compressed(), ephPubBytes, recPub.Compressed(), aad)

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce[:], plaintext, aad)
	env := BboxSealedBody{
		V:      1,
		EphPub: hex.EncodeToString(ephPubBytes),
		Nonce:  hex.EncodeToString(nonce[:]),
		CT:     hex.EncodeToString(ct),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// OpenBboxBody decrypts an E2E-encrypted BboxSealedBody using recipientKey and verifies AAD binding.
func OpenBboxBody(recipientKey *ec.PrivateKey, aad []byte, raw json.RawMessage) ([]byte, error) {
	if recipientKey == nil {
		return nil, errors.New("bbox e2e: recipient private key is nil")
	}
	var env BboxSealedBody
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bbox e2e: parse envelope: %w", err)
	}
	if env.V != 1 || env.EphPub == "" || env.Nonce == "" || env.CT == "" {
		return nil, errors.New("bbox e2e: plaintext or malformed bbox envelope rejected")
	}
	ephBytes, err := hex.DecodeString(env.EphPub)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: decode ephPub: %w", err)
	}
	ephPub, err := ec.ParsePubKey(ephBytes)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: parse ephPub: %w", err)
	}
	nonce, err := hex.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 12 {
		return nil, errors.New("bbox e2e: invalid nonce")
	}
	ct, err := hex.DecodeString(env.CT)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: decode ct: %w", err)
	}
	shared, err := recipientKey.DeriveSharedSecret(ephPub)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: ecdh: %w", err)
	}
	key := deriveBboxAEADKey(shared.Compressed(), ephPub.Compressed(), recipientKey.PubKey().Compressed(), aad)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: decrypt/authenticate failed: %w", err)
	}
	return pt, nil
}
