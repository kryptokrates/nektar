package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// FragmentRecord is one persisted append-only row in a memory shard.
type FragmentRecord struct {
	Shard        string `json:"shard"`
	Seq          int64  `json:"seq"`
	HDP          string `json:"hdp"`
	Leaf         string `json:"leaf"`
	Data         []byte `json:"data,omitempty"`
	Ciphertext   []byte `json:"ciphertext,omitempty"`
	KeyedMeta    []byte `json:"keyedMeta,omitempty"` // BRC-369 keyed envelope metadata
	Provenance   []byte `json:"provenance"`
	Level        int    `json:"level"`
	Class        string `json:"class"`
	BorgGroup    string `json:"borgGroup,omitempty"`
	BorgEpoch    int64  `json:"borgEpoch,omitempty"`
	BatchN       int64  `json:"batchN"`
	LeafIndex    int    `json:"leafIndex"`
	AnchorTxID   string `json:"anchorTxid,omitempty"`
	Supersedes   int64  `json:"supersedes,omitempty"`
	Forgotten    bool   `json:"forgotten,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

// BatchAnchor tracks one closed & anchored batch for a shard.
type BatchAnchor struct {
	Shard      string `json:"shard"`
	ShardID    string `json:"shardId"`
	N          int64  `json:"n"`
	Size       int    `json:"size"`
	RootHex    string `json:"R"`
	ChainedHex string `json:"Rp"`
	TxID       string `json:"txid"`
	Beef       []byte `json:"beef,omitempty"`
	Settled    bool   `json:"settled"`
	BlockHeight int64 `json:"blockHeight"`
	CreatedAt  int64  `json:"createdAt"`
}

// ReceiptRecord indexes one committed nektarr1.* receipt by hash.
type ReceiptRecord struct {
	ReceiptHash string `json:"receiptHash"`
	ShardSeq    int64  `json:"shardSeq"`
	BodyJSON    []byte `json:"bodyJson"`
	AnchorTxID  string `json:"anchorTxid,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
}

// CitationEdge links a source (shard, seq) to a committed receipt hash for AGT-006 correction loops.
type CitationEdge struct {
	Shard       string `json:"shard"`
	Seq         int64  `json:"seq"`
	ReceiptHash string `json:"receiptHash"`
}

// DB wraps pure-Go SQLite (modernc.org/sqlite, zero CGO) for all Nektar v0.4 local state.
type DB struct {
	mu   sync.Mutex
	sql  *sql.DB
	path string
}

// Open initializes (or opens in-memory if path == ":memory:") the Nektar SQLite store.
func Open(path string) (*DB, error) {
	if path == "" {
		path = ":memory:"
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("store: mkdir: %w", err)
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	d := &DB{sql: db, path: path}
	if err := d.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS fragments (
	shard        TEXT NOT NULL,
	seq          INTEGER NOT NULL,
	hdp          TEXT NOT NULL,
	leaf         TEXT NOT NULL,
	data         BLOB,
	ciphertext   BLOB,
	keyed_meta   BLOB,
	provenance   BLOB NOT NULL,
	level        INTEGER NOT NULL DEFAULT 0,
	class        TEXT NOT NULL DEFAULT 'general',
	borg_group   TEXT NOT NULL DEFAULT '',
	borg_epoch   INTEGER NOT NULL DEFAULT 0,
	batch_n      INTEGER NOT NULL DEFAULT -1,
	leaf_index   INTEGER NOT NULL DEFAULT -1,
	anchor_txid  TEXT NOT NULL DEFAULT '',
	supersedes   INTEGER NOT NULL DEFAULT 0,
	forgotten    INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (shard, seq),
	UNIQUE (shard, hdp)
);
CREATE INDEX IF NOT EXISTS idx_fragments_batch ON fragments(shard, batch_n);

CREATE TABLE IF NOT EXISTS anchors (
	shard        TEXT NOT NULL,
	shard_id     TEXT NOT NULL,
	n            INTEGER NOT NULL,
	size         INTEGER NOT NULL,
	root_hex     TEXT NOT NULL,
	chained_hex  TEXT NOT NULL,
	txid         TEXT NOT NULL,
	beef         BLOB,
	settled      INTEGER NOT NULL DEFAULT 0,
	block_height INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (shard, n)
);

CREATE TABLE IF NOT EXISTS receipts (
	receipt_hash TEXT PRIMARY KEY,
	shard_seq    INTEGER NOT NULL,
	body_json    BLOB NOT NULL,
	anchor_txid  TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS citations (
	shard        TEXT NOT NULL,
	seq          INTEGER NOT NULL,
	receipt_hash TEXT NOT NULL,
	PRIMARY KEY (shard, seq, receipt_hash)
);

CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
`
	_, err := d.sql.Exec(schema)
	if err != nil {
		return err
	}
	return d.migrateMarket()
}

// AppendFragment inserts a deduplicated append-only fragment into shard.
// If a fragment with the same (shard, hdp) already exists, it returns the existing record and dedup=true.
func (d *DB) AppendFragment(rec FragmentRecord) (FragmentRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing, err := d.getByHDP(rec.Shard, rec.HDP)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return FragmentRecord{}, false, err
	}

	var nextSeq int64
	if err := d.sql.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM fragments WHERE shard = ?`, rec.Shard).Scan(&nextSeq); err != nil {
		return FragmentRecord{}, false, err
	}
	rec.Seq = nextSeq
	if rec.BatchN == 0 {
		rec.BatchN = -1
		rec.LeafIndex = -1
	}
	if rec.Class == "" {
		rec.Class = "general"
	}
	_, err = d.sql.Exec(`
		INSERT INTO fragments (
			shard, seq, hdp, leaf, data, ciphertext, keyed_meta, provenance,
			level, class, borg_group, borg_epoch, batch_n, leaf_index,
			anchor_txid, supersedes, forgotten, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Shard, rec.Seq, rec.HDP, rec.Leaf, rec.Data, rec.Ciphertext, rec.KeyedMeta, rec.Provenance,
		rec.Level, rec.Class, rec.BorgGroup, rec.BorgEpoch, rec.BatchN, rec.LeafIndex,
		rec.AnchorTxID, rec.Supersedes, boolToInt(rec.Forgotten), rec.CreatedAt,
	)
	if err != nil {
		return FragmentRecord{}, false, err
	}
	return rec, false, nil
}

func (d *DB) getByHDP(shard, hdp string) (FragmentRecord, error) {
	row := d.sql.QueryRow(`
		SELECT shard, seq, hdp, leaf, data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, created_at
		FROM fragments WHERE shard = ? AND hdp = ?`, shard, hdp)
	return scanFragment(row)
}

// GetFragment fetches a single fragment by (shard, seq).
func (d *DB) GetFragment(shard string, seq int64) (FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.sql.QueryRow(`
		SELECT shard, seq, hdp, leaf, data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, created_at
		FROM fragments WHERE shard = ? AND seq = ?`, shard, seq)
	return scanFragment(row)
}

// ListFragments lists all records in a shard ordered by seq.
func (d *DB) ListFragments(shard string) ([]FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, created_at
		FROM fragments WHERE shard = ? ORDER BY seq ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUnanchored returns fragments in shard not yet committed to a batch anchor.
func (d *DB) ListUnanchored(shard string) ([]FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, created_at
		FROM fragments WHERE shard = ? AND batch_n < 0 ORDER BY seq ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListBatchFragments returns fragments belonging to (shard, batchN) in leaf_index order.
func (d *DB) ListBatchFragments(shard string, batchN int64) ([]FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, created_at
		FROM fragments WHERE shard = ? AND batch_n = ? ORDER BY leaf_index ASC`, shard, batchN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListShards returns all distinct shard names.
func (d *DB) ListShards() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT DISTINCT shard FROM fragments ORDER BY shard ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ForgetFragment shreds local plaintext data while preserving the hash/Merkle commitment.
func (d *DB) ForgetFragment(shard string, seq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE fragments SET data = NULL, ciphertext = NULL, forgotten = 1 WHERE shard = ? AND seq = ?`, shard, seq)
	return err
}

// RecordBatchAnchor atomically records an anchor and binds seqs to (batchN, leafIndex, txid).
func (d *DB) RecordBatchAnchor(anchor BatchAnchor, seqs []int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		INSERT OR REPLACE INTO anchors (shard, shard_id, n, size, root_hex, chained_hex, txid, beef, settled, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		anchor.Shard, anchor.ShardID, anchor.N, anchor.Size, anchor.RootHex, anchor.ChainedHex,
		anchor.TxID, anchor.Beef, boolToInt(anchor.Settled), anchor.BlockHeight, anchor.CreatedAt,
	)
	if err != nil {
		return err
	}
	for idx, seq := range seqs {
		if _, err := tx.Exec(
			`UPDATE fragments SET batch_n = ?, leaf_index = ?, anchor_txid = ? WHERE shard = ? AND seq = ?`,
			anchor.N, idx, anchor.TxID, anchor.Shard, seq,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAnchors returns all batch anchors for shard ordered by n ascending.
func (d *DB) ListAnchors(shard string) ([]BatchAnchor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, shard_id, n, size, root_hex, chained_hex, txid, beef, settled, block_height, created_at
		FROM anchors WHERE shard = ? ORDER BY n ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchAnchor
	for rows.Next() {
		var a BatchAnchor
		var settled int
		if err := rows.Scan(&a.Shard, &a.ShardID, &a.N, &a.Size, &a.RootHex, &a.ChainedHex, &a.TxID, &a.Beef, &settled, &a.BlockHeight, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Settled = settled != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveReceipt persists a committed receipt and its citation edges.
func (d *DB) SaveReceipt(rec ReceiptRecord, cites []CitationEdge) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO receipts (receipt_hash, shard_seq, body_json, anchor_txid, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		rec.ReceiptHash, rec.ShardSeq, rec.BodyJSON, rec.AnchorTxID, rec.CreatedAt,
	); err != nil {
		return err
	}
	for _, c := range cites {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO citations (shard, seq, receipt_hash) VALUES (?, ?, ?)`,
			c.Shard, c.Seq, c.ReceiptHash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetReceipt looks up a receipt by its 64-hex hash.
func (d *DB) GetReceipt(hash string) (ReceiptRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r ReceiptRecord
	err := d.sql.QueryRow(`SELECT receipt_hash, shard_seq, body_json, anchor_txid, created_at FROM receipts WHERE receipt_hash = ?`, hash).
		Scan(&r.ReceiptHash, &r.ShardSeq, &r.BodyJSON, &r.AnchorTxID, &r.CreatedAt)
	return r, err
}

// CitingReceipts returns all receipt hashes citing (shard, seq).
func (d *DB) CitingReceipts(shard string, seq int64) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT receipt_hash FROM citations WHERE shard = ? AND seq = ?`, shard, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetJSON stores arbitrary JSON state under key.
func (d *DB) SetJSON(key string, v any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, key, b)
	return err
}

// GetJSON loads JSON state stored under key.
func (d *DB) GetJSON(key string, out any) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var raw []byte
	err := d.sql.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, out)
}

type scanner interface{ Scan(dest ...any) error }

func scanFragment(s scanner) (FragmentRecord, error) {
	var r FragmentRecord
	var forgotten int
	err := s.Scan(
		&r.Shard, &r.Seq, &r.HDP, &r.Leaf, &r.Data, &r.Ciphertext, &r.KeyedMeta, &r.Provenance,
		&r.Level, &r.Class, &r.BorgGroup, &r.BorgEpoch, &r.BatchN, &r.LeafIndex,
		&r.AnchorTxID, &r.Supersedes, &forgotten, &r.CreatedAt,
	)
	r.Forgotten = forgotten != 0
	return r, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
