package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// ComputeFragmentChainHash computes the domain-separated double-SHA-256 chain hash binding
// each appended fragment to its shard predecessor: DSHA256("nektar:frag-chain:v1" || prevChain32 || leaf).
func ComputeFragmentChainHash(prevChainHex, leafHex string) string {
	var prev [32]byte
	if prevChainHex != "" {
		if b, err := hex.DecodeString(prevChainHex); err == nil && len(b) == 32 {
			copy(prev[:], b)
		}
	}
	leafBytes, _ := hex.DecodeString(leafHex)
	buf := make([]byte, 0, len("nektar:frag-chain:v1")+32+len(leafBytes))
	buf = append(buf, "nektar:frag-chain:v1"...)
	buf = append(buf, prev[:]...)
	buf = append(buf, leafBytes...)
	h1 := sha256.Sum256(buf)
	h2 := sha256.Sum256(h1[:])
	return hex.EncodeToString(h2[:])
}

// AppendFragment inserts a deduplicated append-only fragment into shard.
// If a fragment with the same (shard, hdp) already exists, it returns the existing record and dedup=true.
func (d *DB) AppendFragment(rec FragmentRecord) (FragmentRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing, err := d.getByHDP(rec.Shard, rec.HDP)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return FragmentRecord{}, false, err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return FragmentRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM fragments WHERE shard = ?`, rec.Shard).Scan(&nextSeq); err != nil {
		return FragmentRecord{}, false, err
	}
	rec.Seq = nextSeq
	var prevLeaf, prevChain string
	if nextSeq > 1 {
		_ = tx.QueryRow(`SELECT leaf, COALESCE(chain_hash, '') FROM fragments WHERE shard = ? AND seq = ?`, rec.Shard, nextSeq-1).Scan(&prevLeaf, &prevChain)
	}
	rec.PrevLeaf = prevLeaf
	rec.ChainHash = ComputeFragmentChainHash(prevChain, rec.Leaf)
	if rec.BatchN == 0 && rec.AnchorTxID == "" {
		rec.BatchN = -1
		rec.LeafIndex = -1
	}
	if rec.Class == "" {
		rec.Class = "general"
	}
	_, err = tx.Exec(`
		INSERT INTO fragments (
			shard, seq, hdp, leaf, prev_leaf, chain_hash, data, ciphertext, keyed_meta, provenance,
			level, class, borg_group, borg_epoch, batch_n, leaf_index,
			anchor_txid, supersedes, forgotten, tainted, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Shard, rec.Seq, rec.HDP, rec.Leaf, rec.PrevLeaf, rec.ChainHash, rec.Data, rec.Ciphertext, rec.KeyedMeta, rec.Provenance,
		rec.Level, rec.Class, rec.BorgGroup, rec.BorgEpoch, rec.BatchN, rec.LeafIndex,
		rec.AnchorTxID, rec.Supersedes, boolToInt(rec.Forgotten), boolToInt(rec.Tainted), rec.CreatedAt,
	)
	if err != nil {
		return FragmentRecord{}, false, err
	}
	if !rec.Forgotten && rec.BorgGroup == "" && len(rec.Data) > 0 {
		if _, err := tx.Exec(`INSERT INTO fragments_fts (shard, seq, text) VALUES (?, ?, ?)`, rec.Shard, rec.Seq, string(rec.Data)); err != nil {
			return FragmentRecord{}, false, fmt.Errorf("store: index fragment fts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return FragmentRecord{}, false, err
	}
	return rec, false, nil
}

// SearchFTS returns (shard, seq) hits ranked by SQLite FTS5 BM25 relevance.
// If shard is empty, searches across all non-system shards.
func (d *DB) SearchFTS(shard, query string, limit int) ([]struct {
	Shard string
	Seq   int64
}, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	ftsQ := buildFTSQuery(query)
	if ftsQ == "" {
		return nil, nil
	}
	var rows *sql.Rows
	var err error
	if shard != "" {
		rows, err = d.sql.Query(`
			SELECT shard, seq FROM fragments_fts
			WHERE shard = ? AND fragments_fts MATCH ?
			ORDER BY bm25(fragments_fts) ASC LIMIT ?`, shard, ftsQ, limit)
	} else {
		rows, err = d.sql.Query(`
			SELECT shard, seq FROM fragments_fts
			WHERE shard != '__receipts__' AND fragments_fts MATCH ?
			ORDER BY bm25(fragments_fts) ASC LIMIT ?`, ftsQ, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Shard string
		Seq   int64
	}
	for rows.Next() {
		var item struct {
			Shard string
			Seq   int64
		}
		if err := rows.Scan(&item.Shard, &item.Seq); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func buildFTSQuery(raw string) string {
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			parts = append(parts, `"`+string(cur)+`"*`)
		}
		cur = cur[:0]
	}
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r > 127 {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	if len(parts) == 0 {
		return ""
	}
	if len(parts) > 8 {
		parts = parts[:8]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " OR " + p
	}
	return out
}

// SupersededBy returns the newer seq that superseded (shard, seq), or 0 if still current.
func (d *DB) SupersededBy(shard string, seq int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var newer int64
	err := d.sql.QueryRow(`SELECT seq FROM fragments WHERE shard = ? AND supersedes = ? AND forgotten = 0 ORDER BY seq DESC LIMIT 1`, shard, seq).Scan(&newer)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return newer, err
}

func (d *DB) getByHDP(shard, hdp string) (FragmentRecord, error) {
	row := d.sql.QueryRow(`
		SELECT shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at
		FROM fragments WHERE shard = ? AND hdp = ?`, shard, hdp)
	return scanFragment(row)
}

// GetFragment fetches a single fragment by (shard, seq).
func (d *DB) GetFragment(shard string, seq int64) (FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.sql.QueryRow(`
		SELECT shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at
		FROM fragments WHERE shard = ? AND seq = ?`, shard, seq)
	return scanFragment(row)
}

// ListFragments lists all records in a shard ordered by seq.
func (d *DB) ListFragments(shard string) ([]FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at
		FROM fragments WHERE shard = ? ORDER BY seq ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRecentFragments returns the newest active (non-forgotten, non-system) fragments across user shards.
func (d *DB) ListRecentFragments(limit int) ([]FragmentRecord, error) {
	if limit <= 0 {
		limit = 30
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at
		FROM fragments
		WHERE shard != '__receipts__' AND forgotten = 0
		ORDER BY created_at DESC, seq DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUnanchored returns fragments in shard not yet committed to a batch anchor.
func (d *DB) ListUnanchored(shard string) ([]FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at
		FROM fragments WHERE shard = ? AND batch_n < 0 ORDER BY seq ASC`, shard)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListBatchFragments returns fragments belonging to (shard, batchN) in leaf_index order.
func (d *DB) ListBatchFragments(shard string, batchN int64) ([]FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at
		FROM fragments WHERE shard = ? AND batch_n = ? ORDER BY leaf_index ASC`, shard, batchN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentRecord
	for rows.Next() {
		r, err := scanFragment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListShards returns all distinct shard names.
func (d *DB) ListShards() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT DISTINCT shard FROM fragments ORDER BY shard ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ForgetFragment atomically shreds local plaintext, ciphertext, BRC-369 keyed envelope metadata,
// and its FTS5 search index entry while preserving the hash/Merkle commitment.
func (d *DB) ForgetFragment(shard string, seq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE fragments SET data = NULL, ciphertext = NULL, keyed_meta = NULL, forgotten = 1 WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fragments_fts WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return fmt.Errorf("store: shred fts: %w", err)
	}
	return tx.Commit()
}

func scanFragment(s scanner) (FragmentRecord, error) {
	var r FragmentRecord
	var forgotten, tainted int
	err := s.Scan(
		&r.Shard, &r.Seq, &r.HDP, &r.Leaf, &r.PrevLeaf, &r.ChainHash, &r.Data, &r.Ciphertext, &r.KeyedMeta, &r.Provenance,
		&r.Level, &r.Class, &r.BorgGroup, &r.BorgEpoch, &r.BatchN, &r.LeafIndex,
		&r.AnchorTxID, &r.Supersedes, &forgotten, &tainted, &r.CreatedAt,
	)
	r.Forgotten = forgotten != 0
	r.Tainted = tainted != 0
	return r, err
}
