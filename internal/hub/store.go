package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
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
	_, err := s.sql.Exec(schema)
	if err != nil {
		return err
	}
	_, _ = s.sql.Exec(`ALTER TABLE hub_bounty_claims ADD COLUMN status TEXT NOT NULL DEFAULT 'paid'`)
	_, _ = s.sql.Exec(`ALTER TABLE hub_bbox ADD COLUMN sig TEXT NOT NULL DEFAULT ''`)
	_, _ = s.sql.Exec(`ALTER TABLE hub_bbox ADD COLUMN ts INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.sql.Exec(`ALTER TABLE hub_bbox ADD COLUMN nonce TEXT NOT NULL DEFAULT ''`)
	_, _ = s.sql.Exec(`ALTER TABLE hub_bbox ADD COLUMN aud TEXT NOT NULL DEFAULT ''`)
	_, _ = s.sql.Exec(`ALTER TABLE hub_events ADD COLUMN spent INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.sql.Exec(`ALTER TABLE hub_events ADD COLUMN block_height INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.sql.Exec(`CREATE INDEX IF NOT EXISTS idx_hub_events_height ON hub_events(block_height)`)
	return nil
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
