package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/script/interpreter/scriptflag"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// HandleReorg rolls back orphaned proofs in go-wallet-toolbox storage and evicts reorged
// MerklePaths from the local LRU cache so stale proofs are never served after a chain fork.
func (e *Engine) HandleReorg(ctx context.Context, forkHeight uint32, orphanedHashes []string) error {
	e.txCache.EvictFromHeight(forkHeight)
	var firstErr error
	if e.provider != nil && len(orphanedHashes) > 0 {
		if err := e.provider.HandleReorg(ctx, orphanedHashes); err != nil {
			firstErr = err
		}
	}
	// Clear any remaining cached proof metadata at or above forkHeight in user_utxos / transactions / known_txes
	// atomically in a single SQLite transaction in case a header fork wasn't matched by block_hash alone.
	if e.sqlDB != nil && forkHeight > 0 {
		dbTx, err := e.sqlDB.BeginTx(ctx, nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return firstErr
		}
		defer func() { _ = dbTx.Rollback() }()

		if e.tables.userUTXOs != "" {
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET utxo_status = 'unproven' WHERE user_id = ? AND utxo_status = 'mined' AND output_id IN (SELECT o.id FROM "%s" o INNER JOIN "%s" t ON t.id = o.transaction_id INNER JOIN "%s" k ON k.tx_id = t.tx_id WHERE o.user_id = ? AND k.block_height >= ? AND k.block_height > 0)`,
					e.tables.userUTXOs, e.tables.outputs, e.tables.transactions, e.tables.knownTxs),
				e.userID, e.userID, forkHeight,
			); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("wallet: reorg reset user_utxos: %w", err)
				}
				return firstErr
			}
		}
		if _, err := dbTx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET status = 'unproven', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND status = 'completed' AND tx_id IN (SELECT tx_id FROM "%s" WHERE block_height >= ? AND block_height > 0)`, e.tables.transactions, e.tables.knownTxs),
			e.userID, forkHeight,
		); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("wallet: reorg reset transactions: %w", err)
			}
			return firstErr
		}
		if _, err := dbTx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET status = 'unmined', block_height = NULL, block_hash = NULL, merkle_root = NULL, merkle_path = NULL WHERE block_height >= ? AND block_height > 0`, e.tables.knownTxs),
			forkHeight,
		); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("wallet: reorg reset known_txes: %w", err)
			}
			return firstErr
		}
		if err := dbTx.Commit(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("wallet: reorg commit: %w", err)
			}
		}
	}
	return firstErr
}

// promoteTxStatusAtomic atomically transitions a transaction row (from submission_unknown, nosend,
// failed, or aborted) to completed or unproven, restoring outputs.spendable=1, upserting user_utxos,
// and synchronizing known_txes (including block_hash & merkle_root when mined) so UTXOs are never stranded.
func (e *Engine) promoteTxStatusAtomic(
	ctx context.Context,
	dbTxRowID int64,
	txid string,
	newStatus string,
	txObj *transaction.Transaction,
	mp *transaction.MerklePath,
	height uint32,
	blockHash string,
	merkleRoot string,
) error {
	if e.sqlDB == nil || txid == "" {
		return errors.New("wallet: sqlDB unavailable")
	}
	if dbTxRowID == 0 {
		_ = e.readDB.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT id FROM "%s" WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL LIMIT 1`, e.tables.transactions),
			e.userID, txid,
		).Scan(&dbTxRowID)
	}
	if dbTxRowID == 0 {
		return fmt.Errorf("wallet: transaction row not found for %s", txid)
	}

	dbTx, err := e.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = dbTx.Rollback() }()

	if _, err := dbTx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE "%s" SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, e.tables.transactions),
		newStatus, dbTxRowID, e.userID,
	); err != nil {
		return err
	}
	if txObj != nil {
		for _, in := range txObj.Inputs {
			if in == nil || in.SourceTXID == nil {
				continue
			}
			srcTx := in.SourceTXID.String()
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET spendable = 0, spent_by = ? WHERE user_id = ? AND vout = ? AND deleted_at IS NULL AND transaction_id IN (SELECT id FROM "%s" WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL)`,
					e.tables.outputs, e.tables.transactions),
				dbTxRowID, e.userID, in.SourceTxOutIndex, e.userID, srcTx,
			); err != nil {
				return fmt.Errorf("wallet: mark spent output %s:%d: %w", srcTx, in.SourceTxOutIndex, err)
			}
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM "%s" WHERE user_id = ? AND output_id IN (SELECT o.id FROM "%s" o INNER JOIN "%s" t ON t.id = o.transaction_id WHERE o.user_id = ? AND t.tx_id = ? AND o.vout = ? AND t.deleted_at IS NULL)`,
					e.tables.userUTXOs, e.tables.outputs, e.tables.transactions),
				e.userID, e.userID, srcTx, in.SourceTxOutIndex,
			); err != nil {
				return fmt.Errorf("wallet: delete spent user_utxo %s:%d: %w", srcTx, in.SourceTxOutIndex, err)
			}
		}
	}
	if _, err := dbTx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE "%s" SET spendable = 1 WHERE transaction_id = ? AND user_id = ? AND spent_by IS NULL AND deleted_at IS NULL AND basket_name IN ('default','reserve','fuel','nektar funding')`, e.tables.outputs),
		dbTxRowID, e.userID,
	); err != nil {
		return err
	}
	utxoStatus := "unproven"
	if newStatus == StatusCompleted {
		utxoStatus = "mined"
	}
	if _, err := dbTx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO "%s" (user_id, output_id, basket_name, satoshis, estimated_input_size, created_at, utxo_status)
			SELECT user_id, id, basket_name, satoshis, 148, CURRENT_TIMESTAMP, ?
			FROM "%s"
			WHERE transaction_id = ? AND user_id = ? AND spendable = 1 AND spent_by IS NULL AND deleted_at IS NULL AND basket_name IN ('default','reserve','fuel','nektar funding')
			ON CONFLICT(output_id) DO UPDATE SET utxo_status = excluded.utxo_status`,
			e.tables.userUTXOs, e.tables.outputs),
		utxoStatus, dbTxRowID, e.userID,
	); err != nil {
		// Fallback if unique index isn't on output_id alone.
		if _, fErr := dbTx.ExecContext(ctx,
			fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (user_id, output_id, basket_name, satoshis, estimated_input_size, created_at, utxo_status)
				SELECT user_id, id, basket_name, satoshis, 148, CURRENT_TIMESTAMP, ?
				FROM "%s"
				WHERE transaction_id = ? AND user_id = ? AND spendable = 1 AND spent_by IS NULL AND deleted_at IS NULL AND basket_name IN ('default','reserve','fuel','nektar funding')`,
				e.tables.userUTXOs, e.tables.outputs),
			utxoStatus, dbTxRowID, e.userID,
		); fErr != nil {
			return fmt.Errorf("wallet: promote fallback insert user_utxos: %w", fErr)
		}
		if _, uErr := dbTx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET utxo_status = ? WHERE user_id = ? AND output_id IN (SELECT id FROM "%s" WHERE transaction_id = ? AND user_id = ?)`,
				e.tables.userUTXOs, e.tables.outputs),
			utxoStatus, e.userID, dbTxRowID, e.userID,
		); uErr != nil {
			return fmt.Errorf("wallet: promote fallback update user_utxos: %w", uErr)
		}
	}

	var rawTx []byte
	if txObj != nil {
		rawTx = txObj.Bytes()
	}
	if newStatus == StatusCompleted && mp != nil {
		mpBytes := mp.Bytes()
		if len(mpBytes) == 0 {
			return errors.New("wallet: empty merkle path bytes on mined promotion")
		}
		if len(rawTx) > 0 {
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET status = 'completed', block_height = ?, block_hash = ?, merkle_root = ?, merkle_path = ?, raw_tx = CASE WHEN raw_tx IS NULL OR length(raw_tx) = 0 THEN ? ELSE raw_tx END WHERE tx_id = ?`, e.tables.knownTxs),
				height, blockHash, merkleRoot, mpBytes, rawTx, txid,
			); err != nil {
				return err
			}
		} else {
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET status = 'completed', block_height = ?, block_hash = ?, merkle_root = ?, merkle_path = ? WHERE tx_id = ?`, e.tables.knownTxs),
				height, blockHash, merkleRoot, mpBytes, txid,
			); err != nil {
				return err
			}
		}
	} else {
		if len(rawTx) > 0 {
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET status = 'unmined', raw_tx = CASE WHEN raw_tx IS NULL OR length(raw_tx) = 0 THEN ? ELSE raw_tx END WHERE tx_id = ? AND status != 'completed'`, e.tables.knownTxs),
				rawTx, txid,
			); err != nil {
				return err
			}
		} else {
			if _, err := dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET status = 'unmined' WHERE tx_id = ? AND status != 'completed'`, e.tables.knownTxs),
				txid,
			); err != nil {
				return err
			}
		}
	}
	return dbTx.Commit()
}

// verifyOutpointSpent cryptographically verifies that (txid, vout) is spent externally by fetching
// the candidate spending transaction's raw bytes, confirming sha256d == spenderTxID, confirming an
// input spends (txid, vout), and verifying either a header-verified BUMP on spenderTx or valid
// Bitcoin Script execution against lockScript. Never trusts bare indexer JSON or absence.
func (e *Engine) verifyOutpointSpent(ctx context.Context, txid string, vout uint32, satoshis uint64, lockScript []byte) (bool, error) {
	if e.cfg.Chain == nil || txid == "" {
		return false, nil
	}
	spenderTxID, err := e.cfg.Chain.Spender(ctx, txid, vout)
	if err != nil || spenderTxID == "" {
		return false, err
	}
	raw, err := e.cfg.Chain.TxRaw(ctx, spenderTxID)
	if err != nil || len(raw) == 0 {
		return false, err
	}
	spendTx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil || spendTx.TxID().String() != spenderTxID {
		return false, nil
	}
	matchedVin := -1
	for idx, in := range spendTx.Inputs {
		if in != nil && in.SourceTXID != nil && in.SourceTXID.String() == txid && in.SourceTxOutIndex == vout {
			matchedVin = idx
			break
		}
	}
	if matchedVin < 0 {
		return false, nil
	}
	// If the spending transaction is mined with a header-verified BUMP, the spend is consensus-proven.
	if e.svc != nil && e.cfg.Tracker != nil {
		if mp, _, ok, pErr := e.svc.lookupProof(ctx, spenderTxID); pErr == nil && ok && mp != nil {
			return true, nil
		}
	}
	// Otherwise verify the unlocking script cryptographically against the locking script!
	if len(lockScript) == 0 {
		lockScript = []byte(*e.fundScript)
	}
	ls := script.Script(lockScript)
	prevOut := &transaction.TransactionOutput{
		Satoshis:      satoshis,
		LockingScript: &ls,
	}
	spendTx.Inputs[matchedVin].SetSourceTxOutput(prevOut)
	flags := scriptflag.EnableSighashForkID | scriptflag.UTXOAfterGenesis | scriptflag.Genesis | scriptflag.UTXOAfterChronicle | scriptflag.Chronicle
	if err := interpreter.NewEngine().Execute(
		interpreter.WithTx(spendTx, matchedVin, prevOut),
		interpreter.WithFlags(flags),
		interpreter.WithAfterChronicle(),
		interpreter.WithContext(ctx),
	); err != nil {
		return false, nil
	}
	return true, nil
}
