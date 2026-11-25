package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// StoredEvent is one durably persisted admitted market output on the Hub.
type StoredEvent struct {
	Seq        int64                `json:"seq"`
	Outpoint   string               `json:"outpoint"`
	Topic      string               `json:"topic"`
	TxID       string               `json:"txid"`
	Vout       uint32               `json:"vout"`
	Kind       bstack.ArtifactKind  `json:"kind"`
	Pub        string               `json:"pub"`
	Domains    []string             `json:"domains"`
	Payload    bstack.MarketPayload `json:"payload"`
	BEEF       []byte               `json:"beef,omitempty"`
	Exp        int64                `json:"exp"`
	AdmittedAt int64                `json:"admittedAt"`
}

// BboxMessage is a store-and-forward message relayed for counterparties behind NAT.
type BboxMessage struct {
	Seq          int64           `json:"seq"`
	Office       string          `json:"office"`
	RecipientPub string          `json:"recipientPub"`
	SenderPub    string          `json:"senderPub"`
	DealID       string          `json:"dealId"`
	Kind         string          `json:"kind"` // purchase_beef | reveal_key | receipt
	Body         json.RawMessage `json:"body"`
	CreatedAt    int64           `json:"createdAt"`
}

// Bounty describes a domain knowledge bounty the Hub will pay agents to fulfill.
type Bounty struct {
	ID         string `json:"id"`
	Domain     string `json:"domain"`
	Question   string `json:"question"`
	RewardSats uint64 `json:"rewardSats"`
	MaxFills   int    `json:"maxFills"`
	Filled     int    `json:"filled"`
	Active     bool   `json:"active"`
	CreatedAt  int64  `json:"createdAt"`
}

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
	created_at  INTEGER NOT NULL
);
`
	_, err := s.sql.Exec(schema)
	return err
}

// AppendEvent durably stores an admitted output and returns its monotonic seq and whether it was new.
func (s *Store) AppendEvent(ev StoredEvent) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var existingSeq int64
	err := s.sql.QueryRow(`SELECT seq FROM hub_events WHERE outpoint = ?`, ev.Outpoint).Scan(&existingSeq)
	if err == nil {
		return existingSeq, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	doms, _ := json.Marshal(ev.Domains)
	pJSON, err := json.Marshal(ev.Payload)
	if err != nil {
		return 0, false, err
	}
	if ev.AdmittedAt == 0 {
		ev.AdmittedAt = time.Now().UnixMilli()
	}
	res, err := s.sql.Exec(`
		INSERT INTO hub_events (outpoint, topic, txid, vout, kind, pub, domains_json, payload_json, beef, exp, admitted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Outpoint, ev.Topic, ev.TxID, ev.Vout, string(ev.Kind), ev.Pub, string(doms), pJSON, ev.BEEF, ev.Exp, ev.AdmittedAt)
	if err != nil {
		return 0, false, err
	}
	seq, err := res.LastInsertId()
	return seq, true, err
}

// EventsSince loads unexpired events with seq > cursor ordered ascending (for SSE resume & poll floor).
func (s *Store) EventsSince(cursor int64, domainFilter []string, limit int) ([]StoredEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	nowMs := time.Now().UnixMilli()
	rows, err := s.sql.Query(`
		SELECT seq, outpoint, topic, txid, vout, kind, pub, domains_json, payload_json, beef, exp, admitted_at
		FROM hub_events
		WHERE seq > ? AND (exp = 0 OR exp > ?)
		ORDER BY seq ASC LIMIT ?`, cursor, nowMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	wantDom := make(map[string]bool, len(domainFilter))
	for _, d := range domainFilter {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			wantDom[d] = true
		}
	}
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var kindStr, domJSON string
		var pJSON []byte
		if err := rows.Scan(&e.Seq, &e.Outpoint, &e.Topic, &e.TxID, &e.Vout, &kindStr, &e.Pub, &domJSON, &pJSON, &e.BEEF, &e.Exp, &e.AdmittedAt); err != nil {
			return nil, err
		}
		e.Kind = bstack.ArtifactKind(kindStr)
		_ = json.Unmarshal([]byte(domJSON), &e.Domains)
		_ = json.Unmarshal(pJSON, &e.Payload)
		if len(wantDom) > 0 && !matchesAnyDomain(e.Domains, wantDom) {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func matchesAnyDomain(doms []string, want map[string]bool) bool {
	for _, d := range doms {
		if want[strings.ToLower(strings.TrimSpace(d))] {
			return true
		}
	}
	return false
}

// PruneExpired deletes expired ads from hub_events.
func (s *Store) PruneExpired(nowMs int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.sql.Exec(`DELETE FROM hub_events WHERE exp > 0 AND exp < ?`, nowMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SaveBboxMessage stores a NAT-safe relay envelope for recipientPub.
func (s *Store) SaveBboxMessage(msg BboxMessage) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Office == "" {
		msg.Office = "default"
	}
	if msg.CreatedAt == 0 {
		msg.CreatedAt = time.Now().UnixMilli()
	}
	res, err := s.sql.Exec(`
		INSERT INTO hub_bbox (office, recipient_pub, sender_pub, deal_id, kind, body_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.Office, msg.RecipientPub, msg.SenderPub, msg.DealID, msg.Kind, []byte(msg.Body), msg.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListBboxMessages fetches messages addressed to recipientPub with seq > sinceSeq.
func (s *Store) ListBboxMessages(recipientPub string, sinceSeq int64, limit int) ([]BboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.sql.Query(`
		SELECT seq, office, recipient_pub, sender_pub, deal_id, kind, body_json, created_at
		FROM hub_bbox WHERE recipient_pub = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		recipientPub, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BboxMessage
	for rows.Next() {
		var m BboxMessage
		var body []byte
		if err := rows.Scan(&m.Seq, &m.Office, &m.RecipientPub, &m.SenderPub, &m.DealID, &m.Kind, &body, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Body = json.RawMessage(body)
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpsertBounty creates or updates a Hub knowledge bounty.
func (s *Store) UpsertBounty(b Bounty) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.CreatedAt == 0 {
		b.CreatedAt = time.Now().Unix()
	}
	if b.MaxFills <= 0 {
		b.MaxFills = 10
	}
	active := 0
	if b.Active {
		active = 1
	}
	_, err := s.sql.Exec(`
		INSERT OR REPLACE INTO hub_bounties (id, domain, question, reward_sats, max_fills, filled, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, strings.ToLower(strings.TrimSpace(b.Domain)), b.Question, b.RewardSats, b.MaxFills, b.Filled, active, b.CreatedAt)
	return err
}

// ListBounties returns active Hub bounties.
func (s *Store) ListBounties(onlyActive bool) ([]Bounty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT id, domain, question, reward_sats, max_fills, filled, active, created_at FROM hub_bounties`
	if onlyActive {
		q += ` WHERE active = 1 AND filled < max_fills`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.sql.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bounty
	for rows.Next() {
		var b Bounty
		var active int
		if err := rows.Scan(&b.ID, &b.Domain, &b.Question, &b.RewardSats, &b.MaxFills, &b.Filled, &active, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Active = active != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBounty returns a single bounty by ID.
func (s *Store) GetBounty(id string) (Bounty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b Bounty
	var active int
	err := s.sql.QueryRow(`SELECT id, domain, question, reward_sats, max_fills, filled, active, created_at FROM hub_bounties WHERE id = ?`, id).
		Scan(&b.ID, &b.Domain, &b.Question, &b.RewardSats, &b.MaxFills, &b.Filled, &active, &b.CreatedAt)
	b.Active = active != 0
	return b, err
}

// HasBountyClaim reports whether an HDP digest was already paid by the bounty pool.
func (s *Store) HasBountyClaim(hdp string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(*) FROM hub_bounty_claims WHERE hdp = ?`, hdp).Scan(&n)
	return n > 0, err
}

// RecordBountyClaim atomically records a paid bounty claim and increments the bounty's filled count.
func (s *Store) RecordBountyClaim(bountyID, hdp, sellerPub, txid string, rewardSats uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		INSERT INTO hub_bounty_claims (hdp, bounty_id, seller_pub, txid, reward_sats, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hdp, bountyID, sellerPub, txid, rewardSats, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE hub_bounties SET filled = filled + 1 WHERE id = ?`, bountyID); err != nil {
		return err
	}
	return tx.Commit()
}
