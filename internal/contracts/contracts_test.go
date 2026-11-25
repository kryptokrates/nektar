package contracts

import (
	"encoding/hex"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestAnchorEncodeDecodeAndSaleBRC369(t *testing.T) {
	// 1. Verify all 10 compiled Rúnar artifacts load cleanly from go:embed.
	for _, name := range ListArtifacts() {
		a, err := LoadArtifact(name)
		if err != nil || a.Script == "" {
			t.Fatalf("LoadArtifact(%s) failed: err=%v", name, err)
		}
	}

	// 2. NektarAnchor state encode -> decode roundtrip.
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubBytes := priv.PubKey().Compressed()
	pkh := core.Hash160(pubBytes)
	r0 := core.SHA256([]byte("batch0"))
	rp0 := core.ChainRp(core.Zero32, r0, 4)

	st := AnchorState{
		Shard:    core.ShardIDFromPubkey(pubBytes),
		OwnerPub: hex.EncodeToString(pubBytes),
		OwnerPKH: pkh,
		N:        0,
		Size:     4,
		R:        r0,
		Rp:       rp0,
		Tag:      core.TagAnchorTag,
		Metanet:  true,
	}
	lockScript, err := EncodeAnchorLockingScript(st)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript: %v", err)
	}
	dec, err := DecodeNektarAnchor(*lockScript)
	if err != nil {
		t.Fatalf("DecodeNektarAnchor: %v", err)
	}
	if dec.Shard != st.Shard || dec.N != 0 || dec.Size != 4 || !dec.Metanet || !core.EqualBytes(dec.Rp, rp0) {
		t.Fatalf("decoded state mismatch: %+v", dec)
	}

	// 3. BRC-369 Keyed DisclosureSale offer -> verify -> conditional key claim!
	buyer, _ := ec.NewPrivateKey()
	buyerPub := hex.EncodeToString(buyer.PubKey().Compressed())
	secret := []byte("Verifiable alpha insight worth 500 sats")
	offer, err := CreateKeyedSaleOffer(priv, secret, 500, 950000, 100, 949900, buyerPub, "", false)
	if err != nil {
		t.Fatalf("CreateKeyedSaleOffer: %v", err)
	}
	if err := VerifySaleOffer(offer, buyerPub, ""); err != nil {
		t.Fatalf("VerifySaleOffer: %v", err)
	}
	k := offer.ContentKey()
	opened, err := ClaimKeyedSale(offer, k[:])
	if err != nil {
		t.Fatalf("ClaimKeyedSale: %v", err)
	}
	if string(opened) != string(secret) {
		t.Fatalf("claimed plaintext mismatch: got %q want %q", opened, secret)
	}
	// Wrong key must fail closed.
	badKey := make([]byte, 32)
	if _, err := ClaimKeyedSale(offer, badKey); err == nil {
		t.Fatal("expected ClaimKeyedSale to refuse wrong key")
	}
}
