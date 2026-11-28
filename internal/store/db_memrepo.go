package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MemCommitRow persists one signed content-addressed Git/Metanet commit node in SQLite.
type MemCommitRow struct {
	CommitHash     string
	Repo           string
	Branch         string
	OriginRepo     string
	OriginBranch   string
	OriginSeq      int64
	Seq            int64
	CommitKind     int
	TreeHash       string
	MPERoot        string
	ParentsJSON    []byte
	AuthorPub      string
	ModelID        string
	PolicyCommit   string
	OntologyCommit string
	Message        string
	Timestamp      int64
	Signature      string
	AnchorTxID     string
	AnchorN        int64
	FragmentLeaf   string
	GitMetaJSON    []byte
}

// MemObjectRow is one content-addressed blob or tree record for mem_objects.
type MemObjectRow struct {
	Hash      string
	Kind      string
	Body      []byte
	CreatedAt int64
}

// MemBranchRow tracks a branch head commit and on-chain NektarAnchor GenesisOwner per Memory Repo.
type MemBranchRow struct {
	Repo           string
	Branch         string
	HeadCommit     string
	GenesisPub     string
	GenesisShardID string
	UpdatedAt      int64
}

// MemTagRow tracks an immutable signed Git/UTXO checkpoint tag pinned to a commit.
type MemTagRow struct {
	Repo       string
	Tag        string
	CommitHash string
	AnchorTxID string
	AnchorN    int64
	Message    string
	AuthorPub  string
	Signature  string
	CreatedAt  int64
}

func (d *DB) migrateMemRepo() error {
	const schema = `
CREATE TABLE IF NOT EXISTS mem_objects (
	hash       TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	body_json  BLOB NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS mem_commits (
	commit_hash   TEXT PRIMARY KEY,
	repo          TEXT NOT NULL,
	branch        TEXT NOT NULL,
	seq           INTEGER NOT NULL DEFAULT 0,
	tree_hash     TEXT NOT NULL,
	mpe_root      TEXT NOT NULL,
	parents_json  BLOB NOT NULL,
	author_pub      TEXT NOT NULL,
	model_id        TEXT NOT NULL,
	policy_commit   TEXT NOT NULL DEFAULT '',
	ontology_commit TEXT NOT NULL DEFAULT '',
	message         TEXT NOT NULL DEFAULT '',
	timestamp       INTEGER NOT NULL,
	signature       TEXT NOT NULL DEFAULT '',
	anchor_txid     TEXT NOT NULL DEFAULT '',
	anchor_n        INTEGER NOT NULL DEFAULT -1,
	fragment_leaf   TEXT NOT NULL DEFAULT '',
	git_meta_json   BLOB,
	origin_repo     TEXT NOT NULL DEFAULT '',
	origin_branch   TEXT NOT NULL DEFAULT '',
	origin_seq      INTEGER NOT NULL DEFAULT 0,
	commit_kind     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_mem_commits_repo_branch ON mem_commits(repo, branch, timestamp DESC, seq DESC);
CREATE INDEX IF NOT EXISTS idx_mem_commits_repo_ts ON mem_commits(repo, timestamp DESC, seq DESC);
CREATE INDEX IF NOT EXISTS idx_mem_commits_repo_seq ON mem_commits(repo, seq);
CREATE INDEX IF NOT EXISTS idx_anchors_txid ON anchors(txid);
CREATE INDEX IF NOT EXISTS idx_fragments_forgotten_created ON fragments(forgotten, created_at DESC);

CREATE TABLE IF NOT EXISTS redacted_blobs (
	blob_hash        TEXT PRIMARY KEY,
	reason           TEXT NOT NULL DEFAULT '',
	tombstone_commit TEXT NOT NULL DEFAULT '',
	redacted_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS mem_branches (
	repo             TEXT NOT NULL,
	branch           TEXT NOT NULL,
	head_commit      TEXT NOT NULL DEFAULT '',
	genesis_pub      TEXT NOT NULL DEFAULT '',
	genesis_shard_id TEXT NOT NULL DEFAULT '',
	updated_at       INTEGER NOT NULL,
	PRIMARY KEY (repo, branch)
);

CREATE TABLE IF NOT EXISTS mem_tags (
	repo        TEXT NOT NULL,
	tag         TEXT NOT NULL,
	commit_hash TEXT NOT NULL,
	anchor_txid TEXT NOT NULL DEFAULT '',
	anchor_n    INTEGER NOT NULL DEFAULT -1,
	message     TEXT NOT NULL DEFAULT '',
	author_pub  TEXT NOT NULL DEFAULT '',
	signature   TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (repo, tag)
);
`
	_, err := d.sql.Exec(schema)
	if err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureColumnTx(tx, "mem_commits", "git_meta_json", "BLOB"); err != nil {
		return fmt.Errorf("store: migrate mem_commits.git_meta_json: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_commits", "origin_repo", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("store: migrate mem_commits.origin_repo: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_commits", "origin_branch", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("store: migrate mem_commits.origin_branch: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_commits", "origin_seq", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("store: migrate mem_commits.origin_seq: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_commits", "commit_kind", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("store: migrate mem_commits.commit_kind: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_commits", "ontology_commit", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("store: migrate mem_commits.ontology_commit: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_branches", "genesis_pub", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("store: migrate mem_branches.genesis_pub: %w", err)
	}
	if err := ensureColumnTx(tx, "mem_branches", "genesis_shard_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("store: migrate mem_branches.genesis_shard_id: %w", err)
	}
	return tx.Commit()
}

const memCommitSelectCols = `commit_hash, repo, branch, origin_repo, origin_branch, COALESCE(origin_seq, 0), seq, COALESCE(commit_kind, 0), tree_hash, mpe_root, parents_json,
		       author_pub, model_id, policy_commit, COALESCE(ontology_commit, ''), message, timestamp, signature,
		       anchor_txid, anchor_n, fragment_leaf, git_meta_json`

// PutMemObject stores an immutable content-addressed TreeObject or BlobObject record (CBOR or JSON).
func (d *DB) PutMemObject(hash, kind string, body []byte, createdAt int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`INSERT OR IGNORE INTO mem_objects (hash, kind, body_json, created_at) VALUES (?, ?, ?, ?)`, hash, kind, body, createdAt)
	return err
}

// GetMemObject retrieves a content-addressed object by hash.
func (d *DB) GetMemObject(hash string) (string, []byte, error) {
	rdb, release := d.rdb()
	defer release()
	var kind string
	var body []byte
	err := rdb.QueryRow(`SELECT kind, body_json FROM mem_objects WHERE hash = ?`, hash).Scan(&kind, &body)
	return kind, body, err
}

// MarkBlobRedacted records a content-addressed blob hash as cryptographically erased (tombstoned).
func (d *DB) MarkBlobRedacted(blobHash, reason, tombstoneCommit string, redactedAt int64) error {
	if blobHash == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO redacted_blobs (blob_hash, reason, tombstone_commit, redacted_at)
		VALUES (?, ?, ?, ?)`, blobHash, reason, tombstoneCommit, redactedAt)
	return err
}

// IsBlobRedacted reports whether blobHash has been cryptographically erased.
func (d *DB) IsBlobRedacted(blobHash string) (bool, error) {
	if blobHash == "" {
		return false, nil
	}
	rdb, release := d.rdb()
	defer release()
	var one int
	err := rdb.QueryRow(`SELECT 1 FROM redacted_blobs WHERE blob_hash = ?`, blobHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ListRedactedBlobs returns the set of all cryptographically erased blob hashes.
func (d *DB) ListRedactedBlobs() (map[string]bool, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`SELECT blob_hash FROM redacted_blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// SaveMemCommit inserts a commit row (idempotent on commit_hash).
func (d *DB) SaveMemCommit(r MemCommitRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.AnchorN == 0 && r.AnchorTxID == "" {
		r.AnchorN = -1
	}
	if len(r.ParentsJSON) == 0 {
		r.ParentsJSON = []byte("[]")
	}
	_, err := d.sql.Exec(`
		INSERT OR IGNORE INTO mem_commits (
			commit_hash, repo, branch, origin_repo, origin_branch, origin_seq, seq, commit_kind, tree_hash, mpe_root, parents_json,
			author_pub, model_id, policy_commit, ontology_commit, message, timestamp, signature,
			anchor_txid, anchor_n, fragment_leaf, git_meta_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.CommitHash, r.Repo, r.Branch, r.OriginRepo, r.OriginBranch, r.OriginSeq, r.Seq, r.CommitKind, r.TreeHash, r.MPERoot, r.ParentsJSON,
		r.AuthorPub, r.ModelID, r.PolicyCommit, r.OntologyCommit, r.Message, r.Timestamp, r.Signature,
		r.AnchorTxID, r.AnchorN, r.FragmentLeaf, r.GitMetaJSON,
	)
	return err
}

// GetMemCommit fetches a single commit by hash.
func (d *DB) GetMemCommit(commitHash string) (MemCommitRow, error) {
	rdb, release := d.rdb()
	defer release()
	row := rdb.QueryRow(`SELECT `+memCommitSelectCols+` FROM mem_commits WHERE commit_hash = ?`, commitHash)
	return scanMemCommit(row)
}

// ListMemCommits returns commits for repo (and optional branch) ordered newest-first.
func (d *DB) ListMemCommits(repo, branch string, limit int) ([]MemCommitRow, error) {
	rdb, release := d.rdb()
	defer release()
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if branch != "" {
		rows, err = rdb.Query(`
			SELECT `+memCommitSelectCols+`
			FROM mem_commits WHERE repo = ? AND branch = ?
			ORDER BY timestamp DESC, rowid DESC LIMIT ?`, repo, branch, limit)
	} else {
		rows, err = rdb.Query(`
			SELECT `+memCommitSelectCols+`
			FROM mem_commits WHERE repo = ?
			ORDER BY timestamp DESC, rowid DESC LIMIT ?`, repo, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemCommitRow
	for rows.Next() {
		r, err := scanMemCommit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAllMemCommits returns every commit for repo without a row limit (for full Git DAG projections).
func (d *DB) ListAllMemCommits(repo string) ([]MemCommitRow, error) {
	rdb, release := d.rdb()
	defer release()
	rows, err := rdb.Query(`
		SELECT `+memCommitSelectCols+`
		FROM mem_commits WHERE repo = ?
		ORDER BY timestamp ASC, rowid ASC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemCommitRow
	for rows.Next() {
		r, err := scanMemCommit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountMemCommits returns the exact commit count for repo.
func (d *DB) CountMemCommits(repo string) (int, error) {
	rdb, release := d.rdb()
	defer release()
	var n int
	err := rdb.QueryRow(`SELECT COUNT(*) FROM mem_commits WHERE repo = ?`, repo).Scan(&n)
	return n, err
}

// BindMemCommitsAnchor stamps anchored commits belonging to (repo, seqs) with their NektarAnchor txid & batch n.
func (d *DB) BindMemCommitsAnchor(repo string, seqs []int64, anchorN int64, txid string) error {
	if repo == "" || txid == "" || len(seqs) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, seq := range seqs {
		if _, err := tx.Exec(`UPDATE mem_commits SET anchor_txid = ?, anchor_n = ? WHERE repo = ? AND seq = ?`, txid, anchorN, repo, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMemCommitByPrefix resolves a short or full hex commit hash prefix (>= 6 hex chars) inside repo
// using a PK index range scan (never LIKE, avoiding wildcard injection and full table scans).
func (d *DB) GetMemCommitByPrefix(repo, prefix string) (MemCommitRow, error) {
	rdb, release := d.rdb()
	defer release()
	p := strings.ToLower(strings.TrimSpace(prefix))
	if len(p) < 6 || len(p) > 64 {
		return MemCommitRow{}, sql.ErrNoRows
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return MemCommitRow{}, sql.ErrNoRows
		}
	}
	upper := p + "g" // 'g' sorts strictly after all lowercase hex digits ('0'-'9','a'-'f')
	var rows *sql.Rows
	var err error
	if repo != "" {
		rows, err = rdb.Query(`
			SELECT `+memCommitSelectCols+`
			FROM mem_commits WHERE commit_hash >= ? AND commit_hash < ? AND repo = ? LIMIT 2`, p, upper, repo)
	} else {
		rows, err = rdb.Query(`
			SELECT `+memCommitSelectCols+`
			FROM mem_commits WHERE commit_hash >= ? AND commit_hash < ? LIMIT 2`, p, upper)
	}
	if err != nil {
		return MemCommitRow{}, err
	}
	defer rows.Close()
	var matches []MemCommitRow
	for rows.Next() {
		r, sErr := scanMemCommit(rows)
		if sErr != nil {
			return MemCommitRow{}, sErr
		}
		matches = append(matches, r)
	}
	if len(matches) == 0 {
		return MemCommitRow{}, sql.ErrNoRows
	}
	if len(matches) > 1 {
		return MemCommitRow{}, errors.New("store: ambiguous commit hash prefix")
	}
	return matches[0], nil
}

func scanMemCommit(s scanner) (MemCommitRow, error) {
	var r MemCommitRow
	err := s.Scan(
		&r.CommitHash, &r.Repo, &r.Branch, &r.OriginRepo, &r.OriginBranch, &r.OriginSeq, &r.Seq, &r.CommitKind, &r.TreeHash, &r.MPERoot, &r.ParentsJSON,
		&r.AuthorPub, &r.ModelID, &r.PolicyCommit, &r.OntologyCommit, &r.Message, &r.Timestamp, &r.Signature,
		&r.AnchorTxID, &r.AnchorN, &r.FragmentLeaf, &r.GitMetaJSON,
	)
	return r, err
}
