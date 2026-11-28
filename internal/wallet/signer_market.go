package wallet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/kryptokrates/nektar/internal/core"
)

// MarketSigner encapsulates a secp256k1 node/market key inside internal/wallet so raw
// *ec.PrivateKey never leaks across package boundaries (ARCH-S1 / RCA-4).
type MarketSigner struct {
	priv     *ec.PrivateKey
	fastPriv *dsecp.PrivateKey
	pub      *ec.PublicKey
	pubHex   string
}

func makeFastPriv(priv *ec.PrivateKey) *dsecp.PrivateKey {
	if priv == nil {
		return nil
	}
	raw := priv.Serialize()
	var buf [32]byte
	if len(raw) <= 32 {
		copy(buf[32-len(raw):], raw)
	}
	core.ZeroBytes(raw)
	fp := dsecp.PrivKeyFromBytes(buf[:])
	core.Zero32Array(&buf)
	return fp
}

// NewMarketSigner wraps an existing *ec.PrivateKey inside a MarketSigner (used by tests and wallet boundaries).
func NewMarketSigner(priv *ec.PrivateKey) *MarketSigner {
	if priv == nil {
		return nil
	}
	pub := priv.PubKey()
	if pub == nil {
		return nil
	}
	return &MarketSigner{
		priv:     priv,
		fastPriv: makeFastPriv(priv),
		pub:      pub,
		pubHex:   hex.EncodeToString(pub.Compressed()),
	}
}

// NewMarketSignerFromBytes constructs a MarketSigner from 32 raw secret bytes and zeroes the input slice.
func NewMarketSignerFromBytes(secret []byte) (*MarketSigner, error) {
	if len(secret) != 32 {
		core.ZeroBytes(secret)
		return nil, fmt.Errorf("wallet: market signer requires 32 secret bytes (got %d)", len(secret))
	}
	cp := make([]byte, 32)
	copy(cp, secret)
	core.ZeroBytes(secret)
	fastPriv := dsecp.PrivKeyFromBytes(cp)
	priv, pub := ec.PrivateKeyFromBytes(cp)
	core.ZeroBytes(cp)
	if priv == nil || pub == nil {
		return nil, errors.New("wallet: failed to derive secp256k1 keypair")
	}
	return &MarketSigner{
		priv:     priv,
		fastPriv: fastPriv,
		pub:      pub,
		pubHex:   hex.EncodeToString(pub.Compressed()),
	}, nil
}

// NewEphemeralMarketSigner generates a fresh random secp256k1 MarketSigner.
func NewEphemeralMarketSigner() (*MarketSigner, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("wallet: crypto/rand market key: %w", err)
	}
	return NewMarketSignerFromBytes(buf)
}

// GenerateProtectedKeyBytes generates 32 random secret bytes suitable for LoadOrCreateProtectedKey.
func GenerateProtectedKeyBytes() ([]byte, error) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	raw := priv.Serialize()
	out := make([]byte, 32)
	copy(out[32-len(raw):], raw)
	core.ZeroBytes(raw)
	return out, nil
}

// PubKey returns the compressed public key.
func (s *MarketSigner) PubKey() *ec.PublicKey {
	if s == nil {
		return nil
	}
	return s.pub
}

// PubHex returns the 33-byte lowercase compressed hex public key.
func (s *MarketSigner) PubHex() string {
	if s == nil {
		return ""
	}
	return s.pubHex
}

// SignDigest signs a 32-byte digest with ECDSA DER encoding using constant-time pure-Go secp256k1/v4.
func (s *MarketSigner) SignDigest(digest32 []byte) ([]byte, error) {
	if s == nil || s.priv == nil {
		return nil, errors.New("wallet: market signer not initialized")
	}
	if len(digest32) != 32 {
		return nil, fmt.Errorf("wallet: SignDigest requires 32-byte digest (got %d)", len(digest32))
	}
	if s.fastPriv != nil {
		return core.FastSignWithPriv(s.fastPriv, digest32)
	}
	sig, err := s.priv.Sign(digest32)
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}

// SignDigestHex signs a 32-byte digest and returns lowercase DER hex.
func (s *MarketSigner) SignDigestHex(digest32 []byte) (string, error) {
	der, err := s.SignDigest(digest32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(der), nil
}

// PubCompressed returns a copy of the 33-byte compressed public key.
func (s *MarketSigner) PubCompressed() []byte {
	if s == nil || s.pub == nil {
		return nil
	}
	return append([]byte(nil), s.pub.Compressed()...)
}

// ProtoWallet returns a BRC-100 / BRC-42 / BRC-78 sdkwallet.Interface backed by this signer's key
// without exposing raw *ec.PrivateKey outside internal/wallet.
func (s *MarketSigner) ProtoWallet() (sdkwallet.Interface, error) {
	if s == nil || s.priv == nil {
		return nil, errors.New("wallet: market signer not initialized")
	}
	return sdkwallet.NewCompletedProtoWallet(s.priv)
}

// ECDHSharedCompressed returns the 33-byte compressed ECDH shared point (caller must zeroize after KDF).
func (s *MarketSigner) ECDHSharedCompressed(peerPub *ec.PublicKey) ([]byte, error) {
	if s == nil || s.priv == nil || peerPub == nil {
		return nil, errors.New("wallet: ECDH requires initialized signer and peer public key")
	}
	if s.fastPriv != nil {
		return core.FastECDHSharedCompressed(s.fastPriv, peerPub.Compressed())
	}
	shared, err := s.priv.DeriveSharedSecret(peerPub)
	if err != nil {
		return nil, err
	}
	return shared.Compressed(), nil
}

// ECDHSharedKey derives a SHA-256 symmetric secret from ECDH(s.priv, peerPub).
func (s *MarketSigner) ECDHSharedKey(peerPub *ec.PublicKey) ([32]byte, error) {
	var zero [32]byte
	raw, err := s.ECDHSharedCompressed(peerPub)
	if err != nil {
		return zero, err
	}
	defer core.ZeroBytes(raw)
	return sha256.Sum256(raw), nil
}
