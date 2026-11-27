package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kryptokrates/nektar/internal/lattice"
)

// TransitionRow stores one canonical CBOR Merkle Automaton state transition (q_i, sigma, q_j, t, h_i).
type TransitionRow struct {
	Shard        string `json:"shard"`
	Seq          int64  `json:"seq"`
	Hash         string `json:"hash"`
	PrevQ        string `json:"prevQ"`
	NextQ        string `json:"nextQ"`
	CommitHash   string `json:"commitHash,omitempty"`
	TreeHash     string `json:"treeHash,omitempty"`
	PolicyHash   string `json:"policyHash,omitempty"`
	OntologyHash string `json:"ontologyHash,omitempty"`
	BodyCBOR     []byte `json:"bodyCbor,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

func (d *DB) migrateEpistemic() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS transitions (
	shard         TEXT NOT NULL,
	seq           INTEGER NOT NULL,
	hash          TEXT NOT NULL,
	prev_q        TEXT NOT NULL,
	next_q        TEXT NOT NULL,
	commit_hash   TEXT NOT NULL DEFAULT '',
	tree_hash     TEXT NOT NULL DEFAULT '',
	policy_hash   TEXT NOT NULL DEFAULT '',
	ontology_hash TEXT NOT NULL DEFAULT '',
	body_cbor     BLOB NOT NULL,
	created_at    INTEGER NOT NULL,
	PRIMARY KEY (shard, seq)
);
CREATE INDEX IF NOT EXISTS idx_transitions_hash ON transitions(hash);

CREATE TABLE IF NOT EXISTS lattice_states (
	shard      TEXT PRIMARY KEY,
	epoch      INTEGER NOT NULL DEFAULT 1,
	edges_json BLOB NOT NULL
);
`
	if _, err := d.sql.Exec(ddl); err != nil {
		return fmt.Errorf("store: migrate epistemic tables: %w", err)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cols := []struct{ table, col, def string }{
		{"fragments", "lattice_epoch", "INTEGER NOT NULL DEFAULT 0"},
		{"fragments", "public_flag", "INTEGER NOT NULL DEFAULT 0"},
		{"fragments", "transition_hash", "TEXT NOT NULL DEFAULT ''"},
		{"anchors", "state_hash", "TEXT NOT NULL DEFAULT ''"},
		{"anchors", "mmr_root", "TEXT NOT NULL DEFAULT ''"},
		{"pending_anchors", "state_hash", "TEXT NOT NULL DEFAULT ''"},
		{"pending_anchors", "mmr_root", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range cols {
		if err := ensureColumnTx(tx, c.table, c.col, c.def); err != nil {
			return fmt.Errorf("store: ensure %s.%s: %w", c.table, c.col, err)
		}
	}
	return tx.Commit()
}

// GetFragmentByHDP looks up a fragment by (shard, hdp) inside an active RepoTx.
func (rt *RepoTx) GetFragmentByHDP(shard, hdp string) (FragmentRecord, error) {
	return scanFragment(rt.tx.QueryRow(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND hdp = ?`, shard, hdp))
}

// NextFragmentSeq returns the next sequential fragment number for shard inside an active RepoTx.
func (rt *RepoTx) NextFragmentSeq(shard string) (int64, error) {
	var nextSeq int64
	err := rt.tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM fragments WHERE shard = ?`, shard).Scan(&nextSeq)
	return nextSeq, err
}

// LatestStateQ returns the latest automaton state hash (hex) for shard inside an active RepoTx, or "" at genesis.
func (rt *RepoTx) LatestStateQ(shard string) (string, error) {
	var q string
	err := rt.tx.QueryRow(`SELECT next_q FROM transitions WHERE shard = ? ORDER BY seq DESC LIMIT 1`, shard).Scan(&q)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return q, err
}

// SaveTransition inserts an immutable automaton transition row inside an active RepoTx.
func (rt *RepoTx) SaveTransition(r TransitionRow) error {
	_, err := rt.tx.Exec(`
		INSERT INTO transitions (shard, seq, hash, prev_q, next_q, commit_hash, tree_hash, policy_hash, ontology_hash, body_cbor, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Shard, r.Seq, r.Hash, r.PrevQ, r.NextQ, r.CommitHash, r.TreeHash, r.PolicyHash, r.OntologyHash, r.BodyCBOR, r.CreatedAt,
	)
	return err
}

// GetLatestTransition returns the latest TransitionRow for shard (or sql.ErrNoRows if none).
func (d *DB) GetLatestTransition(shard string) (TransitionRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r TransitionRow
	err := d.sql.QueryRow(`
		SELECT shard, seq, hash, prev_q, next_q, commit_hash, tree_hash, policy_hash, ontology_hash, body_cbor, created_at
		FROM transitions WHERE shard = ? ORDER BY seq DESC LIMIT 1`, shard).
		Scan(&r.Shard, &r.Seq, &r.Hash, &r.PrevQ, &r.NextQ, &r.CommitHash, &r.TreeHash, &r.PolicyHash, &r.OntologyHash, &r.BodyCBOR, &r.CreatedAt)
	return r, err
}

// ListTransitions returns transition records for shard in ascending seq order.
func (d *DB) ListTransitions(shard string, limit int) ([]TransitionRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.sql.Query(`
		SELECT shard, seq, hash, prev_q, next_q, commit_hash, tree_hash, policy_hash, ontology_hash, body_cbor, created_at
		FROM transitions WHERE shard = ? ORDER BY seq ASC LIMIT ?`, shard, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransitionRow
	for rows.Next() {
		var r TransitionRow
		if err := rows.Scan(&r.Shard, &r.Seq, &r.Hash, &r.PrevQ, &r.NextQ, &r.CommitHash, &r.TreeHash, &r.PolicyHash, &r.OntologyHash, &r.BodyCBOR, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetFragmentByHDP looks up a fragment by (shard, hdp).
func (d *DB) GetFragmentByHDP(shard, hdp string) (FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return scanFragment(d.sql.QueryRow(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND hdp = ?`, shard, hdp))
}

// GetFragmentByLeaf looks up a fragment by (shard, leaf).
func (d *DB) GetFragmentByLeaf(shard, leaf string) (FragmentRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return scanFragment(d.sql.QueryRow(`SELECT `+fragmentSelectCols+` FROM fragments WHERE shard = ? AND leaf = ?`, shard, leaf))
}

// LoadLatticeStates loads all persisted per-shard CLKA epoch and domain hierarchy states.
func (d *DB) LoadLatticeStates() ([]lattice.ShardState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.Query(`SELECT shard, epoch, edges_json FROM lattice_states ORDER BY shard ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lattice.ShardState
	for rows.Next() {
		var st lattice.ShardState
		var edgesRaw []byte
		if err := rows.Scan(&st.Shard, &st.Epoch, &edgesRaw); err != nil {
			return nil, err
		}
		if len(edgesRaw) > 0 {
			_ = json.Unmarshal(edgesRaw, &st.DomainEdges)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SaveLatticeState persists a shard's CLKA epoch and domain DAG edges (zero raw keys stored!).
func (d *DB) SaveLatticeState(st lattice.ShardState) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	raw, err := json.Marshal(st.DomainEdges)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`INSERT OR REPLACE INTO lattice_states (shard, epoch, edges_json) VALUES (?, ?, ?)`, st.Shard, st.Epoch, raw)
	return err
}
