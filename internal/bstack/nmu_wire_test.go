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

	"github.com/kryptokrates/nektar/internal/nmu"
)

func TestNMUWireValidationAndLookup(t *testing.T) {
	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	ontA := "1111111111111111111111111111111111111111111111111111111111111111"
	leaf := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	edgesRoot := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	now := time.Now()

	// 1. Valid NMU-1 WantAd passes ValidateMarketPayload; invalid enum fails closed.
	want := WantAd{
		WantID:            "w-nmu-1",
		Pub:               pubHex,
		Net:               "testnet",
		Domains:           []string{"ops"},
		Query:             "go 1.27 utxo stalls",
		OntologyID:        ontA,
		MaxSats:           500,
		MinGrade:          "A",
		MaxSatsPer1kUnits: 5000,
		FreshnessClass:    "versioned",
		RequiredRights:    "distill_resellable",
		PricingMode:       "spot",
		Revision:          1,
		Exp:               now.Add(time.Hour).UnixMilli(),
	}
	wSig, _ := SignArtifact(priv, want)
	wPayload, _ := NewWantPayload(SignedWant{Want: want, Sig: wSig})
	if err := ValidateMarketPayload(wPayload, "testnet", LaneOf("testnet", "ops"), now); err != nil {
		t.Fatalf("valid NMU want rejected: %v", err)
	}

	badWant := want
	badWant.MinGrade = "Z"
	bwSig, _ := SignArtifact(priv, badWant)
	bwPayload, _ := NewWantPayload(SignedWant{Want: badWant, Sig: bwSig})
	if err := ValidateMarketPayload(bwPayload, "testnet", LaneOf("testnet", "ops"), now); err == nil {
		t.Fatal("expected invalid minGrade to fail validation")
	}

	// 2. Spoofed satsPer1kUnits on OfferAd fails closed; Grade S without feedUrl fails closed.
	offerA := OfferAd{
		OfferID:        "off-grade-a",
		WantID:         "w-nmu-1",
		Pub:            pubHex,
		BuyerPub:       pubHex,
		Net:            "testnet",
		Domains:        []string{"ops"},
		PriceSats:      50,
		Grade:          "A",
		UnitCount:      10,
		SatsPer1kUnits: nmu.ComputeSatsPer1kUnits(50, 10), // 5000
		FreshnessClass: "versioned",
		SubjectVersion: "go-1.27.4",
		UsageRights:    "distill_resellable",
		PricingMode:    "spot",
		Shard:          "market-ops",
		Leaf:           leaf,
		OntologyID:     ontA,
		EdgesRoot:      edgesRoot,
		Revision:       1,
		Exp:            now.Add(time.Hour).UnixMilli(),
	}
	oSig, _ := SignArtifact(priv, offerA)
	oPayload, _ := NewOfferPayload(SignedOffer{Offer: offerA, Sig: oSig})
	if err := ValidateMarketPayload(oPayload, "testnet", LaneOf("testnet", "ops"), now); err != nil {
		t.Fatalf("valid Grade A offer rejected: %v", err)
	}

	tamperedOffer := offerA
	tamperedOffer.SatsPer1kUnits = 100 // spoofed
	tSig, _ := SignArtifact(priv, tamperedOffer)
	tPayload, _ := NewOfferPayload(SignedOffer{Offer: tamperedOffer, Sig: tSig})
	if err := ValidateMarketPayload(tPayload, "testnet", LaneOf("testnet", "ops"), now); err == nil {
		t.Fatal("expected spoofed satsPer1kUnits offer to fail validation")
	}

	deadGradeS := offerA
	deadGradeS.Grade = "S"
	deadGradeS.FeedURL = ""
	dsSig, _ := SignArtifact(priv, deadGradeS)
	dsPayload, _ := NewOfferPayload(SignedOffer{Offer: deadGradeS, Sig: dsSig})
	if err := ValidateMarketPayload(dsPayload, "testnet", LaneOf("testnet", "ops"), now); err == nil {
		t.Fatal("expected Grade S without feedUrl to fail validation")
	}

	// 3. Multi-mode AnnounceAd + Grade A vs Grade C OfferAds in ls_nektar lookup filtering.
	offerC := offerA
	offerC.OfferID = "off-grade-c"
	offerC.Grade = "C"
	offerC.UnitCount = 1
	offerC.PriceSats = 40
	offerC.SatsPer1kUnits = nmu.ComputeSatsPer1kUnits(40, 1) // 40000
	offerC.UsageRights = "inference_only"
	ocSig, _ := SignArtifact(priv, offerC)
	ocPayload, _ := NewOfferPayload(SignedOffer{Offer: offerC, Sig: ocSig})

	ann := AnnounceAd{
		Pub:   pubHex,
		Net:   "testnet",
		Hosts: []string{"https://hub.example"},
		Packs: []PackRef{{
			Shard:                 "market-ops",
			Domains:               []string{"ops"},
			ChirpURL:              "chirp://" + leaf,
			OntologyID:            ontA,
			Grade:                 "S",
			UnitCount:             10,
			EnabledModes:          []string{"spot", "per_query", "subscription"},
			PriceSats:             50,
			SatsPer1kUnits:        5000,
			QueryPriceSats:        15,
			SubscriptionPriceSats: 250,
			SubscriptionSec:       7 * 86400,
			FreshnessClass:        "versioned",
			UsageRights:           "distill_resellable",
			QueryEndpoint:         "https://seller.example/v1/market/query",
			FeedEndpoint:          "https://seller.example/v1/market/feeds/market-ops",
		}},
		Exp: now.Add(time.Hour).UnixMilli(),
	}
	aSig, _ := SignArtifact(priv, ann)
	aPayload, _ := NewAnnouncePayload(SignedAnnounce{Announce: ann, Sig: aSig})
	if err := ValidateMarketPayload(aPayload, "testnet", LaneOf("testnet", DirDomain), now); err != nil {
		t.Fatalf("valid multi-mode Grade S announce rejected: %v", err)
	}

	oWireA, _ := EncodePayload(oPayload)
	oWireC, _ := EncodePayload(ocPayload)
	aWire, _ := EncodePayload(aPayload)

	ls := NewMarketLookupService(nil, nil)
	makeLock := func(payload []byte) *script.Script {
		s := &script.Script{}
		_ = s.AppendOpcodes(script.OpFALSE, script.OpRETURN)
		_ = s.AppendPushData(payload)
		return s
	}
	tx1, _ := chainhash.NewHashFromHex("3000000000000000000000000000000000000000000000000000000000000001")
	tx2, _ := chainhash.NewHashFromHex("3000000000000000000000000000000000000000000000000000000000000002")
	tx3, _ := chainhash.NewHashFromHex("3000000000000000000000000000000000000000000000000000000000000003")
	_ = ls.OutputAdmittedByTopic(context.Background(), &module.OutputAdmittedByTopic{
		Outpoint: transaction.Outpoint{Txid: *tx1, Index: 0}, Topic: LaneOf("testnet", "ops"), LockingScript: makeLock(oWireA),
	})
	_ = ls.OutputAdmittedByTopic(context.Background(), &module.OutputAdmittedByTopic{
		Outpoint: transaction.Outpoint{Txid: *tx2, Index: 0}, Topic: LaneOf("testnet", "ops"), LockingScript: makeLock(oWireC),
	})
	_ = ls.OutputAdmittedByTopic(context.Background(), &module.OutputAdmittedByTopic{
		Outpoint: transaction.Outpoint{Txid: *tx3, Index: 0}, Topic: LaneOf("testnet", DirDomain), LockingScript: makeLock(aWire),
	})

	// Query offers with MinGrade=A and MaxSatsPer1kUnits=10000 -> only offerA matches.
	q, _ := json.Marshal(MarketQuery{
		Kind:              KindOffer,
		MinGrade:          "A",
		UsageRights:       "distill_resellable",
		MaxSatsPer1kUnits: 10000,
	})
	ans, err := ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: LookupService, Query: q})
	if err != nil {
		t.Fatalf("Lookup NMU offers: %v", err)
	}
	evs := ans.Result.([]MarketEvent)
	if len(evs) != 1 {
		t.Fatalf("expected 1 Grade A offer under 10000 milli-sats, got %d", len(evs))
	}

	// Query multi-mode announce by PricingMode=subscription AND PricingMode=per_query.
	for _, mode := range []string{"spot", "per_query", "subscription"} {
		qm, _ := json.Marshal(MarketQuery{Kind: KindAnnounce, MinGrade: "S", PricingMode: mode})
		am, err := ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: LookupService, Query: qm})
		if err != nil {
			t.Fatalf("Lookup multi-mode %s: %v", mode, err)
		}
		if len(am.Result.([]MarketEvent)) != 1 {
			t.Fatalf("expected 1 announce for mode %s, got %d", mode, len(am.Result.([]MarketEvent)))
		}
	}
}
