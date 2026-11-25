package contracts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

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
	Config        SaleConfig `json:"cfg"`
	SellerPub     string     `json:"sellerPub"`
	KeyCommitment string     `json:"keyCommitment"` // BRC-369 keyed.Commitment(k)
	SaltHex       string     `json:"saltHex"`       // BRC-369 4-byte segment salt
	CiphertextHex string     `json:"ciphertextHex"` // BRC-369 encrypted payload
	ExpiresAt     uint64     `json:"expiresAt,omitempty"`
	OfferSig      string     `json:"offerSig"`
	Artifact      CovenantID `json:"artifact"`
	Direct        bool       `json:"direct,omitempty"`
	// Seller-only secret material (never placed on public wire offer).
	contentKey [32]byte
}

// ContentKey returns the raw BRC-369 32-byte content scalar for seller-side conditional release.
func (o *KeyedSaleOffer) ContentKey() [32]byte { return o.contentKey }

// OfferDigest computes the canonical SHA-256 digest binding every MITM-sensitive field.
func OfferDigest(cfg SaleConfig, buyerPub, leafHex string, expiresAt uint64) ([]byte, error) {
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
	return core.SHA256(core.Concat(
		sellerPKH,
		hWrapped,
		core.U64LE(cfg.Price),
		core.U64LE(cfg.RefundAfter),
		core.U64LE(cfg.MaxFee),
		buyerBytes,
		leafBytes,
		core.U64LE(expiresAt),
	)), nil
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
	hWrapped := core.SHA256(k[:])

	sellerPubBytes := sellerKey.PubKey().Compressed()
	sellerPKH := hex.EncodeToString(core.Hash160(sellerPubBytes))
	cfg := SaleConfig{
		SellerPKH:   sellerPKH,
		Price:       price,
		HWrapped:    hex.EncodeToString(hWrapped),
		RefundAfter: refundAfter,
		MaxFee:      maxFee,
	}
	digest, err := OfferDigest(cfg, buyerPub, leafHex, expiresAt)
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
		Config:        cfg,
		SellerPub:     hex.EncodeToString(sellerPubBytes),
		KeyCommitment: hex.EncodeToString(commit[:]),
		SaltHex:       hex.EncodeToString(salt[:]),
		CiphertextHex: hex.EncodeToString(ct),
		ExpiresAt:     expiresAt,
		OfferSig:      hex.EncodeToString(sig.Serialize()),
		Artifact:      art.ID(),
		Direct:        direct,
		contentKey:    k,
	}, nil
}

// VerifySaleOffer validates the seller's signature and DisclosureSale covenant artifact ID.
func VerifySaleOffer(offer *KeyedSaleOffer, buyerPub, leafHex string) error {
	art, err := LoadArtifact("DisclosureSale")
	if err != nil {
		return err
	}
	if offer.Artifact.ScriptHash != art.ID().ScriptHash {
		return errors.New("sale: DisclosureSale covenant artifact hash mismatch")
	}
	pubBytes, err := hex.DecodeString(offer.SellerPub)
	if err != nil {
		return err
	}
	if hex.EncodeToString(core.Hash160(pubBytes)) != offer.Config.SellerPKH {
		return errors.New("sale: sellerPkh does not match sellerPub")
	}
	pub, err := ec.ParsePubKey(pubBytes)
	if err != nil {
		return err
	}
	sigBytes, err := hex.DecodeString(offer.OfferSig)
	if err != nil {
		return err
	}
	sig, err := ec.ParseDERSignature(sigBytes)
	if err != nil {
		return err
	}
	digest, err := OfferDigest(offer.Config, buyerPub, leafHex, offer.ExpiresAt)
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
