package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SaveBatchAnchorStrict inserts a batch anchor only if no conflicting local anchor exists at (shard, n).
func (d *DB) SaveBatchAnchorStrict(anchor BatchAnchor) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var existingID, existingRoot, existingTx string
	var existingSettled, existingBeefLen int
	err := d.sql.QueryRow(`SELECT shard_id, root_hex, txid, settled, length(beef) FROM anchors WHERE shard = ? AND n = ?`, anchor.Shard, anchor.N).
		Scan(&existingID, &existingRoot, &existingTx, &existingSettled, &existingBeefLen)
	if err == nil {
		if existingID != anchor.ShardID || existingRoot != anchor.RootHex || existingTx != anchor.TxID {
			return fmt.Errorf("store: conflicting local anchor already exists for %s#%d", anchor.Shard, anchor.N)
		}
		// Upgrade unsettled or proofless local anchor when incoming anchor carries verified mined BUMP/BEEF (D3/D6).
		if len(anchor.Beef) > 0 && ((existingSettled == 0 && anchor.Settled) || existingBeefLen == 0) {
			_, uErr := d.sql.Exec(
				`UPDATE anchors SET beef = ?, settled = ?, block_height = ? WHERE shard = ? AND n = ?`,
				anchor.Beef, boolToInt(anchor.Settled || existingSettled != 0), anchor.BlockHeight, anchor.Shard, anchor.N,
			)
			return uErr
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = d.sql.Exec(`
		INSERT INTO anchors (shard, shard_id, n, size, root_hex, chained_hex, state_hash, mmr_root, txid, beef, settled, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		anchor.Shard, anchor.ShardID, anchor.N, anchor.Size, anchor.RootHex, anchor.ChainedHex,
		anchor.StateHash, anchor.MmrRoot, anchor.TxID, anchor.Beef, boolToInt(anchor.Settled), anchor.BlockHeight, anchor.CreatedAt,
	)
	return err
}

// SavePendingAnchor persists an in-flight anchor batch or rotation BEFORE broadcast.
func (d *DB) SavePendingAnchor(p PendingAnchor) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	kind := p.Kind
	if kind == "" {
		kind = "append"
	}
	seqsJSON, err := json.Marshal(p.Seqs)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`
		INSERT OR REPLACE INTO pending_anchors (shard, n, kind, seqs_json, root_hex, chained_hex, state_hash, mmr_root, txid, beef, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Shard, p.N, kind, seqsJSON, p.RootHex, p.ChainedHex, p.StateHash, p.MmrRoot, p.TxID, p.Beef, p.CreatedAt)
	return err
}

// UpdatePendingAnchorTx attaches the signed txid and AtomicBEEF to an in-flight pending anchor.
func (d *DB) UpdatePendingAnchorTx(shard string, n int64, txid string, beef []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE pending_anchors SET txid = ?, beef = ? WHERE shard = ? AND n = ?`, txid, beef, shard, n)
	return err
}

// DeletePendingAnchor removes a completed or cleanly rolled-back pending anchor record.
func (d *DB) DeletePendingAnchor(shard string, n int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`DELETE FROM pending_anchors WHERE shard = ? AND n = ?`, shard, n)
	return err
}

// ListPendingAnchors returns all incomplete in-flight anchor batches ordered oldest first.
func (d *DB) ListPendingAnchors() ([]PendingAnchor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT shard, n, COALESCE(kind, 'append'), seqs_json, root_hex, chained_hex, COALESCE(state_hash, ''), COALESCE(mmr_root, ''), txid, beef, created_at FROM pending_anchors ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAnchor
	for rows.Next() {
		var p PendingAnchor
		var seqsRaw []byte
		if err := rows.Scan(&p.Shard, &p.N, &p.Kind, &seqsRaw, &p.RootHex, &p.ChainedHex, &p.StateHash, &p.MmrRoot, &p.TxID, &p.Beef, &p.CreatedAt); err != nil {
			return nil, err
		}
		if p.Kind == "" {
			p.Kind = "append"
		}
		if len(seqsRaw) > 0 {
			if err := json.Unmarshal(seqsRaw, &p.Seqs); err != nil {
				return nil, fmt.Errorf("store: decode pending_anchor seqs %s#%d: %w", p.Shard, p.N, err)
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountUnsettledAnchors returns the number of broadcast batch anchors without a mined BUMP proof.
func (d *DB) CountUnsettledAnchors() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM anchors WHERE settled = 0`).Scan(&n)
	return n, err
}

// CountPendingAnchors returns the number of in-flight anchor batches awaiting broadcast completion.
func (d *DB) CountPendingAnchors() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM pending_anchors`).Scan(&n)
	return n, err
}

// RecordBatchAnchor atomically records an anchor (using strict INSERT to prevent overwriting existing N)
// and binds fragments, mem_commits, receipts (when shard == "__receipts__"), and clears pending_anchors
// in ONE SQLite transaction.
func (d *DB) RecordBatchAnchor(anchor BatchAnchor, seqs []int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		INSERT INTO anchors (shard, shard_id, n, size, root_hex, chained_hex, state_hash, mmr_root, txid, beef, settled, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		anchor.Shard, anchor.ShardID, anchor.N, anchor.Size, anchor.RootHex, anchor.ChainedHex,
		anchor.StateHash, anchor.MmrRoot, anchor.TxID, anchor.Beef, boolToInt(anchor.Settled), anchor.BlockHeight, anchor.CreatedAt,
	)
	if err != nil {
		return err
	}
	for idx, seq := range seqs {
		if _, err := tx.Exec(
			`UPDATE fragments SET batch_n = ?, leaf_index = ?, anchor_txid = ? WHERE shard = ? AND seq = ?`,
			anchor.N, idx, anchor.TxID, anchor.Shard, seq,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE mem_commits SET anchor_txid = ?, anchor_n = ? WHERE repo = ? AND seq = ?`,
			anchor.TxID, anchor.N, anchor.Shard, seq,
		); err != nil {
			return err
		}
		if anchor.Shard == "__receipts__" {
			if _, err := tx.Exec(
				`UPDATE receipts SET anchor_txid = ? WHERE shard_seq = ?`,
				anchor.TxID, seq,
			); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM pending_anchors WHERE shard = ? AND n = ?`, anchor.Shard, anchor.N); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateAnchorHead updates the latest batch anchor's txid, beef, settled status, and height
// after an on-chain NektarAnchor.Rotate() spend (which preserves n, R, Rp, size while rotating ownerPkh).
func (d *DB) UpdateAnchorHead(shard string, n int64, newTxID string, newBeef []byte, settled bool, blockHeight int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
		UPDATE anchors SET txid = ?, beef = ?, settled = ?, block_height = ?
		WHERE shard = ? AND n = ?`,
		newTxID, newBeef, boolToInt(settled), blockHeight, shard, n,
	)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("store: anchor (%s, n=%d) not found for head update", shard, n)
	}
	if _, err := tx.Exec(`UPDATE fragments SET anchor_txid = ? WHERE shard = ? AND batch_n = ?`, newTxID, shard, n); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE mem_commits SET anchor_txid = ? WHERE repo = ? AND anchor_n = ?`, newTxID, shard, n); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE mem_tags SET anchor_txid = ? WHERE repo = ? AND anchor_n = ?`, newTxID, shard, n); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM pending_anchors WHERE shard = ? AND n = ?`, shard, n); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAnchors returns all batch anchors for shard ordered by n ascending.
func (d *DB) ListAnchors(shard string) ([]BatchAnchor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, shard_id, n, size, root_hex, chained_hex, COALESCE(state_hash, ''), COALESCE(mmr_root, ''), txid, beef, settled, block_height, created_at
		FROM anchors WHERE shard = ? ORDER BY n ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchAnchor
	for rows.Next() {
		var a BatchAnchor
		var settled int
		if err := rows.Scan(&a.Shard, &a.ShardID, &a.N, &a.Size, &a.RootHex, &a.ChainedHex, &a.StateHash, &a.MmrRoot, &a.TxID, &a.Beef, &settled, &a.BlockHeight, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Settled = settled != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAnchorSummaries returns anchor metadata for shard ordered by n ascending WITHOUT loading BEEF blobs.
func (d *DB) ListAnchorSummaries(shard string) ([]BatchAnchor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, shard_id, n, size, root_hex, chained_hex, COALESCE(state_hash, ''), COALESCE(mmr_root, ''), txid, settled, block_height, created_at
		FROM anchors WHERE shard = ? ORDER BY n ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchAnchor
	for rows.Next() {
		var a BatchAnchor
		var settled int
		if err := rows.Scan(&a.Shard, &a.ShardID, &a.N, &a.Size, &a.RootHex, &a.ChainedHex, &a.StateHash, &a.MmrRoot, &a.TxID, &settled, &a.BlockHeight, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Settled = settled != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListUnsettledAnchors returns all anchors with settled=0 across every shard (for live proof refresh).
func (d *DB) ListUnsettledAnchors() ([]BatchAnchor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, shard_id, n, size, root_hex, chained_hex, COALESCE(state_hash, ''), COALESCE(mmr_root, ''), txid, beef, settled, block_height, created_at
		FROM anchors WHERE settled = 0 ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchAnchor
	for rows.Next() {
		var a BatchAnchor
		var settled int
		if err := rows.Scan(&a.Shard, &a.ShardID, &a.N, &a.Size, &a.RootHex, &a.ChainedHex, &a.StateHash, &a.MmrRoot, &a.TxID, &a.Beef, &settled, &a.BlockHeight, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Settled = settled != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAnchorProof upgrades a broadcast anchor to settled=1 once its mined BUMP proof lands.
func (d *DB) UpdateAnchorProof(shard string, n int64, beef []byte, blockHeight int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE anchors SET beef = ?, settled = 1, block_height = ? WHERE shard = ? AND n = ?`,
		beef, blockHeight, shard, n)
	return err
}

// ClearAnchorProofsFrom rolls back settled anchors at or above forkHeight after a chain reorg
// so RefreshAnchorProofs re-acquires valid post-reorg BUMPs instead of serving orphaned proofs.
func (d *DB) ClearAnchorProofsFrom(forkHeight int64) (int64, error) {
	return d.ClearAnchorProofsFromWithStrip(forkHeight, nil)
}

// ClearAnchorProofsFromWithStrip atomically rolls back settled anchors at or above forkHeight
// and, when stripBeef != nil, strips orphaned MerklePath proofs from anchors.beef inside the
// same SQLite transaction so offline verifiers see Unsettled instead of BEEFInvalid.
func (d *DB) ClearAnchorProofsFromWithStrip(forkHeight int64, stripBeef func([]byte) []byte) (int64, error) {
	if forkHeight <= 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if stripBeef == nil {
		res, err := d.sql.Exec(`UPDATE anchors SET settled = 0, block_height = 0 WHERE settled = 1 AND block_height >= ?`, forkHeight)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(`SELECT shard, n, beef FROM anchors WHERE settled = 1 AND block_height >= ?`, forkHeight)
	if err != nil {
		return 0, err
	}
	type anchorRow struct {
		shard string
		n     int64
		beef  []byte
	}
	var affected []anchorRow
	for rows.Next() {
		var r anchorRow
		if err := rows.Scan(&r.shard, &r.n, &r.beef); err != nil {
			_ = rows.Close()
			return 0, err
		}
		affected = append(affected, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, r := range affected {
		cleaned := stripBeef(r.beef)
		if _, err := tx.Exec(
			`UPDATE anchors SET settled = 0, block_height = 0, beef = ? WHERE shard = ? AND n = ?`,
			cleaned, r.shard, r.n,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(affected)), nil
}

// IsAnchorSettled reports whether txid corresponds to a settled batch anchor carrying persisted BEEF.
func (d *DB) IsAnchorSettled(txid string) (bool, error) {
	if txid == "" {
		return false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM anchors WHERE txid = ? AND settled = 1 AND length(beef) > 0`, txid).Scan(&n)
	return n > 0, err
}

// GetAnchorByTxID fetches a batch anchor matching txid, if any.
func (d *DB) GetAnchorByTxID(txid string) (BatchAnchor, bool, error) {
	if txid == "" {
		return BatchAnchor{}, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var a BatchAnchor
	var settled int
	err := d.sql.QueryRow(`
		SELECT shard, shard_id, n, size, root_hex, chained_hex, COALESCE(state_hash, ''), COALESCE(mmr_root, ''), txid, beef, settled, block_height, created_at
		FROM anchors WHERE txid = ? LIMIT 1`, txid).
		Scan(&a.Shard, &a.ShardID, &a.N, &a.Size, &a.RootHex, &a.ChainedHex, &a.StateHash, &a.MmrRoot, &a.TxID, &a.Beef, &settled, &a.BlockHeight, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchAnchor{}, false, nil
	}
	if err != nil {
		return BatchAnchor{}, false, err
	}
	a.Settled = settled != 0
	return a, true, nil
}

// CommitReceiptAtomic atomically appends the __receipts__ fragment (including prev_leaf/chain_hash),
// persists the receipts row, and records all citation edges inside a single SQLite transaction.
func (d *DB) CommitReceiptAtomic(frag FragmentRecord, rec ReceiptRecord, cites []CitationEdge) (FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return FragmentRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if frag.Class == "" {
		frag.Class = "receipt"
	}
	appended, _, err := appendFragmentTx(tx, frag)
	if err != nil {
		return FragmentRecord{}, err
	}
	frag = appended

	rec.ShardSeq = frag.Seq
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO receipts (receipt_hash, shard_seq, body_json, anchor_txid, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		rec.ReceiptHash, rec.ShardSeq, rec.BodyJSON, rec.AnchorTxID, rec.CreatedAt,
	); err != nil {
		return FragmentRecord{}, err
	}
	for _, c := range cites {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO citations (shard, seq, receipt_hash) VALUES (?, ?, ?)`,
			c.Shard, c.Seq, c.ReceiptHash); err != nil {
			return FragmentRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return FragmentRecord{}, err
	}
	return frag, nil
}

// UpdateReceiptAnchor stamps anchor_txid on any receipt pointing to (ReceiptsShard, seq).
func (d *DB) UpdateReceiptAnchor(shardSeq int64, anchorTxID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE receipts SET anchor_txid = ? WHERE shard_seq = ?`, anchorTxID, shardSeq)
	return err
}

// GetReceipt looks up a receipt by its 64-hex hash.
func (d *DB) GetReceipt(hash string) (ReceiptRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r ReceiptRecord
	err := d.sql.QueryRow(`SELECT receipt_hash, shard_seq, body_json, anchor_txid, created_at FROM receipts WHERE receipt_hash = ?`, hash).
		Scan(&r.ReceiptHash, &r.ShardSeq, &r.BodyJSON, &r.AnchorTxID, &r.CreatedAt)
	return r, err
}

// CitingReceipts returns all receipt hashes citing (shard, seq).
func (d *DB) CitingReceipts(shard string, seq int64) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT receipt_hash FROM citations WHERE shard = ? AND seq = ?`, shard, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
