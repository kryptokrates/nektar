package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// FragmentRecord is one persisted append-only row in a memory repo / shard.
type FragmentRecord struct {
	Shard         string `json:"shard"`
	Seq           int64  `json:"seq"`
	HDP           string `json:"hdp"`
	Leaf          string `json:"leaf"`
	PrevLeaf      string `json:"prevLeaf,omitempty"`
	ChainHash     string `json:"chainHash,omitempty"`
	Data          []byte `json:"data,omitempty"`
	Ciphertext    []byte `json:"ciphertext,omitempty"`
	KeyedMeta     []byte `json:"keyedMeta,omitempty"` // BRC-369 keyed envelope metadata
	Provenance    []byte `json:"provenance"`
	Level         int    `json:"level"`
	Class         string `json:"class"`
	BorgGroup     string `json:"borgGroup,omitempty"`
	BorgEpoch     int64  `json:"borgEpoch,omitempty"`
	BatchN        int64  `json:"batchN"`
	LeafIndex     int    `json:"leafIndex"`
	AnchorTxID    string `json:"anchorTxid,omitempty"`
	Supersedes    int64  `json:"supersedes,omitempty"`
	Forgotten     bool   `json:"forgotten,omitempty"`
	Tainted       bool   `json:"tainted,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	Path          string `json:"path,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
	ChirpURL      string `json:"chirpUrl,omitempty"`
	ContentSHA256 string `json:"contentSha256,omitempty"`
	MPERoot       string `json:"mpeRoot,omitempty"`
	CommitHash    string `json:"commitHash,omitempty"`
}

// BatchAnchor tracks one closed & anchored batch for a shard.
type BatchAnchor struct {
	Shard       string `json:"shard"`
	ShardID     string `json:"shardId"`
	N           int64  `json:"n"`
	Size        int    `json:"size"`
	RootHex     string `json:"R"`
	ChainedHex  string `json:"Rp"`
	TxID        string `json:"txid"`
	Beef        []byte `json:"beef,omitempty"`
	Settled     bool   `json:"settled"`
	BlockHeight int64  `json:"blockHeight"`
	CreatedAt   int64  `json:"createdAt"`
}

// PendingAnchor tracks an in-flight shard anchor batch persisted BEFORE broadcast
// so a crash between broadcast and RecordBatchAnchor is recoverable on restart.
type PendingAnchor struct {
	Shard      string  `json:"shard"`
	N          int64   `json:"n"`
	Kind       string  `json:"kind,omitempty"` // "append" (default) | "rotate"
	Seqs       []int64 `json:"seqs"`
	RootHex    string  `json:"R"`
	ChainedHex string  `json:"Rp"`
	TxID       string  `json:"txid,omitempty"`
	Beef       []byte  `json:"beef,omitempty"`
	CreatedAt  int64   `json:"createdAt"`
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

// DB wraps pure-Go SQLite (modernc.org/sqlite, zero CGO) for all Nektar v0.1 local state.
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
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	_, _ = db.Exec(`PRAGMA busy_timeout = 5000`)
	d := &DB{sql: db, path: path}
	if err := d.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		_ = os.Chmod(path, 0o600)
		_ = os.Chmod(path+"-wal", 0o600)
		_ = os.Chmod(path+"-shm", 0o600)
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// Conn returns the underlying *sql.DB handle (for diagnostics and tamper-detection tests).
func (d *DB) Conn() *sql.DB { return d.sql }

func (d *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS fragments (
	shard          TEXT NOT NULL,
	seq            INTEGER NOT NULL,
	hdp            TEXT NOT NULL,
	leaf           TEXT NOT NULL,
	prev_leaf      TEXT NOT NULL DEFAULT '',
	chain_hash     TEXT NOT NULL DEFAULT '',
	data           BLOB,
	ciphertext     BLOB,
	keyed_meta     BLOB,
	provenance     BLOB NOT NULL,
	level          INTEGER NOT NULL DEFAULT 0,
	class          TEXT NOT NULL DEFAULT 'general',
	borg_group     TEXT NOT NULL DEFAULT '',
	borg_epoch     INTEGER NOT NULL DEFAULT 0,
	batch_n        INTEGER NOT NULL DEFAULT -1,
	leaf_index     INTEGER NOT NULL DEFAULT -1,
	anchor_txid    TEXT NOT NULL DEFAULT '',
	supersedes     INTEGER NOT NULL DEFAULT 0,
	forgotten      INTEGER NOT NULL DEFAULT 0,
	tainted        INTEGER NOT NULL DEFAULT 0,
	path           TEXT NOT NULL DEFAULT '',
	media_type     TEXT NOT NULL DEFAULT '',
	chirp_url      TEXT NOT NULL DEFAULT '',
	content_sha256 TEXT NOT NULL DEFAULT '',
	mpe_root       TEXT NOT NULL DEFAULT '',
	commit_hash    TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	PRIMARY KEY (shard, seq),
	UNIQUE (shard, hdp)
);
CREATE INDEX IF NOT EXISTS idx_fragments_batch ON fragments(shard, batch_n);
CREATE INDEX IF NOT EXISTS idx_fragments_supersedes ON fragments(shard, supersedes);

CREATE VIRTUAL TABLE IF NOT EXISTS fragments_fts USING fts5(
	shard UNINDEXED,
	seq UNINDEXED,
	text,
	tokenize = 'porter unicode61'
);

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

CREATE TABLE IF NOT EXISTS pending_anchors (
	shard       TEXT NOT NULL,
	n           INTEGER NOT NULL,
	kind        TEXT NOT NULL DEFAULT 'append',
	seqs_json   BLOB NOT NULL,
	root_hex    TEXT NOT NULL,
	chained_hex TEXT NOT NULL,
	txid        TEXT NOT NULL DEFAULT '',
	beef        BLOB,
	created_at  INTEGER NOT NULL,
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
	// Clean up any orphaned FTS5 rows belonging to forgotten fragments (crash recovery for shredded records).
	if _, err := d.sql.Exec(`
		DELETE FROM fragments_fts
		WHERE EXISTS (
			SELECT 1 FROM fragments f
			WHERE f.shard = fragments_fts.shard AND f.seq = fragments_fts.seq AND f.forgotten = 1
		)`); err != nil {
		return fmt.Errorf("store: clean forgotten fts: %w", err)
	}
	if err := d.migrateMemRepo(); err != nil {
		return err
	}
	return d.migrateMarket()
}

// ensureColumnTx inspects PRAGMA table_info(table) inside tx and adds column only if absent,
// failing closed on any real SQLite error instead of swallowing ALTER TABLE failures.
func ensureColumnTx(tx *sql.Tx, table, column, colDef string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if strings.EqualFold(name, column) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + colDef)
	return err
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
