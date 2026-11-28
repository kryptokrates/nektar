package bstack

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestNkt1CBORUnknownKeysAndUnregisteredArtifact(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())

	// 1. Unregistered map[string]any MUST fail closed (no generic:v2 fallback).
	if _, err := SignArtifact(priv, map[string]any{"foo": "bar"}); err == nil {
		t.Fatal("expected SignArtifact to reject unregistered map[string]any")
	}
	if err := VerifyArtifact(pubHex, "3006020101020101", map[string]any{"foo": "bar"}); err == nil {
		t.Fatal("expected VerifyArtifact to reject unregistered map[string]any")
	}

	// 2. Non-critical odd extension key on nkt1 record is preserved; critical even key fails closed.
	want := WantAd{
		WantID:   "w-ext-1",
		Pub:      pubHex,
		Net:      "testnet",
		Domains:  []string{"ops"},
		Query:    "cbor extension test",
		MaxSats:  100,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, err := SignArtifact(priv, want)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewWantPayload(SignedWant{Want: want, Sig: sig})
	if err != nil {
		t.Fatal(err)
	}
	p.Extra = []cbor.Pair{{Key: uint64(3), Val: "non-critical-ext"}}
	wireOdd, err := EncodePayload(p)
	if err != nil {
		t.Fatalf("EncodePayload with odd extra key: %v", err)
	}
	decOdd, err := DecodePayload(wireOdd)
	if err != nil {
		t.Fatalf("DecodePayload odd extra key failed: %v", err)
	}
	if len(decOdd.Extra) != 1 || decOdd.Extra[0].Key.(uint64) != 3 {
		t.Fatalf("expected odd extra key 3 preserved, got %+v", decOdd.Extra)
	}

	// Critical even extension key (4) on nkt1 record MUST fail closed.
	wireEven, err := record.Encode(cbor.Map{
		{Key: uint64(0), Val: MarketMagic},
		{Key: uint64(1), Val: uint64(1)},
		{Key: uint64(2), Val: p.Body},
	}, cbor.Map{{Key: uint64(4), Val: true}}, MaxPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePayload(wireEven); !errors.Is(err, core.ErrUnknownCriticalKey) {
		t.Fatalf("expected ErrUnknownCriticalKey for even key 4, got %v", err)
	}

	// 3. Non-critical odd extension key inside WantAd CBOR still verifies signature over RawCBOR!
	rawWant, err := want.EncodeCanonicalCBOR()
	if err != nil {
		t.Fatal(err)
	}
	v, err := core.DecodeCBOR(rawWant)
	if err != nil {
		t.Fatal(err)
	}
	extMap := append(v.(cbor.Map), cbor.Pair{Key: uint64(99), Val: "future-hint"})
	extRaw, err := core.EncodeCBOR(extMap)
	if err != nil {
		t.Fatal(err)
	}
	extSig, err := SignArtifactRaw(priv, want.DomainTag(), extRaw)
	if err != nil {
		t.Fatal(err)
	}
	extPayload, err := NewWantPayload(SignedWant{Want: want, Sig: extSig, RawCBOR: extRaw})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMarketPayload(extPayload, "testnet", LaneOf("testnet", "ops"), time.Now()); err != nil {
		t.Fatalf("forward-compatible odd extension inside WantAd failed verify: %v", err)
	}
}

func FuzzDecodeNkt1Payload(f *testing.F) {
	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	want := WantAd{
		WantID:   "w-fuzz",
		Pub:      pubHex,
		Net:      "memory",
		Domains:  []string{"ops"},
		Query:    "fuzz seed",
		MaxSats:  10,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, _ := SignArtifact(priv, want)
	p, _ := NewWantPayload(SignedWant{Want: want, Sig: sig})
	if wire, err := EncodePayload(p); err == nil {
		f.Add(wire)
	}
	f.Add([]byte("nkt1"))
	f.Add([]byte{0xa3, 0x00, 0x44, 'n', 'k', 't', '1', 0x01, 0x01, 0x02, 0x40})

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := DecodePayload(data)
		if err != nil {
			return
		}
		_ = ValidateMarketPayload(p, "memory", "", time.Now())
	})
}
