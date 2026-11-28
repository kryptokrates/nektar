package bstack

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/module"
)

func TestOntologyIDWireValidationAndLookup(t *testing.T) {
	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	ontA := "1111111111111111111111111111111111111111111111111111111111111111"
	ontB := "2222222222222222222222222222222222222222222222222222222222222222"
	leaf := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	edgesRoot := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	now := time.Now()

	// 1. Valid WantAd with OntologyID passes ValidateMarketPayload.
	want := WantAd{
		WantID:     "w-ont-1",
		Pub:        pubHex,
		Net:        "testnet",
		Domains:    []string{"ops"},
		Query:      "utxo stall mitigations",
		OntologyID: ontA,
		MaxSats:    500,
		Revision:   1,
		Exp:        now.Add(time.Hour).UnixMilli(),
	}
	wSig, err := SignArtifact(priv, want)
	if err != nil {
		t.Fatal(err)
	}
	wPayload, err := NewWantPayload(SignedWant{Want: want, Sig: wSig})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMarketPayload(wPayload, "testnet", LaneOf("testnet", "ops"), now); err != nil {
		t.Fatalf("ValidateMarketPayload valid want: %v", err)
	}

	// 2. Malformed OntologyID MUST fail validation.
	badWant := want
	badWant.OntologyID = "not-a-hex-hash"
	bwSig, _ := SignArtifact(priv, badWant)
	bwPayload, _ := NewWantPayload(SignedWant{Want: badWant, Sig: bwSig})
	if err := ValidateMarketPayload(bwPayload, "testnet", LaneOf("testnet", "ops"), now); err == nil {
		t.Fatal("expected invalid want ontologyId to fail validation")
	}

	// 3. Valid OfferAd with OntologyID + EdgesRoot and AnnounceAd with Bridge PackRef index cleanly in ls_nektar.
	offer := OfferAd{
		OfferID:    "off-ont-1",
		WantID:     "w-ont-1",
		Pub:        pubHex,
		BuyerPub:   pubHex,
		Net:        "testnet",
		Domains:    []string{"ops"},
		PriceSats:  100,
		Shard:      "ops",
		Leaf:       leaf,
		OntologyID: ontA,
		EdgesRoot:  edgesRoot,
		Revision:   1,
		Exp:        now.Add(time.Hour).UnixMilli(),
	}
	oSig, _ := SignArtifact(priv, offer)
	oPayload, _ := NewOfferPayload(SignedOffer{Offer: offer, Sig: oSig})
	oWire, err := EncodePayload(oPayload)
	if err != nil {
		t.Fatal(err)
	}

	ann := AnnounceAd{
		Pub:   pubHex,
		Net:   "testnet",
		Hosts: []string{"https://hub.example"},
		Packs: []PackRef{{
			Shard:            "bridge-11111111-22222222",
			Domains:          []string{"ontology-bridge"},
			ChirpURL:         "chirp://" + leaf,
			OntologyID:       ontA,
			TargetOntologyID: ontB,
			Count:            3,
		}},
		Exp: now.Add(time.Hour).UnixMilli(),
	}
	aSig, _ := SignArtifact(priv, ann)
	aPayload, _ := NewAnnouncePayload(SignedAnnounce{Announce: ann, Sig: aSig})
	aWire, err := EncodePayload(aPayload)
	if err != nil {
		t.Fatal(err)
	}

	ls := NewMarketLookupService(nil, nil)
	makeLock := func(payload []byte) *script.Script {
		s := &script.Script{}
		_ = s.AppendOpcodes(script.OpFALSE, script.OpRETURN)
		_ = s.AppendPushData(payload)
		return s
	}
	tx1, _ := chainhash.NewHashFromHex("1000000000000000000000000000000000000000000000000000000000000001")
	tx2, _ := chainhash.NewHashFromHex("2000000000000000000000000000000000000000000000000000000000000002")
	_ = ls.OutputAdmittedByTopic(context.Background(), &module.OutputAdmittedByTopic{
		Outpoint:      transaction.Outpoint{Txid: *tx1, Index: 0},
		Topic:         LaneOf("testnet", "ops"),
		LockingScript: makeLock(oWire),
	})
	_ = ls.OutputAdmittedByTopic(context.Background(), &module.OutputAdmittedByTopic{
		Outpoint:      transaction.Outpoint{Txid: *tx2, Index: 0},
		Topic:         LaneOf("testnet", DirDomain),
		LockingScript: makeLock(aWire),
	})

	// Lookup offers filtered by OntologyID.
	qOffer, _ := json.Marshal(MarketQuery{Kind: KindOffer, OntologyID: ontA})
	ans, err := ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: LookupService, Query: qOffer})
	if err != nil {
		t.Fatalf("Lookup offer by ontologyId: %v", err)
	}
	evs := ans.Result.([]MarketEvent)
	if len(evs) != 1 || evs[0].Payload.Kind != KindOffer {
		t.Fatalf("expected 1 offer matching ontA, got %+v", evs)
	}

	// Lookup bridge announcements filtered by (OntologyID, TargetOntologyID).
	qBridge, _ := json.Marshal(MarketQuery{Kind: KindAnnounce, OntologyID: ontA, TargetOntologyID: ontB})
	ansB, err := ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: LookupService, Query: qBridge})
	if err != nil {
		t.Fatalf("Lookup bridge by (ontA, ontB): %v", err)
	}
	evsB := ansB.Result.([]MarketEvent)
	if len(evsB) != 1 || evsB[0].Payload.Kind != KindAnnounce {
		t.Fatalf("expected 1 bridge announce matching (ontA -> ontB), got %+v", evsB)
	}

	// Non-matching TargetOntologyID returns 0 results.
	qMiss, _ := json.Marshal(MarketQuery{Kind: KindAnnounce, OntologyID: ontA, TargetOntologyID: ontA})
	ansMiss, _ := ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: LookupService, Query: qMiss})
	if len(ansMiss.Result.([]MarketEvent)) != 0 {
		t.Fatal("expected 0 results for mismatched targetOntologyId")
	}
}
