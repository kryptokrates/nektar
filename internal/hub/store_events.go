package hub

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// StoredEvent is one durably persisted admitted market output on the Hub.
type StoredEvent struct {
	Seq         int64                `json:"seq"`
	Outpoint    string               `json:"outpoint"`
	Topic       string               `json:"topic"`
	TxID        string               `json:"txid"`
	Vout        uint32               `json:"vout"`
	Kind        bstack.ArtifactKind  `json:"kind"`
	Pub         string               `json:"pub"`
	Domains     []string             `json:"domains"`
	Payload     bstack.MarketPayload `json:"payload"`
	BEEF        []byte               `json:"beef,omitempty"`
	BlockHeight int64                `json:"blockHeight,omitempty"`
	Exp         int64                `json:"exp"`
	AdmittedAt  int64                `json:"admittedAt"`
}

// DomainDensity summarizes live Hub market activity per domain (for Prometheus & Dashboard).
type DomainDensity struct {
	Domain string `json:"domain"`
	Wants  int64  `json:"wants"`
	Offers int64  `json:"offers"`
}

// AppendEvent durably stores an admitted output and returns its monotonic seq and whether it was new.
// If the outpoint already exists and the incoming event carries a newer mined block height or
// refreshed BEEF bytes (e.g. a newly attached BUMP), the stored row is upgraded in place.
func (s *Store) AppendEvent(ev StoredEvent) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var existingSeq int64
	var existingHeight int64
	var existingBEEF []byte
	err := s.sql.QueryRow(`SELECT seq, COALESCE(block_height, 0), beef FROM hub_events WHERE outpoint = ?`, ev.Outpoint).Scan(&existingSeq, &existingHeight, &existingBEEF)
	if err == nil {
		if len(ev.BEEF) > 0 && (ev.BlockHeight > existingHeight || (ev.BlockHeight > 0 && !bytes.Equal(existingBEEF, ev.BEEF))) {
			if _, uErr := s.sql.Exec(`UPDATE hub_events SET beef = ?, block_height = ? WHERE outpoint = ?`, ev.BEEF, ev.BlockHeight, ev.Outpoint); uErr != nil {
				return existingSeq, false, uErr
			}
		}
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
		INSERT INTO hub_events (outpoint, topic, txid, vout, kind, pub, domains_json, payload_json, beef, exp, admitted_at, spent, block_height)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		ev.Outpoint, ev.Topic, ev.TxID, ev.Vout, string(ev.Kind), ev.Pub, string(doms), pJSON, ev.BEEF, ev.Exp, ev.AdmittedAt, ev.BlockHeight)
	if err != nil {
		return 0, false, err
	}
	seq, err := res.LastInsertId()
	return seq, true, err
}

// FindEventTopicByOutpoint reports whether an outpoint was previously admitted and whether it is spent.
func (s *Store) FindEventTopicByOutpoint(outpoint string) (topic string, spent bool, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sp int
	err = s.sql.QueryRow(`SELECT topic, spent FROM hub_events WHERE outpoint = ?`, outpoint).Scan(&topic, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return topic, sp != 0, true, nil
}

// MarkOutpointSpent marks a previously admitted Hub event outpoint as spent so it is excluded from active scans.
func (s *Store) MarkOutpointSpent(outpoint string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var topic string
	var sp int
	err := s.sql.QueryRow(`SELECT topic, spent FROM hub_events WHERE outpoint = ?`, outpoint).Scan(&topic, &sp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if sp == 0 {
		if _, err := s.sql.Exec(`UPDATE hub_events SET spent = 1 WHERE outpoint = ?`, outpoint); err != nil {
			return "", false, err
		}
	}
	return topic, true, nil
}

// EvictEventsFromHeight removes all persisted Hub events anchored at block_height >= forkHeight
// (triggered on verified chain reorgs) and returns the evicted events so lookup services can evict them too.
func (s *Store) EvictEventsFromHeight(forkHeight uint32) ([]StoredEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.sql.Query(`
		SELECT seq, outpoint, topic, txid, vout, kind, pub, domains_json, payload_json, beef, block_height, exp, admitted_at
		FROM hub_events WHERE block_height >= ? AND block_height > 0`, forkHeight)
	if err != nil {
		return nil, err
	}
	var evicted []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var kindStr, domJSON string
		var pJSON []byte
		if err := rows.Scan(&e.Seq, &e.Outpoint, &e.Topic, &e.TxID, &e.Vout, &kindStr, &e.Pub, &domJSON, &pJSON, &e.BEEF, &e.BlockHeight, &e.Exp, &e.AdmittedAt); err != nil {
			rows.Close()
			return nil, err
		}
		e.Kind = bstack.ArtifactKind(kindStr)
		_ = json.Unmarshal([]byte(domJSON), &e.Domains)
		_ = json.Unmarshal(pJSON, &e.Payload)
		evicted = append(evicted, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(evicted) > 0 {
		if _, err := s.sql.Exec(`DELETE FROM hub_events WHERE block_height >= ? AND block_height > 0`, forkHeight); err != nil {
			return nil, err
		}
	}
	return evicted, nil
}

// LatestSeq returns the highest monotonic sequence number persisted in hub_events (0 if empty).
func (s *Store) LatestSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest int64
	_ = s.sql.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM hub_events`).Scan(&latest)
	return latest
}

// EventsSince loads unexpired events with seq > cursor ordered ascending (for SSE resume & poll floor).
func (s *Store) EventsSince(cursor int64, domainFilter []string, limit int) ([]StoredEvent, error) {
	ev, _, _, err := s.EventsSinceScanned(cursor, domainFilter, limit)
	return ev, err
}

// EventsSinceScanned loads up to `limit` raw rows with seq > cursor, applies optional domainFilter,
// and returns the matched events PLUS the highest raw SQLite seq scanned and raw row count so
// callers can paginate past non-matching domain windows without truncating backlog replay.
func (s *Store) EventsSinceScanned(cursor int64, domainFilter []string, limit int) (events []StoredEvent, nextCursor int64, rawCount int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	nowMs := time.Now().UnixMilli()
	rows, err := s.sql.Query(`
		SELECT seq, outpoint, topic, txid, vout, kind, pub, domains_json, payload_json, beef, COALESCE(block_height, 0), exp, admitted_at
		FROM hub_events
		WHERE seq > ? AND spent = 0 AND (exp = 0 OR exp > ?)
		ORDER BY seq ASC LIMIT ?`, cursor, nowMs, limit)
	if err != nil {
		return nil, cursor, 0, err
	}
	defer rows.Close()
	wantDom := make(map[string]bool, len(domainFilter))
	for _, d := range domainFilter {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			wantDom[d] = true
		}
	}
	nextCursor = cursor
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var kindStr, domJSON string
		var pJSON []byte
		if err := rows.Scan(&e.Seq, &e.Outpoint, &e.Topic, &e.TxID, &e.Vout, &kindStr, &e.Pub, &domJSON, &pJSON, &e.BEEF, &e.BlockHeight, &e.Exp, &e.AdmittedAt); err != nil {
			return nil, nextCursor, rawCount, err
		}
		rawCount++
		if e.Seq > nextCursor {
			nextCursor = e.Seq
		}
		e.Kind = bstack.ArtifactKind(kindStr)
		_ = json.Unmarshal([]byte(domJSON), &e.Domains)
		_ = json.Unmarshal(pJSON, &e.Payload)
		if len(wantDom) > 0 && !matchesAnyDomain(e.Domains, wantDom) {
			continue
		}
		out = append(out, e)
	}
	return out, nextCursor, rawCount, rows.Err()
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

// Carrier journal errors enforcing 1BSV Paper 011 §6 (one funding output admits at most one carrier,
// and a mined kill-switch sweep permanently retracts the carrier across restarts).
var (
	ErrCarrierFundingReused = errors.New("hub: carrier funding outpoint already consumed by another carrier")
	ErrCarrierRetracted     = errors.New("hub: carrier funding outpoint was retracted by kill-switch sweep")
)

// ClaimCarrierFundingOutpoint atomically binds fundingOutpoint to carrierTxID on topic.
// Returns nil on first claim or idempotent replay of the exact same carrierTxID.
// Returns ErrCarrierRetracted if a kill-switch sweep already spent fundingOutpoint, or
// ErrCarrierFundingReused if a different carrierTxID previously claimed fundingOutpoint.
func (s *Store) ClaimCarrierFundingOutpoint(fundingOutpoint, carrierTxID, carrierOutpoint, topic string) error {
	if fundingOutpoint == "" || carrierTxID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var existingTx, sweptBy string
	err := s.sql.QueryRow(
		`SELECT carrier_txid, swept_by_txid FROM hub_carrier_outpoints WHERE funding_outpoint = ?`,
		fundingOutpoint,
	).Scan(&existingTx, &sweptBy)
	if err == nil {
		if sweptBy != "" {
			return ErrCarrierRetracted
		}
		if existingTx != carrierTxID {
			return ErrCarrierFundingReused
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.sql.Exec(`
		INSERT INTO hub_carrier_outpoints (funding_outpoint, carrier_txid, carrier_outpoint, topic, swept_by_txid, admitted_at)
		VALUES (?, ?, ?, ?, '', ?)`,
		fundingOutpoint, carrierTxID, carrierOutpoint, topic, time.Now().UnixMilli())
	return err
}

// MarkCarrierFundingSwept records a verified mineable kill-switch sweep spending fundingOutpoint,
// marks any previously admitted carrier in hub_events as spent=1 so it is dropped from active
// lookups/sync, and returns the retracted carrier outpoint & topic if one was present.
// Even if no carrier was seen yet, a tombstone row is persisted so a late carrier is refused.
func (s *Store) MarkCarrierFundingSwept(fundingOutpoint, sweepTxID string) (carrierOutpoint, topic string, hadCarrier bool, err error) {
	if fundingOutpoint == "" || sweepTxID == "" {
		return "", "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var existingTx string
	err = s.sql.QueryRow(
		`SELECT carrier_txid, carrier_outpoint, topic FROM hub_carrier_outpoints WHERE funding_outpoint = ?`,
		fundingOutpoint,
	).Scan(&existingTx, &carrierOutpoint, &topic)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.sql.Exec(`
			INSERT INTO hub_carrier_outpoints (funding_outpoint, carrier_txid, carrier_outpoint, topic, swept_by_txid, admitted_at)
			VALUES (?, '', '', '', ?, ?)`,
			fundingOutpoint, sweepTxID, time.Now().UnixMilli())
		return "", "", false, err
	}
	if err != nil {
		return "", "", false, err
	}
	if _, err := s.sql.Exec(
		`UPDATE hub_carrier_outpoints SET swept_by_txid = ? WHERE funding_outpoint = ?`,
		sweepTxID, fundingOutpoint,
	); err != nil {
		return "", "", false, err
	}
	if carrierOutpoint != "" {
		_, _ = s.sql.Exec(`UPDATE hub_events SET spent = 1 WHERE outpoint = ?`, carrierOutpoint)
		return carrierOutpoint, topic, true, nil
	}
	return "", "", false, nil
}

// EventsInRange loads unexpired active events with seq >= fromSeq AND seq <= toSeq (ordered ascending)
// for BRC-149 Application-Layer Object Plane NACK repair.
func (s *Store) EventsInRange(fromSeq, toSeq int64, topics []string, limit int) ([]StoredEvent, error) {
	ev, _, _, err := s.EventsInRangeScanned(fromSeq, toSeq, topics, limit)
	return ev, err
}

// EventsInRangeScanned loads up to `limit` raw rows with seq in [fromSeq, toSeq], applies optional
// topic filtering, and returns the matched events PLUS the highest raw SQLite seq scanned and raw row count
// so BRC-149 plane backfill can paginate past 500 frames and non-matching topics without truncation.
func (s *Store) EventsInRangeScanned(fromSeq, toSeq int64, topics []string, limit int) (events []StoredEvent, nextSeq int64, rawCount int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromSeq < 1 {
		fromSeq = 1
	}
	if toSeq < fromSeq {
		return nil, fromSeq - 1, 0, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	nowMs := time.Now().UnixMilli()
	rows, err := s.sql.Query(`
		SELECT seq, outpoint, topic, txid, vout, kind, pub, domains_json, payload_json, beef, COALESCE(block_height, 0), exp, admitted_at
		FROM hub_events
		WHERE seq >= ? AND seq <= ? AND spent = 0 AND (exp = 0 OR exp > ?)
		ORDER BY seq ASC LIMIT ?`, fromSeq, toSeq, nowMs, limit)
	if err != nil {
		return nil, fromSeq - 1, 0, err
	}
	defer rows.Close()
	wantTopic := make(map[string]bool, len(topics))
	for _, t := range topics {
		if t = strings.TrimSpace(t); t != "" {
			wantTopic[t] = true
		}
	}
	nextSeq = fromSeq - 1
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var kindStr, domJSON string
		var pJSON []byte
		if err := rows.Scan(&e.Seq, &e.Outpoint, &e.Topic, &e.TxID, &e.Vout, &kindStr, &e.Pub, &domJSON, &pJSON, &e.BEEF, &e.BlockHeight, &e.Exp, &e.AdmittedAt); err != nil {
			return nil, nextSeq, rawCount, err
		}
		rawCount++
		if e.Seq > nextSeq {
			nextSeq = e.Seq
		}
		if len(wantTopic) > 0 && !wantTopic[e.Topic] {
			continue
		}
		e.Kind = bstack.ArtifactKind(kindStr)
		_ = json.Unmarshal([]byte(domJSON), &e.Domains)
		_ = json.Unmarshal(pJSON, &e.Payload)
		out = append(out, e)
	}
	return out, nextSeq, rawCount, rows.Err()
}

// DomainDensityStats counts unexpired wants and offers per domain on this Hub.
func (s *Store) DomainDensityStats() ([]DomainDensity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowMs := time.Now().UnixMilli()
	rows, err := s.sql.Query(`SELECT kind, domains_json FROM hub_events WHERE spent = 0 AND (exp = 0 OR exp > ?) AND kind IN ('want', 'offer')`, nowMs)
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
