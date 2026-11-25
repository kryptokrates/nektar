package market

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kryptokrates/nektar/internal/store"
)

const (
	ObligationRevealOwed       = "reveal_owed"
	ObligationCountersigOwed   = "receipt_countersig_owed"
	ObligationProofUpgradeOwed = "proof_upgrade_owed"
	ObligationRefundClaimable  = "refund_claimable"
)

// ObligationID returns the deterministic idempotency key for a deal obligation.
func ObligationID(dealID, kind string) string {
	return "ob:" + dealID + ":" + kind
}

// RecordRevealOwed records a buyer's surviving commercial right to receive a BRC-369 content key
// after broadcasting payment (with deadlineUnix derived from DisclosureSale RefundAfter or default TTL).
func RecordRevealOwed(db *store.DB, dealID, sellerPub, txid string, amountSats uint64, deadlineUnix int64) error {
	if db == nil || dealID == "" {
		return nil
	}
	if deadlineUnix <= 0 {
		deadlineUnix = time.Now().Add(2 * time.Hour).Unix()
	}
	return db.UpsertObligation(store.ObligationRecord{
		ID:           ObligationID(dealID, ObligationRevealOwed),
		DealID:       dealID,
		Role:         "buyer",
		Kind:         ObligationRevealOwed,
		Counterparty: sellerPub,
		AmountSats:   amountSats,
		TxID:         txid,
		DeadlineUnix: deadlineUnix,
		Status:       "open",
	})
}

// FulfillRevealOwed marks a buyer's reveal_owed obligation fulfilled upon verified BRC-369 decryption.
func FulfillRevealOwed(db *store.DB, dealID string) error {
	if db == nil || dealID == "" {
		return nil
	}
	return db.FulfillObligation(ObligationID(dealID, ObligationRevealOwed))
}

// RecordCountersigOwed records a seller's open expectation for the buyer's bilateral DealReceipt countersignature.
func RecordCountersigOwed(db *store.DB, dealID, buyerPub, txid string, amountSats uint64, receiptJSON []byte) error {
	if db == nil || dealID == "" {
		return nil
	}
	return db.UpsertObligation(store.ObligationRecord{
		ID:           ObligationID(dealID, ObligationCountersigOwed),
		DealID:       dealID,
		Role:         "seller",
		Kind:         ObligationCountersigOwed,
		Counterparty: buyerPub,
		AmountSats:   amountSats,
		TxID:         txid,
		DeadlineUnix: time.Now().Add(24 * time.Hour).Unix(),
		Status:       "open",
		PayloadJSON:  receiptJSON,
	})
}

// FulfillCountersigOwed marks a seller's receipt_countersig_owed obligation fulfilled.
func FulfillCountersigOwed(db *store.DB, dealID string) error {
	if db == nil || dealID == "" {
		return nil
	}
	return db.FulfillObligation(ObligationID(dealID, ObligationCountersigOwed))
}

// RecordProofUpgradeOwed tracks a 0-conf settled deal/anchor awaiting mined BUMP confirmation.
func RecordProofUpgradeOwed(db *store.DB, dealID, role, counterparty, txid string, amountSats uint64) error {
	if db == nil || dealID == "" || txid == "" {
		return nil
	}
	return db.UpsertObligation(store.ObligationRecord{
		ID:           ObligationID(dealID, ObligationProofUpgradeOwed),
		DealID:       dealID,
		Role:         role,
		Kind:         ObligationProofUpgradeOwed,
		Counterparty: counterparty,
		AmountSats:   amountSats,
		TxID:         txid,
		DeadlineUnix: time.Now().Add(6 * time.Hour).Unix(),
		Status:       "open",
	})
}

// FulfillProofUpgradeOwed marks a proof_upgrade_owed obligation fulfilled once mined BUMP lands.
func FulfillProofUpgradeOwed(db *store.DB, dealID string) error {
	if db == nil || dealID == "" {
		return nil
	}
	return db.FulfillObligation(ObligationID(dealID, ObligationProofUpgradeOwed))
}

// ObligationReconcileReport summarizes one durable obligations reconciliation sweep.
type ObligationReconcileReport struct {
	Checked         int `json:"checked"`
	Fulfilled       int `json:"fulfilled"`
	MarkedOverdue   int `json:"markedOverdue"`
	RefundClaimable int `json:"refundClaimable"`
	StillOpen       int `json:"stillOpen"`
}

// ReconcileObligations sweeps open obligations across restarts/upgrades:
//   - fulfills reveal_owed if the corresponding deal is settled,
//   - transitions expired reveal_owed into overdue + creates a durable refund_claimable obligation,
//   - fulfills receipt_countersig_owed if the deal already holds a verified bilateral DealReceipt,
//   - fulfills proof_upgrade_owed when isTxSettled(txid) returns true (or anchor/deal confirmHeight > 0).
func ReconcileObligations(_ context.Context, db *store.DB, isTxSettled func(txid string) bool, nowUnix int64) (ObligationReconcileReport, error) {
	var rep ObligationReconcileReport
	if db == nil {
		return rep, nil
	}
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	openList, err := db.ListObligations("open", 500)
	if err != nil {
		return rep, err
	}
	for _, ob := range openList {
		rep.Checked++
		deal, dealErr := db.GetDeal(ob.DealID)

		switch ob.Kind {
		case ObligationRevealOwed:
			if dealErr == nil && deal.Status == "settled" {
				_ = db.FulfillObligation(ob.ID)
				rep.Fulfilled++
				continue
			}
			if ob.DeadlineUnix > 0 && nowUnix >= ob.DeadlineUnix {
				_ = db.UpdateObligationStatus(ob.ID, "overdue", fmt.Sprintf("seller reveal deadline %d elapsed without key delivery", ob.DeadlineUnix), true)
				_ = db.UpsertObligation(store.ObligationRecord{
					ID:           ObligationID(ob.DealID, ObligationRefundClaimable),
					DealID:       ob.DealID,
					Role:         "buyer",
					Kind:         ObligationRefundClaimable,
					Counterparty: ob.Counterparty,
					AmountSats:   ob.AmountSats,
					TxID:         ob.TxID,
					DeadlineUnix: ob.DeadlineUnix,
					Status:       "open",
					LastError:    "reveal deadline passed; eligible for escrow refund / dispute evidence",
				})
				rep.MarkedOverdue++
				rep.RefundClaimable++
				continue
			}
			rep.StillOpen++

		case ObligationCountersigOwed:
			if dealErr == nil && len(deal.DealReceiptJSON) > 0 {
				var rcp DealReceipt
				if json.Unmarshal(deal.DealReceiptJSON, &rcp) == nil && VerifyDealReceipt(rcp, true).Bilateral {
					_ = db.FulfillObligation(ob.ID)
					rep.Fulfilled++
					continue
				}
			}
			if ob.DeadlineUnix > 0 && nowUnix >= ob.DeadlineUnix {
				_ = db.UpdateObligationStatus(ob.ID, "overdue", "buyer bilateral receipt countersignature deadline elapsed", true)
				rep.MarkedOverdue++
				continue
			}
			rep.StillOpen++

		case ObligationProofUpgradeOwed:
			settled := false
			if dealErr == nil && deal.ConfirmHeight > 0 {
				settled = true
			}
			if !settled && ob.TxID != "" {
				if ok, _ := db.IsAnchorSettled(ob.TxID); ok {
					settled = true
				}
			}
			if !settled && ob.TxID != "" && isTxSettled != nil && isTxSettled(ob.TxID) {
				settled = true
			}
			if settled {
				_ = db.FulfillObligation(ob.ID)
				rep.Fulfilled++
				continue
			}
			if ob.DeadlineUnix > 0 && nowUnix >= ob.DeadlineUnix {
				_ = db.UpdateObligationStatus(ob.ID, "overdue", "mined BUMP proof upgrade overdue", true)
				rep.MarkedOverdue++
				continue
			}
			rep.StillOpen++

		default:
			if ob.DeadlineUnix > 0 && nowUnix >= ob.DeadlineUnix && ob.Kind != ObligationRefundClaimable {
				_ = db.UpdateObligationStatus(ob.ID, "overdue", "obligation deadline elapsed", true)
				rep.MarkedOverdue++
			} else {
				rep.StillOpen++
			}
		}
	}
	return rep, nil
}
