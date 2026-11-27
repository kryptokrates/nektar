package store

import (
	"testing"
	"time"
)

func TestStoreMigrationsV6V7V8(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// v6: distill cursors, quarantine, fragment distilled/ontology_id, sell_listings allow_raw/dynamic_pricing_json.
	if err := db.SetDistillCursor("work", 5); err != nil {
		t.Fatalf("SetDistillCursor: %v", err)
	}
	if got, err := db.GetDistillCursor("work"); err != nil || got != 5 {
		t.Fatalf("GetDistillCursor = %d, %v", got, err)
	}
	if err := db.RecordDistillQuarantine(DistillQuarantineRecord{
		Shard: "work", Seq: 3, RuleID: "openai-api-key", ContentHash: "deadbeef",
	}); err != nil {
		t.Fatalf("RecordDistillQuarantine: %v", err)
	}
	if n, err := db.CountDistillQuarantine(); err != nil || n != 1 {
		t.Fatalf("CountDistillQuarantine = %d, %v", n, err)
	}

	rec, _, err := db.AppendFragment(FragmentRecord{
		Shard:      "market-ops",
		HDP:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Leaf:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Data:       []byte("distilled ops fact"),
		Provenance: []byte("{}"),
		Public:     true,
		Distilled:  true,
		OntologyID: "ont-ops-1",
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("AppendFragment: %v", err)
	}
	gotFrag, err := db.GetFragment("market-ops", rec.Seq)
	if err != nil || !gotFrag.Distilled || gotFrag.OntologyID != "ont-ops-1" {
		t.Fatalf("GetFragment distilled/ontology mismatch: %+v err=%v", gotFrag, err)
	}

	if err := db.SaveSellListing(SellListing{
		Shard:              "market-ops",
		Domains:            []string{"ops"},
		PriceSats:          50,
		OntologyID:         "ont-ops-1",
		AllowRaw:           false,
		DynamicPricingJSON: []byte(`{"enabled":true,"floorSats":25,"ceilingSats":200,"currentPriceSats":50}`),
		Active:             true,
	}); err != nil {
		t.Fatalf("SaveSellListing: %v", err)
	}
	listings, err := db.ListSellListings()
	if err != nil || len(listings) != 1 || listings[0].OntologyID != "ont-ops-1" || len(listings[0].DynamicPricingJSON) == 0 {
		t.Fatalf("ListSellListings mismatch: %+v err=%v", listings, err)
	}

	// v7: 6D reputation attestations, vectors, and outcomes.
	if err := db.SaveReputationAttestation(ReputationAttestationRow{
		AttestHash:      "1111",
		Role:            "buyer_rates_seller",
		IssuerPub:       "02aa",
		SubjectPub:      "02bb",
		OntologyID:      "ont-ops-1",
		DealID:          "deal-1",
		DealReceiptHash: "2222",
		Token:           "nektarrep1.abc",
		AccBP:           9000,
		CompositeBP:     8800,
	}); err != nil {
		t.Fatalf("SaveReputationAttestation: %v", err)
	}
	if err := db.UpsertReputation6D(Reputation6DRow{
		SubjectPub:   "02bb",
		OntologyID:   "ont-ops-1",
		AccBP:        9000,
		EigenTrustBP: 8500,
		CompositeBP:  8800,
		DirectDeals:  1,
		AttestCount:  1,
	}); err != nil {
		t.Fatalf("UpsertReputation6D: %v", err)
	}
	v, err := db.GetReputation6D("02bb", "ont-ops-1")
	if err != nil || v.CompositeBP != 8800 || v.EigenTrustBP != 8500 {
		t.Fatalf("GetReputation6D mismatch: %+v err=%v", v, err)
	}
	if err := db.RecordDealOutcome(DealOutcomeRow{DealID: "deal-1", SubjectPub: "02bb", OntologyID: "ont-ops-1", ExitCode: 0}); err != nil {
		t.Fatalf("RecordDealOutcome: %v", err)
	}

	// v8: economics events & summary.
	_ = db.RecordEconomicsEvent(EconomicsEventRow{Kind: "tokens_saved", TokensSaved: 30000})
	_ = db.RecordEconomicsEvent(EconomicsEventRow{Kind: "deal_buy", SatsDelta: -50})
	_ = db.RecordEconomicsEvent(EconomicsEventRow{Kind: "deal_sell", SatsDelta: 120})
	_ = db.RecordEconomicsEvent(EconomicsEventRow{Kind: "work_earn", SatsDelta: 40})
	sum, err := db.SummarizeEconomics(0)
	if err != nil {
		t.Fatalf("SummarizeEconomics: %v", err)
	}
	if sum.TokensSaved != 30000 || sum.EarnedSats != 160 || sum.SpentSats != 50 || sum.NetSats != 110 || sum.FactsBought != 1 || sum.FactsSold != 1 || sum.WorkBounties != 1 {
		t.Fatalf("unexpected economics summary: %+v", sum)
	}
}
