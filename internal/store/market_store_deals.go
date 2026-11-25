package store

import (
	"database/sql"
	"errors"
	"time"
)

// SaveDeal persists a DealRecord.
func (d *DB) SaveDeal(rec DealRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	if rec.Revision == 0 {
		rec.Revision = 1
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO deals
		(deal_id, role, status, shard, want_json, chosen_offer_json, match_audit_json, deal_receipt_json, payment_txid, payment_beef,
		 payment_output_index, payment_deriv_prefix, payment_deriv_suffix, payment_sender_key,
		 claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, confirm_height, error, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.DealID, rec.Role, rec.Status, rec.Shard, rec.WantJSON, rec.ChosenOfferJSON, rec.MatchAuditJSON, rec.DealReceiptJSON, rec.PaymentTxID, rec.PaymentBEEF,
		rec.PaymentOutputIndex, rec.PaymentDerivPrefix, rec.PaymentDerivSuffix, rec.PaymentSenderKey,
		rec.ClaimKeyHex, rec.ChirpURL, rec.DeliveredText, rec.CounterpartyPub, rec.PriceSats,
		boolToInt(rec.IsHubSeller), rec.FirstOfferMs, rec.ConfirmHeight, rec.Error, rec.Revision, rec.CreatedAt, rec.UpdatedAt)
	return err
}

// UpdateDealCAS performs an optimistic compare-and-swap update on a DealRecord,
// incrementing revision only if the persisted revision matches rec.Revision.
func (d *DB) UpdateDealCAS(rec DealRecord) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	res, err := d.sql.Exec(`
		UPDATE deals SET
			role = ?, status = ?, shard = ?, want_json = ?, chosen_offer_json = ?, match_audit_json = ?, deal_receipt_json = ?,
			payment_txid = ?, payment_beef = ?,
			payment_output_index = ?, payment_deriv_prefix = ?, payment_deriv_suffix = ?, payment_sender_key = ?,
			claim_key_hex = ?, chirp_url = ?,
			delivered_text = ?, counterparty_pub = ?, price_sats = ?, is_hub_seller = ?,
			first_offer_ms = ?, confirm_height = ?, error = ?, revision = revision + 1, updated_at = ?
		WHERE deal_id = ? AND revision = ?`,
		rec.Role, rec.Status, rec.Shard, rec.WantJSON, rec.ChosenOfferJSON, rec.MatchAuditJSON, rec.DealReceiptJSON,
		rec.PaymentTxID, rec.PaymentBEEF,
		rec.PaymentOutputIndex, rec.PaymentDerivPrefix, rec.PaymentDerivSuffix, rec.PaymentSenderKey,
		rec.ClaimKeyHex, rec.ChirpURL,
		rec.DeliveredText, rec.CounterpartyPub, rec.PriceSats, boolToInt(rec.IsHubSeller),
		rec.FirstOfferMs, rec.ConfirmHeight, rec.Error, now, rec.DealID, rec.Revision)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RollbackDealsFromHeight reopens settled deals whose confirmation block was orphaned by a chain reorg.
func (d *DB) RollbackDealsFromHeight(forkHeight int64) (int64, error) {
	if forkHeight <= 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(`
		UPDATE deals
		SET status = 'paid', confirm_height = 0, revision = revision + 1, updated_at = ?
		WHERE status = 'settled' AND confirm_height >= ?`,
		time.Now().Unix(), forkHeight)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetDeal fetches one deal by dealId.
func (d *DB) GetDeal(dealID string) (DealRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.sql.QueryRow(`
		SELECT deal_id, role, status, shard, want_json, chosen_offer_json, match_audit_json, deal_receipt_json, payment_txid, payment_beef,
		       COALESCE(payment_output_index, 0), COALESCE(payment_deriv_prefix, ''), COALESCE(payment_deriv_suffix, ''), COALESCE(payment_sender_key, ''),
		       claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, COALESCE(confirm_height, 0), error, revision, created_at, updated_at
		FROM deals WHERE deal_id = ?`, dealID)
	var r DealRecord
	var hub int
	err := row.Scan(&r.DealID, &r.Role, &r.Status, &r.Shard, &r.WantJSON, &r.ChosenOfferJSON, &r.MatchAuditJSON, &r.DealReceiptJSON, &r.PaymentTxID, &r.PaymentBEEF,
		&r.PaymentOutputIndex, &r.PaymentDerivPrefix, &r.PaymentDerivSuffix, &r.PaymentSenderKey,
		&r.ClaimKeyHex, &r.ChirpURL, &r.DeliveredText, &r.CounterpartyPub, &r.PriceSats, &hub, &r.FirstOfferMs, &r.ConfirmHeight,
		&r.Error, &r.Revision, &r.CreatedAt, &r.UpdatedAt)
	r.IsHubSeller = hub != 0
	return r, err
}

// ListDeals returns persisted deals newest first.
func (d *DB) ListDeals(limit int) ([]DealRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.Query(`
		SELECT deal_id, role, status, shard, want_json, chosen_offer_json, match_audit_json, deal_receipt_json, payment_txid, payment_beef,
		       COALESCE(payment_output_index, 0), COALESCE(payment_deriv_prefix, ''), COALESCE(payment_deriv_suffix, ''), COALESCE(payment_sender_key, ''),
		       claim_key_hex, chirp_url, delivered_text, counterparty_pub, price_sats, is_hub_seller, first_offer_ms, COALESCE(confirm_height, 0), error, revision, created_at, updated_at
		FROM deals ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DealRecord
	for rows.Next() {
		var r DealRecord
		var hub int
		if err := rows.Scan(&r.DealID, &r.Role, &r.Status, &r.Shard, &r.WantJSON, &r.ChosenOfferJSON, &r.MatchAuditJSON, &r.DealReceiptJSON, &r.PaymentTxID, &r.PaymentBEEF,
			&r.PaymentOutputIndex, &r.PaymentDerivPrefix, &r.PaymentDerivSuffix, &r.PaymentSenderKey,
			&r.ClaimKeyHex, &r.ChirpURL, &r.DeliveredText, &r.CounterpartyPub, &r.PriceSats, &hub, &r.FirstOfferMs, &r.ConfirmHeight,
			&r.Error, &r.Revision, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.IsHubSeller = hub != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountDeals returns total persisted deals in O(1) without loading payment_beef BLOBs.
func (d *DB) CountDeals() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM deals`).Scan(&n)
	return n, err
}

// SaveApproval saves or updates a pending operator approval escalation.
func (d *DB) SaveApproval(a ApprovalRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO approvals (id, deal_id, reason, max_extra_sats, status, awakeable_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.DealID, a.Reason, a.MaxExtraSats, a.Status, a.AwakeableID, a.ExpiresAt, a.CreatedAt)
	return err
}

// GetApproval fetches one approval by ID.
func (d *DB) GetApproval(id string) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var a ApprovalRecord
	err := d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}

// ListPendingApprovals returns all unexpired pending operator approvals.
func (d *DB) ListPendingApprovals() ([]ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at
		FROM approvals WHERE status = 'pending' AND expires_at > ? ORDER BY created_at ASC`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRecord
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval atomically transitions a pending unexpired approval to approved or denied (strict CAS).
func (d *DB) DecideApproval(id string, approve bool) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "denied"
	if approve {
		status = "approved"
	}
	now := time.Now().Unix()
	res, err := d.sql.Exec(`UPDATE approvals SET status = ? WHERE id = ? AND status = 'pending' AND expires_at > ?`, status, id, now)
	if err != nil {
		return ApprovalRecord{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ApprovalRecord{}, ErrApprovalNotPending
	}
	var a ApprovalRecord
	err = d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}

// ConsumeApprovalOverride atomically consumes a single-use approved override ID so it cannot be replayed.
func (d *DB) ConsumeApprovalOverride(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(`UPDATE approvals SET status = 'consumed' WHERE id = ? AND status = 'approved' AND expires_at > ?`, id, time.Now().Unix())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrApprovalNotPending
	}
	return nil
}

// MarkApprovalResolvedAwakeable atomically transitions a pending/approved awakeable approval directly
// to 'consumed' (if approved) or 'denied' (if denied) so it can never be replayed via ApprovedOverrideID.
func (d *DB) MarkApprovalResolvedAwakeable(id string, approve bool) (ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "denied"
	if approve {
		status = "consumed"
	}
	now := time.Now().Unix()
	res, err := d.sql.Exec(`UPDATE approvals SET status = ? WHERE id = ? AND status IN ('pending','approved') AND expires_at > ?`, status, id, now)
	if err != nil {
		return ApprovalRecord{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ApprovalRecord{}, ErrApprovalNotPending
	}
	var a ApprovalRecord
	err = d.sql.QueryRow(`SELECT id, deal_id, reason, max_extra_sats, status, COALESCE(awakeable_id,''), expires_at, created_at FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.DealID, &a.Reason, &a.MaxExtraSats, &a.Status, &a.AwakeableID, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}

// SavePendingSale persists an active BRC-369 offer across seller restarts.
func (d *DB) SavePendingSale(rec PendingSaleRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO pending_sales
		(offer_id, want_id, buyer_pub, shard, seq, sale_offer_json, reveal_key_hex, chirp_url, proof_bundle_json, expires_at, settled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.OfferID, rec.WantID, rec.BuyerPub, rec.Shard, rec.Seq, rec.SaleOfferJSON,
		rec.RevealKeyHex, rec.ChirpURL, rec.ProofBundleJSON, rec.ExpiresAt, boolToInt(rec.Settled))
	return err
}

// GetPendingSale fetches one pending sale by offerId.
func (d *DB) GetPendingSale(offerID string) (PendingSaleRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r PendingSaleRecord
	var settled int
	err := d.sql.QueryRow(`
		SELECT offer_id, want_id, buyer_pub, shard, seq, sale_offer_json, reveal_key_hex, chirp_url, proof_bundle_json, expires_at, settled
		FROM pending_sales WHERE offer_id = ?`, offerID).
		Scan(&r.OfferID, &r.WantID, &r.BuyerPub, &r.Shard, &r.Seq, &r.SaleOfferJSON, &r.RevealKeyHex, &r.ChirpURL, &r.ProofBundleJSON, &r.ExpiresAt, &settled)
	r.Settled = settled != 0
	return r, err
}

// MarkPendingSaleSettled marks an offer settled in SQLite.
func (d *DB) MarkPendingSaleSettled(offerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE pending_sales SET settled = 1 WHERE offer_id = ?`, offerID)
	return err
}

// ListPendingSales returns all unexpired pending sales.
func (d *DB) ListPendingSales(nowUnix int64) ([]PendingSaleRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT offer_id, want_id, buyer_pub, shard, seq, sale_offer_json, reveal_key_hex, chirp_url, proof_bundle_json, expires_at, settled
		FROM pending_sales WHERE expires_at = 0 OR expires_at > ?`, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSaleRecord
	for rows.Next() {
		var r PendingSaleRecord
		var settled int
		if err := rows.Scan(&r.OfferID, &r.WantID, &r.BuyerPub, &r.Shard, &r.Seq, &r.SaleOfferJSON, &r.RevealKeyHex, &r.ChirpURL, &r.ProofBundleJSON, &r.ExpiresAt, &settled); err != nil {
			return nil, err
		}
		r.Settled = settled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneExpiredPendingSales removes expired offers from SQLite.
func (d *DB) PruneExpiredPendingSales(nowUnix int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(`DELETE FROM pending_sales WHERE expires_at > 0 AND expires_at < ?`, nowUnix)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecordReputation updates verified trade counters for a seller pubkey.
func (d *DB) RecordReputation(sellerPub string, settled bool, sats uint64) error {
	if sellerPub == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	var sInc, fInc int64
	if settled {
		sInc = 1
	} else {
		fInc = 1
	}
	_, err := d.sql.Exec(`
		INSERT INTO reputation (seller_pub, settled_deals, failed_deals, total_sats, last_settled_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(seller_pub) DO UPDATE SET
			settled_deals = settled_deals + excluded.settled_deals,
			failed_deals  = failed_deals  + excluded.failed_deals,
			total_sats    = total_sats    + excluded.total_sats,
			last_settled_at = CASE WHEN excluded.settled_deals > 0 THEN excluded.last_settled_at ELSE last_settled_at END`,
		sellerPub, sInc, fInc, sats, now)
	return err
}

// GetReputation fetches a seller's verified reputation row.
func (d *DB) GetReputation(sellerPub string) (SellerReputation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r SellerReputation
	err := d.sql.QueryRow(`SELECT seller_pub, settled_deals, failed_deals, total_sats, last_settled_at FROM reputation WHERE seller_pub = ?`, sellerPub).
		Scan(&r.SellerPub, &r.SettledDeals, &r.FailedDeals, &r.TotalSats, &r.LastSettledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SellerReputation{SellerPub: sellerPub}, nil
	}
	return r, err
}

// ListReputations returns all known sellers ordered by settled deals desc.
func (d *DB) ListReputations() ([]SellerReputation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT seller_pub, settled_deals, failed_deals, total_sats, last_settled_at FROM reputation ORDER BY settled_deals DESC, total_sats DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SellerReputation
	for rows.Next() {
		var r SellerReputation
		if err := rows.Scan(&r.SellerPub, &r.SettledDeals, &r.FailedDeals, &r.TotalSats, &r.LastSettledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
