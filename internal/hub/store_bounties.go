package hub

import (
	"errors"
	"strings"
	"time"
)

// BountyClaim represents one persisted Hub bounty claim row.
type BountyClaim struct {
	HDP        string `json:"hdp"`
	BountyID   string `json:"bountyId"`
	SellerPub  string `json:"sellerPub"`
	TxID       string `json:"txid"`
	RewardSats uint64 `json:"rewardSats"`
	Status     string `json:"status"` // pending | broadcast | paid
	CreatedAt  int64  `json:"createdAt"`
}

// Bounty describes a domain knowledge bounty the Hub will pay agents to fulfill.
type Bounty struct {
	ID                   string `json:"id"`
	Domain               string `json:"domain"`
	Question             string `json:"question"`
	RewardSats           uint64 `json:"rewardSats"`
	MaxFills             int    `json:"maxFills"`
	Filled               int    `json:"filled"`
	Sponsored            bool   `json:"sponsored,omitempty"`
	AnchorFeeReserveSats uint64 `json:"anchorFeeReserveSats,omitempty"`
	Active               bool   `json:"active"`
	CreatedAt            int64  `json:"createdAt"`
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
	sponsored := 0
	if b.Sponsored {
		sponsored = 1
	}
	_, err := s.sql.Exec(`
		INSERT OR REPLACE INTO hub_bounties (id, domain, question, reward_sats, max_fills, filled, sponsored, anchor_fee_reserve_sats, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, strings.ToLower(strings.TrimSpace(b.Domain)), b.Question, b.RewardSats, b.MaxFills, b.Filled, sponsored, b.AnchorFeeReserveSats, active, b.CreatedAt)
	return err
}

// ListBounties returns active Hub bounties.
func (s *Store) ListBounties(onlyActive bool) ([]Bounty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT id, domain, question, reward_sats, max_fills, filled, COALESCE(sponsored, 0), COALESCE(anchor_fee_reserve_sats, 0), active, created_at FROM hub_bounties`
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
		var sponsored, active int
		if err := rows.Scan(&b.ID, &b.Domain, &b.Question, &b.RewardSats, &b.MaxFills, &b.Filled, &sponsored, &b.AnchorFeeReserveSats, &active, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Sponsored = sponsored != 0
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
	var sponsored, active int
	err := s.sql.QueryRow(`SELECT id, domain, question, reward_sats, max_fills, filled, COALESCE(sponsored, 0), COALESCE(anchor_fee_reserve_sats, 0), active, created_at FROM hub_bounties WHERE id = ?`, id).
		Scan(&b.ID, &b.Domain, &b.Question, &b.RewardSats, &b.MaxFills, &b.Filled, &sponsored, &b.AnchorFeeReserveSats, &active, &b.CreatedAt)
	b.Sponsored = sponsored != 0
	b.Active = active != 0
	return b, err
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
		if isSQLiteConstraintErr(err) {
			return ErrBountyDuplicate
		}
		return err
	}
	return tx.Commit()
}

// GetBountyClaim returns the persisted bounty claim record for a canonical HDP digest.
func (s *Store) GetBountyClaim(hdp string) (BountyClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c BountyClaim
	err := s.sql.QueryRow(`SELECT hdp, bounty_id, seller_pub, txid, reward_sats, status, created_at FROM hub_bounty_claims WHERE hdp = ?`, hdp).
		Scan(&c.HDP, &c.BountyID, &c.SellerPub, &c.TxID, &c.RewardSats, &c.Status, &c.CreatedAt)
	return c, err
}

// MarkBountyClaimBroadcast records a broadcast payout txid while keeping status='broadcast'
// until confirmed at settleDepth (preventing unconfirmed broadcasts from being marked permanently paid).
func (s *Store) MarkBountyClaimBroadcast(hdp, txid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_bounty_claims SET txid = ?, status = 'broadcast' WHERE hdp = ? AND status IN ('pending', 'broadcast')`, txid, hdp)
	return err
}

// FinalizeBountyClaim marks a reserved bounty claim as paid and records its payout txid.
func (s *Store) FinalizeBountyClaim(hdp, txid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_bounty_claims SET txid = ?, status = 'paid' WHERE hdp = ?`, txid, hdp)
	return err
}

// FinalizeBountyClaimByTxID promotes a 'broadcast' bounty claim to 'paid' once its payout tx mines at settleDepth.
func (s *Store) FinalizeBountyClaimByTxID(txid string) error {
	if txid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_bounty_claims SET status = 'paid' WHERE txid = ? AND status = 'broadcast'`, txid)
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
