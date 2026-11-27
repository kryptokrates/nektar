package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
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

// EscrowObligationPayload stores the full DisclosureSale escrow metadata inside a buyer's
// reveal_owed / refund_claimable obligation so restarts can autonomously extract a revealed
// content key from chain OR broadcast Refund() after RefundAfter without seller cooperation.
type EscrowObligationPayload struct {
	OfferID      string `json:"offerId"`
	EscrowTxID   string `json:"escrowTxid"`
	EscrowVout   uint32 `json:"escrowVout"`
	EscrowBEEF   string `json:"escrowBeefHex"`
	SellerPKH    string `json:"sellerPkh"`
	DepositorPKH string `json:"depositorPkh"`
	Price        uint64 `json:"price"`
	HWrapped     string `json:"hWrapped"`
	RefundAfter  uint64 `json:"refundAfter"`
	MaxFee       uint64 `json:"maxFee"`
	RefundTxID   string `json:"refundTxid,omitempty"`
}

func (e EscrowObligationPayload) toParams() (contracts.DisclosureSaleParams, error) {
	sellerPKH, err := hex.DecodeString(e.SellerPKH)
	if err != nil {
		return contracts.DisclosureSaleParams{}, err
	}
	depPKH, err := hex.DecodeString(e.DepositorPKH)
	if err != nil {
		return contracts.DisclosureSaleParams{}, err
	}
	hw, err := hex.DecodeString(e.HWrapped)
	if err != nil {
		return contracts.DisclosureSaleParams{}, err
	}
	p := contracts.DisclosureSaleParams{
		SellerPKH:    sellerPKH,
		Price:        e.Price,
		HWrapped:     hw,
		RefundAfter:  e.RefundAfter,
		MaxFee:       e.MaxFee,
		DepositorPKH: depPKH,
	}
	return p, p.Validate()
}

// RecordRevealOwed records a buyer's surviving commercial right to receive a BRC-369 content key
// after broadcasting payment (with deadlineUnix derived from DisclosureSale RefundAfter or default TTL).
func RecordRevealOwed(db *store.DB, dealID, sellerPub, txid string, amountSats uint64, deadlineUnix int64) error {
	return RecordRevealOwedWithEscrow(db, dealID, sellerPub, txid, amountSats, deadlineUnix, nil)
}

// RecordRevealOwedWithEscrow persists a reveal_owed obligation optionally carrying DisclosureSale
// escrow parameters for autonomous on-chain key extraction and Refund() recovery.
func RecordRevealOwedWithEscrow(db *store.DB, dealID, sellerPub, txid string, amountSats uint64, deadlineUnix int64, escrow *EscrowObligationPayload) error {
	if db == nil || dealID == "" {
		return nil
	}
	if deadlineUnix <= 0 {
		deadlineUnix = time.Now().Add(2 * time.Hour).Unix()
	}
	var payloadJSON []byte
	if escrow != nil {
		payloadJSON, _ = json.Marshal(escrow)
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
		PayloadJSON:  payloadJSON,
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

// ObligationReconcileReport summarizes one durable obligations reconciliation sweep.
type ObligationReconcileReport struct {
	Checked              int      `json:"checked"`
	Fulfilled            int      `json:"fulfilled"`
	MarkedOverdue        int      `json:"markedOverdue"`
	RefundClaimable      int      `json:"refundClaimable"`
	StillOpen            int      `json:"stillOpen"`
	KeysExtractedOnChain int      `json:"keysExtractedOnChain,omitempty"`
	ExtractedKeyHex      string   `json:"extractedKeyHex,omitempty"`
	ExtractedClaimTxID   string   `json:"extractedClaimTxid,omitempty"`
	RefundsBroadcast     int      `json:"refundsBroadcast,omitempty"`
	RefundsSettled       int      `json:"refundsSettled,omitempty"`
	RefundPendingTxID    string   `json:"refundPendingTxid,omitempty"`
	RefundTxIDs          []string `json:"refundTxids,omitempty"`
}

// ReconcileObligations sweeps open obligations across restarts/upgrades:
//   - fulfills reveal_owed if the corresponding deal is settled,
//   - transitions expired reveal_owed into overdue + creates a durable refund_claimable obligation,
//   - fulfills receipt_countersig_owed if the deal already holds a verified bilateral DealReceipt,
//   - fulfills proof_upgrade_owed when isTxSettled(txid) confirms a verified BUMP at SettleDepth.
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
					PayloadJSON:  ob.PayloadJSON,
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
			if isTxSettled != nil {
				// Live/wired path: require verified BUMP at SettleDepth via isTxSettled; never trust bare ConfirmHeight.
				if ob.TxID != "" && isTxSettled(ob.TxID) {
					settled = true
				}
			} else {
				// Offline/unit-test fallback when no chain verifier callback is supplied.
				if dealErr == nil && deal.ConfirmHeight > 0 {
					settled = true
				}
				if !settled && ob.TxID != "" {
					if ok, _ := db.IsAnchorSettled(ob.TxID); ok {
						settled = true
					}
				}
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

// ReconcileEscrowObligations executes autonomous on-chain DisclosureSale recovery for open buyer
// reveal_owed and refund_claimable obligations:
//  1. If the seller already spent the escrow UTXO via DisclosureSale.Buy(k) on-chain (even after
//     dropping the HTTP connection), extracts `k` via ExtractRevealedKeyFromBuyTx, stores it on the
//     buyer's deal record, and fulfills the obligation.
//  2. Otherwise, once RefundAfter has elapsed (block height or unix timestamp domain), constructs,
//     Chronicle-VM-verifies, and broadcasts DisclosureSale.Refund() returning funds to DepositorPKH,
//     and keeps the obligation open until the refund tx mines to SettleDepth!
func ReconcileEscrowObligations(ctx context.Context, db *store.DB, port *chain.Port, nowUnix int64) (ObligationReconcileReport, error) {
	return reconcileEscrowObligationsFiltered(ctx, db, port, "", "", nowUnix)
}

// ReconcileSingleEscrowObligation performs autonomous on-chain DisclosureSale recovery strictly for one
// escrowTxID (and optional dealID), preventing concurrent EscrowGuardian workflows from observing or
// claiming another escrow's refund or key extraction.
func ReconcileSingleEscrowObligation(ctx context.Context, db *store.DB, port *chain.Port, dealID, escrowTxID string, nowUnix int64) (ObligationReconcileReport, error) {
	return reconcileEscrowObligationsFiltered(ctx, db, port, strings.TrimSpace(dealID), strings.TrimSpace(escrowTxID), nowUnix)
}

func reconcileEscrowObligationsFiltered(ctx context.Context, db *store.DB, port *chain.Port, filterDealID, filterEscrowTxID string, nowUnix int64) (ObligationReconcileReport, error) {
	var rep ObligationReconcileReport
	if db == nil || port == nil {
		return rep, nil
	}
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	openList, err := db.ListObligations("open", 500)
	if err != nil {
		return rep, err
	}
	curHeight, _ := port.Height(ctx)
	for _, ob := range openList {
		if (ob.Kind != ObligationRevealOwed && ob.Kind != ObligationRefundClaimable) || len(ob.PayloadJSON) == 0 {
			continue
		}
		var esc EscrowObligationPayload
		if json.Unmarshal(ob.PayloadJSON, &esc) != nil || esc.EscrowTxID == "" || esc.EscrowBEEF == "" {
			continue
		}
		if filterEscrowTxID != "" && !strings.EqualFold(esc.EscrowTxID, filterEscrowTxID) {
			continue
		}
		if filterDealID != "" && filterEscrowTxID == "" && ob.DealID != filterDealID {
			continue
		}
		reconcileOneEscrow(ctx, db, port, ob, esc, curHeight, nowUnix, &rep)
	}
	return rep, nil
}

func reconcileOneEscrow(ctx context.Context, db *store.DB, port *chain.Port, ob store.ObligationRecord, esc EscrowObligationPayload, curHeight uint32, nowUnix int64, rep *ObligationReconcileReport) {
	rep.Checked++

	// 1. Check if Seller already spent the escrow on-chain via DisclosureSale.Buy(k)!
	// Fail closed on spender-lookup transport errors: never treat an outage as unspent or race a Refund().
	spendTx, vin, found, fErr := port.FindSpendingTx(ctx, esc.EscrowTxID, esc.EscrowVout)
	if fErr != nil {
		_ = db.UpdateObligationStatus(ob.ID, "open", "spender lookup unavailable: "+fErr.Error(), true)
		rep.StillOpen++
		return
	}
	if found && spendTx != nil {
		if revealedK, kErr := contracts.ExtractRevealedKeyFromBuyTx(spendTx, vin, esc.HWrapped); kErr == nil {
			keyHex := hex.EncodeToString(revealedK)
			if deal, dErr := db.GetDeal(ob.DealID); dErr == nil {
				deal.ClaimKeyHex = keyHex
				if deal.Status == "paid_awaiting_reveal" || deal.Status == "expired" || deal.Status == "failed" || deal.Status == "refund_broadcast" {
					deal.Status = "key_extracted_onchain"
					deal.Error = ""
				}
				_, _ = db.UpdateDealCAS(deal)
			}
			_ = db.FulfillObligation(ob.ID)
			_ = db.FulfillObligation(ObligationID(ob.DealID, ObligationRevealOwed))
			_ = db.FulfillObligation(ObligationID(ob.DealID, ObligationRefundClaimable))
			rep.Fulfilled++
			rep.KeysExtractedOnChain++
			if rep.ExtractedKeyHex == "" {
				rep.ExtractedKeyHex = keyHex
				rep.ExtractedClaimTxID = spendTx.TxID().String()
			}
			return
		}
	}

	// 1b. If a Refund() tx was already broadcast on a live network, wait until it mines to SettleDepth
	// before fulfilling the obligation and marking the deal refunded.
	if esc.RefundTxID != "" {
		refundSettled := port.Network() == "memory"
		if !refundSettled {
			mp, bh, ok, pErr := port.ProofChecked(ctx, esc.RefundTxID)
			if pErr == nil && ok && mp != nil {
				depth := port.SettleDepth()
				if depth <= 0 {
					depth = 1
				}
				if tip, tErr := port.Height(ctx); tErr == nil && tip >= bh && int(tip-bh)+1 >= depth {
					refundSettled = true
				}
			}
		}
		if refundSettled {
			if deal, dErr := db.GetDeal(ob.DealID); dErr == nil && deal.Status != "settled" {
				deal.Status = "refunded"
				deal.Error = "refund_txid=" + esc.RefundTxID
				_, _ = db.UpdateDealCAS(deal)
			}
			_ = db.FulfillObligation(ob.ID)
			_ = db.FulfillObligation(ObligationID(ob.DealID, ObligationRevealOwed))
			_ = db.FulfillObligation(ObligationID(ob.DealID, ObligationRefundClaimable))
			rep.Fulfilled++
			rep.RefundsSettled++
			rep.RefundTxIDs = append(rep.RefundTxIDs, esc.RefundTxID)
			return
		}
		rep.RefundPendingTxID = esc.RefundTxID
		rep.StillOpen++
		return
	}

	// 2. Check if RefundAfter has elapsed in its matching F-02 locktime domain.
	var lockTime uint32
	readyForRefund := false
	if esc.RefundAfter < 500_000_000 {
		if uint64(curHeight) >= esc.RefundAfter {
			lockTime = curHeight
			readyForRefund = true
		}
	} else {
		if uint64(nowUnix) >= esc.RefundAfter {
			lockTime = uint32(nowUnix)
			readyForRefund = true
		}
	}
	if !readyForRefund {
		rep.StillOpen++
		return
	}

	params, pErr := esc.toParams()
	if pErr != nil {
		_ = db.UpdateObligationStatus(ob.ID, "open", "invalid escrow params: "+pErr.Error(), true)
		rep.StillOpen++
		return
	}
	rawBEEF, dErr := hex.DecodeString(esc.EscrowBEEF)
	if dErr != nil {
		_ = db.UpdateObligationStatus(ob.ID, "open", "corrupt escrow BEEF hex", true)
		rep.StillOpen++
		return
	}
	_, escrowTx, _, bErr := guard.ParseBEEF(rawBEEF, guard.DefaultBound)
	if bErr != nil || escrowTx == nil {
		_ = db.UpdateObligationStatus(ob.ID, "open", "parse escrow BEEF failed", true)
		rep.StillOpen++
		return
	}

	refundTx, rErr := contracts.BuildDisclosureSaleRefundTx(
		ctx, escrowTx, esc.EscrowVout, params, lockTime, 0, port.SignCovenantSighash,
	)
	if rErr != nil {
		_ = db.UpdateObligationStatus(ob.ID, "open", "build Refund tx: "+rErr.Error(), true)
		rep.StillOpen++
		return
	}
	refundTxID, sErr := port.SubmitCovenantTx(ctx, refundTx)
	if sErr != nil {
		_ = db.UpdateObligationStatus(ob.ID, "open", "broadcast Refund tx: "+sErr.Error(), true)
		rep.StillOpen++
		return
	}
	_, _ = port.AdoptReceived(ctx, refundTx)
	if port.Network() == "memory" {
		if deal, dErr := db.GetDeal(ob.DealID); dErr == nil && deal.Status != "settled" {
			deal.Status = "refunded"
			deal.Error = "refund_txid=" + refundTxID
			_, _ = db.UpdateDealCAS(deal)
		}
		_ = db.FulfillObligation(ob.ID)
		_ = db.FulfillObligation(ObligationID(ob.DealID, ObligationRevealOwed))
		_ = db.FulfillObligation(ObligationID(ob.DealID, ObligationRefundClaimable))
		rep.Fulfilled++
		rep.RefundsBroadcast++
		rep.RefundsSettled++
		rep.RefundTxIDs = append(rep.RefundTxIDs, refundTxID)
		return
	}
	// On live networks, persist refundTxID and keep obligations open until mined to SettleDepth.
	esc.RefundTxID = refundTxID
	if updatedPayload, mErr := json.Marshal(esc); mErr == nil {
		ob.PayloadJSON = updatedPayload
		ob.LastError = "refund broadcast; awaiting mined BUMP confirmation"
		_ = db.UpsertObligation(ob)
	}
	if deal, dErr := db.GetDeal(ob.DealID); dErr == nil && deal.Status != "settled" {
		deal.Status = "refund_broadcast"
		deal.Error = "refund_txid=" + refundTxID
		_, _ = db.UpdateDealCAS(deal)
	}
	rep.RefundsBroadcast++
	rep.RefundPendingTxID = refundTxID
	rep.RefundTxIDs = append(rep.RefundTxIDs, refundTxID)
	rep.StillOpen++
}
