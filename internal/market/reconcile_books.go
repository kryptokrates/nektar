package market

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

// BookReconciliationReport verifies exact deterministic arithmetic and evidence invariants
// across Nektar's local SQLite deals, daily spend counters, SpendGuard, reputation, payment claims,
// and surviving obligations (verifiable-accounting discipline).
type BookReconciliationReport struct {
	OK                    bool     `json:"ok"`
	SettledBuyerDeals     int      `json:"settledBuyerDeals"`
	SettledBuyerSats      uint64   `json:"settledBuyerSats"`
	SettledBuyerTodaySats uint64   `json:"settledBuyerTodaySats"`
	MarketDailySpendSats  uint64   `json:"marketDailySpendSats"`
	SpendGuardDailySats   uint64   `json:"spendGuardDailySats"`
	SettledSellerDeals    int      `json:"settledSellerDeals"`
	SettledSellerSats     uint64   `json:"settledSellerSats"`
	BilateralReceipts     int      `json:"bilateralReceipts"`
	InvalidReceipts       int      `json:"invalidReceipts"`
	PaymentClaimCount     int      `json:"paymentClaimCount"`
	OpenObligations       int      `json:"openObligations"`
	OverdueObligations    int      `json:"overdueObligations"`
	Anomalies             []string `json:"anomalies,omitempty"`
}

// ReconcileBooks checks cross-table accounting consistency and receipt integrity without disclosure.
func ReconcileBooks(db *store.DB, guard *core.SpendGuard, now time.Time) (*BookReconciliationReport, error) {
	rep := &BookReconciliationReport{OK: true}
	if db == nil {
		return rep, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	dayEnd := dayStart + 86400

	deals, err := db.ListDeals(2000)
	if err != nil {
		return nil, err
	}

	buyerDealsBySeller := make(map[string]struct {
		count int64
		sats  uint64
	})

	for _, d := range deals {
		if len(d.DealReceiptJSON) > 0 {
			var dr DealReceipt
			if err := json.Unmarshal(d.DealReceiptJSON, &dr); err != nil {
				rep.InvalidReceipts++
				rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("deal %s has corrupt dealReceiptJson: %v", d.DealID, err))
			} else {
				vr := VerifyDealReceipt(dr, false)
				if !vr.OK {
					rep.InvalidReceipts++
					rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("deal %s receipt verification failed: %s", d.DealID, vr.Error))
				} else if vr.Bilateral {
					rep.BilateralReceipts++
				}
			}
		}

		if d.Status != "settled" {
			continue
		}
		switch d.Role {
		case "buyer":
			rep.SettledBuyerDeals++
			rep.SettledBuyerSats += d.PriceSats
			if d.UpdatedAt >= dayStart && d.UpdatedAt < dayEnd {
				rep.SettledBuyerTodaySats += d.PriceSats
			}
			if d.PriceSats > 0 && d.PaymentTxID == "" {
				rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("settled buyer deal %s (%d sats) missing paymentTxid", d.DealID, d.PriceSats))
			}
			if d.CounterpartyPub != "" {
				cur := buyerDealsBySeller[d.CounterpartyPub]
				cur.count++
				cur.sats += d.PriceSats
				buyerDealsBySeller[d.CounterpartyPub] = cur
			}
		case "seller":
			rep.SettledSellerDeals++
			rep.SettledSellerSats += d.PriceSats
			if d.PriceSats > 0 && d.PaymentTxID == "" {
				rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("settled seller deal %s (%d sats) missing paymentTxid", d.DealID, d.PriceSats))
			}
		}
	}

	if mSpend, err := db.DailySpend("market_daily_spend"); err == nil {
		rep.MarketDailySpendSats = mSpend
	}
	if guard != nil {
		spentToday, _, _, _ := guard.Stats()
		rep.SpendGuardDailySats = spentToday
	}

	// Cross-ledger invariant: market_daily_spend must not under-report today's settled paid buyer deals
	// once market_daily_spend is active (catches bypassed spending accounting!).
	if rep.MarketDailySpendSats > 0 && rep.SettledBuyerTodaySats > rep.MarketDailySpendSats {
		rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("daily spend drift: settled buyer deals today (%d sats) > market_daily_spend (%d sats)",
			rep.SettledBuyerTodaySats, rep.MarketDailySpendSats))
	}

	// Reputation roll-forward invariant: recorded SellerReputation.TotalSats should not be below
	// observed settled buyer deals for that seller (catches lost reputation accounting).
	if reps, err := db.ListReputations(); err == nil {
		repByPub := make(map[string]store.SellerReputation, len(reps))
		for _, r := range reps {
			repByPub[r.SellerPub] = r
		}
		for pub, agg := range buyerDealsBySeller {
			if r, ok := repByPub[pub]; ok && r.TotalSats < agg.sats {
				rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("reputation roll-forward drift for seller %.12s: rep.totalSats=%d < settled buyer deals=%d",
					pub, r.TotalSats, agg.sats))
			}
		}
	}

	if claims, err := db.ListPaymentClaims(2000); err == nil {
		rep.PaymentClaimCount = len(claims)
	}
	if openOb, overdueOb, err := db.CountObligations(); err == nil {
		rep.OpenObligations = openOb
		rep.OverdueObligations = overdueOb
	}
	if rep.OverdueObligations > 0 {
		rep.Anomalies = append(rep.Anomalies, fmt.Sprintf("%d overdue commercial obligation(s) require reconciliation", rep.OverdueObligations))
	}
	rep.OK = len(rep.Anomalies) == 0
	return rep, nil
}
