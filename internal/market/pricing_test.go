package market

import "testing"

func TestCalibratedTokenCounter(t *testing.T) {
	tc := DefaultTokenCounter()
	if got := tc.CountTokens(""); got != 0 {
		t.Fatalf("empty string tokens = %d, want 0", got)
	}
	sample := "Fix Go 1.27 modernc.org/sqlite SQLITE_BUSY by enabling WAL mode and setting SetMaxOpenConns(1)."
	n := tc.CountTokens(sample)
	if n < 15 || n > 45 {
		t.Fatalf("unexpected token estimate %d for %q", n, sample)
	}
}

func TestComputeAutoBidSats_DeterministicArbitrageAndPolicyCap(t *testing.T) {
	// Saving 30,000 reasoning tokens @ 30 sats/1k = 900 sats value; alpha=30% -> 270 sats; high rep (~94%) -> ~253 sats.
	hi := ComputeAutoBidSats(ArbitrageBidInput{
		EstTokensSaved:     30000,
		SatPer1kTokens:     30,
		AlphaBP:            3000,
		SellerCompositeBP:  9500,
		SellerEigenTrustBP: 9000,
		SellerBonded:       true,
		MaxPolicyDealSats:  500,
	})
	if hi.RawSavingsSats != 900 {
		t.Fatalf("RawSavingsSats = %d, want 900", hi.RawSavingsSats)
	}
	if hi.MaxBidSats < 200 || hi.MaxBidSats > 270 {
		t.Fatalf("expected high-rep arbitrage bid in [200..270], got %+v", hi)
	}
	if hi.CappedByPolicy {
		t.Fatal("should not be capped by 500 sat policy cap")
	}

	// Unrated unbonded seller receives strictly lower bid than high-reputation seller.
	lo := ComputeAutoBidSats(ArbitrageBidInput{
		EstTokensSaved:    30000,
		SatPer1kTokens:    30,
		AlphaBP:           3000,
		MaxPolicyDealSats: 500,
	})
	if lo.MaxBidSats >= hi.MaxBidSats {
		t.Fatalf("expected unrated bid (%d) < high-rep bid (%d)", lo.MaxBidSats, hi.MaxBidSats)
	}

	// Strict policy ceiling cap is never exceeded.
	capped := ComputeAutoBidSats(ArbitrageBidInput{
		EstTokensSaved:     100000,
		SatPer1kTokens:     50,
		AlphaBP:            5000,
		SellerCompositeBP:  10000,
		SellerEigenTrustBP: 10000,
		MaxPolicyDealSats:  120,
	})
	if capped.MaxBidSats != 120 || !capped.CappedByPolicy {
		t.Fatalf("expected strict policy cap at 120, got %+v", capped)
	}
}

func TestDynamicPriceController_RatchetDecayAndPriorityDiscount(t *testing.T) {
	st := DynamicPriceState{
		Enabled:          true,
		FloorSats:        20,
		CeilingSats:      200,
		CurrentPriceSats: 100,
		DecayIntervalSec: 3600,
	}

	// 1. Demand hits ratchet price upward up to ceiling.
	now := int64(1_700_000_000)
	p1 := st.OnDemandHit(100, 9500, now)
	if p1 <= 100 {
		t.Fatalf("expected ratchet up above 100, got %d", p1)
	}
	for i := 0; i < 30; i++ {
		st.OnDemandHit(100, 9500, now+int64(i))
	}
	if st.CurrentPriceSats != 200 {
		t.Fatalf("expected price clamped at ceiling 200, got %d", st.CurrentPriceSats)
	}

	// 2. Priority buyer gets up to 15% discount, standard buyer pays full price.
	stdAsk := st.QuotePriceForBuyer(100, false, 5000)
	prioAsk := st.QuotePriceForBuyer(100, true, 10000)
	if stdAsk != 200 || prioAsk != 170 {
		t.Fatalf("unexpected priority discount: std=%d prio=%d (want 200, 170)", stdAsk, prioAsk)
	}

	// 3. Idle intervals decay price smoothly down toward floor (never below floor).
	changed := st.ApplyIdleDecay(100, now+30+3600*3)
	if !changed || st.CurrentPriceSats >= 200 {
		t.Fatalf("expected downward decay after 3 idle intervals, got changed=%v price=%d", changed, st.CurrentPriceSats)
	}
	st.ApplyIdleDecay(100, now+30+3600*50)
	if st.CurrentPriceSats < 20 {
		t.Fatalf("price decayed below floor 20: %d", st.CurrentPriceSats)
	}
}
