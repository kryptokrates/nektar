package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DistillCursor tracks the highest processed sequence number for a private working shard.
type DistillCursor struct {
	Shard     string `json:"shard"`
	LastSeq   int64  `json:"lastSeq"`
	UpdatedAt int64  `json:"updatedAt"`
}

// DistillQuarantineRecord logs a fail-closed secret/PII block during distillation or selling.
// Raw secret plaintext is NEVER persisted — only the rule ID and canonical SHA-256 content hash.
type DistillQuarantineRecord struct {
	ID          string `json:"id"`
	Shard       string `json:"shard"`
	Seq         int64  `json:"seq"`
	RuleID      string `json:"ruleId"`
	ContentHash string `json:"contentHash"`
	CreatedAt   int64  `json:"createdAt"`
}

func migrateDistillTx(tx *sql.Tx) error {
	fragCols := []struct{ table, col, def string }{
		{"fragments", "distilled", "INTEGER NOT NULL DEFAULT 0"},
		{"fragments", "ontology_id", "TEXT NOT NULL DEFAULT ''"},
		{"fragments", "triples_json", "BLOB"},
		{"sell_listings", "ontology_id", "TEXT NOT NULL DEFAULT ''"},
		{"sell_listings", "allow_raw", "INTEGER NOT NULL DEFAULT 0"},
		{"sell_listings", "dynamic_pricing_json", "BLOB"},
	}
	for _, c := range fragCols {
		if err := ensureColumnTx(tx, c.table, c.col, c.def); err != nil {
			return fmt.Errorf("store: ensure %s.%s: %w", c.table, c.col, err)
		}
	}
	const ddl = `
CREATE TABLE IF NOT EXISTS distill_cursors (
	shard      TEXT PRIMARY KEY,
	last_seq   INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS distill_quarantine (
	id           TEXT PRIMARY KEY,
	shard        TEXT NOT NULL,
	seq          INTEGER NOT NULL DEFAULT 0,
	rule_id      TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_distill_quarantine_created ON distill_quarantine(created_at);
`
	_, err := tx.Exec(ddl)
	return err
}

// GetDistillCursor returns the last distilled fragment seq for shard (0 if never scanned).
func (d *DB) GetDistillCursor(shard string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var seq int64
	err := d.sql.QueryRow(`SELECT last_seq FROM distill_cursors WHERE shard = ?`, shard).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// SetDistillCursor advances the monotonic distillation cursor for shard.
func (d *DB) SetDistillCursor(shard string, lastSeq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`
		INSERT INTO distill_cursors (shard, last_seq, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(shard) DO UPDATE SET last_seq = MAX(distill_cursors.last_seq, excluded.last_seq), updated_at = excluded.updated_at`,
		shard, lastSeq, time.Now().Unix())
	return err
}

// RecordDistillQuarantine persists a secret/PII gate block event (strictly zero plaintext secret!).
func (d *DB) RecordDistillQuarantine(rec DistillQuarantineRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("q-%s-%d-%s", rec.Shard, rec.Seq, rec.RuleID)
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO distill_quarantine (id, shard, seq, rule_id, content_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Shard, rec.Seq, rec.RuleID, rec.ContentHash, rec.CreatedAt)
	return err
}

// ListDistillQuarantine returns recent quarantined block records newest first.
func (d *DB) ListDistillQuarantine(limit int) ([]DistillQuarantineRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.sql.Query(`
		SELECT id, shard, seq, rule_id, content_hash, created_at
		FROM distill_quarantine ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DistillQuarantineRecord
	for rows.Next() {
		var r DistillQuarantineRecord
		if err := rows.Scan(&r.ID, &r.Shard, &r.Seq, &r.RuleID, &r.ContentHash, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountDistillQuarantine returns the total count of quarantined secret/PII blocks.
func (d *DB) CountDistillQuarantine() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM distill_quarantine`).Scan(&n)
	return n, err
}

// CountDistilledFragments returns the number of active distilled fragments.
func (d *DB) CountDistilledFragments() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM fragments WHERE distilled = 1 AND forgotten = 0`).Scan(&n)
	return n, err
}
