package wallet

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/script/interpreter/scriptflag"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
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
	// Clear any remaining cached proof metadata at or above forkHeight in known_txes / transactions
	// in case a header fork wasn't matched by block_hash alone.
	if e.sqlDB != nil && forkHeight > 0 {
		_, _ = e.sqlDB.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET status = 'unproven', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND status = 'completed' AND tx_id IN (SELECT tx_id FROM "%s" WHERE block_height >= ?)`, e.tables.transactions, e.tables.knownTxs),
			e.userID, forkHeight,
		)
		_, _ = e.sqlDB.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET status = 'unmined', block_height = NULL, block_hash = NULL, merkle_root = NULL, merkle_path = NULL WHERE block_height >= ?`, e.tables.knownTxs),
			forkHeight,
		)
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
			_, _ = dbTx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE "%s" SET spendable = 0, spent_by = ? WHERE user_id = ? AND tx_id = ? AND vout = ? AND deleted_at IS NULL`, e.tables.outputs),
				dbTxRowID, e.userID, srcTx, in.SourceTxOutIndex,
			)
			_, _ = dbTx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM "%s" WHERE user_id = ? AND output_id IN (SELECT id FROM "%s" WHERE user_id = ? AND tx_id = ? AND vout = ?)`, e.tables.userUTXOs, e.tables.outputs),
				e.userID, e.userID, srcTx, in.SourceTxOutIndex,
			)
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
		_, _ = dbTx.ExecContext(ctx,
			fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (user_id, output_id, basket_name, satoshis, estimated_input_size, created_at, utxo_status)
				SELECT user_id, id, basket_name, satoshis, 148, CURRENT_TIMESTAMP, ?
				FROM "%s"
				WHERE transaction_id = ? AND user_id = ? AND spendable = 1 AND spent_by IS NULL AND deleted_at IS NULL AND basket_name IN ('default','reserve','fuel','nektar funding')`,
				e.tables.userUTXOs, e.tables.outputs),
			utxoStatus, dbTxRowID, e.userID,
		)
		_, _ = dbTx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET utxo_status = ? WHERE user_id = ? AND output_id IN (SELECT id FROM "%s" WHERE transaction_id = ? AND user_id = ?)`,
				e.tables.userUTXOs, e.tables.outputs),
			utxoStatus, e.userID, dbTxRowID, e.userID,
		)
	}

	if newStatus == StatusCompleted && mp != nil {
		mpBytes := mp.Bytes()
		if len(mpBytes) == 0 {
			return errors.New("wallet: empty merkle path bytes on mined promotion")
		}
		if _, err := dbTx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET status = 'completed', block_height = ?, block_hash = ?, merkle_root = ?, merkle_path = ? WHERE tx_id = ?`, e.tables.knownTxs),
			height, blockHash, merkleRoot, mpBytes, txid,
		); err != nil {
			return err
		}
	} else {
		_, _ = dbTx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE "%s" SET status = 'unmined' WHERE tx_id = ? AND status != 'completed'`, e.tables.knownTxs),
			txid,
		)
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

// -----------------------------------------------------------------------------
// NoSend broadcast bridge (for chain.Port BuildFundedTx -> Broadcast callers)
// -----------------------------------------------------------------------------

// SendNoSendTx broadcasts a previously created NoSend transaction via go-wallet-toolbox SendWith.
func (e *Engine) SendNoSendTx(ctx context.Context, txid string) error {
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return fmt.Errorf("wallet: invalid txid %q: %w", txid, err)
	}
	acceptDelayed := false
	res, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "broadcast nektar funded transaction",
		Options: &sdkwallet.CreateActionOptions{
			SendWith:               []chainhash.Hash{*h},
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		if lastErr := e.svc.takeLastErr(txid); lastErr != nil {
			if errors.Is(lastErr, ErrBroadcastUnknown) {
				_ = e.MarkSubmissionUnknown(ctx, txid)
			}
			return lastErr
		}
		if errors.Is(err, ErrBroadcastUnknown) {
			_ = e.MarkSubmissionUnknown(ctx, txid)
		}
		return err
	}
	foundAccepted := false
	for _, sw := range res.SendWithResults {
		if sw.Txid.String() != txid {
			continue
		}
		switch sw.Status {
		case sdkwallet.ActionResultStatusUnproven:
			foundAccepted = true
		case sdkwallet.ActionResultStatusSending:
			_ = e.MarkSubmissionUnknown(ctx, txid)
			if lastErr := e.svc.takeLastErr(txid); lastErr != nil {
				return fmt.Errorf("%w: %w", ErrBroadcastUnknown, lastErr)
			}
			return fmt.Errorf("%w: tx %s still in sending state", ErrBroadcastUnknown, txid)
		case sdkwallet.ActionResultStatusFailed:
			if lastErr := e.svc.takeLastErr(txid); lastErr != nil {
				if errors.Is(lastErr, ErrBroadcastUnknown) {
					_ = e.MarkSubmissionUnknown(ctx, txid)
				}
				return lastErr
			}
			return fmt.Errorf("wallet: broadcast failed for tx %s", txid)
		}
	}
	if !foundAccepted && !e.cfg.InMemoryStore {
		_ = e.MarkSubmissionUnknown(ctx, txid)
		if lastErr := e.svc.takeLastErr(txid); lastErr != nil {
			return fmt.Errorf("%w: %w", ErrBroadcastUnknown, lastErr)
		}
		return fmt.Errorf("%w: SendWith did not confirm acceptance for tx %s", ErrBroadcastUnknown, txid)
	}
	e.svc.takeLastErr(txid)
	e.fuel.Wake()
	return nil
}

// AbortNoSendByTxID looks up the true go-wallet-toolbox action reference for txid and aborts it,
// atomically releasing reserved inputs so UTXOs never remain locked after pre-broadcast refusal.
func (e *Engine) AbortNoSendByTxID(ctx context.Context, txid string) error {
	if e.readDB == nil || txid == "" {
		return nil
	}
	var refStr string
	q := fmt.Sprintf(`SELECT COALESCE(reference, '') FROM "%s" WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL LIMIT 1`, e.tables.transactions)
	if err := e.readDB.QueryRowContext(ctx, q, e.userID, txid).Scan(&refStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if refStr == "" {
		return nil
	}
	// go-wallet-toolbox stores transactions.reference as base64-encoded bytes.
	refBytes, err := base64.StdEncoding.DecodeString(refStr)
	if err != nil || len(refBytes) == 0 {
		refBytes = []byte(refStr)
	}
	if _, err := e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: refBytes}, e.cfg.Originator); err != nil {
		// Fall back to raw string bytes if toolbox stored literal reference bytes.
		if _, err2 := e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: []byte(refStr)}, e.cfg.Originator); err2 != nil {
			return err
		}
	}
	return nil
}

// isSpendableParentStatus returns true only for transaction states whose outputs may safely be spent as parents.
// Never includes TxStatusSending (unconfirmed self-submission whose broadcast outcome is still unverified).
func isSpendableParentStatus(st wdk.TxStatus) bool {
	switch st {
	case wdk.TxStatusCompleted, wdk.TxStatusUnproven:
		return true
	default:
		return false
	}
}

// MarkSubmissionUnknown transitions a transaction whose broadcast outcome is uncertain (timeout/EOF/5xx)
// to status='submission_unknown' and marks its outputs unspendable atomically so inputs remain reserved
// and child transactions cannot spend phantom outputs until Monitor.UnFail confirms or proves it.
func (e *Engine) MarkSubmissionUnknown(ctx context.Context, txid string) error {
	if e.sqlDB == nil || txid == "" {
		return nil
	}
	tx, err := e.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := fmt.Sprintf(`UPDATE "%s" SET status = 'submission_unknown', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL AND status NOT IN ('completed','failed','aborted')`, e.tables.transactions)
	if _, err := tx.ExecContext(ctx, q, e.userID, txid); err != nil {
		return err
	}
	qOut := fmt.Sprintf(`UPDATE "%s" SET spendable = 0 WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL`, e.tables.outputs)
	if _, err := tx.ExecContext(ctx, qOut, e.userID, txid); err != nil {
		return err
	}
	qUtxo := fmt.Sprintf(`DELETE FROM "%s" WHERE user_id = ? AND output_id IN (SELECT id FROM "%s" WHERE user_id = ? AND tx_id = ?)`, e.tables.userUTXOs, e.tables.outputs)
	_, _ = tx.ExecContext(ctx, qUtxo, e.userID, e.userID, txid)
	_, _ = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE "%s" SET status = 'unmined' WHERE tx_id = ? AND status = 'nosend'`, e.tables.knownTxs), txid)
	return tx.Commit()
}

// ReconcileOrphanedNoSend reconciles stale 'nosend' transactions older than ReservationTTL:
//   - If a header-verified BUMP exists, atomically promotes to 'completed' (restoring outputs + user_utxos + known_txes).
//   - If verified raw tx bytes exist on chain, atomically promotes to 'unproven' (restoring outputs + user_utxos + known_txes).
//   - Never aborts on upstream error or single-host 404 before 2*ReservationTTL.
func (e *Engine) ReconcileOrphanedNoSend(ctx context.Context) (int, error) {
	if e.readDB == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-e.cfg.ReservationTTL)
	q := fmt.Sprintf(`SELECT id, COALESCE(tx_id, ''), updated_at FROM "%s" WHERE user_id = ? AND deleted_at IS NULL AND status = 'nosend' AND updated_at < ? AND tx_id != ''`, e.tables.transactions)
	rows, err := e.readDB.QueryContext(ctx, q, e.userID, cutoff)
	if err != nil {
		return 0, err
	}
	type nsRow struct {
		id        int64
		txid      string
		updatedAt time.Time
	}
	var cands []nsRow
	for rows.Next() {
		var r nsRow
		if rows.Scan(&r.id, &r.txid, &r.updatedAt) == nil && r.txid != "" {
			cands = append(cands, r)
		}
	}
	_ = rows.Close()
	aborted := 0
	for _, c := range cands {
		if e.svc != nil {
			// Single verified proof lookup (validates ComputeRoot + IsValidRootForHeight).
			mp, h, ok, pErr := e.svc.lookupProof(ctx, c.txid)
			if pErr != nil {
				// Fail closed on tracker/upstream error.
				continue
			}
			if ok && mp != nil {
				if h == 0 {
					h = mp.BlockHeight
				}
				txHash, _ := chainhash.NewHashFromHex(c.txid)
				root, rErr := mp.ComputeRoot(txHash)
				hdr, hdrErr := e.svc.ChainHeaderByHeight(ctx, h)
				txObj, _ := e.LoadParsedTx(ctx, c.txid)
				if rErr == nil && root != nil && hdrErr == nil && hdr != nil && txObj != nil {
					if e.cfg.InMemoryStore || strings.EqualFold(hdr.MerkleRoot, root.String()) {
						txObj.MerklePath = mp
						if e.promoteTxStatusAtomic(ctx, c.id, c.txid, StatusCompleted, txObj, mp, h, hdr.Hash, root.String()) == nil {
							e.txCache.Put(c.txid, txObj)
							if e.cfg.OnTxMined != nil {
								e.cfg.OnTxMined(c.txid, h, mp)
							}
						}
					}
				}
				continue
			}
			if e.cfg.Chain != nil {
				raw, rErr := e.cfg.Chain.TxRaw(ctx, c.txid)
				if rErr == nil && len(raw) > 0 {
					if parsed, parseErr := transaction.NewTransactionFromBytes(raw); parseErr == nil && parsed.TxID().String() == c.txid {
						_ = e.promoteTxStatusAtomic(ctx, c.id, c.txid, StatusUnproven, parsed, nil, 0, "", "")
						continue
					}
				}
				if rErr != nil {
					low := strings.ToLower(rErr.Error())
					if !strings.Contains(low, "404") && !strings.Contains(low, "not found") && !strings.Contains(low, "no such") {
						// Upstream unreachable: fail closed (never abort on network error).
						continue
					}
				}
				// Never abort from a single-host 404 until the explicit abandon horizon (2*ReservationTTL) passes.
				if !e.cfg.InMemoryStore && time.Since(c.updatedAt) < 2*e.cfg.ReservationTTL {
					continue
				}
			} else if !e.cfg.InMemoryStore {
				// No external chain source to rule out broadcast: fail closed on live networks.
				continue
			}
		}
		if err := e.AbortNoSendByTxID(ctx, c.txid); err == nil {
			aborted++
		}
	}
	return aborted, nil
}
