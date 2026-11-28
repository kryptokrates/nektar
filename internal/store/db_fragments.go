package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lightwebinc/bcommon/cbor"
)

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

type blobPayloadLite struct {
	Inline    []byte
	KeyedMeta []byte
	Forgotten bool
	Redacted  bool
}

func decodeBlobPayloadLite(raw []byte) (blobPayloadLite, bool) {
	if len(raw) == 0 {
		return blobPayloadLite{}, false
	}
	if raw[0] == '{' {
		var j struct {
			Inline    []byte `json:"inline"`
			KeyedMeta []byte `json:"keyedMeta"`
			Forgotten bool   `json:"forgotten"`
			Redacted  bool   `json:"redacted"`
		}
		if json.Unmarshal(raw, &j) != nil {
			return blobPayloadLite{}, false
		}
		return blobPayloadLite{
			Inline:    j.Inline,
			KeyedMeta: j.KeyedMeta,
			Forgotten: j.Forgotten,
			Redacted:  j.Redacted,
		}, true
	}
	v, err := cbor.DecodeValue(raw)
	if err != nil {
		return blobPayloadLite{}, false
	}
	m, ok := v.(cbor.Map)
	if !ok {
		return blobPayloadLite{}, false
	}
	var out blobPayloadLite
	for _, p := range m {
		k, ok := p.Key.(uint64)
		if !ok {
			continue
		}
		switch k {
		case 14:
			if b, ok := p.Val.([]byte); ok {
				out.Inline = b
			}
		case 15:
			if b, ok := p.Val.([]byte); ok {
				out.KeyedMeta = b
			}
		case 16:
			if b, ok := p.Val.(bool); ok {
				out.Redacted = b
			}
		case 17:
			if b, ok := p.Val.(bool); ok {
				out.Forgotten = b
			}
		}
	}
	return out, true
}

func hydrateFragment(q rowQuerier, r *FragmentRecord) {
	if r == nil || r.Forgotten || r.BlobHash == "" || len(r.Data) > 0 || len(r.KeyedMeta) > 0 {
		return
	}
	var raw []byte
	if err := q.QueryRow(`SELECT body_json FROM mem_objects WHERE hash = ? AND kind = 'blob'`, r.BlobHash).Scan(&raw); err != nil {
		return
	}
	bp, ok := decodeBlobPayloadLite(raw)
	if !ok || bp.Forgotten || bp.Redacted {
		return
	}
	if r.BorgGroup == "" && r.LatticeEpoch == 0 && len(bp.Inline) > 0 {
		r.Data = bp.Inline
	}
	if len(bp.KeyedMeta) > 0 {
		r.KeyedMeta = bp.KeyedMeta
		if len(r.Ciphertext) == 0 {
			var env struct {
				Ciphertext string `json:"ciphertext"`
			}
			if json.Unmarshal(bp.KeyedMeta, &env) == nil && env.Ciphertext != "" {
				if ct, err := hex.DecodeString(env.Ciphertext); err == nil {
					r.Ciphertext = ct
				}
			}
		}
	}
}

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

// appendFragmentTx inserts a deduplicated append-only fragment inside an existing SQLite tx,
// always computing prev_leaf and chain_hash so shard integrity holds for every shard (including __receipts__).
func appendFragmentTx(tx *sql.Tx, rec FragmentRecord) (FragmentRecord, bool, error) {
	existing, err := scanFragment(tx.QueryRow(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND hdp = ?`, rec.Shard, rec.HDP))
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return FragmentRecord{}, false, err
	}

	var nextSeq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM fragments WHERE shard = ?`, rec.Shard).Scan(&nextSeq); err != nil {
		return FragmentRecord{}, false, err
	}
	rec.Seq = nextSeq
	var prevLeaf, prevChain string
	if nextSeq > 1 {
		if err := tx.QueryRow(`SELECT leaf, COALESCE(chain_hash, '') FROM fragments WHERE shard = ? AND seq = ?`, rec.Shard, nextSeq-1).Scan(&prevLeaf, &prevChain); err != nil {
			return FragmentRecord{}, false, fmt.Errorf("store: read predecessor %s#%d: %w", rec.Shard, nextSeq-1, err)
		}
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
	// Single-source payload storage: avoid duplicating inline plaintext / keyedMeta / ciphertext
	// in fragments when mem_objects already holds the identical content-addressed BlobObject.
	storeData := rec.Data
	storeCiphertext := rec.Ciphertext
	storeKeyedMeta := rec.KeyedMeta
	if rec.BlobHash != "" {
		var blobRaw []byte
		if qErr := tx.QueryRow(`SELECT body_json FROM mem_objects WHERE hash = ? AND kind = 'blob'`, rec.BlobHash).Scan(&blobRaw); qErr == nil {
			if bp, ok := decodeBlobPayloadLite(blobRaw); ok && !bp.Forgotten && !bp.Redacted {
				if rec.BorgGroup == "" && rec.LatticeEpoch == 0 && len(storeData) > 0 && bytes.Equal(bp.Inline, storeData) {
					storeData = nil
				}
				if len(storeKeyedMeta) > 0 && bytes.Equal(bp.KeyedMeta, storeKeyedMeta) {
					storeKeyedMeta = nil
					// Ciphertext is deterministically recoverable from KeyedMeta JSON "ciphertext" hex.
					var env struct {
						Ciphertext string `json:"ciphertext"`
					}
					if json.Unmarshal(bp.KeyedMeta, &env) == nil && env.Ciphertext != "" {
						if ct, cErr := hex.DecodeString(env.Ciphertext); cErr == nil && bytes.Equal(ct, storeCiphertext) {
							storeCiphertext = nil
						}
					}
				}
			}
		}
	}
	_, err = tx.Exec(`
		INSERT INTO fragments (
			shard, seq, hdp, leaf, prev_leaf, chain_hash, data, ciphertext, keyed_meta, provenance,
			level, class, borg_group, borg_epoch, lattice_epoch, public_flag, batch_n, leaf_index,
			anchor_txid, supersedes, forgotten, tainted, created_at,
			path, media_type, chirp_url, content_sha256, mpe_root, commit_hash, transition_hash, blob_hash,
			distilled, ontology_id, triples_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Shard, rec.Seq, rec.HDP, rec.Leaf, rec.PrevLeaf, rec.ChainHash, storeData, storeCiphertext, storeKeyedMeta, rec.Provenance,
		rec.Level, rec.Class, rec.BorgGroup, rec.BorgEpoch, rec.LatticeEpoch, boolToInt(rec.Public), rec.BatchN, rec.LeafIndex,
		rec.AnchorTxID, rec.Supersedes, boolToInt(rec.Forgotten), boolToInt(rec.Tainted), rec.CreatedAt,
		rec.Path, rec.MediaType, rec.ChirpURL, rec.ContentSHA256, rec.MPERoot, rec.CommitHash, rec.TransitionHash, rec.BlobHash,
		boolToInt(rec.Distilled), rec.OntologyID, rec.TriplesJSON,
	)
	if err != nil {
		return FragmentRecord{}, false, err
	}
	if !rec.Forgotten && rec.BorgGroup == "" && rec.LatticeEpoch == 0 && len(rec.Data) > 0 {
		if _, err := tx.Exec(`INSERT INTO fragments_fts (shard, seq, text) VALUES (?, ?, ?)`, rec.Shard, rec.Seq, string(rec.Data)); err != nil {
			return FragmentRecord{}, false, fmt.Errorf("store: index fragment fts: %w", err)
		}
	}
	return rec, false, nil
}

// AppendFragment inserts a deduplicated append-only fragment into shard.
// If a fragment with the same (shard, hdp) already exists, it returns the existing record and dedup=true.
func (d *DB) AppendFragment(rec FragmentRecord) (FragmentRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.sql.Begin()
	if err != nil {
		return FragmentRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	out, dedup, err := appendFragmentTx(tx, rec)
	if err != nil {
		return FragmentRecord{}, false, err
	}
	if dedup {
		return out, true, nil
	}
	if err := tx.Commit(); err != nil {
		return FragmentRecord{}, false, err
	}
	return out, false, nil
}

// SearchFTS returns (shard, seq) hits ranked by SQLite FTS5 BM25 relevance.
// If shard is empty, searches across all non-system shards.
func (d *DB) SearchFTS(shard, query string, limit int) ([]struct {
	Shard string
	Seq   int64
}, error) {
	rdb, release := d.rdb()
	defer release()
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
		rows, err = rdb.Query(`
			SELECT shard, seq FROM fragments_fts
			WHERE shard = ? AND fragments_fts MATCH ?
			ORDER BY bm25(fragments_fts) ASC LIMIT ?`, shard, ftsQ, limit)
	} else {
		rows, err = rdb.Query(`
			SELECT shard, seq FROM fragments_fts
			WHERE shard NOT IN ('__receipts__', '__deals__', '__policies__', '__ontology__') AND fragments_fts MATCH ?
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
	rdb, release := d.rdb()
	defer release()
	var newer int64
	err := rdb.QueryRow(`SELECT seq FROM fragments WHERE shard = ? AND supersedes = ? AND forgotten = 0 ORDER BY seq DESC LIMIT 1`, shard, seq).Scan(&newer)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return newer, err
}

const fragmentSelectCols = `shard, seq, hdp, leaf, COALESCE(prev_leaf, ''), COALESCE(chain_hash, ''), data, ciphertext, keyed_meta, provenance,
		       level, class, borg_group, borg_epoch, COALESCE(lattice_epoch, 0), COALESCE(public_flag, 0), batch_n, leaf_index,
		       anchor_txid, supersedes, forgotten, tainted, created_at,
		       COALESCE(path, ''), COALESCE(media_type, ''), COALESCE(chirp_url, ''),
		       COALESCE(content_sha256, ''), COALESCE(mpe_root, ''), COALESCE(commit_hash, ''), COALESCE(transition_hash, ''), COALESCE(blob_hash, ''),
		       COALESCE(distilled, 0), COALESCE(ontology_id, ''), triples_json`

func (d *DB) getByHDP(shard, hdp string) (FragmentRecord, error) {
	rdb, release := d.rdb()
	defer release()
	row := rdb.QueryRow(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND hdp = ?`, shard, hdp)
	r, err := scanFragment(row)
	if err != nil {
		return r, err
	}
	hydrateFragment(rdb, &r)
	return r, nil
}

// GetFragment fetches a single fragment by (shard, seq).
func (d *DB) GetFragment(shard string, seq int64) (FragmentRecord, error) {
	rdb, release := d.rdb()
	defer release()
	row := rdb.QueryRow(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND seq = ?`, shard, seq)
	r, err := scanFragment(row)
	if err != nil {
		return r, err
	}
	hydrateFragment(rdb, &r)
	return r, nil
}

// ListFragments lists all records in a shard ordered by seq.
func (d *DB) ListFragments(shard string) ([]FragmentRecord, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? ORDER BY seq ASC`, shard)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		hydrateFragment(rdb, &out[i])
	}
	return out, nil
}

// ListFragmentsSince returns non-forgotten fragments in shard with seq > afterSeq ordered by seq ASC.
func (d *DB) ListFragmentsSince(shard string, afterSeq int64, limit int) ([]FragmentRecord, error) {
	rdb, release := d.rdb()
	defer release()
	if limit <= 0 {
		limit = 256
	}
	rows, err := rdb.Query(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND seq > ? AND forgotten = 0 ORDER BY seq ASC LIMIT ?`, shard, afterSeq, limit)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		hydrateFragment(rdb, &out[i])
	}
	return out, nil
}

// ListRecentFragments returns the newest active (non-forgotten, non-system) fragments across user shards.
func (d *DB) ListRecentFragments(limit int) ([]FragmentRecord, error) {
	if limit <= 0 {
		limit = 30
	}
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT `+fragmentSelectCols+` FROM fragments
		WHERE shard NOT IN ('__receipts__', '__deals__', '__policies__', '__ontology__') AND forgotten = 0
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		hydrateFragment(rdb, &out[i])
	}
	return out, nil
}

// ListUnanchored returns fragments in shard not yet committed to a batch anchor.
func (d *DB) ListUnanchored(shard string) ([]FragmentRecord, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND batch_n < 0 ORDER BY seq ASC`, shard)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		hydrateFragment(rdb, &out[i])
	}
	return out, nil
}

// ListBatchFragments returns fragments belonging to (shard, batchN) in leaf_index order.
func (d *DB) ListBatchFragments(shard string, batchN int64) ([]FragmentRecord, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND batch_n = ? ORDER BY leaf_index ASC`, shard, batchN)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		hydrateFragment(rdb, &out[i])
	}
	return out, nil
}

// ListBatchLeafHashes returns ONLY the 32-byte hex leaf hashes in (shard, batchN) in leaf_index order,
// avoiding multi-MB data/ciphertext/keyed_meta/provenance BLOB reads during Merkle proof construction.
func (d *DB) ListBatchLeafHashes(shard string, batchN int64) ([]string, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT leaf FROM fragments WHERE shard = ? AND batch_n = ? ORDER BY leaf_index ASC`, shard, batchN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateFragmentCommitHash binds a persisted fragment row to its canonical Git/Metanet CommitHash.
func (d *DB) UpdateFragmentCommitHash(shard string, seq int64, commitHash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`UPDATE fragments SET commit_hash = ? WHERE shard = ? AND seq = ?`, commitHash, shard, seq)
	return err
}

// ListShards returns all distinct shard names.
func (d *DB) ListShards() ([]string, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT DISTINCT shard FROM fragments ORDER BY shard ASC`)
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
// its FTS5 search index entry, and any matching content-addressed mem_objects blob payload
// while preserving the hash/Merkle commitment.
func (d *DB) ForgetFragment(shard string, seq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var commitHash, path, contentSHA, blobHash string
	qErr := tx.QueryRow(`SELECT COALESCE(commit_hash, ''), COALESCE(path, ''), COALESCE(content_sha256, ''), COALESCE(blob_hash, '') FROM fragments WHERE shard = ? AND seq = ?`, shard, seq).
		Scan(&commitHash, &path, &contentSHA, &blobHash)
	if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
		return qErr
	}

	if _, err := tx.Exec(`UPDATE fragments SET data = NULL, ciphertext = NULL, keyed_meta = NULL, chirp_url = '', forgotten = 1 WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fragments_fts WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return fmt.Errorf("store: shred fts: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM knowledge_edges WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return fmt.Errorf("store: shred knowledge_edges: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM fragment_graph_meta WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return fmt.Errorf("store: shred fragment_graph_meta: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM fragment_nmu_meta WHERE shard = ? AND seq = ?`, shard, seq); err != nil {
		return fmt.Errorf("store: shred fragment_nmu_meta: %w", err)
	}
	// Strictly append-only: NEVER mutate committed mem_commits.message (which would break CommitHash
	// and author ECDSA signatures). Shred only blob payloads into hash-preserving BlobStubs.
	if err := shredMemObjectsForFragmentTx(tx, shard, seq, commitHash, path, contentSHA, blobHash); err != nil {
		return fmt.Errorf("store: shred mem_objects: %w", err)
	}
	return tx.Commit()
}

// CountShardFragments returns the total fragment count for shard without loading data/ciphertext blobs.
func (d *DB) CountShardFragments(shard string) (int, error) {
	rdb, release := d.rdb()
	defer release()
	var n int
	err := rdb.QueryRow(`SELECT COUNT(*) FROM fragments WHERE shard = ?`, shard).Scan(&n)
	return n, err
}

// shredMemObjectsForFragmentTx strips inline/keyedMeta/chirpUrl payloads from any mem_objects
// blob row referenced by the forgotten fragment's commit tree (both legacy flat trees and 256-bucket
// radix subtrees, or matching its contentSha256), replacing it with a forgotten tombstone so
// RepoTree/Git/ExportCapsule cannot leak forgotten data.
func shredMemObjectsForFragmentTx(tx *sql.Tx, shard string, seq int64, commitHash, fragPath, contentSHA, directBlobHash string) error {
	blobHashes := make(map[string]bool)
	if directBlobHash != "" {
		blobHashes[directBlobHash] = true
	}
	now := time.Now().Unix()
	for bh := range blobHashes {
		var raw []byte
		if err := tx.QueryRow(`SELECT body_json FROM mem_objects WHERE hash = ? AND kind = 'blob'`, bh).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		shredded, err := redactBlobRawPreserveHash(raw)
		if err != nil {
			continue
		}
		if _, err := tx.Exec(`UPDATE mem_objects SET body_json = ? WHERE hash = ?`, shredded, bh); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO redacted_blobs (blob_hash, reason, tombstone_commit, redacted_at) VALUES (?, 'forgotten', '', ?)`, bh, now); err != nil {
			return err
		}
	}
	if len(blobHashes) > 0 {
		return nil
	}
	if commitHash == "" {
		var ch string
		if err := tx.QueryRow(`SELECT commit_hash FROM mem_commits WHERE repo = ? AND seq = ? LIMIT 1`, shard, seq).Scan(&ch); err == nil {
			commitHash = ch
		}
	}
	if commitHash != "" {
		var treeHash string
		if err := tx.QueryRow(`SELECT tree_hash FROM mem_commits WHERE commit_hash = ?`, commitHash).Scan(&treeHash); err == nil && treeHash != "" {
			type treeEntryLite struct {
				Path     string `json:"path"`
				BlobHash string `json:"blobHash"`
			}
			type treeLite struct {
				Entries  []treeEntryLite   `json:"entries"`
				Subtrees map[string]string `json:"subtrees"`
			}
			var allEntries []treeEntryLite
			var treeRaw []byte
			if err := tx.QueryRow(`SELECT body_json FROM mem_objects WHERE hash = ? AND kind = 'tree'`, treeHash).Scan(&treeRaw); err == nil {
				var tree treeLite
				if json.Unmarshal(treeRaw, &tree) == nil {
					allEntries = append(allEntries, tree.Entries...)
					for _, subHash := range tree.Subtrees {
						if subHash == "" {
							continue
						}
						var subRaw []byte
						if tx.QueryRow(`SELECT body_json FROM mem_objects WHERE hash = ? AND kind = 'tree'`, subHash).Scan(&subRaw) == nil {
							var subTree treeLite
							if json.Unmarshal(subRaw, &subTree) == nil {
								allEntries = append(allEntries, subTree.Entries...)
							}
						}
					}
				}
			}
			if len(allEntries) > 0 {
				defaultPathMD := fmt.Sprintf("facts/%04d.md", seq)
				defaultPathTxt := fmt.Sprintf("facts/%d.txt", seq)
				sidecarFor := func(p string) string {
					if p == "" {
						return ""
					}
					return "graph/" + p + ".edges.json"
				}
				sidecarFrag := sidecarFor(fragPath)
				sidecarMD := sidecarFor(defaultPathMD)
				sidecarTxt := sidecarFor(defaultPathTxt)
				for _, e := range allEntries {
					if e.BlobHash == "" {
						continue
					}
					if (fragPath != "" && e.Path == fragPath) || e.Path == defaultPathMD || e.Path == defaultPathTxt ||
						(sidecarFrag != "" && e.Path == sidecarFrag) || e.Path == sidecarMD || e.Path == sidecarTxt {
						blobHashes[e.BlobHash] = true
					}
				}
			}
		}
	}
	for bh := range blobHashes {
		var raw []byte
		if err := tx.QueryRow(`SELECT body_json FROM mem_objects WHERE hash = ? AND kind = 'blob'`, bh).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		shredded, err := redactBlobRawPreserveHash(raw)
		if err != nil {
			continue
		}
		if _, err := tx.Exec(`UPDATE mem_objects SET body_json = ? WHERE hash = ?`, shredded, bh); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO redacted_blobs (blob_hash, reason, tombstone_commit, redacted_at) VALUES (?, 'forgotten', '', ?)`, bh, now); err != nil {
			return err
		}
	}
	return nil
}

// redactBlobRawPreserveHash strips Inline and KeyedMeta payloads from a stored CBOR or JSON BlobObject
// while preserving KeyedMetaSHA256, ChirpURL, and every field committed into BlobHash!
func redactBlobRawPreserveHash(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty blob")
	}
	if raw[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		delete(obj, "inline")
		delete(obj, "keyedMeta")
		delete(obj, "chirpUrl")
		obj["redacted"] = true
		obj["forgotten"] = true
		return json.Marshal(obj)
	}
	v, err := cbor.DecodeValue(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(cbor.Map)
	if !ok {
		return nil, errors.New("not a cbor map")
	}
	var kmSHA string
	var kmBytes []byte
	for _, p := range m {
		if k, ok := p.Key.(uint64); ok {
			if k == 10 {
				kmSHA, _ = p.Val.(string)
			}
			if k == 15 {
				kmBytes, _ = p.Val.([]byte)
			}
		}
	}
	if kmSHA == "" && len(kmBytes) > 0 {
		sum := sha256.Sum256(kmBytes)
		kmSHA = hex.EncodeToString(sum[:])
	}
	out := make(cbor.Map, 0, len(m)+2)
	hasKMSHA := false
	for _, p := range m {
		k, ok := p.Key.(uint64)
		if !ok {
			out = append(out, p)
			continue
		}
		if k == 14 || k == 15 || k == 16 || k == 17 {
			continue
		}
		if k == 10 {
			hasKMSHA = true
		}
		out = append(out, p)
	}
	if !hasKMSHA && kmSHA != "" {
		out = append(out, cbor.Pair{Key: uint64(10), Val: kmSHA})
	}
	out = append(out, cbor.Pair{Key: uint64(16), Val: true})
	out = append(out, cbor.Pair{Key: uint64(17), Val: true})
	return cbor.Encode(out)
}

func scanFragment(s scanner) (FragmentRecord, error) {
	var r FragmentRecord
	var forgotten, tainted, pubFlag, distilled int
	err := s.Scan(
		&r.Shard, &r.Seq, &r.HDP, &r.Leaf, &r.PrevLeaf, &r.ChainHash, &r.Data, &r.Ciphertext, &r.KeyedMeta, &r.Provenance,
		&r.Level, &r.Class, &r.BorgGroup, &r.BorgEpoch, &r.LatticeEpoch, &pubFlag, &r.BatchN, &r.LeafIndex,
		&r.AnchorTxID, &r.Supersedes, &forgotten, &tainted, &r.CreatedAt,
		&r.Path, &r.MediaType, &r.ChirpURL, &r.ContentSHA256, &r.MPERoot, &r.CommitHash, &r.TransitionHash, &r.BlobHash,
		&distilled, &r.OntologyID, &r.TriplesJSON,
	)
	r.Public = pubFlag != 0
	r.Forgotten = forgotten != 0
	r.Tainted = tainted != 0
	r.Distilled = distilled != 0
	return r, err
}
