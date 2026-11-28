package store

import (
	"database/sql"
	"errors"
	"time"
)

// ObligationRecord tracks a surviving commercial obligation across agent restarts,
// session disconnects, and software upgrades ("The Agent Has Left. The Obligation Has Not.").
type ObligationRecord struct {
	ID             string `json:"id"` // deterministic idempotency key e.g. "ob:<dealId>:<kind>"
	DealID         string `json:"dealId"`
	Role           string `json:"role"` // buyer | seller
	Kind           string `json:"kind"` // reveal_owed | receipt_countersig_owed | proof_upgrade_owed | refund_claimable
	Counterparty   string `json:"counterparty,omitempty"`
	AmountSats     uint64 `json:"amountSats,omitempty"`
	TxID           string `json:"txid,omitempty"`
	DeadlineUnix   int64  `json:"deadlineUnix,omitempty"`
	DeadlineHeight int64  `json:"deadlineHeight,omitempty"`
	Status         string `json:"status"` // open | fulfilled | overdue | cancelled
	PayloadJSON    []byte `json:"payloadJson,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
	LastError      string `json:"lastError,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

func upsertObligationTx(tx *sql.Tx, ob ObligationRecord, now int64) error {
	if ob.ID == "" {
		return errors.New("store: obligation id is required")
	}
	if ob.Status == "" {
		ob.Status = "open"
	}
	if ob.CreatedAt == 0 {
		ob.CreatedAt = now
	}
	ob.UpdatedAt = now
	_, err := tx.Exec(`
		INSERT INTO obligations (
			id, deal_id, role, kind, counterparty, amount_sats, txid,
			deadline_unix, deadline_height, status, payload_json, attempts, last_error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			counterparty = CASE WHEN excluded.counterparty != '' THEN excluded.counterparty ELSE obligations.counterparty END,
			amount_sats = CASE WHEN excluded.amount_sats > 0 THEN excluded.amount_sats ELSE obligations.amount_sats END,
			txid = CASE WHEN excluded.txid != '' THEN excluded.txid ELSE obligations.txid END,
			deadline_unix = CASE WHEN excluded.deadline_unix > 0 THEN excluded.deadline_unix ELSE obligations.deadline_unix END,
			deadline_height = CASE WHEN excluded.deadline_height > 0 THEN excluded.deadline_height ELSE obligations.deadline_height END,
			payload_json = CASE WHEN length(excluded.payload_json) > 0 THEN excluded.payload_json ELSE obligations.payload_json END,
			status = CASE WHEN obligations.status IN ('fulfilled', 'cancelled') THEN obligations.status ELSE excluded.status END,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		ob.ID, ob.DealID, ob.Role, ob.Kind, ob.Counterparty, ob.AmountSats, ob.TxID,
		ob.DeadlineUnix, ob.DeadlineHeight, ob.Status, ob.PayloadJSON, ob.Attempts, ob.LastError, ob.CreatedAt, ob.UpdatedAt,
	)
	return err
}

// UpsertObligation idempotently persists an open commercial obligation without regressing fulfilled/cancelled rows.
func (d *DB) UpsertObligation(ob ObligationRecord) error {
	if ob.ID == "" {
		return errors.New("store: obligation id is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertObligationTx(tx, ob, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// FulfillObligation marks an obligation fulfilled idempotently.
func (d *DB) FulfillObligation(id string) error {
	if id == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE obligations SET status = 'fulfilled', last_error = '', updated_at = ? WHERE id = ? AND status != 'cancelled'`,
		time.Now().Unix(), id)
	return err
}

// UpdateObligationStatus updates status, increments attempts, and records any error note.
func (d *DB) UpdateObligationStatus(id, status, lastErr string, incAttempt bool) error {
	if id == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	inc := 0
	if incAttempt {
		inc = 1
	}
	_, err := d.sql.Exec(`
		UPDATE obligations
		SET status = ?, attempts = attempts + ?, last_error = ?, updated_at = ?
		WHERE id = ? AND status NOT IN ('fulfilled', 'cancelled')`,
		status, inc, lastErr, time.Now().Unix(), id)
	return err
}

// GetObligation fetches one obligation by ID.
func (d *DB) GetObligation(id string) (ObligationRecord, error) {
	rdb, release := d.rdb()
	defer release()
	var ob ObligationRecord
	err := rdb.QueryRow(`
		SELECT id, deal_id, role, kind, counterparty, amount_sats, txid,
		       deadline_unix, deadline_height, status, payload_json, attempts, last_error, created_at, updated_at
		FROM obligations WHERE id = ?`, id).
		Scan(&ob.ID, &ob.DealID, &ob.Role, &ob.Kind, &ob.Counterparty, &ob.AmountSats, &ob.TxID,
			&ob.DeadlineUnix, &ob.DeadlineHeight, &ob.Status, &ob.PayloadJSON, &ob.Attempts, &ob.LastError, &ob.CreatedAt, &ob.UpdatedAt)
	return ob, err
}

// ListObligations lists obligations optionally filtered by status ("" returns all), newest first.
func (d *DB) ListObligations(status string, limit int) ([]ObligationRecord, error) {
	rdb, release := d.rdb()
	defer release()
	if limit <= 0 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = rdb.Query(`
			SELECT id, deal_id, role, kind, counterparty, amount_sats, txid,
			       deadline_unix, deadline_height, status, payload_json, attempts, last_error, created_at, updated_at
			FROM obligations WHERE status = ? ORDER BY deadline_unix ASC, created_at ASC LIMIT ?`, status, limit)
	} else {
		rows, err = rdb.Query(`
			SELECT id, deal_id, role, kind, counterparty, amount_sats, txid,
			       deadline_unix, deadline_height, status, payload_json, attempts, last_error, created_at, updated_at
			FROM obligations ORDER BY updated_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObligationRecord
	for rows.Next() {
		var ob ObligationRecord
		if err := rows.Scan(&ob.ID, &ob.DealID, &ob.Role, &ob.Kind, &ob.Counterparty, &ob.AmountSats, &ob.TxID,
			&ob.DeadlineUnix, &ob.DeadlineHeight, &ob.Status, &ob.PayloadJSON, &ob.Attempts, &ob.LastError, &ob.CreatedAt, &ob.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ob)
	}
	return out, rows.Err()
}

// CountObligations returns counts of open and overdue obligations in O(1) for doctor & Prometheus metrics.
func (d *DB) CountObligations() (open int, overdue int, err error) {
	rdb, release := d.rdb()
	defer release()
	err = rdb.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'overdue' THEN 1 ELSE 0 END), 0)
		FROM obligations`).Scan(&open, &overdue)
	return open, overdue, err
}
