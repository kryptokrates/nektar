package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

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
	b, _ := json.Marshal(inputs)
	if _, err := tx.Exec(`INSERT INTO payment_claims (txid, inputs_json, claimed_at) VALUES (?, ?, ?)`, txid, b, now); err != nil {
		return ErrPaymentClaimReplay
	}
	for _, op := range inputs {
		if op == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO payment_claim_inputs (outpoint, txid, claimed_at) VALUES (?, ?, ?)`, op, txid, now); err != nil {
			return ErrPaymentClaimConflict
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
	_, _ = tx.Exec(`DELETE FROM payment_claim_inputs WHERE txid = ?`, txid)
	_, err = tx.Exec(`DELETE FROM payment_claims WHERE txid = ?`, txid)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SavePaymentClaim persists a claimed payment txid and its spent inputs (legacy helper; prefer ClaimPaymentAtomic).
func (d *DB) SavePaymentClaim(txid string, inputs []string) error {
	err := d.ClaimPaymentAtomic(txid, inputs)
	if errors.Is(err, ErrPaymentClaimReplay) {
		return nil
	}
	return err
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
		_ = json.Unmarshal(raw, &r.Inputs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetFaucetClaim loads the last faucet claim for an address.
func (d *DB) GetFaucetClaim(address string) (FaucetClaim, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c FaucetClaim
	err := d.sql.QueryRow(`SELECT address, total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).
		Scan(&c.Address, &c.TotalSats, &c.LastClaimAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FaucetClaim{Address: address}, nil
	}
	return c, err
}

// RecordFaucetClaim records a completed testnet faucet drip to address,
// resetting the per-address 24h rolling accumulator once 86400s have elapsed.
func (d *DB) RecordFaucetClaim(address string, sats uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.sql.Exec(`
		INSERT INTO faucet_claims (address, total_sats, last_claim_at) VALUES (?, ?, ?)
		ON CONFLICT(address) DO UPDATE SET
			total_sats = CASE WHEN excluded.last_claim_at - faucet_claims.last_claim_at >= 86400
				THEN excluded.total_sats
				ELSE faucet_claims.total_sats + excluded.total_sats END,
			last_claim_at = excluded.last_claim_at`,
		address, sats, now)
	return err
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
	var globalUsed uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &globalUsed)
	}
	if globalCap > 0 && globalUsed+dripSats > globalCap {
		return ErrFaucetCapExceeded
	}

	var prevSats uint64
	var lastAt int64
	if qErr := tx.QueryRow(`SELECT total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).Scan(&prevSats, &lastAt); qErr == nil {
		if now-lastAt >= 86400 {
			prevSats = 0
		}
		if perAddressCap > 0 && prevSats+dripSats > perAddressCap {
			return ErrFaucetCapExceeded
		}
	}

	nextGlobal := globalUsed + dripSats
	gb, _ := json.Marshal(nextGlobal)
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
	var globalUsed uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &globalUsed)
	}
	if globalUsed >= dripSats {
		globalUsed -= dripSats
	} else {
		globalUsed = 0
	}
	gb, _ := json.Marshal(globalUsed)
	_, _ = tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, dayKey, gb)

	var prevSats uint64
	var lastAt int64
	if qErr := tx.QueryRow(`SELECT total_sats, last_claim_at FROM faucet_claims WHERE address = ?`, address).Scan(&prevSats, &lastAt); qErr == nil {
		if prevSats >= dripSats {
			prevSats -= dripSats
		} else {
			prevSats = 0
		}
		_, _ = tx.Exec(`UPDATE faucet_claims SET total_sats = ? WHERE address = ?`, prevSats, address)
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
	if qErr := tx.QueryRow(`SELECT status FROM spend_reservations WHERE op_id = ? AND attempt = ?`, opID, attempt).Scan(&existingStatus); qErr == nil {
		if existingStatus == "reserved" {
			return true, nil
		}
	}

	dayKey := "market_daily_spend:" + time.Now().UTC().Format("2006-01-02")
	var used uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &used)
	}
	if maxPerDay == 0 || used+amountSats > maxPerDay {
		return false, ErrSpendCapExceeded
	}
	next := used + amountSats
	nb, _ := json.Marshal(next)
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
	var used uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, dayKey).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &used)
	}
	if used >= amount {
		used -= amount
	} else {
		used = 0
	}
	nb, _ := json.Marshal(used)
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
	var cur uint64
	var raw []byte
	if qErr := tx.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&raw); qErr == nil {
		_ = json.Unmarshal(raw, &cur)
	}
	next := cur + delta
	nb, _ := json.Marshal(next)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)`, key, nb); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

// LoadOrCreateStableKey loads or generates a persistent 32-byte hex private key in KV
// so market offers and directory announcements keep a stable pubkey across restarts.
func (d *DB) LoadOrCreateStableKey(keyName string, gen func() ([]byte, error)) ([]byte, error) {
	var hexKey string
	ok, err := d.GetJSON(keyName, &hexKey)
	if err == nil && ok && len(hexKey) == 64 {
		var raw [32]byte
		if n, decErr := decodeHex32(hexKey, raw[:]); decErr == nil && n == 32 {
			return raw[:], nil
		}
	}
	created, err := gen()
	if err != nil {
		return nil, err
	}
	_ = d.SetJSON(keyName, encodeHex(created))
	return created, nil
}

func decodeHex32(s string, dst []byte) (int, error) {
	if len(s) != 64 || len(dst) < 32 {
		return 0, errors.New("invalid 32-byte hex")
	}
	for i := 0; i < 32; i++ {
		hi := fromHexChar(s[2*i])
		lo := fromHexChar(s[2*i+1])
		if hi > 15 || lo > 15 {
			return 0, errors.New("invalid hex char")
		}
		dst[i] = (hi << 4) | lo
	}
	return 32, nil
}

func fromHexChar(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 255
	}
}

func encodeHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = digits[v>>4]
		out[2*i+1] = digits[v&0x0f]
	}
	return string(out)
}
