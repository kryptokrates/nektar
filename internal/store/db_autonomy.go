package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// RecallMissRecord tracks repeated local recall misses for closed-loop SleepCycle auto-WantAd triggering.
type RecallMissRecord struct {
	TopicKey   string `json:"topicKey"`
	Shard      string `json:"shard,omitempty"`
	Query      string `json:"query"`
	MissCount  int    `json:"missCount"`
	FirstSeen  int64  `json:"firstSeen"`
	LastSeen   int64  `json:"lastSeen"`
	LastWantAt int64  `json:"lastWantAt,omitempty"`
	WantID     string `json:"wantId,omitempty"`
}

const maxRecallMissRows = 500

func migrateAutonomyTx(tx *sql.Tx) error {
	cols := []struct{ col, def string }{
		{"role", "TEXT NOT NULL DEFAULT 'seller'"},
		{"feed_url", "TEXT NOT NULL DEFAULT ''"},
		{"ontology_id", "TEXT NOT NULL DEFAULT ''"},
		{"domains_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"target_shard", "TEXT NOT NULL DEFAULT ''"},
		{"deal_receipt_json", "BLOB"},
		{"slashed", "INTEGER NOT NULL DEFAULT 0"},
		{"last_event_at", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range cols {
		if err := ensureColumnTx(tx, "shard_subscriptions", c.col, c.def); err != nil {
			return fmt.Errorf("store: shard_subscriptions add %s: %w", c.col, err)
		}
	}
	const ddl = `
CREATE INDEX IF NOT EXISTS idx_shard_subs_role_active ON shard_subscriptions(role, active, expires_at);

CREATE TABLE IF NOT EXISTS recall_misses (
	topic_key    TEXT PRIMARY KEY,
	shard        TEXT NOT NULL DEFAULT '',
	query        TEXT NOT NULL,
	miss_count   INTEGER NOT NULL DEFAULT 1,
	first_seen   INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL,
	last_want_at INTEGER NOT NULL DEFAULT 0,
	want_id      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_recall_misses_seen ON recall_misses(last_seen DESC);
`
	if _, err := tx.Exec(ddl); err != nil {
		return fmt.Errorf("store: migrate autonomy tables: %w", err)
	}
	return nil
}

const shardSubSelectCols = `sub_id, role, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid, usage_rights, started_at, expires_at, last_seq, active, feed_url, ontology_id, domains_json, target_shard, deal_receipt_json, slashed, last_event_at`

func scanShardSubscriptionFull(s scanner) (ShardSubscription, error) {
	var sub ShardSubscription
	var active, slashed int
	var domainsJSON string
	err := s.Scan(
		&sub.SubID, &sub.Role, &sub.Shard, &sub.BuyerPub, &sub.SellerPub, &sub.PricingMode,
		&sub.PriceSats, &sub.PaymentTxID, &sub.UsageRights, &sub.StartedAt,
		&sub.ExpiresAt, &sub.LastSeq, &active, &sub.FeedURL, &sub.OntologyID,
		&domainsJSON, &sub.TargetShard, &sub.DealReceiptJSON, &slashed, &sub.LastEventAt,
	)
	sub.Active = active != 0
	sub.Slashed = slashed != 0
	if domainsJSON != "" {
		_ = json.Unmarshal([]byte(domainsJSON), &sub.Domains)
	}
	return sub, err
}

// UpsertBuyerSubscription persists (or refreshes) a paid buyer-side Grade S subscription so restarts
// and laptop sleep resume streaming cleanly from the last persisted sequence cursor.
func (d *DB) UpsertBuyerSubscription(sub ShardSubscription) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.TrimSpace(sub.SubID) == "" || strings.TrimSpace(sub.Shard) == "" || strings.TrimSpace(sub.FeedURL) == "" {
		return errors.New("store: buyer subscription requires subId, shard, and feedUrl")
	}
	if strings.TrimSpace(sub.PaymentTxID) == "" {
		return errors.New("store: buyer subscription requires paymentTxid")
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
	if strings.TrimSpace(sub.TargetShard) == "" {
		sub.TargetShard = "sub-" + sub.Shard
	}
	domRaw, _ := json.Marshal(sub.Domains)
	if len(domRaw) == 0 || string(domRaw) == "null" {
		domRaw = []byte("[]")
	}
	_, err := d.sql.Exec(`
		INSERT INTO shard_subscriptions (
			sub_id, role, shard, buyer_pub, seller_pub, pricing_mode, price_sats, payment_txid,
			usage_rights, started_at, expires_at, last_seq, active, feed_url, ontology_id,
			domains_json, target_shard, deal_receipt_json, slashed, last_event_at
		) VALUES (?, 'buyer', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sub_id) DO UPDATE SET
			role = 'buyer',
			feed_url = excluded.feed_url,
			ontology_id = excluded.ontology_id,
			domains_json = excluded.domains_json,
			target_shard = excluded.target_shard,
			deal_receipt_json = COALESCE(excluded.deal_receipt_json, shard_subscriptions.deal_receipt_json),
			expires_at = MAX(shard_subscriptions.expires_at, excluded.expires_at),
			last_seq = MAX(shard_subscriptions.last_seq, excluded.last_seq),
			active = excluded.active`,
		sub.SubID, sub.Shard, strings.ToLower(strings.TrimSpace(sub.BuyerPub)),
		strings.ToLower(strings.TrimSpace(sub.SellerPub)), sub.PricingMode, sub.PriceSats,
		strings.ToLower(strings.TrimSpace(sub.PaymentTxID)), strings.ToLower(strings.TrimSpace(sub.UsageRights)),
		sub.StartedAt, sub.ExpiresAt, sub.LastSeq, boolToInt(sub.Active),
		strings.TrimSpace(sub.FeedURL), strings.TrimSpace(sub.OntologyID), string(domRaw),
		strings.TrimSpace(sub.TargetShard), sub.DealReceiptJSON, boolToInt(sub.Slashed), sub.LastEventAt,
	)
	return err
}

// ListActiveBuyerSubscriptions returns all active, unexpired, non-slashed buyer subscriptions for startup recovery.
func (d *DB) ListActiveBuyerSubscriptions(nowUnix int64) ([]ShardSubscription, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	rows, err := d.sql.Query(`
		SELECT `+shardSubSelectCols+`
		FROM shard_subscriptions
		WHERE role = 'buyer' AND active = 1 AND slashed = 0 AND expires_at > ?
		ORDER BY started_at ASC`, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShardSubscription
	for rows.Next() {
		sub, err := scanShardSubscriptionFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// UpdateSubscriptionCursor advances last_seq monotonically and stamps last_event_at atomically.
func (d *DB) UpdateSubscriptionCursor(subID string, lastSeq, lastEventAt int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if lastEventAt <= 0 {
		lastEventAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		UPDATE shard_subscriptions
		SET last_seq = MAX(last_seq, ?), last_event_at = MAX(last_event_at, ?)
		WHERE sub_id = ?`, lastSeq, lastEventAt, subID)
	return err
}

// MarkSubscriptionSlashed records that a buyer emitted a dead-feed slashing attestation and stops supervision.
func (d *DB) MarkSubscriptionSlashed(subID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE shard_subscriptions SET slashed = 1, active = 0 WHERE sub_id = ?`, subID)
	return err
}

// DeactivateExpiredSubscriptions marks all expired subscriptions inactive and returns the count updated.
func (d *DB) DeactivateExpiredSubscriptions(nowUnix int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	res, err := d.sql.Exec(`UPDATE shard_subscriptions SET active = 0 WHERE active = 1 AND expires_at <= ?`, nowUnix)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NormalizeRecallMissKey produces a canonical topic key for deduplicating repeated recall misses.
func NormalizeRecallMissKey(shard, query string) string {
	s := strings.ToLower(strings.TrimSpace(shard))
	q := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(query)), " "))
	if len(q) > 120 {
		q = q[:120]
	}
	return s + "|" + q
}

// RecordRecallMiss increments the miss counter for (shard, query) and enforces bounded LRU size (500 rows).
func (d *DB) RecordRecallMiss(shard, query string, nowUnix int64) error {
	q := strings.TrimSpace(query)
	if len(q) < 4 {
		return nil
	}
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	key := NormalizeRecallMissKey(shard, q)
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`
		INSERT INTO recall_misses (topic_key, shard, query, miss_count, first_seen, last_seen, last_want_at, want_id)
		VALUES (?, ?, ?, 1, ?, ?, 0, '')
		ON CONFLICT(topic_key) DO UPDATE SET
			miss_count = recall_misses.miss_count + 1,
			last_seen = MAX(recall_misses.last_seen, excluded.last_seen),
			query = excluded.query`,
		key, strings.TrimSpace(shard), q, nowUnix, nowUnix,
	)
	if err != nil {
		return err
	}
	_, _ = d.sql.Exec(`
		DELETE FROM recall_misses
		WHERE topic_key IN (
			SELECT topic_key FROM recall_misses ORDER BY last_seen DESC LIMIT -1 OFFSET ?
		)`, maxRecallMissRows)
	return nil
}

// ListEligibleRecallMisses returns topics that missed >= minCount times since lookbackCutoff
// and have not triggered an auto-WantAd within cooldownCutoff.
func (d *DB) ListEligibleRecallMisses(minCount int, lookbackCutoff, cooldownCutoff int64, limit int) ([]RecallMissRecord, error) {
	if minCount <= 0 {
		minCount = 3
	}
	if limit <= 0 {
		limit = 10
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT topic_key, shard, query, miss_count, first_seen, last_seen, last_want_at, want_id
		FROM recall_misses
		WHERE miss_count >= ? AND last_seen >= ? AND (last_want_at = 0 OR last_want_at <= ?)
		ORDER BY miss_count DESC, last_seen DESC
		LIMIT ?`, minCount, lookbackCutoff, cooldownCutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecallMissRecord
	for rows.Next() {
		var r RecallMissRecord
		if err := rows.Scan(&r.TopicKey, &r.Shard, &r.Query, &r.MissCount, &r.FirstSeen, &r.LastSeen, &r.LastWantAt, &r.WantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRecallMissWanted stamps last_want_at and want_id and resets miss_count so the topic cools down.
func (d *DB) MarkRecallMissWanted(topicKey, wantID string, nowUnix int64) error {
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`
		UPDATE recall_misses
		SET last_want_at = ?, want_id = ?, miss_count = 0
		WHERE topic_key = ?`, nowUnix, wantID, topicKey)
	return err
}

// CountAutoWantsSince counts how many recall_misses triggered auto-WantAds since sinceUnix.
func (d *DB) CountAutoWantsSince(sinceUnix int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM recall_misses WHERE last_want_at >= ? AND want_id != ''`, sinceUnix).Scan(&n)
	return n, err
}

// Path returns the configured SQLite file path (or ":memory:").
func (d *DB) Path() string { return d.path }

// UserVersion returns SQLite's current PRAGMA user_version schema number.
func (d *DB) UserVersion() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ver int
	err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&ver)
	return ver, err
}

// VacuumInto writes an atomic, WAL-safe point-in-time SQLite snapshot to dstPath without blocking live writes.
func (d *DB) VacuumInto(ctx context.Context, dstPath string) error {
	if strings.TrimSpace(dstPath) == "" {
		return errors.New("store: vacuum destination path required")
	}
	clean := filepath.Clean(dstPath)
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.ExecContext(ctx, `VACUUM INTO ?`, clean)
	return err
}
