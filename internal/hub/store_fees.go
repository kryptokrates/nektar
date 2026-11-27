package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Hub Fee Claims, Earnings Ledger, Metered Egress Credits & Flagged Payers
// -----------------------------------------------------------------------------

var (
	ErrFeePaymentReplayed = errors.New("hub: payment txid already claimed")
	ErrFeePaymentConflict = errors.New("hub: payment input outpoint already spent by another claimed payment")
)

// FeeLedgerEntry records one collected or held Hub operator fee payment.
type FeeLedgerEntry struct {
	ID          int64  `json:"id,omitempty"`
	TxID        string `json:"txid"`
	Action      string `json:"action"` // submit:want | submit:offer | submit:announce | bbox:send | lookup:history | lookup:export | egress:topup
	Kind        string `json:"kind,omitempty"`
	PayerPub    string `json:"payerPub,omitempty"`
	Sats        uint64 `json:"sats"`
	Decision    string `json:"decision"` // fast | hold | mined
	Status      string `json:"status"`   // unmined | mined | lost | held
	BlockHeight uint32 `json:"blockHeight,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
}

// FeeEarningsSummary aggregates operator revenue across all 4 Spine-and-Leaf fee valves.
type FeeEarningsSummary struct {
	TotalEarnedSats      uint64            `json:"totalEarnedSats"`
	Earned24hSats        uint64            `json:"earned24hSats"`
	MinedEarnedSats      uint64            `json:"minedEarnedSats"`
	UnminedFastSats      uint64            `json:"unminedFastSats"`
	TotalPayments        int64             `json:"totalPayments"`
	ByActionSats         map[string]uint64 `json:"byActionSats"`
	ByActionCount        map[string]int64  `json:"byActionCount"`
	TotalEgressDelivered int64             `json:"totalEgressDelivered"`
	ActiveCreditAccounts int64             `json:"activeCreditAccounts"`
	FlaggedPayers        int64             `json:"flaggedPayers"`
}

// EgressCredit tracks one downstream Leaf Hub's or Agent's prepaid BRC-105 egress meter balance.
type EgressCredit struct {
	PayerPub             string `json:"payerPub"`
	BalanceMilliSats     int64  `json:"balanceMilliSats"`
	TotalFundedSats      uint64 `json:"totalFundedSats"`
	TotalEventsDelivered int64  `json:"totalEventsDelivered"`
	UpdatedAt            int64  `json:"updatedAt"`
}

// ClaimFeePaymentAtomic atomically locks txid AND every spent input outpoint in SQLite
// (matching Lightweb bcommon/payee.Claims anti-replay & anti-conflict semantics).
// If txid was previously recorded as 'held' for the exact same (action, payerPub) and status == "claimed",
// it atomically promotes the held claim to 'claimed' so held payments succeed cleanly once mined.
func (s *Store) ClaimFeePaymentAtomic(txid string, inputs []string, action, payerPub string, sats uint64, status string) error {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if txid == "" {
		return errors.New("hub: empty payment txid")
	}
	if status == "" {
		status = "claimed"
	}
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existingAction, existingPayer, existingStatus string
	err = tx.QueryRow(`SELECT action, payer_pub, status FROM hub_fee_claims WHERE txid = ?`, txid).
		Scan(&existingAction, &existingPayer, &existingStatus)
	if err == nil {
		// Allow idempotent upgrade of a previously held payment once mined for the exact same action & payer.
		if existingStatus == "held" && status == "claimed" && existingAction == action && strings.EqualFold(existingPayer, payerPub) {
			if _, uErr := tx.Exec(`UPDATE hub_fee_claims SET status = 'claimed' WHERE txid = ?`, txid); uErr != nil {
				return uErr
			}
			return tx.Commit()
		}
		if existingStatus == "held" && status == "held" && existingAction == action && strings.EqualFold(existingPayer, payerPub) {
			return tx.Commit()
		}
		return ErrFeePaymentReplayed
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Enforce spent-input-outpoint uniqueness across distinct txids (blocks conflicting 0-conf double-spends).
	for _, op := range inputs {
		op = strings.ToLower(strings.TrimSpace(op))
		if op == "" {
			continue
		}
		if _, iErr := tx.Exec(`INSERT INTO hub_fee_claim_inputs (outpoint, txid, created_at) VALUES (?, ?, ?)`, op, txid, now); iErr != nil {
			if isSQLiteConstraintErr(iErr) {
				return ErrFeePaymentConflict
			}
			return iErr
		}
	}

	inJSON, _ := json.Marshal(inputs)
	if _, err := tx.Exec(`
		INSERT INTO hub_fee_claims (txid, input_outpoints_json, action, payer_pub, sats, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		txid, string(inJSON), action, strings.ToLower(strings.TrimSpace(payerPub)), sats, status, now); err != nil {
		if isSQLiteConstraintErr(err) {
			return ErrFeePaymentReplayed
		}
		return err
	}
	return tx.Commit()
}

// ReleaseFeeClaim removes a claimed payment and its input locks on pre-admission / pre-internalize rollback.
func (s *Store) ReleaseFeeClaim(txid string) error {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if txid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, _ = tx.Exec(`DELETE FROM hub_fee_claim_inputs WHERE txid = ?`, txid)
	_, _ = tx.Exec(`DELETE FROM hub_fee_claims WHERE txid = ?`, txid)
	return tx.Commit()
}

// RecordFeeLedger inserts or updates a durable Hub operator earnings row.
func (s *Store) RecordFeeLedger(e FeeLedgerEntry) error {
	e.TxID = strings.ToLower(strings.TrimSpace(e.TxID))
	if e.TxID == "" {
		return errors.New("hub: fee ledger requires txid")
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	if e.Decision == "" {
		e.Decision = "fast"
	}
	if e.Status == "" {
		e.Status = "unmined"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`
		INSERT INTO hub_fee_ledger (txid, action, kind, payer_pub, sats, decision, status, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(txid) DO UPDATE SET
			decision = excluded.decision,
			status = excluded.status,
			block_height = CASE WHEN excluded.block_height > 0 THEN excluded.block_height ELSE hub_fee_ledger.block_height END`,
		e.TxID, e.Action, e.Kind, strings.ToLower(strings.TrimSpace(e.PayerPub)), e.Sats, e.Decision, e.Status, e.BlockHeight, e.CreatedAt)
	return err
}

// MarkFeeMined promotes a collected fee payment to status='mined' once confirmed at settleDepth.
func (s *Store) MarkFeeMined(txid string, height uint32) error {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if txid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_fee_ledger SET status = 'mined', block_height = ? WHERE txid = ? AND status != 'lost'`, height, txid)
	return err
}

// MarkFeeLost marks an unsettleable/double-spent fast fee payment as 'lost'.
func (s *Store) MarkFeeLost(txid string) error {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if txid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_fee_ledger SET status = 'lost' WHERE txid = ?`, txid)
	return err
}

// UnminedFastExposure returns unmined fast-path satoshis accepted within the rolling window
// for payerPub and globally across all payers (matching Lightweb bcommon/acceptance.Exposure).
func (s *Store) UnminedFastExposure(payerPub string, windowSec int64) (payerSats uint64, totalSats uint64, err error) {
	if windowSec <= 0 {
		windowSec = 3600
	}
	cutoff := time.Now().Unix() - windowSec
	payerPub = strings.ToLower(strings.TrimSpace(payerPub))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.sql.QueryRow(`SELECT COALESCE(SUM(sats), 0) FROM hub_fee_ledger WHERE decision = 'fast' AND status = 'unmined' AND created_at >= ?`, cutoff).Scan(&totalSats); err != nil {
		return 0, 0, err
	}
	if payerPub != "" {
		if err = s.sql.QueryRow(`SELECT COALESCE(SUM(sats), 0) FROM hub_fee_ledger WHERE decision = 'fast' AND status = 'unmined' AND payer_pub = ? AND created_at >= ?`, payerPub, cutoff).Scan(&payerSats); err != nil {
			return 0, 0, err
		}
	}
	return payerSats, totalSats, nil
}

// FlagPayer permanently flags a payer pubkey whose fast payment double-spent or failed to mine
// (durable SQLite twin of Lightweb's unsettleable.jsonl), forcing future payments into 202 hold.
func (s *Store) FlagPayer(payerPub, txid, reason string) error {
	payerPub = strings.ToLower(strings.TrimSpace(payerPub))
	if payerPub == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`
		INSERT INTO hub_flagged_payers (payer_pub, txid, reason, flagged_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(payer_pub) DO UPDATE SET txid = excluded.txid, reason = excluded.reason, flagged_at = excluded.flagged_at`,
		payerPub, strings.ToLower(strings.TrimSpace(txid)), reason, time.Now().Unix())
	return err
}

// IsPayerFlagged reports whether payerPub is currently flagged as unsettleable.
func (s *Store) IsPayerFlagged(payerPub string) (bool, string, error) {
	payerPub = strings.ToLower(strings.TrimSpace(payerPub))
	if payerPub == "" {
		return false, "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var reason string
	err := s.sql.QueryRow(`SELECT reason FROM hub_flagged_payers WHERE payer_pub = ?`, payerPub).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, reason, nil
}

// CreditEgressMilliSats atomically adds prepaid egress credit to payerPub's metered account.
func (s *Store) CreditEgressMilliSats(payerPub string, addMilliSats int64, fundedSats uint64) (EgressCredit, error) {
	payerPub = strings.ToLower(strings.TrimSpace(payerPub))
	if payerPub == "" {
		return EgressCredit{}, errors.New("hub: payerPub required for egress credit")
	}
	if addMilliSats <= 0 {
		return EgressCredit{}, errors.New("hub: egress credit increment must be > 0")
	}
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.sql.Exec(`
		INSERT INTO hub_egress_credits (payer_pub, balance_msats, total_funded_sats, total_events_delivered, updated_at)
		VALUES (?, ?, ?, 0, ?)
		ON CONFLICT(payer_pub) DO UPDATE SET
			balance_msats = hub_egress_credits.balance_msats + excluded.balance_msats,
			total_funded_sats = hub_egress_credits.total_funded_sats + excluded.total_funded_sats,
			updated_at = excluded.updated_at`,
		payerPub, addMilliSats, fundedSats, now); err != nil {
		return EgressCredit{}, err
	}
	var out EgressCredit
	err := s.sql.QueryRow(`SELECT payer_pub, balance_msats, total_funded_sats, total_events_delivered, updated_at FROM hub_egress_credits WHERE payer_pub = ?`, payerPub).
		Scan(&out.PayerPub, &out.BalanceMilliSats, &out.TotalFundedSats, &out.TotalEventsDelivered, &out.UpdatedAt)
	return out, err
}

// ChargeEgressEvents atomically deducts eventCount * msatsPerEvent from payerPub's egress meter.
// Returns (remainingBalance, true, nil) on success, or (currentBalance, false, nil) if insufficient credit.
func (s *Store) ChargeEgressEvents(payerPub string, eventCount, msatsPerEvent int64) (int64, bool, error) {
	payerPub = strings.ToLower(strings.TrimSpace(payerPub))
	if eventCount <= 0 || msatsPerEvent <= 0 {
		return 0, true, nil
	}
	if payerPub == "" {
		return 0, false, nil
	}
	cost := eventCount * msatsPerEvent
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.sql.Exec(`
		UPDATE hub_egress_credits
		SET balance_msats = balance_msats - ?,
		    total_events_delivered = total_events_delivered + ?,
		    updated_at = ?
		WHERE payer_pub = ? AND balance_msats >= ?`,
		cost, eventCount, now, payerPub, cost)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	var bal int64
	if qErr := s.sql.QueryRow(`SELECT COALESCE(balance_msats, 0) FROM hub_egress_credits WHERE payer_pub = ?`, payerPub).Scan(&bal); qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
		return 0, false, qErr
	}
	return bal, n == 1, nil
}

// GetEgressCredit returns the current egress credit balance for payerPub (zero-valued if unseen).
func (s *Store) GetEgressCredit(payerPub string) (EgressCredit, error) {
	payerPub = strings.ToLower(strings.TrimSpace(payerPub))
	if payerPub == "" {
		return EgressCredit{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out EgressCredit
	err := s.sql.QueryRow(`SELECT payer_pub, balance_msats, total_funded_sats, total_events_delivered, updated_at FROM hub_egress_credits WHERE payer_pub = ?`, payerPub).
		Scan(&out.PayerPub, &out.BalanceMilliSats, &out.TotalFundedSats, &out.TotalEventsDelivered, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EgressCredit{PayerPub: payerPub}, nil
	}
	return out, err
}

// FeeEarningsSummary returns aggregate Hub operator earnings and egress telemetry.
func (s *Store) FeeEarningsSummary(sinceUnix int64) (FeeEarningsSummary, error) {
	out := FeeEarningsSummary{
		ByActionSats:  make(map[string]uint64),
		ByActionCount: make(map[string]int64),
	}
	if sinceUnix <= 0 {
		sinceUnix = time.Now().Add(-24 * time.Hour).Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.sql.Query(`
		SELECT action, status, decision, sats, created_at
		FROM hub_fee_ledger
		WHERE status NOT IN ('lost', 'held')`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var action, status, decision string
		var sats uint64
		var createdAt int64
		if err := rows.Scan(&action, &status, &decision, &sats, &createdAt); err != nil {
			rows.Close()
			return out, err
		}
		out.TotalEarnedSats += sats
		out.TotalPayments++
		if createdAt >= sinceUnix {
			out.Earned24hSats += sats
		}
		if status == "mined" {
			out.MinedEarnedSats += sats
		} else if decision == "fast" {
			out.UnminedFastSats += sats
		}
		out.ByActionSats[action] += sats
		out.ByActionCount[action]++
	}
	rows.Close()

	_ = s.sql.QueryRow(`SELECT COALESCE(SUM(total_events_delivered), 0), COUNT(1) FROM hub_egress_credits`).
		Scan(&out.TotalEgressDelivered, &out.ActiveCreditAccounts)
	_ = s.sql.QueryRow(`SELECT COUNT(1) FROM hub_flagged_payers`).Scan(&out.FlaggedPayers)
	return out, nil
}
