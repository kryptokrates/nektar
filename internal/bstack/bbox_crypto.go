package bstack

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

// BboxSealedBody is the authenticated E2E-encrypted envelope stored inside BboxMessage.Body.
// Intermediate NAT relay Hubs see only ciphertext and never learn BRC-369 content keys or payment BEEFs.
type BboxSealedBody struct {
	V      int    `json:"v"`      // protocol version (strictly 2 = HKDF-SHA256 + framed AAD)
	EphPub string `json:"ephPub"` // 33-byte compressed ephemeral secp256k1 pubkey hex
	Nonce  string `json:"nonce"`  // 12-byte AES-GCM nonce hex
	CT     string `json:"ct"`     // AES-256-GCM ciphertext+tag hex
}

// BboxAAD returns canonical length-prefixed associated data (v2) binding the encrypted payload
// unambiguously to its exact envelope metadata (eliminates delimiter collision across fields).
func BboxAAD(office, senderPub, recipientPub, dealID, kind string) []byte {
	return core.Concat(
		[]byte("nektar/bbox-e2e/v2"),
		core.Field([]byte(office)),
		core.Field([]byte(senderPub)),
		core.Field([]byte(recipientPub)),
		core.Field([]byte(dealID)),
		core.Field([]byte(kind)),
	)
}

// deriveBboxAEADKeyV2 derives a 32-byte AES-256-GCM key via RFC 5869 HKDF-SHA256 (stdlib crypto/hkdf).
func deriveBboxAEADKeyV2(sharedCompressed, ephPub, recipientPub, aad []byte) ([32]byte, error) {
	var out [32]byte
	salt := sha256.Sum256(core.Concat([]byte("nektar/bbox-salt/v2"), ephPub, recipientPub))
	aadHash := sha256.Sum256(aad)
	info := "nektar/bbox-kdf/v2|" + hex.EncodeToString(aadHash[:])
	derived, err := hkdf.Key(sha256.New, sharedCompressed, salt[:], info, 32)
	if err != nil {
		return out, err
	}
	copy(out[:], derived)
	core.ZeroBytes(derived)
	return out, nil
}

// SealBboxBody encrypts plaintext end-to-end to recipientPubHex using ephemeral secp256k1 ECDH,
// RFC 5869 HKDF-SHA256 key derivation (v2), and AES-256-GCM with ephemeral secret zeroization.
func SealBboxBody(recipientPubHex string, aad, plaintext []byte) (json.RawMessage, error) {
	recPub, recCompressed, err := core.ParseStrictCompressedPubKey(recipientPubHex)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: invalid recipientPub: %w", err)
	}
	ephPriv, err := ec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: generate ephemeral key: %w", err)
	}
	defer func() {
		if ephPriv != nil && ephPriv.D != nil {
			ephPriv.D.SetInt64(0)
		}
	}()
	shared, err := ephPriv.DeriveSharedSecret(recPub)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: ecdh: %w", err)
	}
	sharedBytes := shared.Compressed()
	defer core.ZeroBytes(sharedBytes)

	ephPubBytes := ephPriv.PubKey().Compressed()
	key, err := deriveBboxAEADKeyV2(sharedBytes, ephPubBytes, recCompressed, aad)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: hkdf: %w", err)
	}
	defer core.Zero32Array(&key)

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
		V:      2,
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

// OpenBboxBody decrypts an E2E-encrypted v2 BboxSealedBody (HKDF-SHA256 + length-framed AAD)
// using recipientKey and verifies AAD binding, zeroizing intermediate ECDH/AEAD secrets.
func OpenBboxBody(recipientKey *ec.PrivateKey, aad []byte, raw json.RawMessage) ([]byte, error) {
	if recipientKey == nil {
		return nil, errors.New("bbox e2e: recipient private key is nil")
	}
	var env BboxSealedBody
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bbox e2e: parse envelope: %w", err)
	}
	if env.V != 2 || env.EphPub == "" || env.Nonce == "" || env.CT == "" {
		return nil, errors.New("bbox e2e: non-v2, plaintext, or malformed bbox envelope rejected")
	}
	ephPub, ephCompressed, err := core.ParseStrictCompressedPubKey(env.EphPub)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: invalid ephPub: %w", err)
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
	sharedBytes := shared.Compressed()
	defer core.ZeroBytes(sharedBytes)

	key, err := deriveBboxAEADKeyV2(sharedBytes, ephCompressed, recipientKey.PubKey().Compressed(), aad)
	if err != nil {
		return nil, fmt.Errorf("bbox e2e: hkdf: %w", err)
	}
	defer core.Zero32Array(&key)

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
