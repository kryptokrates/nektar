package hub

import (
	"encoding/json"
	"time"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

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

// DomainTag returns the explicit bbox relay send domain-separation tag.
func (p BboxSignablePayload) DomainTag() []byte { return core.TagBboxSendV1 }

// EncodeCanonicalCBOR encodes p into canonical RFC 8949 CDE CBOR for signing.
func (p BboxSignablePayload) EncodeCanonicalCBOR() ([]byte, error) {
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: p.Office},
		{Key: uint64(2), Val: p.RecipientPub},
		{Key: uint64(3), Val: p.SenderPub},
		{Key: uint64(4), Val: p.DealID},
		{Key: uint64(5), Val: p.Kind},
		{Key: uint64(6), Val: p.TS},
		{Key: uint64(7), Val: p.Nonce},
		{Key: uint64(8), Val: p.Aud},
		{Key: uint64(9), Val: []byte(p.Body)},
	})
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
		var boundarySeq int64
		if err := s.sql.QueryRow(`SELECT seq FROM hub_bbox ORDER BY seq DESC LIMIT 1 OFFSET ?`, maxRows).Scan(&boundarySeq); err == nil && boundarySeq > 0 {
			if res, dErr := s.sql.Exec(`DELETE FROM hub_bbox WHERE seq <= ?`, boundarySeq); dErr == nil {
				n, _ := res.RowsAffected()
				total += n
			}
		}
	}
	return total, nil
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
	rdb, release := s.rdb()
	defer release()
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := rdb.Query(`
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
