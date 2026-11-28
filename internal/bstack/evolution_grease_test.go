package bstack

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
	"github.com/lightwebinc/bstore/storerec"

	"github.com/kryptokrates/nektar/internal/core"
)

func TestNkt1EvolutionRoundTripPreservesOddKeys(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	exp := time.Now().Add(time.Hour).UnixMilli()

	// AnnounceAd with nested PackRef + HubFeeAdvert + SignedAnnounce envelope all carrying odd extension keys.
	ann := AnnounceAd{
		Pub:   pubHex,
		Net:   "testnet",
		Hosts: []string{"https://hub.example.com"},
		Packs: []PackRef{{
			Shard:    "ops",
			ChirpURL: "chirp://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Extra:    []cbor.Pair{{Key: uint64(101), Val: "pack-ext"}},
		}},
		HubFees: &HubFeeAdvert{
			SubmitWantSats: 10,
			Extra:          []cbor.Pair{{Key: uint64(103), Val: uint64(7)}},
		},
		Exp:   exp,
		Extra: []cbor.Pair{{Key: uint64(105), Val: "ann-ext"}},
	}
	sig, err := SignArtifact(priv, ann)
	if err != nil {
		t.Fatal(err)
	}
	sa := SignedAnnounce{
		Announce: ann,
		Sig:      sig,
		Extra:    []cbor.Pair{{Key: uint64(107), Val: "env-ext"}},
	}
	p, err := NewAnnouncePayload(sa)
	if err != nil {
		t.Fatal(err)
	}
	wire1, err := EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	decP, err := DecodePayload(wire1)
	if err != nil {
		t.Fatal(err)
	}
	decSA, err := DecodeAnnouncePayload(decP.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(decSA.Extra) != 1 || decSA.Extra[0].Key.(uint64) != 107 {
		t.Fatalf("SignedAnnounce extra lost: %+v", decSA.Extra)
	}
	if len(decSA.Announce.Extra) != 1 || decSA.Announce.Extra[0].Key.(uint64) != 105 {
		t.Fatalf("AnnounceAd extra lost: %+v", decSA.Announce.Extra)
	}
	if len(decSA.Announce.Packs) != 1 || len(decSA.Announce.Packs[0].Extra) != 1 || decSA.Announce.Packs[0].Extra[0].Key.(uint64) != 101 {
		t.Fatalf("PackRef extra lost: %+v", decSA.Announce.Packs)
	}
	if decSA.Announce.HubFees == nil || len(decSA.Announce.HubFees.Extra) != 1 || decSA.Announce.HubFees.Extra[0].Key.(uint64) != 103 {
		t.Fatalf("HubFeeAdvert extra lost: %+v", decSA.Announce.HubFees)
	}

	// Re-encoding decoded struct (without RawCBOR) MUST reproduce byte-identical canonical CBOR and keep sig valid (DDIA Ch. 4 Fig. 4-7).
	decSA.RawCBOR = nil
	reP, err := NewAnnouncePayload(decSA)
	if err != nil {
		t.Fatal(err)
	}
	wire2, err := EncodePayload(reP)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire1, wire2) {
		t.Fatalf("round-trip wire mismatch: odd extension keys were not preserved identically")
	}
	if err := ValidateMarketPayload(reP, "testnet", LaneOf("testnet", DirDomain), time.Now()); err != nil {
		t.Fatalf("re-encoded AnnounceAd failed signature validation: %v", err)
	}

	// Unknown EVEN key inside nested PackRef MUST fail closed.
	badRaw, err := core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: pubHex},
		{Key: uint64(2), Val: "testnet"},
		{Key: uint64(6), Val: core.CBORStringsVal([]string{"https://hub.example.com"})},
		{Key: uint64(10), Val: []cbor.Value{cbor.Map{
			{Key: uint64(1), Val: "ops"},
			{Key: uint64(3), Val: "chirp://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Key: uint64(100), Val: true}, // critical even key
		}}},
		{Key: uint64(18), Val: exp},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeAnnounceCBOR(badRaw); !errors.Is(err, core.ErrUnknownCriticalKey) {
		t.Fatalf("expected ErrUnknownCriticalKey for nested PackRef key 100, got %v", err)
	}
}

func TestNkt1UnknownKindParityRFC6709(t *testing.T) {
	innerBody, err := core.EncodeCBOR(cbor.Map{{Key: uint64(1), Val: []byte("future-kind-payload")}})
	if err != nil {
		t.Fatal(err)
	}
	// Odd unknown kind (5) MUST decode & round-trip cleanly as ext:5.
	oddWire, err := EncodePayload(MarketPayload{
		Kind:    "ext:5",
		KindNum: 5,
		Body:    innerBody,
		Extra:   []cbor.Pair{{Key: uint64(99), Val: "hint"}},
	})
	if err != nil {
		t.Fatalf("EncodePayload odd extension kind 5 failed: %v", err)
	}
	decOdd, err := DecodePayload(oddWire)
	if err != nil {
		t.Fatalf("DecodePayload odd extension kind 5 failed: %v", err)
	}
	if !decOdd.IsUnknownExtensionKind() || decOdd.Kind != "ext:5" || decOdd.KindNum != 5 {
		t.Fatalf("unexpected decoded odd extension kind: %+v", decOdd)
	}
	if !bytes.Equal(decOdd.Body, innerBody) {
		t.Fatalf("odd extension body corrupted on round-trip")
	}

	// Even unknown kind (6) MUST fail closed with ErrUnknownCriticalKind.
	evenWire, err := record.Encode(cbor.Map{
		{Key: uint64(0), Val: MarketMagic},
		{Key: uint64(1), Val: uint64(6)},
		{Key: uint64(2), Val: innerBody},
	}, nil, MaxPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePayload(evenWire); !errors.Is(err, ErrUnknownCriticalKind) {
		t.Fatalf("expected ErrUnknownCriticalKind for kind 6, got %v", err)
	}
}

func TestWireGreaseEnvFlagRFC9170(t *testing.T) {
	t.Setenv("NEKTAR_WIRE_GREASE", "1")
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	want := WantAd{
		WantID:   "w-grease-1",
		Pub:      pubHex,
		Net:      "testnet",
		Domains:  []string{"ops"},
		Query:    "greasing check",
		MaxSats:  50,
		Revision: 1,
		Exp:      time.Now().Add(time.Hour).UnixMilli(),
	}
	canon, err := want.EncodeCanonicalCBOR()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignArtifactRaw(priv, want.DomainTag(), canon)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewWantPayload(SignedWant{Want: want, Sig: sig, RawCBOR: canon})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	decP, err := DecodePayload(wire)
	if err != nil {
		t.Fatalf("greased payload decode failed: %v", err)
	}
	foundOuterGrease := false
	for _, kv := range decP.Extra {
		if k, ok := kv.Key.(uint64); ok && k == core.GreaseKey {
			foundOuterGrease = true
		}
	}
	if !foundOuterGrease {
		t.Fatalf("expected outer RFC 9170 grease key %d in %+v", core.GreaseKey, decP.Extra)
	}
	decSW, err := DecodeWantPayload(decP.Body)
	if err != nil {
		t.Fatalf("greased want decode failed: %v", err)
	}
	foundInnerGrease := false
	for _, kv := range decSW.Want.Extra {
		if k, ok := kv.Key.(uint64); ok && k == core.GreaseKey {
			foundInnerGrease = true
		}
	}
	if !foundInnerGrease {
		t.Fatalf("expected inner RFC 9170 grease key %d in %+v", core.GreaseKey, decSW.Want.Extra)
	}
	if err := ValidateMarketPayload(decP, "testnet", LaneOf("testnet", "ops"), time.Now()); err != nil {
		t.Fatalf("greased payload failed ValidateMarketPayload: %v", err)
	}
}

func TestBoxrecAndStorerecEvolutionInterop(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.PubKey().Compressed()

	// 1. boxrec.Envelope preserves unknown keys > 7 in Extra verbatim.
	env := &boxrec.Envelope{
		Office:  NormalizeBboxOffice("nektar"),
		To:      pub,
		Box:     "market",
		From:    pub,
		Created: 1700000000,
		Expires: 0,
		Content: []byte(`{"metanetHandles":{"v":1}}`),
		Extra:   cbor.Map{{Key: uint64(99), Val: "bbox-future-ext"}},
	}
	envBytes, err := env.Encode()
	if err != nil {
		t.Fatalf("boxrec.Envelope.Encode: %v", err)
	}
	decEnv, err := boxrec.DecodeEnvelope(envBytes)
	if err != nil {
		t.Fatalf("boxrec.DecodeEnvelope: %v", err)
	}
	if len(decEnv.Extra) != 1 || decEnv.Extra[0].Key.(uint64) != 99 {
		t.Fatalf("boxrec.Envelope lost Extra key 99: %+v", decEnv.Extra)
	}

	// 2. storerec.Entry preserves unknown Kind (3..255) in Unknown and keys >= 16 in Extra verbatim.
	bucket := NormalizeBstoreBucket("nektar")
	entry := &storerec.Entry{
		Bucket:  bucket,
		ID:      ComputeBstoreBucketID(pub, bucket),
		Writer:  pub,
		Key:     "capsules/future.nkt",
		Kind:    7, // future unknown kind in 3..255
		Created: 1700000000000,
		Unknown: cbor.Map{{Key: uint64(7), Val: "future-kind-field"}},
		Extra:   cbor.Map{{Key: uint64(99), Val: "bstore-future-ext"}},
	}
	entryBytes, err := entry.Encode()
	if err != nil {
		t.Fatalf("storerec.Entry.Encode: %v", err)
	}
	decEntry, err := storerec.DecodeEntry(entryBytes)
	if err != nil {
		t.Fatalf("storerec.DecodeEntry: %v", err)
	}
	if decEntry.Kind != 7 || len(decEntry.Unknown) != 1 || len(decEntry.Extra) != 1 || decEntry.Extra[0].Key.(uint64) != 99 {
		t.Fatalf("storerec.Entry lost unknown kind/extra: %+v", decEntry)
	}

	// 3. IsNativeBboxContent rejects spoofed JSON where "metanetHandles" appears only inside a string value.
	spoof := json.RawMessage(`{"v":2,"ct":"fake \"metanetHandles\" inside value"}`)
	if IsNativeBboxContent(spoof) {
		t.Fatalf("IsNativeBboxContent falsely matched spoofed nested string")
	}
	realNative := json.RawMessage(`{"metanetHandles":{"protocol":"brc169"}}`)
	if !IsNativeBboxContent(realNative) {
		t.Fatalf("IsNativeBboxContent failed to recognize top-level metanetHandles object")
	}
}
