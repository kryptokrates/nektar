package store

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func isConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "constraint") || strings.Contains(low, "unique")
}

// readKVUint64Tx reads a uint64 counter from kv inside tx.
// Missing rows return (0, nil); any DB error or malformed JSON fails closed.
func readKVUint64Tx(tx *sql.Tx, key string) (uint64, error) {
	var raw []byte
	err := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read kv %s: %w", key, err)
	}
	var v uint64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("store: corrupt kv counter %s: %w", key, err)
	}
	return v, nil
}

// ClaimPaymentAtomic atomically claims txid and all of its spent input outpoints inside one
// SQLite transaction. Shared by BOTH HTTP 402 paywall and /v1/market/purchase so a payment tx
// or double-spent UTXO can never be accepted twice across interfaces or restarts.
func (d *DB) ClaimPaymentAtomic(txid string, inputs []string) error {
	if txid == "" {
		return errors.New("store: empty payment txid")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	b, err := json.Marshal(inputs)
	if err != nil {
		return fmt.Errorf("store: marshal payment inputs: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO payment_claims (txid, inputs_json, claimed_at) VALUES (?, ?, ?)`, txid, b, now); err != nil {
		if isConstraintErr(err) {
			return ErrPaymentClaimReplay
		}
		return err
	}
	for _, op := range inputs {
		if op == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO payment_claim_inputs (outpoint, txid, claimed_at) VALUES (?, ?, ?)`, op, txid, now); err != nil {
			if isConstraintErr(err) {
				return ErrPaymentClaimConflict
			}
			return err
		}
	}
	return tx.Commit()
}

// ReleasePaymentClaim deletes a previously claimed txid and its input outpoints (used strictly
// when payment internalization/verification fails before delivering goods/services).
func (d *DB) ReleasePaymentClaim(txid string) error {
	if txid == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM payment_claim_inputs WHERE txid = ?`, txid); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM payment_claims WHERE txid = ?`, txid); err != nil {
		return err
	}
	return tx.Commit()
}

// ListPaymentClaims loads persisted payment claims for replay protection across restarts
// and deterministic book reconciliation (optional limit caps rows returned).
func (d *DB) ListPaymentClaims(limits ...int) ([]PaymentClaimRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var (
		rows *sql.Rows
		err  error
	)
	if len(limits) > 0 && limits[0] > 0 {
		rows, err = d.sql.Query(`SELECT txid, inputs_json, claimed_at FROM payment_claims ORDER BY claimed_at DESC LIMIT ?`, limits[0])
	} else {
		rows, err = d.sql.Query(`SELECT txid, inputs_json, claimed_at FROM payment_claims ORDER BY claimed_at ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentClaimRecord
	for rows.Next() {
		var r PaymentClaimRecord
		var raw []byte
		if err := rows.Scan(&r.TxID, &raw, &r.ClaimedAt); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &r.Inputs); err != nil {
				return nil, fmt.Errorf("store: corrupt payment claim inputs for %s: %w", r.TxID, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReserveFaucetDrip atomically verifies and reserves BOTH per-address 24h cap and global UTC-day cap
// inside ONE SQLite transaction BEFORE broadcasting a faucet drip.
func (d *DB) ReserveFaucetDrip(address string, dripSats, perAddressCap, globalCap uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	dayKey := "faucet_global_daily:" + time.Now().UTC().Format("2006-01-02")
	globalUsed, err := readKVUint64Tx(tx, dayKey)
	if err != nil {
		return err
	}
	if globalCap > 0 && (dripSats > globalCap || globalUsed > globalCap-dripSats) {
		return ErrFaucetCapExceeded
	}

	var prevSats uint64
	var lastAt int64
	qErr := tx.QueryRow(`SELECT total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).Scan(&prevSats, &lastAt)
	if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
		return fmt.Errorf("store: read faucet claim for %s: %w", address, qErr)
	}
	if qErr == nil {
		if now-lastAt >= 86400 {
			prevSats = 0
		}
		if perAddressCap > 0 && (dripSats > perAddressCap || prevSats > perAddressCap-dripSats) {
			return ErrFaucetCapExceeded
		}
	}

	nextGlobal := globalUsed + dripSats
	gb, err := json.Marshal(nextGlobal)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, gb); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO faucet_claims (address, total_sats, last_claim_at) VALUES (?, ?, ?)`, address, prevSats+dripSats, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RefundFaucetDrip rolls back a reserved faucet drip on definitive pre-broadcast failure.
func (d *DB) RefundFaucetDrip(address string, dripSats uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	dayKey := "faucet_global_daily:" + time.Now().UTC().Format("2006-01-02")
	globalUsed, err := readKVUint64Tx(tx, dayKey)
	if err != nil {
		return err
	}
	if globalUsed >= dripSats {
		globalUsed -= dripSats
	} else {
		globalUsed = 0
	}
	gb, err := json.Marshal(globalUsed)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, gb); err != nil {
		return err
	}

	var prevSats uint64
	var lastAt int64
	qErr := tx.QueryRow(`SELECT total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).Scan(&prevSats, &lastAt)
	if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
		return fmt.Errorf("store: read faucet claim for refund %s: %w", address, qErr)
	}
	if qErr == nil {
		if prevSats >= dripSats {
			prevSats -= dripSats
		} else {
			prevSats = 0
		}
		if _, err := tx.Exec(`UPDATE faucet_claims SET total_sats = ? WHERE address = ?`, prevSats, address); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReserveSpendAtomic atomically reserves (opID, attempt) against maxPerDeal and UTC-day spend caps.
// Idempotent on identical (opID, attempt): returns (alreadyReserved=true, nil) on replay.
func (d *DB) ReserveSpendAtomic(opID string, attempt int, amountSats, maxPerDeal, maxPerDay uint64) (bool, error) {
	if opID == "" {
		return false, errors.New("store: empty opID")
	}
	if maxPerDeal == 0 || amountSats > maxPerDeal {
		return false, ErrSpendCapExceeded
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingStatus string
	qErr := tx.QueryRow(`SELECT status FROM spend_reservations WHERE op_id = ? AND attempt = ?`, opID, attempt).Scan(&existingStatus)
	if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
		return false, fmt.Errorf("store: check spend reservation %s#%d: %w", opID, attempt, qErr)
	}
	if qErr == nil && existingStatus == "reserved" {
		return true, nil
	}

	dayKey := "market_daily_spend:" + time.Now().UTC().Format("2006-01-02")
	used, err := readKVUint64Tx(tx, dayKey)
	if err != nil {
		return false, err
	}
	if maxPerDay == 0 || amountSats > maxPerDay || used > maxPerDay-amountSats {
		return false, ErrSpendCapExceeded
	}
	next := used + amountSats
	nb, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, nb); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO spend_reservations (op_id, attempt, amount_sats, status, created_at)
		VALUES (?, ?, ?, 'reserved', ?)`, opID, attempt, amountSats, time.Now().Unix()); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// ReleaseSpendReservation refunds a previously reserved (opID, attempt) on pre-broadcast failure.
func (d *DB) ReleaseSpendReservation(opID string, attempt int) error {
	if opID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var amount uint64
	var status string
	var createdAt int64
	if qErr := tx.QueryRow(`SELECT amount_sats, status, created_at FROM spend_reservations WHERE op_id = ? AND attempt = ?`, opID, attempt).Scan(&amount, &status, &createdAt); qErr != nil {
		if errors.Is(qErr, sql.ErrNoRows) {
			return nil
		}
		return qErr
	}
	if status != "reserved" {
		return nil
	}
	if _, err := tx.Exec(`UPDATE spend_reservations SET status = 'released' WHERE op_id = ? AND attempt = ?`, opID, attempt); err != nil {
		return err
	}
	refTime := time.Unix(createdAt, 0).UTC()
	if createdAt == 0 {
		refTime = time.Now().UTC()
	}
	dayKey := "market_daily_spend:" + refTime.Format("2006-01-02")
	used, err := readKVUint64Tx(tx, dayKey)
	if err != nil {
		return err
	}
	if used >= amount {
		used -= amount
	} else {
		used = 0
	}
	nb, err := json.Marshal(used)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, nb); err != nil {
		return err
	}
	return tx.Commit()
}

// DailySpend returns a UTC-day accumulator stored in KV under prefix:YYYY-MM-DD.
func (d *DB) DailySpend(prefix string) (uint64, error) {
	key := prefix + ":" + time.Now().UTC().Format("2006-01-02")
	var v uint64
	ok, err := d.GetJSON(key, &v)
	if err != nil || !ok {
		return 0, err
	}
	return v, nil
}

// AddDailySpend atomically increments a UTC-day accumulator in KV inside a single SQLite transaction.
func (d *DB) AddDailySpend(prefix string, delta uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	key := prefix + ":" + time.Now().UTC().Format("2006-01-02")
	cur, err := readKVUint64Tx(tx, key)
	if err != nil {
		return 0, err
	}
	next := cur + delta
	nb, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, key, nb); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

// LoadOrCreateStableKey loads or generates a persistent 32-byte hex private key in KV
// so market offers and directory announcements keep a stable pubkey across restarts.
// Serialized under d.mu inside one SQLite transaction so concurrent boots never generate
// conflicting keys, and fails closed on read/corrupt errors without overwriting existing keys.
func (d *DB) LoadOrCreateStableKey(keyName string, gen func() ([]byte, error)) ([]byte, error) {
	if keyName == "" || gen == nil {
		return nil, errors.New("store: keyName and generator are required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin stable key %s: %w", keyName, err)
	}
	defer func() { _ = tx.Rollback() }()

	var rawJSON []byte
	qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, keyName).Scan(&rawJSON)
	if qErr == nil {
		var hexKey string
		if uErr := json.Unmarshal(rawJSON, &hexKey); uErr != nil {
			return nil, fmt.Errorf("store: decode stable key %s: %w", keyName, uErr)
		}
		if len(hexKey) != 64 {
			return nil, fmt.Errorf("store: corrupt stable key %s (len=%d)", keyName, len(hexKey))
		}
		raw, decErr := hex.DecodeString(hexKey)
		if decErr != nil || len(raw) != 32 {
			return nil, fmt.Errorf("store: corrupt stable key hex %s: %w", keyName, decErr)
		}
		return raw, nil
	}
	if !errors.Is(qErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: read stable key %s: %w", keyName, qErr)
	}

	raw, err := gen()
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("store: stable key %s must be 32 bytes (got %d)", keyName, len(raw))
	}
	encoded, err := json.Marshal(hex.EncodeToString(raw))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)`, keyName, encoded); err != nil {
		return nil, fmt.Errorf("store: persist stable key %s: %w", keyName, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit stable key %s: %w", keyName, err)
	}
	cp := make([]byte, 32)
	copy(cp, raw)
	return cp, nil
}
