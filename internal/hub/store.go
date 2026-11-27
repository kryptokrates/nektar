package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists Hub market events, NAT bbox relay messages, and bounty pools in pure-Go SQLite.
type Store struct {
	mu  sync.Mutex
	sql *sql.DB
}

// OpenStore initializes the Hub's standalone SQLite store (path == "" or ":memory:" for in-memory).
func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	origPath := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("hub store: mkdir: %w", err)
		}
		path = path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("hub store: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{sql: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if origPath != ":memory:" {
		_ = os.Chmod(origPath, 0o600)
		_ = os.Chmod(origPath+"-wal", 0o600)
		_ = os.Chmod(origPath+"-shm", 0o600)
	}
	return s, nil
}

func (s *Store) Close() error { return s.sql.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS hub_events (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	outpoint     TEXT NOT NULL UNIQUE,
	topic        TEXT NOT NULL,
	txid         TEXT NOT NULL,
	vout         INTEGER NOT NULL,
	kind         TEXT NOT NULL,
	pub          TEXT NOT NULL DEFAULT '',
	domains_json TEXT NOT NULL DEFAULT '[]',
	payload_json BLOB NOT NULL,
	beef         BLOB,
	exp          INTEGER NOT NULL DEFAULT 0,
	spent        INTEGER NOT NULL DEFAULT 0,
	block_height INTEGER NOT NULL DEFAULT 0,
	admitted_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hub_events_exp ON hub_events(exp);

CREATE TABLE IF NOT EXISTS hub_bbox (
	seq           INTEGER PRIMARY KEY AUTOINCREMENT,
	office        TEXT NOT NULL DEFAULT 'default',
	recipient_pub TEXT NOT NULL,
	sender_pub    TEXT NOT NULL,
	deal_id       TEXT NOT NULL,
	kind          TEXT NOT NULL,
	body_json     BLOB NOT NULL,
	sig           TEXT NOT NULL DEFAULT '',
	ts            INTEGER NOT NULL DEFAULT 0,
	nonce         TEXT NOT NULL DEFAULT '',
	aud           TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hub_bbox_recip ON hub_bbox(recipient_pub, seq);

CREATE TABLE IF NOT EXISTS hub_bounties (
	id          TEXT PRIMARY KEY,
	domain      TEXT NOT NULL,
	question    TEXT NOT NULL,
	reward_sats INTEGER NOT NULL,
	max_fills   INTEGER NOT NULL DEFAULT 10,
	filled      INTEGER NOT NULL DEFAULT 0,
	active      INTEGER NOT NULL DEFAULT 1,
	created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_bounty_claims (
	hdp         TEXT PRIMARY KEY,
	bounty_id   TEXT NOT NULL,
	seller_pub  TEXT NOT NULL,
	txid        TEXT NOT NULL DEFAULT '',
	reward_sats INTEGER NOT NULL,
	status      TEXT NOT NULL DEFAULT 'paid',
	created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_fed_cursors (
	peer       TEXT PRIMARY KEY,
	cursor     INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_fee_claims (
	txid                 TEXT PRIMARY KEY,
	input_outpoints_json TEXT NOT NULL DEFAULT '[]',
	action               TEXT NOT NULL,
	payer_pub            TEXT NOT NULL DEFAULT '',
	sats                 INTEGER NOT NULL DEFAULT 0,
	status               TEXT NOT NULL DEFAULT 'claimed',
	created_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_fee_claim_inputs (
	outpoint   TEXT PRIMARY KEY,
	txid       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_fee_ledger (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	txid         TEXT NOT NULL UNIQUE,
	action       TEXT NOT NULL,
	kind         TEXT NOT NULL DEFAULT '',
	payer_pub    TEXT NOT NULL DEFAULT '',
	sats         INTEGER NOT NULL DEFAULT 0,
	decision     TEXT NOT NULL DEFAULT 'fast',
	status       TEXT NOT NULL DEFAULT 'unmined',
	block_height INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hub_fee_ledger_created ON hub_fee_ledger(created_at);
CREATE INDEX IF NOT EXISTS idx_hub_fee_ledger_payer ON hub_fee_ledger(payer_pub, status);

CREATE TABLE IF NOT EXISTS hub_egress_credits (
	payer_pub              TEXT PRIMARY KEY,
	balance_msats          INTEGER NOT NULL DEFAULT 0,
	total_funded_sats      INTEGER NOT NULL DEFAULT 0,
	total_events_delivered INTEGER NOT NULL DEFAULT 0,
	updated_at             INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_flagged_payers (
	payer_pub  TEXT PRIMARY KEY,
	txid       TEXT NOT NULL DEFAULT '',
	reason     TEXT NOT NULL DEFAULT '',
	flagged_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_kv (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_carrier_outpoints (
	funding_outpoint TEXT PRIMARY KEY,
	carrier_txid     TEXT NOT NULL,
	carrier_outpoint TEXT NOT NULL,
	topic            TEXT NOT NULL,
	swept_by_txid    TEXT NOT NULL DEFAULT '',
	admitted_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hub_carrier_outpoints_txid ON hub_carrier_outpoints(carrier_txid);
`
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("hub store: begin migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("hub store: base schema: %w", err)
	}
	migrations := []struct {
		table string
		cols  []columnSpec
	}{
		{"hub_bounty_claims", []columnSpec{
			{name: "status", def: "TEXT NOT NULL DEFAULT 'paid'"},
		}},
		{"hub_bbox", []columnSpec{
			{name: "sig", def: "TEXT NOT NULL DEFAULT ''"},
			{name: "ts", def: "INTEGER NOT NULL DEFAULT 0"},
			{name: "nonce", def: "TEXT NOT NULL DEFAULT ''"},
			{name: "aud", def: "TEXT NOT NULL DEFAULT ''"},
		}},
		{"hub_events", []columnSpec{
			{name: "spent", def: "INTEGER NOT NULL DEFAULT 0"},
			{name: "block_height", def: "INTEGER NOT NULL DEFAULT 0"},
		}},
	}
	for _, m := range migrations {
		if err := ensureColumnsTx(tx, m.table, m.cols); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_hub_events_height ON hub_events(block_height)`); err != nil {
		return fmt.Errorf("hub store: idx_hub_events_height: %w", err)
	}
	return tx.Commit()
}

type columnSpec struct {
	name string
	def  string
}

func ensureColumnsTx(tx *sql.Tx, table string, cols []columnSpec) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("hub store: table_info(%s): %w", table, err)
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if sErr := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); sErr != nil {
			_ = rows.Close()
			return fmt.Errorf("hub store: scan table_info(%s): %w", table, sErr)
		}
		existing[strings.ToLower(name)] = true
	}
	if rErr := rows.Err(); rErr != nil {
		_ = rows.Close()
		return fmt.Errorf("hub store: iter table_info(%s): %w", table, rErr)
	}
	if cErr := rows.Close(); cErr != nil {
		return fmt.Errorf("hub store: close table_info(%s): %w", table, cErr)
	}
	for _, col := range cols {
		if existing[strings.ToLower(col.name)] {
			continue
		}
		if _, aErr := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col.name + ` ` + col.def); aErr != nil {
			return fmt.Errorf("hub store: add column %s.%s: %w", table, col.name, aErr)
		}
	}
	return nil
}

func isSQLiteConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint failed") || strings.Contains(msg, "sqlite_constraint")
}

// GetFedCursor loads the persisted federation cursor for peer.
func (s *Store) GetFedCursor(peer string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c int64
	_ = s.sql.QueryRow(`SELECT cursor FROM hub_fed_cursors WHERE peer = ?`, peer).Scan(&c)
	return c
}

// SetFedCursor atomically advances the persisted federation cursor for peer (monotonic max).
func (s *Store) SetFedCursor(peer string, cursor int64) error {
	if peer == "" || cursor <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`
		INSERT INTO hub_fed_cursors (peer, cursor, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(peer) DO UPDATE SET
			cursor = CASE WHEN excluded.cursor > hub_fed_cursors.cursor THEN excluded.cursor ELSE hub_fed_cursors.cursor END,
			updated_at = excluded.updated_at`,
		peer, cursor, time.Now().Unix())
	return err
}

// GetOrCreateKV loads key from hub_kv or atomically generates and persists it via createFn.
func (s *Store) GetOrCreateKV(key string, createFn func() ([]byte, error)) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var val []byte
	err := s.sql.QueryRow(`SELECT value FROM hub_kv WHERE key = ?`, key).Scan(&val)
	if err == nil && len(val) > 0 {
		return val, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	fresh, err := createFn()
	if err != nil {
		return nil, err
	}
	if _, err := s.sql.Exec(`INSERT OR REPLACE INTO hub_kv (key, value) VALUES (?, ?)`, key, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}
