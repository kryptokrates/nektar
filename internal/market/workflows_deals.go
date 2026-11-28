package market

import (
	"encoding/json"
	"errors"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

// clearPendingPayment explicitly wipes unbroadcast PaymentTxID/BEEF/BRC-29 fields on a non-settled
// buyer deal after a definitive pre-broadcast failure so multi-seller failover never reuses them.
func (e *Engine) clearPendingPayment(dealID string) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || dealID == "" {
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		existing, err := e.mem.DB().GetDeal(dealID)
		if err != nil || existing.Status == "settled" {
			return
		}
		existing.PaymentTxID = ""
		existing.PaymentBEEF = nil
		existing.PaymentOutputIndex = 0
		existing.PaymentDerivPrefix = ""
		existing.PaymentDerivSuffix = ""
		existing.PaymentSenderKey = ""
		if ok, casErr := e.mem.DB().UpdateDealCAS(existing); casErr == nil && ok {
			return
		}
	}
}

// ListDeals reads durable deal records directly from SQLite (single verdict authority — zero RAM shadow store).
func (e *Engine) ListDeals() []DealState {
	if e.mem == nil || e.mem.DB() == nil {
		return nil
	}
	recs, err := e.mem.DB().ListDeals(100)
	if err != nil {
		return nil
	}
	out := make([]DealState, 0, len(recs))
	for _, r := range recs {
		out = append(out, dealStateFromRecord(r))
	}
	return out
}

func dealStateFromRecord(r store.DealRecord) DealState {
	st := DealState{
		DealID:       r.DealID,
		Role:         r.Role,
		Status:       r.Status,
		ChirpURL:     r.ChirpURL,
		PaymentTxID:  r.PaymentTxID,
		Shard:        r.Shard,
		DeliveredTxt: r.DeliveredText,
		Error:        r.Error,
		UpdatedAt:    r.UpdatedAt,
	}
	if len(r.WantJSON) > 0 {
		_ = json.Unmarshal(r.WantJSON, &st.Want)
	}
	if len(r.ChosenOfferJSON) > 0 {
		var off bstack.OfferAd
		if json.Unmarshal(r.ChosenOfferJSON, &off) == nil {
			st.ChosenOffer = &off
		}
	}
	if len(r.MatchAuditJSON) > 0 {
		var ma MatchAudit
		if json.Unmarshal(r.MatchAuditJSON, &ma) == nil {
			st.MatchAudit = &ma
		}
	}
	if len(r.DealReceiptJSON) > 0 {
		var dr DealReceipt
		if json.Unmarshal(r.DealReceiptJSON, &dr) == nil {
			st.DealReceipt = &dr
		}
	}
	return st
}

// mergeExistingDealFields preserves already-committed fields on CAS updates and enforces monotonic
// terminal protection so a 'settled' deal never regresses except on verified chain reorg ('payment_pending'/'reorged').
func mergeExistingDealFields(rec, existing store.DealRecord) store.DealRecord {
	if existing.Status == "settled" && rec.Status != "settled" && rec.Status != "reorged" && rec.Status != "payment_pending" {
		rec.Status = "settled"
		if rec.Error != "" && existing.Error == "" {
			rec.Error = ""
		}
	}
	if len(rec.WantJSON) == 0 {
		rec.WantJSON = existing.WantJSON
	}
	if len(rec.ChosenOfferJSON) == 0 {
		rec.ChosenOfferJSON = existing.ChosenOfferJSON
	}
	if len(rec.MatchAuditJSON) == 0 {
		rec.MatchAuditJSON = existing.MatchAuditJSON
	}
	if len(rec.DealReceiptJSON) == 0 {
		rec.DealReceiptJSON = existing.DealReceiptJSON
	}
	if rec.PaymentTxID == "" {
		rec.PaymentTxID = existing.PaymentTxID
	}
	if len(rec.PaymentBEEF) == 0 {
		rec.PaymentBEEF = existing.PaymentBEEF
	}
	if rec.PaymentDerivPrefix == "" {
		rec.PaymentOutputIndex = existing.PaymentOutputIndex
		rec.PaymentDerivPrefix = existing.PaymentDerivPrefix
		rec.PaymentDerivSuffix = existing.PaymentDerivSuffix
		rec.PaymentSenderKey = existing.PaymentSenderKey
	}
	if rec.ClaimKeyHex == "" {
		rec.ClaimKeyHex = existing.ClaimKeyHex
	}
	if rec.ChirpURL == "" {
		rec.ChirpURL = existing.ChirpURL
	}
	if rec.CounterpartyPub == "" {
		rec.CounterpartyPub = existing.CounterpartyPub
	}
	if rec.PriceSats == 0 {
		rec.PriceSats = existing.PriceSats
	}
	if rec.FirstOfferMs == 0 {
		rec.FirstOfferMs = existing.FirstOfferMs
	}
	if rec.Shard == "" {
		rec.Shard = existing.Shard
	}
	rec.CreatedAt = existing.CreatedAt
	rec.Revision = existing.Revision
	return rec
}

// persistDealRecordChecked performs CAS-protected SQLite persistence, returning any SQLite error
// instead of swallowing it (RCA-2), and notifies EventBus subscribers immediately for zero-poll wakes.
func (e *Engine) persistDealRecordChecked(rec store.DealRecord) error {
	if e == nil || e.mem == nil || e.mem.DB() == nil || rec.DealID == "" {
		return nil
	}
	db := e.mem.DB()
	existing, err := db.GetDeal(rec.DealID)
	if err != nil {
		if sErr := db.SaveDeal(rec); sErr != nil {
			return sErr
		}
		e.publishDealEvent(rec.DealID, rec.Status)
		return nil
	}
	rec = mergeExistingDealFields(rec, existing)
	ok, casErr := db.UpdateDealCAS(rec)
	if casErr != nil {
		return casErr
	}
	if !ok {
		latest, gErr := db.GetDeal(rec.DealID)
		if gErr != nil {
			return gErr
		}
		rec = mergeExistingDealFields(rec, latest)
		ok2, casErr2 := db.UpdateDealCAS(rec)
		if casErr2 != nil {
			return casErr2
		}
		if !ok2 {
			return errors.New("market: concurrent deal CAS conflict")
		}
	}
	e.publishDealEvent(rec.DealID, rec.Status)
	return nil
}

func (e *Engine) persistDealRecord(rec store.DealRecord) {
	_ = e.persistDealRecordChecked(rec)
}

func (e *Engine) publishDealEvent(dealID, status string) {
	if e != nil && e.mem != nil && e.mem.EventBus() != nil && dealID != "" {
		e.mem.EventBus().Publish("deal:"+dealID, "deal_updated", map[string]string{
			"dealId": dealID,
			"status": status,
		})
	}
}
