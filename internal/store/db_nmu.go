package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FragmentNMUMeta persists NMU-1 commodity metadata for one fragment (shard, seq).
type FragmentNMUMeta struct {
	Shard          string `json:"shard"`
	Seq            int64  `json:"seq"`
	Grade          string `json:"grade"`
	UnitCount      uint32 `json:"unitCount"`
	FreshnessClass string `json:"freshnessClass,omitempty"`
	ValidUntilUnix int64  `json:"validUntilUnix,omitempty"`
	SubjectVersion string `json:"subjectVersion,omitempty"`
	UsageRights    string `json:"usageRights,omitempty"`
	GradeMismatch  bool   `json:"gradeMismatch,omitempty"`
	UpdatedAt      int64  `json:"updatedAt,omitempty"`
}

// ShardSubscription persists a paid time-windowed Shard Feed subscription (and Grade S update SLA).
type ShardSubscription struct {
	SubID       string `json:"subId"`
	Shard       string `json:"shard"`
	BuyerPub    string `json:"buyerPub"`
	SellerPub   string `json:"sellerPub,omitempty"`
	PricingMode string `json:"pricingMode"` // "subscription"
	PriceSats   uint64 `json:"priceSats"`
	PaymentTxID string `json:"paymentTxid"`
	UsageRights string `json:"usageRights"`
	StartedAt   int64  `json:"startedAt"`
	ExpiresAt   int64  `json:"expiresAt"`
	LastSeq     int64  `json:"lastSeq"`
	Active      bool   `json:"active"`
}

func migrateNMUTx(tx *sql.Tx) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS fragment_nmu_meta (
	shard           TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	grade           TEXT NOT NULL DEFAULT '',
	unit_count      INTEGER NOT NULL DEFAULT 0,
	freshness_class TEXT NOT NULL DEFAULT '',
	valid_until     INTEGER NOT NULL DEFAULT 0,
	subject_version TEXT NOT NULL DEFAULT '',
	usage_rights    TEXT NOT NULL DEFAULT 'inference_only',
	grade_mismatch  INTEGER NOT NULL DEFAULT 0,
	updated_at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (shard, seq)
);
CREATE INDEX IF NOT EXISTS idx_fragment_nmu_grade ON fragment_nmu_meta(shard, grade);

CREATE TABLE IF NOT EXISTS shard_subscriptions (
	sub_id       TEXT PRIMARY KEY,
	shard        TEXT NOT NULL,
	buyer_pub    TEXT NOT NULL,
	seller_pub   TEXT NOT NULL DEFAULT '',
	pricing_mode TEXT NOT NULL DEFAULT 'subscription',
	price_sats   INTEGER NOT NULL DEFAULT 0,
	payment_txid TEXT NOT NULL UNIQUE,
	usage_rights TEXT NOT NULL DEFAULT 'inference_only',
	started_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	last_seq     INTEGER NOT NULL DEFAULT 0,
	active       INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_shard_subs_active ON shard_subscriptions(shard, active, expires_at);
CREATE INDEX IF NOT EXISTS idx_shard_subs_buyer ON shard_subscriptions(buyer_pub, active);
`
	if _, err := tx.Exec(ddl); err != nil {
		return fmt.Errorf("store: migrate nmu tables: %w", err)
	}
	// Clean up any orphaned NMU rows for forgotten fragments.
	if _, err := tx.Exec(`
		DELETE FROM fragment_nmu_meta
		WHERE EXISTS (
			SELECT 1 FROM fragments f
			WHERE f.shard = fragment_nmu_meta.shard AND f.seq = fragment_nmu_meta.seq AND f.forgotten = 1
		)`); err != nil {
		return fmt.Errorf("store: clean forgotten fragment_nmu_meta: %w", err)
	}
	cols := []struct{ col, def string }{
		{"price_per_unit", "INTEGER NOT NULL DEFAULT 0"},
		{"query_price_sats", "INTEGER NOT NULL DEFAULT 0"},
		{"subscription_price_sats", "INTEGER NOT NULL DEFAULT 0"},
		{"subscription_duration_sec", "INTEGER NOT NULL DEFAULT 0"},
		{"freshness_class", "TEXT NOT NULL DEFAULT ''"},
		{"usage_rights", "TEXT NOT NULL DEFAULT 'inference_only'"},
		{"min_advertised_grade", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range cols {
		if err := ensureColumnTx(tx, "sell_listings", c.col, c.def); err != nil {
			return fmt.Errorf("store: sell_listings add %s: %w", c.col, err)
		}
	}
	return nil
}

func saveFragmentNMUMetaTx(tx *sql.Tx, m FragmentNMUMeta) error {
	if strings.TrimSpace(m.Shard) == "" || m.Seq <= 0 {
		return nil
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = time.Now().Unix()
	}
	if strings.TrimSpace(m.UsageRights) == "" {
		m.UsageRights = "inference_only"
	}
	_, err := tx.Exec(`
		INSERT OR REPLACE INTO fragment_nmu_meta (
			shard, seq, grade, unit_count, freshness_class, valid_until, subject_version, usage_rights, grade_mismatch, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Shard, m.Seq, strings.ToUpper(strings.TrimSpace(m.Grade)), m.UnitCount,
		strings.ToLower(strings.TrimSpace(m.FreshnessClass)), m.ValidUntilUnix,
		strings.TrimSpace(m.SubjectVersion), strings.ToLower(strings.TrimSpace(m.UsageRights)),
		boolToInt(m.GradeMismatch), m.UpdatedAt,
	)
	return err
}

// SaveFragmentNMUMeta atomically persists NMU-1 metadata inside an active RepoTx.
func (rt *RepoTx) SaveFragmentNMUMeta(m FragmentNMUMeta) error {
	return saveFragmentNMUMetaTx(rt.tx, m)
}

// SaveFragmentNMUMeta persists NMU-1 commodity metadata for (shard, seq).
func (d *DB) SaveFragmentNMUMeta(m FragmentNMUMeta) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveFragmentNMUMetaTx(tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

// GetFragmentNMUMeta retrieves NMU-1 metadata for (shard, seq), returning (nil, nil) if absent.
func (d *DB) GetFragmentNMUMeta(shard string, seq int64) (*FragmentNMUMeta, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var m FragmentNMUMeta
	var mismatch int
	err := d.sql.QueryRow(`
		SELECT shard, seq, grade, unit_count, freshness_class, valid_until, subject_version, usage_rights, grade_mismatch, updated_at
		FROM fragment_nmu_meta WHERE shard = ? AND seq = ?`, shard, seq).Scan(
		&m.Shard, &m.Seq, &m.Grade, &m.UnitCount, &m.FreshnessClass, &m.ValidUntilUnix,
		&m.SubjectVersion, &m.UsageRights, &mismatch, &m.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.GradeMismatch = mismatch != 0
	return &m, nil
}

// ListFragmentNMUMeta returns all NMU-1 metadata rows for shard keyed by seq.
func (d *DB) ListFragmentNMUMeta(shard string) (map[int64]FragmentNMUMeta, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, grade, unit_count, freshness_class, valid_until, subject_version, usage_rights, grade_mismatch, updated_at
		FROM fragment_nmu_meta WHERE shard = ?`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]FragmentNMUMeta)
	for rows.Next() {
		var m FragmentNMUMeta
		var mismatch int
		if err := rows.Scan(
			&m.Shard, &m.Seq, &m.Grade, &m.UnitCount, &m.FreshnessClass, &m.ValidUntilUnix,
			&m.SubjectVersion, &m.UsageRights, &mismatch, &m.UpdatedAt,
		); err != nil {
			return nil, err
		}
		m.GradeMismatch = mismatch != 0
		out[m.Seq] = m
	}
	return out, rows.Err()
}

// CreateShardSubscription persists a new paid Shard Feed subscription (fail closed on duplicate payment_txid).
func (d *DB) CreateShardSubscription(sub ShardSubscription) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.TrimSpace(sub.SubID) == "" || strings.TrimSpace(sub.Shard) == "" || strings.TrimSpace(sub.BuyerPub) == "" {
		return errors.New("store: subscription requires subId, shard, and buyerPub")
	}
	if strings.TrimSpace(sub.PaymentTxID) == "" {
		return errors.New("store: subscription requires paymentTxid")
	}
	if sub.StartedAt == 0 {
		sub.StartedAt = time.Now().Unix()
	}
	if sub.PricingMode == "" {
		sub.PricingMode = "subscription"
	}
	if sub.UsageRights == "" {
		sub.UsageRights = "inference_only"
	}
	_, err := d.sql.Exec(`
		INSERT INTO shard_subscriptions (
			sub_id, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid, usage_rights, started_at, expires_at, last_seq, active
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.SubID, sub.Shard, strings.ToLower(strings.TrimSpace(sub.BuyerPub)),
		strings.ToLower(strings.TrimSpace(sub.SellerPub)), sub.PricingMode, sub.PriceSats,
		strings.ToLower(strings.TrimSpace(sub.PaymentTxID)), strings.ToLower(strings.TrimSpace(sub.UsageRights)),
		sub.StartedAt, sub.ExpiresAt, sub.LastSeq, boolToInt(sub.Active),
	)
	return err
}

func scanShardSubscription(s scanner) (ShardSubscription, error) {
	var sub ShardSubscription
	var active int
	err := s.Scan(
		&sub.SubID, &sub.Shard, &sub.BuyerPub, &sub.SellerPub, &sub.PricingMode,
		&sub.PriceSats, &sub.PaymentTxID, &sub.UsageRights, &sub.StartedAt,
		&sub.ExpiresAt, &sub.LastSeq, &active,
	)
	sub.Active = active != 0
	return sub, err
}

// GetShardSubscription fetches one subscription by subId.
func (d *DB) GetShardSubscription(subID string) (*ShardSubscription, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sub, err := scanShardSubscription(d.sql.QueryRow(`
		SELECT sub_id, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid, usage_rights, started_at, expires_at, last_seq, active
		FROM shard_subscriptions WHERE sub_id = ?`, subID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// GetShardSubscriptionByPaymentTx fetches a subscription by its unique BRC-29 payment txid.
func (d *DB) GetShardSubscriptionByPaymentTx(txid string) (*ShardSubscription, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sub, err := scanShardSubscription(d.sql.QueryRow(`
		SELECT sub_id, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid, usage_rights, started_at, expires_at, last_seq, active
		FROM shard_subscriptions WHERE payment_txid = ?`, strings.ToLower(strings.TrimSpace(txid))))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// ListActiveSubscriptions returns active, non-expired subscriptions (optionally filtered by shard).
func (d *DB) ListActiveSubscriptions(shard string, nowUnix int64) ([]ShardSubscription, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(shard) != "" {
		rows, err = d.sql.Query(`
			SELECT sub_id, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid, usage_rights, started_at, expires_at, last_seq, active
			FROM shard_subscriptions WHERE shard = ? AND active = 1 AND expires_at > ? ORDER BY started_at DESC`, shard, nowUnix)
	} else {
		rows, err = d.sql.Query(`
			SELECT sub_id, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid, usage_rights, started_at, expires_at, last_seq, active
			FROM shard_subscriptions WHERE active = 1 AND expires_at > ? ORDER BY started_at DESC`, nowUnix)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShardSubscription
	for rows.Next() {
		sub, err := scanShardSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// UpdateSubscriptionLastSeq advances the delivered sequence cursor for a subscription.
func (d *DB) UpdateSubscriptionLastSeq(subID string, lastSeq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE shard_subscriptions SET last_seq = MAX(last_seq, ?) WHERE sub_id = ?`, lastSeq, subID)
	return err
}

// RevokeShardSubscription marks a subscription inactive.
func (d *DB) RevokeShardSubscription(subID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE shard_subscriptions SET active = 0 WHERE sub_id = ?`, subID)
	return err
}
