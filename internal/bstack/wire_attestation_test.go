package bstack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/module"
)

func TestKindAttestationWireAndLookup(t *testing.T) {
	issuerKey, _ := ec.NewPrivateKey()
	subjectKey, _ := ec.NewPrivateKey()
	issuerPub := strings.ToLower(hex.EncodeToString(issuerKey.PubKey().Compressed()))
	subjectPub := strings.ToLower(hex.EncodeToString(subjectKey.PubKey().Compressed()))

	h1 := sha256.Sum256([]byte("attest-1"))
	h2 := sha256.Sum256([]byte("deal-receipt-1"))
	ont := sha256.Sum256([]byte("ont-ops"))
	now := time.Now()

	ad := AttestationAd{
		AttestHash:      hex.EncodeToString(h1[:]),
		Role:            "buyer_rates_seller",
		Pub:             issuerPub,
		SubjectPub:      subjectPub,
		Net:             "memory",
		Domains:         []string{"ops"},
		OntologyID:      hex.EncodeToString(ont[:]),
		DealID:          "deal-42",
		DealReceiptHash: hex.EncodeToString(h2[:]),
		Token:           "nektarrep1.AQIDBAUGBwgJCgsMDQ4PEBESExQ",
		CompositeBP:     9100,
		Exp:             now.Add(24 * time.Hour).UnixMilli(),
	}
	sig, err := SignArtifact(issuerKey, ad)
	if err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	body, _ := json.Marshal(SignedAttestation{Attestation: ad, Sig: sig})
	payload := MarketPayload{Kind: KindAttestation, Body: body}

	// 1. Canonical CBOR nkt1 kind=4 round-trip.
	enc, err := EncodePayload(payload)
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	dec, err := DecodePayload(enc)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if dec.Kind != KindAttestation {
		t.Fatalf("expected kind %q, got %q", KindAttestation, dec.Kind)
	}

	// 2. ValidateMarketPayload accepts valid attestation on ops or dir lane.
	topic := LaneOf("memory", "ops")
	if err := ValidateMarketPayload(dec, "memory", topic, now); err != nil {
		t.Fatalf("ValidateMarketPayload valid: %v", err)
	}

	// 3. Reject self-rating attestation.
	selfAd := ad
	selfAd.SubjectPub = issuerPub
	selfSig, _ := SignArtifact(issuerKey, selfAd)
	selfBody, _ := json.Marshal(SignedAttestation{Attestation: selfAd, Sig: selfSig})
	if err := ValidateMarketPayload(MarketPayload{Kind: KindAttestation, Body: selfBody}, "memory", topic, now); err == nil {
		t.Fatal("expected self-rating attestation to be rejected")
	}

	// 4. ls_nektar indexes and filters KindAttestation by SubjectPub and OntologyID.
	ls := NewMarketLookupService(nil, nil)
	lock := &script.Script{}
	_ = lock.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = lock.AppendPushData(enc)
	if err := ls.OutputAdmittedByTopic(context.Background(), &module.OutputAdmittedByTopic{
		Topic:         topic,
		Outpoint:      transaction.Outpoint{Index: 0},
		LockingScript: lock,
	}); err != nil {
		t.Fatalf("OutputAdmittedByTopic: %v", err)
	}

	qRaw, _ := json.Marshal(MarketQuery{
		Kind:       KindAttestation,
		SubjectPub: subjectPub,
		OntologyID: hex.EncodeToString(ont[:]),
	})
	ans, err := ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: LookupService, Query: qRaw})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	evs, _ := ans.Result.([]MarketEvent)
	if len(evs) != 1 || evs[0].Payload.Kind != KindAttestation {
		t.Fatalf("expected 1 KindAttestation event from lookup, got %d", len(evs))
	}
}
