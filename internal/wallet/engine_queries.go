package wallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// Balance, UTXO & Action Telemetry (via go-wallet-toolbox typed CRUD)
// -----------------------------------------------------------------------------

// Balance computes confirmed, unproven, reserved, and per-basket telemetry in O(unspent) time
// using a partial-index-backed SQL UNION ALL GROUP BY over the dedicated read-only WAL pool
// (zero writer contention, zero O(N_history) scans!).
func (e *Engine) Balance(ctx context.Context) (BalanceBreakdown, error) {
	var b BalanceBreakdown
	// Branch 1 hits partial index idx_nektar_outputs_unspent (WHERE deleted_at IS NULL AND spent_by IS NULL).
	// Branch 2 hits idx_nektar_txs_user_status + idx_nektar_outputs_spent_fee only when unsigned/unprocessed txs exist.
	q := fmt.Sprintf(`
		SELECT basket, is_reserved, is_confirmed, COUNT(1) AS cnt, COALESCE(SUM(sats), 0) AS total_sats
		FROM (
			SELECT
				COALESCE(o.basket_name, '') AS basket,
				CASE WHEN u.reserved_by_id IS NOT NULL OR o.spendable = 0 THEN 1 ELSE 0 END AS is_reserved,
				CASE WHEN owner.status = 'completed' OR u.utxo_status = 'mined' THEN 1 ELSE 0 END AS is_confirmed,
				o.satoshis AS sats
			FROM "%s" o
			INNER JOIN "%s" owner ON owner.id = o.transaction_id
			LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
			WHERE o.user_id = ?
			  AND o.deleted_at IS NULL
			  AND o.spent_by IS NULL
			  AND o.satoshis > 0
			  AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed','nosend','sending','submission_unknown')
			UNION ALL
			SELECT
				COALESCE(o.basket_name, '') AS basket,
				1 AS is_reserved,
				0 AS is_confirmed,
				o.satoshis AS sats
			FROM "%s" sp
			INNER JOIN "%s" o ON o.spent_by = sp.id AND o.deleted_at IS NULL
			INNER JOIN "%s" owner ON owner.id = o.transaction_id
			WHERE sp.user_id = ?
			  AND sp.deleted_at IS NULL
			  AND sp.status IN ('unsigned','unprocessed','nosend','sending','submission_unknown')
			  AND o.user_id = ?
			  AND o.satoshis > 0
			  AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed','nosend','sending','submission_unknown')
		)
		GROUP BY basket, is_reserved, is_confirmed`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs,
		e.tables.transactions, e.tables.outputs, e.tables.transactions,
	)

	rows, err := e.readDB.QueryContext(ctx, q, e.userID, e.userID, e.userID)
	if err != nil {
		return b, err
	}
	defer rows.Close()

	for rows.Next() {
		var basket string
		var isReserved, isConfirmed, cnt int
		var sumSats int64
		if err := rows.Scan(&basket, &isReserved, &isConfirmed, &cnt, &sumSats); err != nil {
			return b, err
		}
		if sumSats <= 0 || cnt <= 0 {
			continue
		}
		if !IsSpendingBasket(basket) {
			if isReserved == 0 {
				b.CustomCount += cnt
			}
			continue
		}
		sats := uint64(sumSats)
		b.UTXOCount += cnt
		b.TotalSats += sats
		if isReserved != 0 {
			b.ReservedSats += sats
			continue
		}
		b.SpendableSats += sats
		b.SpendableCount += cnt
		if isConfirmed != 0 {
			b.ConfirmedSats += sats
		} else {
			b.UnprovenSats += sats
		}
		switch basket {
		case BasketFuel:
			b.FuelCount += cnt
			b.FuelSats += sats
			if isConfirmed != 0 {
				b.ProvenFuelCount += cnt
				b.ProvenFuelSats += sats
			} else {
				b.UnprovenFuelCount += cnt
			}
		case BasketReserve:
			b.ReserveCount += cnt
			b.ReserveSats += sats
		case BasketDefault:
			b.DefaultCount += cnt
			b.DefaultSats += sats
		case BasketFunding:
			b.FundingCount += cnt
			b.FundingSats += sats
		}
	}
	if err := rows.Err(); err != nil {
		return b, err
	}

	pendingQ := fmt.Sprintf(`SELECT COUNT(1) FROM "%s" WHERE user_id = ? AND deleted_at IS NULL AND status IN ('unsigned','unprocessed','sending','submission_unknown')`, e.tables.transactions)
	_ = e.readDB.QueryRowContext(ctx, pendingQ, e.userID).Scan(&b.PendingTxCount)
	depth, _, dErr := e.computeUnprovenDepthBounds(ctx)
	if dErr != nil {
		e.logf("wallet: computeMaxUnprovenDepth warning: %v", dErr)
	}
	b.MaxUnprovenDepth = depth
	return b, nil
}

func (e *Engine) computeUnprovenDepthBounds(ctx context.Context) (maxDepth int, minDepth int, err error) {
	if e.readDB == nil {
		return 0, 0, nil
	}
	q := fmt.Sprintf(`
		SELECT DISTINCT COALESCE(owner.tx_id, '')
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		WHERE o.user_id = ?
		  AND o.deleted_at IS NULL
		  AND o.spent_by IS NULL
		  AND o.satoshis > 0
		  AND owner.status IN ('unproven','sending','nosend')
		  AND owner.tx_id IS NOT NULL
		  AND owner.tx_id != ''`,
		e.tables.outputs, e.tables.transactions,
	)
	rows, err := e.readDB.QueryContext(ctx, q, e.userID)
	if err != nil {
		return 0, 0, err
	}
	var unproved []string
	for rows.Next() {
		var txid string
		if rows.Scan(&txid) == nil && txid != "" {
			unproved = append(unproved, txid)
		}
	}
	_ = rows.Close()
	if len(unproved) == 0 {
		return 0, 0, nil
	}

	// Build parent-tx status lookup for fast bounded DFS.
	maxCap := e.cfg.MaxUnprovenDepth + 4
	if maxCap < 16 {
		maxCap = 16
	}
	memo := make(map[string]int, len(unproved)*2)
	var depthOf func(txid string, seen map[string]bool) int
	depthOf = func(txid string, seen map[string]bool) int {
		if d, ok := memo[txid]; ok {
			return d
		}
		if seen[txid] || len(seen) > maxCap {
			return 0
		}
		tx, err := e.LoadParsedTx(ctx, txid)
		if err != nil || tx == nil {
			memo[txid] = 1
			return 1
		}
		if tx.MerklePath != nil {
			memo[txid] = 0
			return 0
		}
		seen[txid] = true
		best := 1
		for _, in := range tx.Inputs {
			if in.SourceTXID == nil {
				continue
			}
			pid := in.SourceTXID.String()
			pd := depthOf(pid, seen)
			if pd+1 > best {
				best = pd + 1
			}
		}
		delete(seen, txid)
		memo[txid] = best
		return best
	}

	minDepth = -1
	for _, id := range unproved {
		d := depthOf(id, make(map[string]bool))
		if d > maxDepth {
			maxDepth = d
		}
		if minDepth < 0 || d < minDepth {
			minDepth = d
		}
	}
	if minDepth < 0 {
		minDepth = 0
	}
	return maxDepth, minDepth, nil
}

// ListUTXOs returns unspent UTXO records filtered by basket ("all", "default", "reserve", "fuel", etc.)
// directly from the read-only WAL connection pool in O(limit) with SQL-level filtering & pagination.
func (e *Engine) ListUTXOs(ctx context.Context, basket string, spendableOnly bool, limit, offset int) ([]OutputRecord, int, error) {
	if limit <= 0 {
		limit = 100
	}
	where := fmt.Sprintf(`
		o.user_id = ?
		AND o.deleted_at IS NULL
		AND o.spent_by IS NULL
		AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed','nosend','submission_unknown')`)
	args := []any{e.userID}
	if basket != "" && basket != "all" {
		where += ` AND o.basket_name = ?`
		args = append(args, basket)
	}
	if spendableOnly {
		where += ` AND o.spendable = 1 AND u.reserved_by_id IS NULL`
	}

	countQ := fmt.Sprintf(`
		SELECT COUNT(1)
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
		WHERE %s`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs, where,
	)
	var total int
	if err := e.readDB.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || offset >= total {
		return []OutputRecord{}, total, nil
	}

	selQ := fmt.Sprintf(`
		SELECT
			o.id,
			COALESCE(owner.tx_id, ''),
			o.vout,
			o.satoshis,
			o.locking_script,
			COALESCE(o.basket_name, ''),
			o.spendable,
			CASE WHEN u.reserved_by_id IS NOT NULL THEN 1 ELSE 0 END,
			COALESCE(o.derivation_prefix, ''),
			COALESCE(o.derivation_suffix, ''),
			COALESCE(o.sender_identity_key, ''),
			COALESCE(o.custom_instructions, ''),
			COALESCE(o.description, ''),
			o.created_at,
			COALESCE(owner.status, '')
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
		WHERE %s
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT ? OFFSET ?`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs, where,
	)
	qArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := e.readDB.QueryContext(ctx, selQ, qArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]OutputRecord, 0, min(limit, total-offset))
	for rows.Next() {
		var (
			id                             int64
			txid, bName, pre, suf, snd, ci string
			desc, pStatus                  string
			vout                           uint32
			sats                           int64
			lockScript                     []byte
			spendableInt, reservedInt      int
			createdAt                      time.Time
		)
		if err := rows.Scan(&id, &txid, &vout, &sats, &lockScript, &bName, &spendableInt, &reservedInt,
			&pre, &suf, &snd, &ci, &desc, &createdAt, &pStatus); err != nil {
			return nil, 0, err
		}
		keyKind := KeyKindCustom
		if bName == BasketFunding {
			keyKind = KeyKindFund
		} else if pre != "" {
			keyKind = KeyKindBRC29
		}
		out = append(out, OutputRecord{
			ID:                 id,
			TxID:               txid,
			Vout:               vout,
			Satoshis:           uint64(max(0, sats)),
			LockingScript:      lockScript,
			LockingScriptHex:   hex.EncodeToString(lockScript),
			Basket:             bName,
			Spendable:          spendableInt != 0 && reservedInt == 0,
			Reserved:           reservedInt != 0,
			KeyKind:            keyKind,
			DerivationPrefix:   pre,
			DerivationSuffix:   suf,
			SenderIdentityKey:  snd,
			CustomInstructions: ci,
			OutputDescription:  desc,
			CreatedAt:          createdAt.Unix(),
			ParentStatus:       pStatus,
		})
	}
	return out, total, rows.Err()
}

// ListRecentActions returns recent wallet transactions ordered newest first in ONE indexed SQL query
// over the dedicated read-only WAL pool (eliminating the previous N+1 KnownTx + BEEF decode loop!).
func (e *Engine) ListRecentActions(ctx context.Context, limit, offset int) ([]TxRecord, int, error) {
	if limit <= 0 {
		limit = 50
	}
	countQ := fmt.Sprintf(`SELECT COUNT(1) FROM "%s" WHERE user_id = ? AND deleted_at IS NULL`, e.tables.transactions)
	var total int
	if err := e.readDB.QueryRowContext(ctx, countQ, e.userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || offset >= total {
		return []TxRecord{}, total, nil
	}

	selQ := fmt.Sprintf(`
		SELECT
			COALESCE(t.tx_id, ''),
			COALESCE(t.reference, ''),
			COALESCE(t.status, ''),
			t.is_outgoing,
			t.satoshis,
			COALESCE(t.description, ''),
			t.version,
			t.lock_time,
			COALESCE(k.block_height, 0),
			t.created_at,
			t.updated_at,
			COALESCE((SELECT SUM(satoshis) FROM "%s" WHERE spent_by = t.id AND deleted_at IS NULL), 0) AS in_sats,
			COALESCE((SELECT SUM(satoshis) FROM "%s" WHERE transaction_id = t.id AND deleted_at IS NULL), 0) AS out_sats
		FROM "%s" t
		LEFT JOIN "%s" k ON k.tx_id = t.tx_id
		WHERE t.user_id = ? AND t.deleted_at IS NULL
		ORDER BY t.id DESC
		LIMIT ? OFFSET ?`,
		e.tables.outputs, e.tables.outputs, e.tables.transactions, e.tables.knownTxs,
	)
	rows, err := e.readDB.QueryContext(ctx, selQ, e.userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]TxRecord, 0, min(limit, total-offset))
	for rows.Next() {
		var (
			txid, ref, status, desc string
			isOutInt                int
			sats, inSats, outSats   int64
			ver, lockTime, height   uint32
			createdAt, updatedAt    time.Time
		)
		if err := rows.Scan(&txid, &ref, &status, &isOutInt, &sats, &desc, &ver, &lockTime,
			&height, &createdAt, &updatedAt, &inSats, &outSats); err != nil {
			return nil, 0, err
		}
		var feeSats uint64
		if isOutInt != 0 {
			if inSats > outSats {
				feeSats = uint64(inSats - outSats)
			} else if txid != "" {
				// Fallback to in-memory LRU only (0 SQL!) if tx spent external non-wallet inputs.
				if cached, ok := e.txCache.Get(txid); ok && cached != nil {
					feeSats = computeTxFee(cached)
				}
			}
		}
		out = append(out, TxRecord{
			TxID:        txid,
			Reference:   ref,
			Status:      status,
			IsOutgoing:  isOutInt != 0,
			Satoshis:    sats,
			FeeSats:     feeSats,
			Description: desc,
			Version:     ver,
			LockTime:    lockTime,
			BlockHeight: height,
			CreatedAt:   createdAt.Unix(),
			UpdatedAt:   updatedAt.Unix(),
		})
	}
	return out, total, rows.Err()
}
