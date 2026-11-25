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

// BboxMessage is a signed, E2E-encrypted store-and-forward message relayed for counterparties behind NAT.
type BboxMessage struct {
	Seq          int64           `json:"seq"`
	Office       string          `json:"office"`
	RecipientPub string          `json:"recipientPub"`
	SenderPub    string          `json:"senderPub"`
	DealID       string          `json:"dealId"`
	Kind         string          `json:"kind"` // purchase_beef | reveal_key | receipt
	TS           int64           `json:"ts,omitempty"`
	Nonce        string          `json:"nonce,omitempty"`
	Aud          string          `json:"aud,omitempty"`
	Body         json.RawMessage `json:"body"`
	Sig          string          `json:"sig,omitempty"`
	CreatedAt    int64           `json:"createdAt"`
}

// BboxSignablePayload is the canonical signed portion of BboxMessage (excluding server-assigned Seq/CreatedAt/Sig).
type BboxSignablePayload struct {
	Office       string          `json:"office"`
	RecipientPub string          `json:"recipientPub"`
	SenderPub    string          `json:"senderPub"`
	DealID       string          `json:"dealId"`
	Kind         string          `json:"kind"`
	TS           int64           `json:"ts"`
	Nonce        string          `json:"nonce"`
	Aud          string          `json:"aud"`
	Body         json.RawMessage `json:"body"`
}

// Signable returns the canonical signable struct for BboxMessage.
func (m BboxMessage) Signable() BboxSignablePayload {
	return BboxSignablePayload{
		Office:       m.Office,
		RecipientPub: m.RecipientPub,
		SenderPub:    m.SenderPub,
		DealID:       m.DealID,
		Kind:         m.Kind,
		TS:           m.TS,
		Nonce:        m.Nonce,
		Aud:          m.Aud,
		Body:         m.Body,
	}
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
	status      TEXT NOT NULL DEFAULT 'paid',
	created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS hub_fed_cursors (
	peer       TEXT PRIMARY KEY,
	cursor     INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
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

// PruneBbox deletes old bbox relay envelopes and enforces a hard row cap to bound storage.
func (s *Store) PruneBbox(cutoffMs int64, maxRows int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	if cutoffMs > 0 {
		if res, err := s.sql.Exec(`DELETE FROM hub_bbox WHERE created_at > 0 AND created_at < ?`, cutoffMs); err == nil {
			n, _ := res.RowsAffected()
			total += n
		}
	}
	if maxRows > 0 {
		if res, err := s.sql.Exec(`DELETE FROM hub_bbox WHERE seq NOT IN (SELECT seq FROM hub_bbox ORDER BY seq DESC LIMIT ?)`, maxRows); err == nil {
			n, _ := res.RowsAffected()
			total += n
		}
	}
	return total, nil
}

// DomainDensity summarizes live Hub market activity per domain (for Prometheus & Dashboard).
type DomainDensity struct {
	Domain string `json:"domain"`
	Wants  int64  `json:"wants"`
	Offers int64  `json:"offers"`
}

// DomainDensityStats counts unexpired wants and offers per domain on this Hub.
func (s *Store) DomainDensityStats() ([]DomainDensity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowMs := time.Now().UnixMilli()
	rows, err := s.sql.Query(`SELECT kind, domains_json FROM hub_events WHERE (exp = 0 OR exp > ?) AND kind IN ('want', 'offer')`, nowMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]*DomainDensity)
	for rows.Next() {
		var kind, domJSON string
		if err := rows.Scan(&kind, &domJSON); err != nil {
			return nil, err
		}
		var doms []string
		_ = json.Unmarshal([]byte(domJSON), &doms)
		for _, d := range doms {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "" {
				continue
			}
			c, ok := counts[d]
			if !ok {
				c = &DomainDensity{Domain: d}
				counts[d] = c
			}
			if kind == "want" {
				c.Wants++
			} else if kind == "offer" {
				c.Offers++
			}
		}
	}
	out := make([]DomainDensity, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	return out, rows.Err()
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
		INSERT INTO hub_bbox (office, recipient_pub, sender_pub, deal_id, kind, body_json, sig, ts, nonce, aud, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.Office, msg.RecipientPub, msg.SenderPub, msg.DealID, msg.Kind, []byte(msg.Body), msg.Sig, msg.TS, msg.Nonce, msg.Aud, msg.CreatedAt)
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
		SELECT seq, office, recipient_pub, sender_pub, deal_id, kind, body_json, COALESCE(sig, ''), COALESCE(ts, 0), COALESCE(nonce, ''), COALESCE(aud, ''), created_at
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
		if err := rows.Scan(&m.Seq, &m.Office, &m.RecipientPub, &m.SenderPub, &m.DealID, &m.Kind, &body, &m.Sig, &m.TS, &m.Nonce, &m.Aud, &m.CreatedAt); err != nil {
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

var (
	ErrBountyFilled    = errors.New("hub: bounty not found, inactive, or already filled")
	ErrBountyDuplicate = errors.New("hub: duplicate fragment HDP already claimed")
)

// ReserveBountyClaim atomically increments filled (guarding active=1 AND filled < max_fills)
// and inserts a pending claim for hdp inside ONE SQLite transaction BEFORE payout.
func (s *Store) ReserveBountyClaim(bountyID, hdp, sellerPub string, rewardSats uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE hub_bounties SET filled = filled + 1 WHERE id = ? AND active = 1 AND filled < max_fills`, bountyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrBountyFilled
	}
	if _, err := tx.Exec(`
		INSERT INTO hub_bounty_claims (hdp, bounty_id, seller_pub, txid, reward_sats, status, created_at)
		VALUES (?, ?, ?, '', ?, 'pending', ?)`,
		hdp, bountyID, sellerPub, rewardSats, time.Now().Unix()); err != nil {
		return ErrBountyDuplicate
	}
	return tx.Commit()
}

// FinalizeBountyClaim marks a reserved bounty claim as paid and records its payout txid.
func (s *Store) FinalizeBountyClaim(hdp, txid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_bounty_claims SET txid = ?, status = 'paid' WHERE hdp = ?`, txid, hdp)
	return err
}

// CancelBountyClaim rolls back a pending bounty claim reservation if validation or pre-broadcast payout fails.
func (s *Store) CancelBountyClaim(bountyID, hdp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM hub_bounty_claims WHERE hdp = ? AND status = 'pending'`, hdp)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		_, _ = tx.Exec(`UPDATE hub_bounties SET filled = CASE WHEN filled > 0 THEN filled - 1 ELSE 0 END WHERE id = ?`, bountyID)
	}
	return tx.Commit()
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
		INSERT INTO hub_bounty_claims (hdp, bounty_id, seller_pub, txid, reward_sats, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'paid', ?)`,
		hdp, bountyID, sellerPub, txid, rewardSats, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE hub_bounties SET filled = filled + 1 WHERE id = ?`, bountyID); err != nil {
		return err
	}
	return tx.Commit()
}
