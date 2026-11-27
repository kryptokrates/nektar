package wallet

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
	"github.com/lightwebinc/bcommon/guard"
)

func isSafeSQLiteIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// discoverAndIndexToolboxTables inspects sqlite_master once after Migrate to locate the exact
// GORM table names (honoring defs.DefaultTablePrefix) and adds covering partial indexes for
// O(unspent) Balance, ListUTXOs, and ListRecentActions queries without altering toolbox schema.
func discoverAndIndexToolboxTables(db *sql.DB) toolboxTables {
	t := toolboxTables{
		outputs:      defs.DefaultTablePrefix + "outputs",
		transactions: defs.DefaultTablePrefix + "transactions",
		userUTXOs:    defs.DefaultTablePrefix + "user_utxos",
		knownTxs:     defs.DefaultTablePrefix + "known_txes",
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) != nil || !isSafeSQLiteIdent(name) {
				continue
			}
			switch {
			case name == t.outputs || strings.HasSuffix(name, "_outputs") || name == "outputs":
				t.outputs = name
			case name == t.transactions || strings.HasSuffix(name, "_transactions") || name == "transactions":
				t.transactions = name
			case name == t.userUTXOs || strings.HasSuffix(name, "_user_utxos") || name == "user_utxos":
				t.userUTXOs = name
			case name == t.knownTxs || strings.HasSuffix(name, "_known_txes") || strings.HasSuffix(name, "_known_txs") || name == "known_txes" || name == "known_txs":
				t.knownTxs = name
			}
		}
	}
	// Covering partial indexes for O(unspent) reads and O(1) per-tx fee aggregation.
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_outputs_unspent ON "%s"(user_id, spendable, basket_name) WHERE deleted_at IS NULL AND spent_by IS NULL`, t.outputs))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_outputs_tx_fee ON "%s"(transaction_id, satoshis) WHERE deleted_at IS NULL`, t.outputs))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_outputs_spent_fee ON "%s"(spent_by, satoshis) WHERE deleted_at IS NULL AND spent_by IS NOT NULL`, t.outputs))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_txs_user_id_desc ON "%s"(user_id, id DESC) WHERE deleted_at IS NULL`, t.transactions))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_txs_user_status ON "%s"(user_id, status) WHERE deleted_at IS NULL`, t.transactions))
	// Durable pre-broadcast journal for BRC-29 desktop fund/sweep transfers (prevents orphaned funds on crash).
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS nektar_pending_brc29 (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			direction          TEXT NOT NULL,
			wire_url           TEXT NOT NULL DEFAULT '',
			counterparty_pub   TEXT NOT NULL,
			derivation_prefix  TEXT NOT NULL,
			derivation_suffix  TEXT NOT NULL,
			locking_script_hex TEXT NOT NULL DEFAULT '',
			satoshis           INTEGER NOT NULL DEFAULT 0,
			tx_id              TEXT NOT NULL DEFAULT '',
			output_index       INTEGER NOT NULL DEFAULT 0,
			atomic_beef        BLOB,
			status             TEXT NOT NULL DEFAULT 'pending',
			created_at         INTEGER NOT NULL,
			updated_at         INTEGER NOT NULL
		)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_nektar_pending_brc29_status ON nektar_pending_brc29(status, id)`)
	return t
}

func (e *Engine) isIncomingTx(txid string) bool {
	if e.readDB == nil || txid == "" {
		return false
	}
	var isOut int
	q := fmt.Sprintf(`SELECT is_outgoing FROM "%s" WHERE tx_id = ? AND user_id = ? AND deleted_at IS NULL LIMIT 1`, e.tables.transactions)
	if err := e.readDB.QueryRow(q, txid, e.userID).Scan(&isOut); err != nil {
		return false
	}
	return isOut == 0
}

func (e *Engine) loadBeefFromStorage(ctx context.Context, txid string) (*transaction.Beef, error) {
	if e.provider == nil || txid == "" {
		return nil, fmt.Errorf("wallet: storage unavailable")
	}
	return e.provider.GetBeefForTransaction(ctx, txid, wdk.StorageGetBeefOptions{})
}

func (e *Engine) loadTxFromKnownTxs(ctx context.Context, txid string) *transaction.Transaction {
	if e.provider == nil || txid == "" {
		return nil
	}
	ktxs, err := e.provider.KnownTxEntity().Read().TxID(txid).Find(ctx)
	if err != nil || len(ktxs) == 0 {
		return nil
	}
	ktx := ktxs[0]
	if len(ktx.RawTx) == 0 {
		return nil
	}
	tx, pErr := transaction.NewTransactionFromBytes(ktx.RawTx)
	if pErr != nil {
		return nil
	}
	if len(ktx.MerklePath) > 0 {
		if mp, mpErr := transaction.NewMerklePathFromBinary(ktx.MerklePath); mpErr == nil {
			tx.MerklePath = mp
		}
	}
	if tx.MerklePath == nil && len(ktx.InputBEEF) > 0 {
		if inBeef, _, _, bErr := guard.ParseBEEF(ktx.InputBEEF, guard.DefaultBound); bErr == nil && inBeef != nil {
			hydrateInputs(tx, inBeef, e.txCache, nil, 0, e.cfg.MaxUnprovenDepth)
		}
	}
	return tx
}

func (e *Engine) ownedOutputSumForTx(txid string) (uint64, bool) {
	if e.readDB == nil || txid == "" {
		return 0, false
	}
	q := fmt.Sprintf(`
		SELECT t.id, COALESCE((
			SELECT SUM(o.satoshis)
			FROM "%s" o
			WHERE o.transaction_id = t.id
			  AND o.deleted_at IS NULL
			  AND o.basket_name IN ('default','reserve','fuel','nektar funding')
		), 0)
		FROM "%s" t
		WHERE t.tx_id = ?
		  AND t.user_id = ?
		  AND t.deleted_at IS NULL
		LIMIT 1`,
		e.tables.outputs, e.tables.transactions,
	)
	var tid, sum int64
	if err := e.readDB.QueryRow(q, txid, e.userID).Scan(&tid, &sum); err != nil {
		return 0, false
	}
	if sum < 0 {
		sum = 0
	}
	return uint64(sum), true
}

func (e *Engine) countDustUTXOs(ctx context.Context, belowSats uint64) int {
	if e.readDB == nil || belowSats == 0 {
		return 0
	}
	q := fmt.Sprintf(`
		SELECT COUNT(1)
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
		WHERE o.user_id = ?
		  AND o.deleted_at IS NULL
		  AND o.spent_by IS NULL
		  AND o.spendable = 1
		  AND u.reserved_by_id IS NULL
		  AND o.satoshis > 0
		  AND o.satoshis <= ?
		  AND o.basket_name IN ('default','fuel')
		  AND owner.status NOT IN (`+sqlNonSpendableParentStatuses+`)`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs,
	)
	var cnt int
	_ = e.readDB.QueryRowContext(ctx, q, e.userID, belowSats).Scan(&cnt)
	return cnt
}

// SavePendingBRC29 persists BRC-29 derivation nonces BEFORE broadcasting a desktop fund/sweep tx.
func (e *Engine) SavePendingBRC29(ctx context.Context, rec PendingBRC29Record) (int64, error) {
	if e.sqlDB == nil {
		return 0, nil
	}
	now := time.Now().Unix()
	if rec.Status == "" {
		rec.Status = "pending"
	}
	res, err := e.sqlDB.ExecContext(ctx, `
		INSERT INTO nektar_pending_brc29
		(direction, wire_url, counterparty_pub, derivation_prefix, derivation_suffix, locking_script_hex, satoshis, tx_id, output_index, atomic_beef, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Direction, rec.WireURL, rec.CounterpartyPub, rec.DerivationPrefix, rec.DerivationSuffix,
		rec.LockingScriptHex, rec.Satoshis, rec.TxID, rec.OutputIndex, rec.AtomicBEEF, rec.Status, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePendingBRC29Tx attaches the broadcast txid, outputIndex, and AtomicBEEF to a pending BRC-29 row.
func (e *Engine) UpdatePendingBRC29Tx(ctx context.Context, id int64, txid string, outIdx uint32, beef []byte, sats uint64) error {
	if e.sqlDB == nil || id <= 0 {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `
		UPDATE nektar_pending_brc29
		SET tx_id = ?, output_index = ?, atomic_beef = ?, satoshis = CASE WHEN ? > 0 THEN ? ELSE satoshis END, updated_at = ?
		WHERE id = ?`,
		txid, outIdx, beef, sats, sats, time.Now().Unix(), id)
	return err
}

// MarkPendingBRC29Done marks a pending BRC-29 transfer internalized and complete.
func (e *Engine) MarkPendingBRC29Done(ctx context.Context, id int64) error {
	if e.sqlDB == nil || id <= 0 {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `UPDATE nektar_pending_brc29 SET status = 'done', updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// DeletePendingBRC29 deletes a pending BRC-29 journal row after a definite pre-broadcast failure
// (e.g. user rejected BSV Desktop prompt or SpendGuard pre-flight refusal) so stale rows never
// stall Monitor's reconciliation pass.
func (e *Engine) DeletePendingBRC29(ctx context.Context, id int64) error {
	if e.sqlDB == nil || id <= 0 {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `DELETE FROM nektar_pending_brc29 WHERE id = ?`, id)
	return err
}

// MarkPendingBRC29DoneByTx marks a pending BRC-29 transfer complete by its broadcast txid and vout.
func (e *Engine) MarkPendingBRC29DoneByTx(ctx context.Context, txid string, outIdx uint32) error {
	if e.sqlDB == nil || txid == "" {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `UPDATE nektar_pending_brc29 SET status = 'done', updated_at = ? WHERE tx_id = ? AND output_index = ?`, time.Now().Unix(), txid, outIdx)
	return err
}

// ListPendingBRC29 returns all incomplete pending BRC-29 transfers ordered oldest first.
func (e *Engine) ListPendingBRC29(ctx context.Context) ([]PendingBRC29Record, error) {
	if e.readDB == nil {
		return nil, nil
	}
	rows, err := e.readDB.QueryContext(ctx, `
		SELECT id, direction, wire_url, counterparty_pub, derivation_prefix, derivation_suffix, locking_script_hex, satoshis, tx_id, output_index, atomic_beef, status, created_at, updated_at
		FROM nektar_pending_brc29 WHERE status = 'pending' ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingBRC29Record
	for rows.Next() {
		var r PendingBRC29Record
		if err := rows.Scan(&r.ID, &r.Direction, &r.WireURL, &r.CounterpartyPub, &r.DerivationPrefix, &r.DerivationSuffix,
			&r.LockingScriptHex, &r.Satoshis, &r.TxID, &r.OutputIndex, &r.AtomicBEEF, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TxStatus returns the persisted go-wallet-toolbox transactions.status for txid.
func (e *Engine) TxStatus(ctx context.Context, txid string) (string, bool) {
	if e.readDB == nil || txid == "" {
		return "", false
	}
	var st string
	q := fmt.Sprintf(`SELECT COALESCE(status, '') FROM "%s" WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL LIMIT 1`, e.tables.transactions)
	if err := e.readDB.QueryRowContext(ctx, q, e.userID, txid).Scan(&st); err != nil || st == "" {
		return "", false
	}
	return st, true
}

// HasOutput reports whether outpoint (txid, vout) is already persisted in outputs for this user.
func (e *Engine) HasOutput(ctx context.Context, txid string, vout uint32) bool {
	if e.readDB == nil || txid == "" {
		return false
	}
	var one int
	q := fmt.Sprintf(`SELECT 1 FROM "%s" o INNER JOIN "%s" t ON t.id = o.transaction_id WHERE o.user_id = ? AND t.tx_id = ? AND o.vout = ? AND o.deleted_at IS NULL AND t.deleted_at IS NULL LIMIT 1`, e.tables.outputs, e.tables.transactions)
	return e.readDB.QueryRowContext(ctx, q, e.userID, txid, vout).Scan(&one) == nil
}

// findTxByLockingScript scans local outputs for a transaction containing lockScriptHex (used by ReconcilePendingBRC29
// when a crash occurred after broadcast but before UpdatePendingBRC29Tx persisted tx_id/atomic_beef).
func (e *Engine) findTxByLockingScript(ctx context.Context, lockScriptHex string) (string, uint32, []byte) {
	if e.readDB == nil || lockScriptHex == "" {
		return "", 0, nil
	}
	wantBytes, err := hex.DecodeString(strings.TrimSpace(lockScriptHex))
	if err != nil || len(wantBytes) == 0 {
		return "", 0, nil
	}
	q := fmt.Sprintf(`SELECT COALESCE(t.tx_id, ''), o.vout FROM "%s" o INNER JOIN "%s" t ON t.id = o.transaction_id WHERE o.user_id = ? AND o.deleted_at IS NULL AND t.deleted_at IS NULL AND t.tx_id IS NOT NULL AND t.tx_id != '' AND o.locking_script = ? ORDER BY o.id DESC LIMIT 1`, e.tables.outputs, e.tables.transactions)
	var txid string
	var vout uint32
	if err := e.readDB.QueryRowContext(ctx, q, e.userID, wantBytes).Scan(&txid, &vout); err != nil || txid == "" {
		return "", 0, nil
	}
	tx, tErr := e.LoadParsedTx(ctx, txid)
	if tErr != nil || tx == nil {
		return txid, vout, nil
	}
	beef, _ := SafeAtomicBEEF(tx)
	return txid, vout, beef
}

// LoadParsedTx resolves a transaction by txid from LRU cache or go-wallet-toolbox KnownTx storage,
// hydrating unmined parent inputs from InputBEEF and SQLite known_txes before caching.
func (e *Engine) LoadParsedTx(ctx context.Context, txid string) (*transaction.Transaction, error) {
	if tx, ok := e.txCache.Get(txid); ok && tx != nil {
		return tx, nil
	}
	loader := e.svc.dbLoader(ctx)
	if beef, err := e.provider.GetBeefForTransaction(ctx, txid, wdk.StorageGetBeefOptions{IgnoreServices: true}); err == nil && beef != nil {
		if tx := beef.FindTransaction(txid); tx != nil {
			hydrateInputs(tx, beef, e.txCache, loader, 0, e.cfg.MaxUnprovenDepth)
			e.txCache.Put(txid, tx)
			return tx, nil
		}
	}
	ktxs, err := e.provider.KnownTxEntity().Read().TxID(txid).Find(ctx)
	if err == nil && len(ktxs) > 0 {
		ktx := ktxs[0]
		if len(ktx.RawTx) > 0 {
			if tx, pErr := transaction.NewTransactionFromBytes(ktx.RawTx); pErr == nil {
				if len(ktx.MerklePath) > 0 {
					if mp, mpErr := transaction.NewMerklePathFromBinary(ktx.MerklePath); mpErr == nil {
						tx.MerklePath = mp
					}
				}
				if tx.MerklePath == nil {
					var inBeef *transaction.Beef
					if len(ktx.InputBEEF) > 0 {
						inBeef, _, _, _ = guard.ParseBEEF(ktx.InputBEEF, guard.DefaultBound)
					}
					hydrateInputs(tx, inBeef, e.txCache, loader, 0, e.cfg.MaxUnprovenDepth)
				}
				e.txCache.Put(txid, tx)
				return tx, nil
			}
		}
	}
	return nil, fmt.Errorf("wallet: tx %s not found", txid)
}

func (e *Engine) CacheTx(tx *transaction.Transaction) {
	if tx != nil {
		e.txCache.Put(tx.TxID().String(), tx)
	}
}

// TxFeeSats computes the exact fee paid by a wallet transaction from its hydrated inputs/outputs.
func (e *Engine) TxFeeSats(ctx context.Context, txid string) uint64 {
	tx, err := e.LoadParsedTx(ctx, txid)
	if err != nil || tx == nil {
		return 0
	}
	return ComputeTxFee(tx)
}

// ComputeTxFee computes the exact fee paid by a hydrated transaction (inTotal - outTotal, or 0 if incomplete).
func ComputeTxFee(tx *transaction.Transaction) uint64 {
	if tx == nil {
		return 0
	}
	var inTotal, outTotal uint64
	for _, in := range tx.Inputs {
		if in.SourceTransaction == nil || int(in.SourceTxOutIndex) >= len(in.SourceTransaction.Outputs) {
			return 0
		}
		inTotal += in.SourceTransaction.Outputs[in.SourceTxOutIndex].Satoshis
	}
	for _, o := range tx.Outputs {
		outTotal += o.Satoshis
	}
	if inTotal > outTotal {
		return inTotal - outTotal
	}
	return 0
}

func (e *Engine) estimateFeeSats(ctx context.Context, inputs, outputs int) uint64 {
	rate := e.FeeRateSatPerKB(ctx)
	bytesEst := uint64(10 + inputs*148 + outputs*34)
	fee := (bytesEst*rate + 999) / 1000
	if fee < 1 {
		fee = 1
	}
	return fee
}
