package nmu

import (
	"errors"
	"fmt"
	"math"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/reasoning"
)

func TestComputeSatsPer1kUnitsAndTotal(t *testing.T) {
	// Sub-satoshi precision: 10 sats / 20 triples = 0.500 sats/triple = 500 satsPer1kUnits.
	if got := ComputeSatsPer1kUnits(10, 20); got != 500 {
		t.Fatalf("ComputeSatsPer1kUnits(10, 20) = %d, want 500", got)
	}
	if got := FormatSatsPerTriple(500); got != "0.500" {
		t.Fatalf("FormatSatsPerTriple(500) = %q, want 0.500", got)
	}
	// Very cheap huge pack floors to 1 milli-sat instead of 0.
	if got := ComputeSatsPer1kUnits(1, 4096); got != 1 {
		t.Fatalf("ComputeSatsPer1kUnits(1, 4096) = %d, want 1", got)
	}
	// Free stays 0.
	if got := ComputeSatsPer1kUnits(0, 10); got != 0 {
		t.Fatalf("ComputeSatsPer1kUnits(0, 10) = %d, want 0", got)
	}
	// Overflow-safe large value doesn't wrap around.
	if got := ComputeSatsPer1kUnits(math.MaxUint64/2, 1); got != math.MaxUint64 {
		t.Fatalf("overflow saturation expected MaxUint64, got %d", got)
	}
	// Total sats calculation rounds up cleanly.
	if got := ComputeTotalSatsForUnits(500, 20); got != 10 {
		t.Fatalf("ComputeTotalSatsForUnits(500, 20) = %d, want 10", got)
	}
	if got := ComputeTotalSatsForUnits(500, 1); got != 1 {
		t.Fatalf("ComputeTotalSatsForUnits(500, 1) = %d, want 1", got)
	}
}

func TestCountVerifiedTriplesAntiStuffing(t *testing.T) {
	allowed := map[string]struct{}{
		"mitigates": {},
		"affects":   {},
	}
	var triples []reasoning.Triple
	// 1. Exact duplicate pair (should count once).
	triples = append(triples,
		reasoning.Triple{Subject: "fix:utxo-split", Predicate: "mitigates", Object: "err:tx-mempool-conflict"},
		reasoning.Triple{Subject: "fix:utxo-split", Predicate: "mitigates", Object: "err:tx-mempool-conflict"},
	)
	// 2. Semantic stuffing with unknown predicate not in OntologyManifest (must be dropped).
	triples = append(triples,
		reasoning.Triple{Subject: "fix:utxo-split", Predicate: "random_junk_pred", Object: "val1"},
		reasoning.Triple{Subject: "fix:utxo-split", Predicate: "padding_note", Object: "val2"},
	)
	// 3. Per-(subject, predicate) stuffing: 12 distinct objects on same (S, P) pair -> capped at 8!
	for i := range 12 {
		triples = append(triples, reasoning.Triple{
			Subject:   "cve:2026-0001",
			Predicate: "affects",
			Object:    fmt.Sprintf("pkg:lib-%d", i),
		})
	}
	// 4. Invalid empty triple (dropped).
	triples = append(triples, reasoning.Triple{Subject: "", Predicate: "affects", Object: "x"})

	res := CountVerifiedTriples(triples, TripleCountOptions{AllowedPredicates: allowed})
	if res.UnitCount != 9 { // 1 from mitigates + 8 capped from affects
		t.Fatalf("expected 9 verified units (1 + 8 capped), got %+v", res)
	}
	if res.DuplicateDropped != 1 {
		t.Fatalf("expected 1 exact duplicate dropped, got %d", res.DuplicateDropped)
	}
	if res.UnknownPredDropped != 2 {
		t.Fatalf("expected 2 unknown-predicate triples dropped, got %d", res.UnknownPredDropped)
	}
	if res.PerPairCappedDropped != 4 {
		t.Fatalf("expected 4 per-pair overflow triples dropped, got %d", res.PerPairCappedDropped)
	}
	if res.InvalidDropped != 1 {
		t.Fatalf("expected 1 invalid triple dropped, got %d", res.InvalidDropped)
	}
}

func TestClassifyAndVerifyDeliveredGrade(t *testing.T) {
	ontID := "1111111111111111111111111111111111111111111111111111111111111111"
	edgesRoot := "2222222222222222222222222222222222222222222222222222222222222222"
	allowed := map[string]struct{}{"mitigates": {}}
	triples := []reasoning.Triple{
		{Subject: "fix:1", Predicate: "mitigates", Object: "err:1"},
		{Subject: "fix:2", Predicate: "mitigates", Object: "err:2"},
	}

	// Grade C: text only.
	c := ClassifyFragmentGrade(FragmentGradeInput{HasCleanText: true})
	if c.Grade != GradeC || c.UnitCount != 1 {
		t.Fatalf("expected Grade C / 1 unit, got %+v", c)
	}

	// Grade B: distilled + pinned ontology + Mangle consistent + verified triples.
	b := ClassifyFragmentGrade(FragmentGradeInput{
		HasCleanText:      true,
		Distilled:         true,
		OntologyID:        ontID,
		Triples:           triples,
		AllowedPredicates: allowed,
		MangleConsistent:  true,
	})
	if b.Grade != GradeB || b.UnitCount != 2 {
		t.Fatalf("expected Grade B / 2 units, got %+v", b)
	}

	// Grade A: Grade B + L1 MPE edgesRoot + active bond.
	a := ClassifyFragmentGrade(FragmentGradeInput{
		HasCleanText:           true,
		Distilled:              true,
		OntologyID:             ontID,
		Triples:                triples,
		AllowedPredicates:      allowed,
		MangleConsistent:       true,
		EdgesRoot:              edgesRoot,
		MPEHasOntologyAndEdges: true,
		ActiveBond:             true,
	})
	if a.Grade != GradeA || a.UnitCount != 2 {
		t.Fatalf("expected Grade A / 2 units, got %+v", a)
	}

	// Grade S: Grade A + live SSE supersession FeedURL.
	s := ClassifyFragmentGrade(FragmentGradeInput{
		HasCleanText:           true,
		Distilled:              true,
		OntologyID:             ontID,
		Triples:                triples,
		AllowedPredicates:      allowed,
		MangleConsistent:       true,
		EdgesRoot:              edgesRoot,
		MPEHasOntologyAndEdges: true,
		ActiveBond:             true,
		LiveFeedURL:            "https://seller.example/v1/market/feeds/market-ops",
	})
	if s.Grade != GradeS || s.UnitCount != 2 {
		t.Fatalf("expected Grade S / 2 units, got %+v", s)
	}

	// Post-reveal verification: honest Grade A delivery passes cleanly.
	claimedA := Spec{
		Version:        SpecVersion,
		Grade:          GradeA,
		UnitCount:      2,
		SatsPer1kUnits: ComputeSatsPer1kUnits(20, 2),
		FreshnessClass: FreshnessVersioned,
		UsageRights:    RightsDistillResellable,
		PricingMode:    ModeSpot,
	}
	if err := ValidateOfferSpec(claimedA, 20); err != nil {
		t.Fatalf("ValidateOfferSpec honest Grade A: %v", err)
	}
	okVer := VerifyDeliveredGrade(claimedA, DeliveryObservation{
		HasCleanText:           true,
		DistilledOrStructured:  true,
		OntologyID:             ontID,
		Triples:                triples,
		AllowedPredicates:      allowed,
		MangleConsistent:       true,
		EdgesRoot:              edgesRoot,
		SPVVerified:            true,
		MPEHasOntologyAndEdges: true,
		ActiveBondVerified:     true,
		NowUnix:                1700000000,
	})
	if !okVer.OK || okVer.GradeMismatch || okVer.QuarantineEdges || okVer.AchievedGrade != GradeA {
		t.Fatalf("expected clean Grade A verify, got %+v", okVer)
	}

	// Dishonest seller claims Grade A / 2 units, delivers Grade C plain text (or unknown predicates).
	badVer := VerifyDeliveredGrade(claimedA, DeliveryObservation{
		HasCleanText:          true,
		DistilledOrStructured: true,
		OntologyID:            ontID,
		Triples: []reasoning.Triple{
			{Subject: "a", Predicate: "unknown_junk", Object: "b"},
			{Subject: "c", Predicate: "unknown_junk", Object: "d"},
		},
		AllowedPredicates: allowed,
		MangleConsistent:  true,
		SPVVerified:       true,
		NowUnix:           1700000000,
	})
	if badVer.OK || !badVer.GradeMismatch || !badVer.QuarantineEdges {
		t.Fatalf("expected grade mismatch + default quarantine on stuffed unknown predicates, got %+v", badVer)
	}
	if badVer.AchievedGrade != GradeC {
		t.Fatalf("expected achieved Grade C, got %s", badVer.AchievedGrade)
	}
	if badVer.SlashAccCapBP != 2000 || badVer.SlashDispCapBP != 2500 || badVer.SlashProvCapBP != 2500 {
		t.Fatalf("unexpected slashing caps: %+v", badVer)
	}
}

func TestValidateOfferSpecRejectsTamperedRatesAndDeadGradeS(t *testing.T) {
	// Spoofed satsPer1kUnits must fail closed.
	err := ValidateOfferSpec(Spec{
		Grade:          GradeB,
		UnitCount:      2,
		SatsPer1kUnits: 1, // actual is 50000
	}, 100)
	if err == nil {
		t.Fatal("expected tampered satsPer1kUnits to fail validation")
	}

	// Grade S without FeedURL must fail closed.
	err = ValidateOfferSpec(Spec{
		Grade:          GradeS,
		UnitCount:      2,
		SatsPer1kUnits: ComputeSatsPer1kUnits(100, 2),
	}, 100)
	if err == nil {
		t.Fatal("expected Grade S without feedUrl to fail validation")
	}
}

func TestFeedAuthReplayWindowAndBinding(t *testing.T) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1750000000)
	nonce := "aabbccddeeff001122334455"
	aud := "node.example:18420"
	pubHex, sigHex, err := SignFeedAuth(priv, "market-ops", "sub-123", now, nonce, aud)
	if err != nil {
		t.Fatalf("SignFeedAuth: %v", err)
	}

	// Exact time and +/-60s boundaries pass.
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now, nonce, aud); err != nil {
		t.Fatalf("VerifyFeedAuth exact time: %v", err)
	}
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now+60, nonce, aud); err != nil {
		t.Fatalf("VerifyFeedAuth +60s boundary: %v", err)
	}
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now-60, nonce, aud); err != nil {
		t.Fatalf("VerifyFeedAuth -60s boundary: %v", err)
	}

	// Outside +/-60s MUST fail with ErrFeedAuthExpired (blocks captured-header replay).
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now+61, nonce, aud); !errors.Is(err, ErrFeedAuthExpired) {
		t.Fatalf("expected ErrFeedAuthExpired at +61s, got %v", err)
	}
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now-61, nonce, aud); !errors.Is(err, ErrFeedAuthExpired) {
		t.Fatalf("expected ErrFeedAuthExpired at -61s, got %v", err)
	}

	// Cross-shard, cross-subscription, cross-nonce, or cross-host replay MUST fail.
	if err := VerifyFeedAuth(pubHex, sigHex, "market-other", "sub-123", now, now, nonce, aud); !errors.Is(err, ErrFeedAuthInvalid) {
		t.Fatalf("expected ErrFeedAuthInvalid on cross-shard replay, got %v", err)
	}
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-999", now, now, nonce, aud); !errors.Is(err, ErrFeedAuthInvalid) {
		t.Fatalf("expected ErrFeedAuthInvalid on cross-subId replay, got %v", err)
	}
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now, "other-nonce", aud); !errors.Is(err, ErrFeedAuthInvalid) {
		t.Fatalf("expected ErrFeedAuthInvalid on nonce mismatch, got %v", err)
	}
	if err := VerifyFeedAuth(pubHex, sigHex, "market-ops", "sub-123", now, now, nonce, "evil.example:18420"); !errors.Is(err, ErrFeedAuthInvalid) {
		t.Fatalf("expected ErrFeedAuthInvalid on cross-host replay, got %v", err)
	}

	var cache FeedNonceCache
	if !cache.Claim(pubHex, nonce, now) {
		t.Fatal("first nonce claim should succeed")
	}
	if cache.Claim(pubHex, nonce, now+10) {
		t.Fatal("duplicate nonce claim within replay window must fail")
	}
}
