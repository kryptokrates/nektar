package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// KnowledgeEdgeRow stores one JevGraph-extracted semantic triple edge linked to (shard, seq).
type KnowledgeEdgeRow struct {
	Shard         string `json:"shard"`
	Seq           int64  `json:"seq"`
	TripleHash    string `json:"tripleHash"`
	OntologyID    string `json:"ontologyId"`
	Subject       string `json:"subject"`
	Predicate     string `json:"predicate"`
	Object        string `json:"object"`
	SubjectType   string `json:"subjectType,omitempty"`
	ObjectType    string `json:"objectType,omitempty"`
	Status        string `json:"status"` // accepted | review | rejected
	EvidenceQuote string `json:"evidenceQuote"`
	StartOffset   int    `json:"startOffset"`
	EndOffset     int    `json:"endOffset"`
	ConfidenceBP  int64  `json:"confidenceBp"`
	MarginBP      int64  `json:"marginBp"`
	SupportBP     int64  `json:"supportBp"`
	ModelID       string `json:"modelId,omitempty"`
	Reason        string `json:"reason,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
}

// KnowledgeEdgeFilter filters persisted knowledge_edges queries.
type KnowledgeEdgeFilter struct {
	Shard           string
	Seq             int64 // > 0 to filter to a specific fragment seq
	OntologyID      string
	Subject         string
	Predicate       string
	Object          string
	Status          string // accepted | review | rejected (empty = all)
	IncludeInactive bool   // include forgotten or superseded fragments when true
	Limit           int
}

func (d *DB) migrateGraph() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS knowledge_edges (
	shard          TEXT NOT NULL,
	seq            INTEGER NOT NULL,
	triple_hash    TEXT NOT NULL,
	ontology_id    TEXT NOT NULL,
	subject        TEXT NOT NULL,
	predicate      TEXT NOT NULL,
	object         TEXT NOT NULL,
	subject_type   TEXT NOT NULL DEFAULT '',
	object_type    TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL,
	evidence_quote TEXT NOT NULL,
	start_offset   INTEGER NOT NULL,
	end_offset     INTEGER NOT NULL,
	confidence_bp  INTEGER NOT NULL DEFAULT 0,
	margin_bp      INTEGER NOT NULL DEFAULT 0,
	support_bp     INTEGER NOT NULL DEFAULT 0,
	model_id       TEXT NOT NULL DEFAULT '',
	reason         TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	PRIMARY KEY (shard, seq, triple_hash)
);
CREATE INDEX IF NOT EXISTS idx_knowledge_edges_ont_status ON knowledge_edges(ontology_id, status);
CREATE INDEX IF NOT EXISTS idx_knowledge_edges_spo ON knowledge_edges(subject, predicate, object);
CREATE INDEX IF NOT EXISTS idx_knowledge_edges_shard_status ON knowledge_edges(shard, status);
CREATE TABLE IF NOT EXISTS fragment_graph_meta (
	shard       TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	ontology_id TEXT NOT NULL,
	edges_root  TEXT NOT NULL,
	PRIMARY KEY (shard, seq)
);
CREATE INDEX IF NOT EXISTS idx_fragment_graph_meta_ont ON fragment_graph_meta(ontology_id);
`
	if _, err := d.sql.Exec(ddl); err != nil {
		return fmt.Errorf("store: migrate knowledge_edges: %w", err)
	}
	// Crash-recovery hygiene: delete any orphaned graph edges belonging to forgotten fragments.
	if _, err := d.sql.Exec(`
		DELETE FROM knowledge_edges
		WHERE EXISTS (
			SELECT 1 FROM fragments f
			WHERE f.shard = knowledge_edges.shard AND f.seq = knowledge_edges.seq AND f.forgotten = 1
		)`); err != nil {
		return fmt.Errorf("store: clean forgotten knowledge_edges: %w", err)
	}
	return nil
}

// SaveFragmentGraphMeta records the committed (ontology_id, edges_root) bound into (shard, seq)'s MPE root.
func (rt *RepoTx) SaveFragmentGraphMeta(shard string, seq int64, ontologyID, edgesRoot string) error {
	if strings.TrimSpace(ontologyID) == "" && strings.TrimSpace(edgesRoot) == "" {
		return nil
	}
	_, err := rt.tx.Exec(
		`INSERT OR REPLACE INTO fragment_graph_meta (shard, seq, ontology_id, edges_root) VALUES (?, ?, ?, ?)`,
		shard, seq, strings.ToLower(strings.TrimSpace(ontologyID)), strings.ToLower(strings.TrimSpace(edgesRoot)),
	)
	return err
}

// GetFragmentGraphMeta returns the committed (ontologyID, edgesRoot) for (shard, seq), if any.
func (d *DB) GetFragmentGraphMeta(shard string, seq int64) (string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ontID, edgesRoot string
	err := d.sql.QueryRow(
		`SELECT ontology_id, edges_root FROM fragment_graph_meta WHERE shard = ? AND seq = ?`,
		shard, seq,
	).Scan(&ontID, &edgesRoot)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return ontID, edgesRoot, err
}

func saveKnowledgeEdgesTx(tx *sql.Tx, edges []KnowledgeEdgeRow) error {
	for _, e := range edges {
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO knowledge_edges (
				shard, seq, triple_hash, ontology_id, subject, predicate, object,
				subject_type, object_type, status, evidence_quote, start_offset, end_offset,
				confidence_bp, margin_bp, support_bp, model_id, reason, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.Shard, e.Seq, e.TripleHash, e.OntologyID, e.Subject, e.Predicate, e.Object,
			e.SubjectType, e.ObjectType, e.Status, e.EvidenceQuote, e.StartOffset, e.EndOffset,
			e.ConfidenceBP, e.MarginBP, e.SupportBP, e.ModelID, e.Reason, e.CreatedAt,
		); err != nil {
			return fmt.Errorf("store: save knowledge edge %s#%d/%s: %w", e.Shard, e.Seq, e.TripleHash, err)
		}
	}
	return nil
}

// SaveKnowledgeEdges persists extracted graph edges inside an active RepoTx atomically.
func (rt *RepoTx) SaveKnowledgeEdges(edges []KnowledgeEdgeRow) error {
	return saveKnowledgeEdgesTx(rt.tx, edges)
}

// SaveKnowledgeEdges persists extracted graph edges in a standalone SQLite transaction.
func (d *DB) SaveKnowledgeEdges(edges []KnowledgeEdgeRow) error {
	if len(edges) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveKnowledgeEdgesTx(tx, edges); err != nil {
		return err
	}
	return tx.Commit()
}

// ListKnowledgeEdges queries indexed JevGraph edges with optional filtering,
// excluding forgotten and superseded fragments by default.
func (d *DB) ListKnowledgeEdges(f KnowledgeEdgeFilter) ([]KnowledgeEdgeRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var where []string
	var args []any

	if !f.IncludeInactive {
		where = append(where, `NOT EXISTS (SELECT 1 FROM fragments fg WHERE fg.shard = ke.shard AND fg.seq = ke.seq AND fg.forgotten = 1)`)
		where = append(where, `NOT EXISTS (SELECT 1 FROM fragments fs WHERE fs.shard = ke.shard AND fs.supersedes = ke.seq AND fs.forgotten = 0)`)
	}
	if s := strings.TrimSpace(f.Shard); s != "" {
		where = append(where, `ke.shard = ?`)
		args = append(args, s)
	}
	if f.Seq > 0 {
		where = append(where, `ke.seq = ?`)
		args = append(args, f.Seq)
	}
	if s := strings.TrimSpace(f.OntologyID); s != "" {
		where = append(where, `ke.ontology_id = ?`)
		args = append(args, strings.ToLower(s))
	}
	if s := strings.TrimSpace(f.Subject); s != "" {
		where = append(where, `ke.subject = ?`)
		args = append(args, s)
	}
	if s := strings.TrimSpace(f.Predicate); s != "" {
		where = append(where, `ke.predicate = ?`)
		args = append(args, s)
	}
	if s := strings.TrimSpace(f.Object); s != "" {
		where = append(where, `ke.object = ?`)
		args = append(args, s)
	}
	if s := strings.TrimSpace(f.Status); s != "" {
		where = append(where, `ke.status = ?`)
		args = append(args, strings.ToLower(s))
	}

	limit := f.Limit
	if limit <= 0 || limit > 2000 {
		limit = 500
	}

	q := `SELECT ke.shard, ke.seq, ke.triple_hash, ke.ontology_id, ke.subject, ke.predicate, ke.object,
		ke.subject_type, ke.object_type, ke.status, ke.evidence_quote, ke.start_offset, ke.end_offset,
		ke.confidence_bp, ke.margin_bp, ke.support_bp, ke.model_id, ke.reason, ke.created_at
		FROM knowledge_edges ke`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ke.created_at ASC, ke.seq ASC, ke.triple_hash ASC LIMIT ?"
	args = append(args, limit)

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KnowledgeEdgeRow
	for rows.Next() {
		var r KnowledgeEdgeRow
		if err := rows.Scan(
			&r.Shard, &r.Seq, &r.TripleHash, &r.OntologyID, &r.Subject, &r.Predicate, &r.Object,
			&r.SubjectType, &r.ObjectType, &r.Status, &r.EvidenceQuote, &r.StartOffset, &r.EndOffset,
			&r.ConfidenceBP, &r.MarginBP, &r.SupportBP, &r.ModelID, &r.Reason, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetKnowledgeEdge fetches one persisted edge row by (shard, seq, tripleHash).
func (d *DB) GetKnowledgeEdge(shard string, seq int64, tripleHash string) (KnowledgeEdgeRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r KnowledgeEdgeRow
	err := d.sql.QueryRow(`
		SELECT shard, seq, triple_hash, ontology_id, subject, predicate, object,
			subject_type, object_type, status, evidence_quote, start_offset, end_offset,
			confidence_bp, margin_bp, support_bp, model_id, reason, created_at
		FROM knowledge_edges
		WHERE shard = ? AND seq = ? AND triple_hash = ?`, shard, seq, tripleHash).Scan(
		&r.Shard, &r.Seq, &r.TripleHash, &r.OntologyID, &r.Subject, &r.Predicate, &r.Object,
		&r.SubjectType, &r.ObjectType, &r.Status, &r.EvidenceQuote, &r.StartOffset, &r.EndOffset,
		&r.ConfidenceBP, &r.MarginBP, &r.SupportBP, &r.ModelID, &r.Reason, &r.CreatedAt,
	)
	return r, err
}

// UpdateKnowledgeEdgeStatus updates a persisted edge's status (e.g. review -> accepted|rejected).
func (d *DB) UpdateKnowledgeEdgeStatus(shard string, seq int64, tripleHash, status, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.sql.Exec(
		`UPDATE knowledge_edges SET status = ?, reason = ? WHERE shard = ? AND seq = ? AND triple_hash = ?`,
		status, reason, shard, seq, tripleHash,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// FragmentOntologyID returns the distinct ontology_id associated with (shard, seq), if any.
func (d *DB) FragmentOntologyID(shard string, seq int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ontID string
	err := d.sql.QueryRow(
		`SELECT ontology_id FROM fragment_graph_meta WHERE shard = ? AND seq = ? AND ontology_id != '' LIMIT 1`,
		shard, seq,
	).Scan(&ontID)
	if err == nil && ontID != "" {
		return ontID, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	err = d.sql.QueryRow(
		`SELECT ontology_id FROM knowledge_edges WHERE shard = ? AND seq = ? AND status = 'accepted' LIMIT 1`,
		shard, seq,
	).Scan(&ontID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return ontID, err
}
