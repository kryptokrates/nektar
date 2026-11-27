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
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

// -----------------------------------------------------------------------------
// NoSend broadcast bridge & submission_unknown state transitions
// -----------------------------------------------------------------------------

// SendNoSendTx broadcasts a previously created NoSend transaction via go-wallet-toolbox SendWith.
func (e *Engine) SendNoSendTx(ctx context.Context, txid string) error {
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return fmt.Errorf("wallet: invalid txid %q: %w", txid, err)
	}
	acceptDelayed := false
	res, err := e.callTBCreateAction(ctx, sdkwallet.CreateActionArgs{
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
	qOut := fmt.Sprintf(`UPDATE "%s" SET spendable = 0 WHERE user_id = ? AND deleted_at IS NULL AND transaction_id IN (SELECT id FROM "%s" WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL)`, e.tables.outputs, e.tables.transactions)
	if _, err := tx.ExecContext(ctx, qOut, e.userID, e.userID, txid); err != nil {
		return err
	}
	qUtxo := fmt.Sprintf(`DELETE FROM "%s" WHERE user_id = ? AND output_id IN (SELECT o.id FROM "%s" o INNER JOIN "%s" t ON t.id = o.transaction_id WHERE o.user_id = ? AND t.tx_id = ? AND t.deleted_at IS NULL)`, e.tables.userUTXOs, e.tables.outputs, e.tables.transactions)
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
				if rErr == nil && len(raw) == 0 {
					// Empty response without explicit not-found error: fail closed (never abort on silence).
					continue
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
