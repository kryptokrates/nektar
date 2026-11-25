package contracts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bcommon/keyed"

	"github.com/kryptokrates/nektar/internal/core"
)

// SaleConfig holds the DisclosureSale Rúnar covenant parameters.
type SaleConfig struct {
	SellerPKH   string `json:"sellerPkh"`
	Price       uint64 `json:"price"`
	HWrapped    string `json:"hWrapped"` // sha256(wrappedKey) — binds escrow to BRC-369 content key!
	RefundAfter uint64 `json:"refundAfter"`
	MaxFee      uint64 `json:"maxFee"`
}

// KeyedSaleOffer pairs a BRC-369 keyed content commitment with a signed DisclosureSale offer.
type KeyedSaleOffer struct {
	Config           SaleConfig `json:"cfg"`
	SellerPub        string     `json:"sellerPub"`
	KeyCommitment    string     `json:"keyCommitment"`    // BRC-369 keyed.Commitment(k)
	SaltHex          string     `json:"saltHex"`          // BRC-369 4-byte segment salt
	CiphertextHex    string     `json:"ciphertextHex"`    // BRC-369 encrypted payload (may be stripped on wire if CHIRP-offloaded)
	CiphertextSHA256 string     `json:"ciphertextSha256,omitempty"` // SHA-256 of ciphertext (allows signature verification even when CiphertextHex is offloaded to CHIRP)
	ExpiresAt        uint64     `json:"expiresAt,omitempty"`
	OfferSig         string     `json:"offerSig"`
	Artifact         CovenantID `json:"artifact"`
	Direct           bool       `json:"direct,omitempty"`
	Version          int        `json:"version,omitempty"` // 0/1 = legacy OfferDigest, 2 = framed OfferDigestV2
	// Seller-only secret material (never placed on public wire offer).
	contentKey [32]byte
}

// ContentKey returns the raw BRC-369 32-byte content scalar for seller-side conditional release.
func (o *KeyedSaleOffer) ContentKey() [32]byte { return o.contentKey }

// ClearContentKey wipes the seller-side secret content key from memory.
func (o *KeyedSaleOffer) ClearContentKey() { core.Zero32Array(&o.contentKey) }

// OfferDigest computes the canonical SHA-256 digest binding every MITM-sensitive field,
// including keyCommitment, salt, and ciphertext hash when provided.
func OfferDigest(cfg SaleConfig, buyerPub, leafHex string, expiresAt uint64, extra ...[]byte) ([]byte, error) {
	sellerPKH, err := hex.DecodeString(cfg.SellerPKH)
	if err != nil {
		return nil, err
	}
	hWrapped, err := hex.DecodeString(cfg.HWrapped)
	if err != nil {
		return nil, err
	}
	buyerBytes := []byte{0x00}
	if buyerPub != "" {
		buyerBytes, err = hex.DecodeString(buyerPub)
		if err != nil {
			return nil, err
		}
	}
	leafBytes := make([]byte, 32)
	if leafHex != "" {
		leafBytes, err = hex.DecodeString(leafHex)
		if err != nil {
			return nil, err
		}
	}
	parts := [][]byte{
		sellerPKH,
		hWrapped,
		core.U64LE(cfg.Price),
		core.U64LE(cfg.RefundAfter),
		core.U64LE(cfg.MaxFee),
		buyerBytes,
		leafBytes,
		core.U64LE(expiresAt),
	}
	parts = append(parts, extra...)
	return core.SHA256(core.Concat(parts...)), nil
}

// CreateKeyedSaleOffer seals plaintext under a fresh BRC-369 content key (lightwebinc/bcommon/keyed),
// commits sha256(k) into DisclosureSale's hWrapped, and signs the offer with sellerKey.
func CreateKeyedSaleOffer(
	sellerKey *ec.PrivateKey,
	plaintext []byte,
	price, refundAfter, maxFee, expiresAt uint64,
	buyerPub, leafHex string,
	direct bool,
) (*KeyedSaleOffer, error) {
	k, err := keyed.SampleKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sale: sample BRC-369 key: %w", err)
	}
	salt, err := keyed.SampleSalt(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sale: sample BRC-369 salt: %w", err)
	}
	ct, err := keyed.SealSegment(k, salt, plaintext)
	if err != nil {
		return nil, fmt.Errorf("sale: seal segment: %w", err)
	}
	commit := keyed.Commitment(k)
	// F-05 native BRC-369 fair exchange: DisclosureSale verifies sha256("metanet keyed content commitment v1" || wrapped) === hWrapped
	// directly under Bitcoin Script consensus, so hWrapped MUST equal keyed.Commitment(k) (KeyCommitment).
	hWrapped := commit[:]

	sellerPubBytes := sellerKey.PubKey().Compressed()
	sellerPKH := hex.EncodeToString(core.Hash160(sellerPubBytes))
	cfg := SaleConfig{
		SellerPKH:   sellerPKH,
		Price:       price,
		HWrapped:    hex.EncodeToString(hWrapped),
		RefundAfter: refundAfter,
		MaxFee:      maxFee,
	}
	ctHash := core.SHA256(ct)
	digest, err := OfferDigest(cfg, buyerPub, leafHex, expiresAt, commit[:], salt[:], ctHash)
	if err != nil {
		return nil, err
	}
	sig, err := sellerKey.Sign(digest)
	if err != nil {
		return nil, fmt.Errorf("sale: sign offer: %w", err)
	}
	art, err := LoadArtifact("DisclosureSale")
	if err != nil {
		return nil, err
	}
	return &KeyedSaleOffer{
		Config:           cfg,
		SellerPub:        hex.EncodeToString(sellerPubBytes),
		KeyCommitment:    hex.EncodeToString(commit[:]),
		SaltHex:          hex.EncodeToString(salt[:]),
		CiphertextHex:    hex.EncodeToString(ct),
		CiphertextSHA256: hex.EncodeToString(ctHash),
		ExpiresAt:        expiresAt,
		OfferSig:         hex.EncodeToString(sig.Serialize()),
		Artifact:         art.ID(),
		Direct:           direct,
		contentKey:       k,
	}, nil
}

// VerifySaleOffer validates expiry, exact cryptographic field lengths, canonical compressed pubkey,
// low-S DER signature (binding keyCommitment, salt, and ciphertext hash), and the DisclosureSale artifact ID.
func VerifySaleOffer(offer *KeyedSaleOffer, buyerPub, leafHex string) error {
	if offer == nil {
		return errors.New("sale: nil offer")
	}
	if offer.ExpiresAt == 0 || int64(offer.ExpiresAt) < time.Now().Unix() {
		return errors.New("sale: offer missing expiry or expired")
	}
	art, err := LoadArtifact("DisclosureSale")
	if err != nil {
		return err
	}
	if offer.Artifact.ScriptHash != art.ID().ScriptHash {
		return errors.New("sale: DisclosureSale covenant artifact hash mismatch")
	}
	pub, pubBytes, err := core.ParseStrictCompressedPubKey(offer.SellerPub)
	if err != nil {
		return fmt.Errorf("sale: invalid sellerPub: %w", err)
	}
	if len(offer.Config.SellerPKH) != 40 || hex.EncodeToString(core.Hash160(pubBytes)) != offer.Config.SellerPKH {
		return errors.New("sale: sellerPkh does not match sellerPub")
	}
	if hwBytes, hwErr := hex.DecodeString(offer.Config.HWrapped); hwErr != nil || len(hwBytes) != 32 {
		return errors.New("sale: hWrapped must be 32-byte hex")
	}
	// F-05 native BRC-369 fair exchange: on-chain DisclosureSale checks keyed.Commitment(wrapped) === hWrapped,
	// so hWrapped MUST equal KeyCommitment before any buyer funds the escrow.
	if offer.Config.HWrapped != offer.KeyCommitment {
		return errors.New("sale: hWrapped does not match BRC-369 keyCommitment")
	}
	if buyerPub != "" {
		if _, _, bErr := core.ParseStrictCompressedPubKey(buyerPub); bErr != nil {
			return fmt.Errorf("sale: invalid buyerPub: %w", bErr)
		}
	}
	if leafHex != "" {
		if lb, lErr := hex.DecodeString(leafHex); lErr != nil || len(lb) != 32 {
			return errors.New("sale: leafHex must be 32-byte hex")
		}
	}
	sig, err := core.ParseStrictLowSDER(offer.OfferSig)
	if err != nil {
		return fmt.Errorf("sale: invalid offerSig: %w", err)
	}
	commitBytes, err := hex.DecodeString(offer.KeyCommitment)
	if err != nil || len(commitBytes) != 32 {
		return errors.New("sale: keyCommitment must be 32-byte hex")
	}
	saltBytes, err := hex.DecodeString(offer.SaltHex)
	if err != nil || len(saltBytes) != keyed.SaltLen {
		return errors.New("sale: saltHex must be 4-byte BRC-369 salt hex")
	}
	var ctHash []byte
	if offer.CiphertextHex != "" {
		ctBytes, err := hex.DecodeString(offer.CiphertextHex)
		if err != nil {
			return errors.New("sale: invalid ciphertextHex")
		}
		ctHash = core.SHA256(ctBytes)
		if offer.CiphertextSHA256 != "" && offer.CiphertextSHA256 != hex.EncodeToString(ctHash) {
			return errors.New("sale: ciphertextHex does not match ciphertextSha256")
		}
	} else if offer.CiphertextSHA256 != "" {
		// Offloaded >4KB CHIRP ciphertext: verify signature over committed ciphertextSha256.
		ctHash, err = hex.DecodeString(offer.CiphertextSHA256)
		if err != nil || len(ctHash) != 32 {
			return errors.New("sale: invalid ciphertextSha256")
		}
	} else {
		ctHash = core.SHA256(nil)
	}
	digest, err := OfferDigest(offer.Config, buyerPub, leafHex, offer.ExpiresAt, commitBytes, saltBytes, ctHash)
	if err != nil {
		return err
	}
	if !sig.Verify(digest, pub) {
		return errors.New("sale: invalid offer signature")
	}
	return nil
}

// ClaimKeyedSale verifies a released 32-byte BRC-369 content key against BOTH
// the DisclosureSale hWrapped commitment AND the BRC-369 keyed.CheckOpened commitment,
// then decrypts the ciphertext segment.
func ClaimKeyedSale(offer *KeyedSaleOffer, releasedKey []byte) ([]byte, error) {
	if hex.EncodeToString(core.SHA256(releasedKey)) != offer.Config.HWrapped {
		return nil, errors.New("sale: released key does not match covenant hWrapped")
	}
	commitBytes, err := hex.DecodeString(offer.KeyCommitment)
	if err != nil || len(commitBytes) != 32 {
		return nil, errors.New("sale: invalid BRC-369 keyCommitment")
	}
	var commit [32]byte
	copy(commit[:], commitBytes)
	k, err := keyed.CheckOpened(releasedKey, commit)
	if err != nil {
		return nil, fmt.Errorf("sale: BRC-369 CheckOpened failed: %w", err)
	}
	saltBytes, err := hex.DecodeString(offer.SaltHex)
	if err != nil || len(saltBytes) != keyed.SaltLen {
		return nil, errors.New("sale: invalid BRC-369 salt")
	}
	var salt [keyed.SaltLen]byte
	copy(salt[:], saltBytes)
	ct, err := hex.DecodeString(offer.CiphertextHex)
	if err != nil {
		return nil, err
	}
	return keyed.OpenSegment(k, salt, ct)
}
