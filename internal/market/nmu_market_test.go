package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestNMURankingUnitNormalizationAndGradeSDeadFeedGuard(t *testing.T) {
	now := int64(1750000000)
	want := bstack.WantAd{
		WantID:  "w-unit-1",
		Net:     "memory",
		Domains: []string{"ops"},
		MaxSats: 100,
	}

	// Offer 1: 1-triple Grade C raw note for 40 sats (40,000 satsPer1kUnits = 40 sats/triple).
	offC := bstack.OfferAd{
		OfferID:        "off-cheap-raw",
		WantID:         want.WantID,
		Pub:            "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Net:            "memory",
		Domains:        []string{"ops"},
		PriceSats:      40,
		Grade:          "C",
		UnitCount:      1,
		SatsPer1kUnits: nmu.ComputeSatsPer1kUnits(40, 1),
		FreshnessClass: "evergreen",
		UsageRights:    "inference_only",
	}
	// Offer 2: 10-triple Grade A FactPack for 50 sats (5,000 satsPer1kUnits = 5 sats/triple) with active bond.
	offA := bstack.OfferAd{
		OfferID:        "off-grade-a-pack",
		WantID:         want.WantID,
		Pub:            "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Net:            "memory",
		Domains:        []string{"ops"},
		PriceSats:      50,
		Grade:          "A",
		UnitCount:      10,
		SatsPer1kUnits: nmu.ComputeSatsPer1kUnits(50, 10),
		FreshnessClass: "versioned",
		UsageRights:    "distill_resellable",
		BondOutpoint:   "txid:0",
	}
	// Offer 3: Expired ephemeral offer (must be filtered out!).
	offExpired := offA
	offExpired.OfferID = "off-expired"
	offExpired.FreshnessClass = "ephemeral"
	offExpired.ValidUntilUnix = now - 10

	bonds := map[string]uint64{offA.OfferID: 10000}
	ranked := RankOffersWith6DReputationAt(want, []bstack.OfferAd{offC, offA, offExpired}, nil, nil, nil, bonds, now)
	if len(ranked) != 2 {
		t.Fatalf("expected 2 non-expired offers, got %d", len(ranked))
	}
	if ranked[0].Offer.OfferID != offA.OfferID {
		t.Fatalf("expected 10-triple Grade A pack (5 sats/triple) to beat 1-triple Grade C (40 sats/triple); got %+v", ranked)
	}

	// Dead-feed anti-gaming: Grade S seller with penalized DispBP < 5000 MUST NOT beat honest Grade S seller.
	honestS := offA
	honestS.OfferID = "off-s-honest"
	honestS.Pub = "02cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	honestS.Grade = "S"
	honestS.FeedURL = "https://honest.example/v1/market/feeds/market-ops"

	deadS := honestS
	deadS.OfferID = "off-s-deadfeed"
	deadS.Pub = "03dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	deadS.FeedURL = "https://dead.example/v1/market/feeds/market-ops"

	bondsS := map[string]uint64{honestS.OfferID: 10000, deadS.OfferID: 10000}
	rep6D := map[string]reputation.Vector6D{
		honestS.Pub: {SubjectPub: honestS.Pub, AttestCount: 3, AccBP: 9000, DispBP: 9500, CollabBP: 9000, CompositeBP: 9000, EigenTrustBP: 8000},
		deadS.Pub:   {SubjectPub: deadS.Pub, AttestCount: 3, AccBP: 8000, DispBP: 2500, CollabBP: 3000, CompositeBP: 6000, EigenTrustBP: 5000},
	}
	rankedS := RankOffersWith6DReputationAt(want, []bstack.OfferAd{deadS, honestS}, nil, nil, rep6D, bondsS, now)
	if len(rankedS) != 2 || rankedS[0].Offer.OfferID != honestS.OfferID {
		t.Fatalf("expected honest Grade S seller to outrank dead-feed penalized seller, got %+v", rankedS)
	}
	if rankedS[0].ScoreBP-rankedS[1].ScoreBP < 1500 {
		t.Fatalf("expected sharp score separation for dead-feed Grade S penalty: honest=%d dead=%d", rankedS[0].ScoreBP, rankedS[1].ScoreBP)
	}
}

func TestNMUUnitAutoBidAndPerUnitDynamicPricing(t *testing.T) {
	now := int64(1750000000)
	bidS := ComputeUnitAutoBid(UnitArbitrageBidInput{
		ArbitrageBidInput: ArbitrageBidInput{
			EstTokensSaved:    10000,
			SatPer1kTokens:    30,
			AlphaBP:           3000,
			SellerBonded:      true,
			MaxPolicyDealSats: 500,
		},
		TargetGrade:    nmu.GradeS,
		DesiredRights:  nmu.RightsDistillResellable,
		FreshnessClass: nmu.FreshnessVersioned,
		ExpectedUnits:  10,
		NowUnix:        now,
	})
	bidC := ComputeUnitAutoBid(UnitArbitrageBidInput{
		ArbitrageBidInput: ArbitrageBidInput{
			EstTokensSaved:    10000,
			SatPer1kTokens:    30,
			AlphaBP:           3000,
			SellerBonded:      true,
			MaxPolicyDealSats: 500,
		},
		TargetGrade:    nmu.GradeC,
		DesiredRights:  nmu.RightsInferenceOnly,
		FreshnessClass: nmu.FreshnessEphemeral,
		ValidUntilUnix: now + 86400, // 1 day left
		ExpectedUnits:  10,
		NowUnix:        now,
	})
	if bidS.MaxBidSats <= bidC.MaxBidSats || bidS.MaxSatsPer1kUnits <= bidC.MaxSatsPer1kUnits {
		t.Fatalf("expected Grade S resellable bid (%+v) > decaying Grade C bid (%+v)", bidS, bidC)
	}

	// PerUnitMode pricing: 5000 satsPer1kUnits (= 5 sats/triple) * 8 triples = 40 sats total.
	dp := DynamicPriceState{Enabled: true, PerUnitMode: true, FloorSats: 1000, CeilingSats: 20000, CurrentPriceSats: 5000}
	total, rate := dp.QuoteForFactPack(5000, 8, false, 0)
	if total != 40 || rate != 5000 {
		t.Fatalf("PerUnitMode QuoteForFactPack(5000, 8) = (%d, %d), want (40, 5000)", total, rate)
	}
}

func TestNMUDealReceiptBindingAndPostRevealGradeInflationSlashAndQuarantine(t *testing.T) {
	ctx := context.Background()
	sellerDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer sellerDB.Close()
	buyerDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer buyerDB.Close()

	sellerPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer sellerPort.Close()
	buyerPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer buyerPort.Close()

	sellerMem, err := memory.New(sellerDB, sellerPort, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	buyerMem, err := memory.New(buyerDB, buyerPort, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sellerEng := NewEngine(sellerMem, nil)
	buyerEng := NewEngine(buyerMem, nil)

	// Commit an ontology manifest on buyer so buyer enforces strict ontology-declared predicates!
	loadedOnt, err := buyerMem.CommitOntology(ctx, reasoning.DefaultOntologyManifest("ops"), false)
	if err != nil {
		t.Fatalf("CommitOntology: %v", err)
	}
	ontID := loadedOnt.Manifest.Hash

	// Dishonest seller commits plain text padded with unknown-predicate triples (NOT in ops ontology)
	// and falsely advertises Grade A, UnitCount=5!
	paddedText := "Quick unverified note.\n" +
		`triple("a", "junk_noise_pred", "1")` + "\n" +
		`triple("a", "junk_noise_pred", "2")` + "\n" +
		`triple("a", "junk_noise_pred", "3")`
	rem, err := sellerMem.Remember(ctx, memory.RememberInput{
		Shard:      "market-ops",
		Text:       paddedText,
		Public:     true,
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("seller Remember: %v", err)
	}
	pb, _, err := sellerMem.Prove(ctx, "market-ops", rem.Seq)
	if err != nil || pb == nil {
		t.Fatalf("seller Prove: %v pb=%+v", err, pb)
	}
	saleOffer, revealKey, chirpURL, err := sellerEng.PrepareDirectSale(ctx, "market-ops", rem.Seq, 50, buyerEng.salePubHex())
	if err != nil {
		t.Fatalf("PrepareDirectSale: %v", err)
	}

	// Seller signs DealReceipt binding the false Grade A / UnitCount=5 claim into MPE FieldsRoot.
	payTxID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	claimedSpec := &nmu.Spec{
		Version:        nmu.SpecVersion,
		Grade:          nmu.GradeA,
		UnitCount:      5,
		SatsPer1kUnits: nmu.ComputeSatsPer1kUnits(50, 5),
		FreshnessClass: nmu.FreshnessVersioned,
		UsageRights:    nmu.RightsDistillResellable,
		PricingMode:    nmu.ModeSpot,
	}
	dr := DealReceipt{
		Version:       1,
		DealID:        "deal-nmu-slash-1",
		OfferID:       "off-nmu-slash-1",
		BuyerPub:      buyerEng.salePubHex(),
		SellerPub:     sellerEng.salePubHex(),
		Domains:       []string{"ops"},
		PriceSats:     50,
		PaymentTxID:   payTxID,
		KeyCommitment: saleOffer.KeyCommitment,
		FragmentLeaf:  rem.Leaf,
		NMU:           claimedSpec,
		SettledAt:     time.Now().Unix(),
	}
	if err := SignDealReceiptSeller(ctx, sellerEng.salePubHex(), sellerEng.signSaleDigest, &dr); err != nil {
		t.Fatalf("SignDealReceiptSeller: %v", err)
	}
	// Verify selective disclosure of nmuGrade and nmuUnitCount from MPE FieldsRoot!
	sdp, err := dr.SelectiveDisclose([]string{"nmuGrade", "nmuUnitCount", "priceSats"})
	if err != nil {
		t.Fatalf("SelectiveDisclose NMU fields: %v", err)
	}
	if err := core.VerifySelectiveDisclosure(dr.FieldsRoot, sdp); err != nil {
		t.Fatalf("NMU selective disclosure verification failed: %v", err)
	}
	disclosed := make(map[string]string, len(sdp.Fields))
	for _, f := range sdp.Fields {
		disclosed[f.Key] = string(f.Value)
	}
	if disclosed["nmuGrade"] != "A" || disclosed["nmuUnitCount"] != "5" {
		t.Fatalf("NMU selective disclosure fields mismatch: %+v", disclosed)
	}

	// Run buyerVerifyAndCommit with seller's revealed key and inflated OfferAd.
	saleOfferJSON, _ := json.Marshal(saleOffer)
	chosen := bstack.OfferAd{
		OfferID:        dr.OfferID,
		WantID:         dr.DealID,
		Pub:            sellerEng.salePubHex(),
		BuyerPub:       buyerEng.salePubHex(),
		Net:            "memory",
		Domains:        []string{"ops"},
		PriceSats:      50,
		Grade:          "A",
		UnitCount:      5,
		SatsPer1kUnits: claimedSpec.SatsPer1kUnits,
		FreshnessClass: "versioned",
		UsageRights:    "distill_resellable",
		PricingMode:    "spot",
		Shard:          "market-ops",
		Leaf:           rem.Leaf,
		OntologyID:     ontID,
		EdgesRoot:      "2222222222222222222222222222222222222222222222222222222222222222",
		ChirpURL:       chirpURL,
		KeyCommitHex:   saleOffer.KeyCommitment,
		SaleOffer:      saleOfferJSON,
	}
	want := bstack.WantAd{
		WantID:     dr.DealID,
		Pub:        buyerEng.salePubHex(),
		Net:        "memory",
		Domains:    []string{"ops"},
		Query:      "ops fix",
		OntologyID: ontID,
		MaxSats:    100,
	}
	res, err := buyerEng.buyerVerifyAndCommit(ctx, dr.DealID, want, "market/ops", chosen, payTxID, nil, ClaimSignal{
		ContentKeyHex: hex.EncodeToString(revealKey[:]),
		CiphertextHex: saleOffer.CiphertextHex,
		ProofBundle:   pb,
		DealReceipt:   &dr,
	})
	if err != nil || res.FailedReason != "" {
		t.Fatalf("buyerVerifyAndCommit unexpected failure: err=%v reason=%q", err, res.FailedReason)
	}

	// 1. Paid plaintext IS stored for auditability, tagged with AchievedGrade=C and GradeMismatch=true!
	frags, err := buyerDB.ListFragments("market/ops")
	if err != nil || len(frags) != 1 {
		t.Fatalf("expected 1 stored fragment for audit, got %d err=%v", len(frags), err)
	}
	meta, err := buyerDB.GetFragmentNMUMeta("market/ops", frags[0].Seq)
	if err != nil || meta == nil {
		t.Fatalf("expected fragment_nmu_meta row, got %+v err=%v", meta, err)
	}
	if !meta.GradeMismatch || meta.Grade != "C" {
		t.Fatalf("expected AchievedGrade=C & GradeMismatch=true, got %+v", meta)
	}

	// 2. Quarantined by default: ZERO untrusted edges entered buyer's knowledge_edges!
	edges, err := buyerDB.ListKnowledgeEdges(store.KnowledgeEdgeFilter{Shard: "market/ops"})
	if err != nil || len(edges) != 0 {
		t.Fatalf("expected mismatched seller edges quarantined (0 rows), got %d err=%v", len(edges), err)
	}

	// 3. Automatic slashing nektarrep1.* attestation emitted with AccBP <= 2000, DispBP <= 2500, ProvBP <= 2500!
	atts, err := buyerDB.ListReputationAttestations(strings.ToLower(ontID), 10)
	if err != nil || len(atts) != 1 {
		t.Fatalf("expected 1 slashing reputation attestation, got %d err=%v", len(atts), err)
	}
	if atts[0].AccBP > 2000 || atts[0].DispBP > 2500 || atts[0].ProvBP > 2500 {
		t.Fatalf("expected slashed 6D scores (Acc<=2000, Disp<=2500, Prov<=2500), got %+v", atts[0])
	}
}
