package contracts

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/kryptokrates/nektar/internal/core"
)

// TestOfferDigestV2Framing verifies F-13 length-framed sale offer signing and strict verification.
func TestOfferDigestV2Framing(t *testing.T) {
	seller, _ := ec.NewPrivateKey()
	buyer, _ := ec.NewPrivateKey()
	buyerHex := hex.EncodeToString(buyer.PubKey().Compressed())
	exp := uint64(time.Now().Unix() + 3600)

	leafHex := hex.EncodeToString(core.SHA256([]byte("fuzz-leaf")))
	offer, err := CreateKeyedSaleOffer(seller, []byte("verified-knowledge"), 1000, exp+600, 100, exp, buyerHex, leafHex, false)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	defer offer.ClearContentKey()
	if offer.Version != 2 {
		t.Fatalf("expected Offer Version=2, got %d", offer.Version)
	}
	if err := VerifySaleOffer(offer, buyerHex, leafHex); err != nil {
		t.Fatalf("VerifySaleOffer v2 failed: %v", err)
	}
	// Mutating CiphertextHex MUST fail signature verification (bound via ciphertextSha256).
	tampered := *offer
	tampered.CiphertextHex = offer.CiphertextHex + "00"
	if err := VerifySaleOffer(&tampered, buyerHex, leafHex); err == nil {
		t.Fatal("VerifySaleOffer accepted tampered ciphertext")
	}
	// Verify ClearContentKey zeroizes the secret scalar.
	offer.ClearContentKey()
	k := offer.ContentKey()
	if !bytes.Equal(k[:], make([]byte, 32)) {
		t.Fatal("ClearContentKey did not zeroize contentKey")
	}
}

// FuzzVerifyFragment exercises ProofBundle verification against malformed sizes, paths, and BEEFs (F-07, F-08).
func FuzzVerifyFragment(f *testing.F) {
	seed, _ := json.Marshal(ProofBundle{
		Version:   1,
		Shard:     hex.EncodeToString(make([]byte, 20)),
		N:         0,
		Size:      1,
		Leaf:      hex.EncodeToString(core.SHA256([]byte("leaf"))),
		LeafIndex: 0,
	})
	f.Add(seed)
	f.Add([]byte(`{"version":1,"size":18446744073709551615,"leafIndex":-1,"path":["aa"]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var pb ProofBundle
		if err := json.Unmarshal(raw, &pb); err != nil {
			return
		}
		_ = VerifyFragment(pb, nil)
		_ = VerifyFragmentStructural(pb)
	})
}
