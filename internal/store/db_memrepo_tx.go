package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrBranchConflict is returned when a branch head moved concurrently (CAS mismatch)
// or when CreateMemBranchStrict targets a branch that already exists.
var ErrBranchConflict = errors.New("store: branch head conflict")

// RepoTx exposes atomic read/write helpers inside a single DB.RunRepoTx SQLite transaction.
type RepoTx struct {
	tx *sql.Tx
}

// RunRepoTx executes fn inside a single SQLite transaction while holding d.mu.
// Callers MUST use the provided RepoTx methods (never call back into DB methods inside fn).
func (d *DB) RunRepoTx(fn func(tx *RepoTx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&RepoTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (rt *RepoTx) AppendFragment(rec FragmentRecord) (FragmentRecord, bool, error) {
	return appendFragmentTx(rt.tx, rec)
}

func (rt *RepoTx) UpdateFragmentCommitHash(shard string, seq int64, commitHash string) error {
	_, err := rt.tx.Exec(`UPDATE fragments SET commit_hash = ? WHERE shard = ? AND seq = ?`, commitHash, shard, seq)
	return err
}

func (rt *RepoTx) PutMemObject(hash, kind string, body []byte, createdAt int64) error {
	_, err := rt.tx.Exec(`INSERT OR IGNORE INTO mem_objects (hash, kind, body_json, created_at) VALUES (?, ?, ?, ?)`, hash, kind, body, createdAt)
	return err
}

func (rt *RepoTx) GetMemObject(hash string) (string, []byte, error) {
	var kind string
	var body []byte
	err := rt.tx.QueryRow(`SELECT kind, body_json FROM mem_objects WHERE hash = ?`, hash).Scan(&kind, &body)
	return kind, body, err
}

func (rt *RepoTx) SaveMemCommit(r MemCommitRow) error {
	if r.AnchorN == 0 && r.AnchorTxID == "" {
		r.AnchorN = -1
	}
	if len(r.ParentsJSON) == 0 {
		r.ParentsJSON = []byte("[]")
	}
	_, err := rt.tx.Exec(`
		INSERT OR IGNORE INTO mem_commits (
			commit_hash, repo, branch, origin_repo, origin_branch, seq, tree_hash, mpe_root, parents_json,
			author_pub, model_id, policy_commit, ontology_commit, message, timestamp, signature,
			anchor_txid, anchor_n, fragment_leaf, git_meta_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.CommitHash, r.Repo, r.Branch, r.OriginRepo, r.OriginBranch, r.Seq, r.TreeHash, r.MPERoot, r.ParentsJSON,
		r.AuthorPub, r.ModelID, r.PolicyCommit, r.OntologyCommit, r.Message, r.Timestamp, r.Signature,
		r.AnchorTxID, r.AnchorN, r.FragmentLeaf, r.GitMetaJSON,
	)
	return err
}

func (rt *RepoTx) GetMemCommit(commitHash string) (MemCommitRow, error) {
	return scanMemCommit(rt.tx.QueryRow(`SELECT `+memCommitSelectCols+` FROM mem_commits WHERE commit_hash = ?`, commitHash))
}

func (rt *RepoTx) GetMemBranch(repo, branch string) (MemBranchRow, error) {
	var r MemBranchRow
	err := rt.tx.QueryRow(`
		SELECT repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at
		FROM mem_branches WHERE repo = ? AND branch = ?`, repo, branch).
		Scan(&r.Repo, &r.Branch, &r.HeadCommit, &r.GenesisPub, &r.GenesisShardID, &r.UpdatedAt)
	return r, err
}

func (rt *RepoTx) UpsertMemBranch(r MemBranchRow) error {
	var exPub, exID, exHead string
	err := rt.tx.QueryRow(`SELECT head_commit, genesis_pub, genesis_shard_id FROM mem_branches WHERE repo = ? AND branch = ?`, r.Repo, r.Branch).
		Scan(&exHead, &exPub, &exID)
	if err == nil {
		if r.HeadCommit == "" {
			r.HeadCommit = exHead
		}
		if r.GenesisPub == "" {
			r.GenesisPub = exPub
		}
		if r.GenesisShardID == "" {
			r.GenesisShardID = exID
		}
		_, err = rt.tx.Exec(`
			UPDATE mem_branches SET head_commit = ?, genesis_pub = ?, genesis_shard_id = ?, updated_at = ?
			WHERE repo = ? AND branch = ?`,
			r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt, r.Repo, r.Branch,
		)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = rt.tx.Exec(`
		INSERT INTO mem_branches (repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Branch, r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt,
	)
	return err
}

// CompareAndSwapMemBranch atomically advances (repo, branch) only if its current head equals expectedHead.
func (rt *RepoTx) CompareAndSwapMemBranch(r MemBranchRow, expectedHead string) error {
	var exPub, exID, exHead string
	err := rt.tx.QueryRow(`SELECT head_commit, genesis_pub, genesis_shard_id FROM mem_branches WHERE repo = ? AND branch = ?`, r.Repo, r.Branch).
		Scan(&exHead, &exPub, &exID)
	if err == nil {
		if exHead != expectedHead {
			return fmt.Errorf("%w: %s/%s expected head %q got %q", ErrBranchConflict, r.Repo, r.Branch, expectedHead, exHead)
		}
		if r.HeadCommit == "" {
			r.HeadCommit = exHead
		}
		if r.GenesisPub == "" {
			r.GenesisPub = exPub
		}
		if r.GenesisShardID == "" {
			r.GenesisShardID = exID
		}
		res, uErr := rt.tx.Exec(`
			UPDATE mem_branches SET head_commit = ?, genesis_pub = ?, genesis_shard_id = ?, updated_at = ?
			WHERE repo = ? AND branch = ? AND head_commit = ?`,
			r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt, r.Repo, r.Branch, expectedHead,
		)
		if uErr != nil {
			return uErr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s/%s head changed concurrently", ErrBranchConflict, r.Repo, r.Branch)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if expectedHead != "" {
		return fmt.Errorf("%w: %s/%s missing expected head %q", ErrBranchConflict, r.Repo, r.Branch, expectedHead)
	}
	_, err = rt.tx.Exec(`
		INSERT INTO mem_branches (repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Branch, r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt,
	)
	return err
}

func (rt *RepoTx) SaveBatchAnchorStrict(anchor BatchAnchor) error {
	var existingID, existingRoot, existingTx string
	var existingSettled, existingBeefLen int
	err := rt.tx.QueryRow(`SELECT shard_id, root_hex, txid, settled, length(beef) FROM anchors WHERE shard = ? AND n = ?`, anchor.Shard, anchor.N).
		Scan(&existingID, &existingRoot, &existingTx, &existingSettled, &existingBeefLen)
	if err == nil {
		if existingID != anchor.ShardID || existingRoot != anchor.RootHex || existingTx != anchor.TxID {
			return fmt.Errorf("store: conflicting local anchor already exists for %s#%d", anchor.Shard, anchor.N)
		}
		if len(anchor.Beef) > 0 && ((existingSettled == 0 && anchor.Settled) || existingBeefLen == 0) {
			_, uErr := rt.tx.Exec(
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
	_, err = rt.tx.Exec(`
		INSERT INTO anchors (shard, shard_id, n, size, root_hex, chained_hex, state_hash, mmr_root, txid, beef, settled, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		anchor.Shard, anchor.ShardID, anchor.N, anchor.Size, anchor.RootHex, anchor.ChainedHex,
		anchor.StateHash, anchor.MmrRoot, anchor.TxID, anchor.Beef, boolToInt(anchor.Settled), anchor.BlockHeight, anchor.CreatedAt,
	)
	return err
}

// UpsertMemBranch saves or updates a branch ref (preserving existing GenesisPub/GenesisShardID if omitted).
func (d *DB) UpsertMemBranch(r MemBranchRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var exPub, exID, exHead string
	err := d.sql.QueryRow(`SELECT head_commit, genesis_pub, genesis_shard_id FROM mem_branches WHERE repo = ? AND branch = ?`, r.Repo, r.Branch).
		Scan(&exHead, &exPub, &exID)
	if err == nil {
		if r.HeadCommit == "" {
			r.HeadCommit = exHead
		}
		if r.GenesisPub == "" {
			r.GenesisPub = exPub
		}
		if r.GenesisShardID == "" {
			r.GenesisShardID = exID
		}
		_, err = d.sql.Exec(`
			UPDATE mem_branches SET head_commit = ?, genesis_pub = ?, genesis_shard_id = ?, updated_at = ?
			WHERE repo = ? AND branch = ?`,
			r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt, r.Repo, r.Branch,
		)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = d.sql.Exec(`
		INSERT INTO mem_branches (repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Branch, r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt,
	)
	return err
}

// CreateMemBranchStrict inserts a brand-new branch and returns ErrBranchConflict if it already exists.
func (d *DB) CreateMemBranchStrict(r MemBranchRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var exists int
	if err := d.sql.QueryRow(`SELECT 1 FROM mem_branches WHERE repo = ? AND branch = ?`, r.Repo, r.Branch).Scan(&exists); err == nil {
		return fmt.Errorf("%w: branch %s/%s already exists", ErrBranchConflict, r.Repo, r.Branch)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err := d.sql.Exec(`
		INSERT INTO mem_branches (repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Branch, r.HeadCommit, r.GenesisPub, r.GenesisShardID, r.UpdatedAt,
	)
	return err
}

// GetMemBranch returns the branch record for (repo, branch).
func (d *DB) GetMemBranch(repo, branch string) (MemBranchRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r MemBranchRow
	err := d.sql.QueryRow(`
		SELECT repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at
		FROM mem_branches WHERE repo = ? AND branch = ?`, repo, branch).
		Scan(&r.Repo, &r.Branch, &r.HeadCommit, &r.GenesisPub, &r.GenesisShardID, &r.UpdatedAt)
	return r, err
}

// ListMemBranches returns all branches of repo ordered with main first.
func (d *DB) ListMemBranches(repo string) ([]MemBranchRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT repo, branch, head_commit, genesis_pub, genesis_shard_id, updated_at
		FROM mem_branches WHERE repo = ?
		ORDER BY CASE WHEN branch = 'main' THEN 0 ELSE 1 END, branch ASC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemBranchRow
	for rows.Next() {
		var r MemBranchRow
		if err := rows.Scan(&r.Repo, &r.Branch, &r.HeadCommit, &r.GenesisPub, &r.GenesisShardID, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAllMemRepos returns the union of all repo names from fragments and mem_branches.
func (d *DB) ListAllMemRepos() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT DISTINCT name FROM (
			SELECT shard AS name FROM fragments
			UNION
			SELECT repo AS name FROM mem_branches
		) ORDER BY name ASC`)
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

// SaveMemTag inserts an immutable checkpoint tag (fails if tag already exists in repo).
func (d *DB) SaveMemTag(t MemTagRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t.AnchorN == 0 && t.AnchorTxID == "" {
		t.AnchorN = -1
	}
	_, err := d.sql.Exec(`
		INSERT INTO mem_tags (repo, tag, commit_hash, anchor_txid, anchor_n, message, author_pub, signature, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Repo, t.Tag, t.CommitHash, t.AnchorTxID, t.AnchorN, t.Message, t.AuthorPub, t.Signature, t.CreatedAt,
	)
	return err
}

// GetMemTag fetches one tag record by (repo, tag).
func (d *DB) GetMemTag(repo, tag string) (MemTagRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r MemTagRow
	err := d.sql.QueryRow(`
		SELECT repo, tag, commit_hash, anchor_txid, anchor_n, message, author_pub, signature, created_at
		FROM mem_tags WHERE repo = ? AND tag = ?`, repo, tag).
		Scan(&r.Repo, &r.Tag, &r.CommitHash, &r.AnchorTxID, &r.AnchorN, &r.Message, &r.AuthorPub, &r.Signature, &r.CreatedAt)
	return r, err
}

// ListMemTags returns all tags in repo ordered newest first.
func (d *DB) ListMemTags(repo string) ([]MemTagRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`
		SELECT repo, tag, commit_hash, anchor_txid, anchor_n, message, author_pub, signature, created_at
		FROM mem_tags WHERE repo = ? ORDER BY created_at DESC, tag ASC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemTagRow
	for rows.Next() {
		var r MemTagRow
		if err := rows.Scan(&r.Repo, &r.Tag, &r.CommitHash, &r.AnchorTxID, &r.AnchorN, &r.Message, &r.AuthorPub, &r.Signature, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
