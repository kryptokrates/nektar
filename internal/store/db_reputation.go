package store

import (
	"database/sql"
	"strings"
	"time"
)

// ReputationAttestationRow stores one verified bilateral nektarrep1.* attestation.
type ReputationAttestationRow struct {
	AttestHash      string `json:"attestHash"`
	Role            string `json:"role"` // buyer_rates_seller | seller_rates_buyer
	IssuerPub       string `json:"issuerPub"`
	SubjectPub      string `json:"subjectPub"`
	OntologyID      string `json:"ontologyId"`
	DealID          string `json:"dealId"`
	DealReceiptHash string `json:"dealReceiptHash"`
	Token           string `json:"token"`
	EvidenceHash    string `json:"evidenceHash,omitempty"`
	AccBP           int64  `json:"accBp"`
	ConsBP          int64  `json:"consBp"`
	DispBP          int64  `json:"dispBp"`
	PredBP          int64  `json:"predBp"`
	CollabBP        int64  `json:"collabBp"`
	ProvBP          int64  `json:"provBp"`
	CompositeBP     int64  `json:"compositeBp"`
	CreatedAt       int64  `json:"createdAt"`
}

// Reputation6DRow stores the computed Personalized EigenTrust + 6D reputation vector per (SubjectPub, OntologyID).
type Reputation6DRow struct {
	SubjectPub   string `json:"subjectPub"`
	OntologyID   string `json:"ontologyId"`
	AccBP        int64  `json:"accBp"`
	ConsBP       int64  `json:"consBp"`
	DispBP       int64  `json:"dispBp"`
	PredBP       int64  `json:"predBp"`
	CollabBP     int64  `json:"collabBp"`
	ProvBP       int64  `json:"provBp"`
	EigenTrustBP int64  `json:"eigenTrustBp"`
	CompositeBP  int64  `json:"compositeBp"`
	DirectDeals  int64  `json:"directDeals"`
	AttestCount  int64  `json:"attestCount"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// DealOutcomeRow persists a downstream task outcome (`nektar hook outcome`) linked to a settled deal.
type DealOutcomeRow struct {
	DealID     string `json:"dealId"`
	SubjectPub string `json:"subjectPub"`
	OntologyID string `json:"ontologyId"`
	ExitCode   int    `json:"exitCode"`
	Note       string `json:"note,omitempty"`
	RecordedAt int64  `json:"recordedAt"`
}

func migrateReputationTx(tx *sql.Tx) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS reputation_attestations (
	attest_hash       TEXT PRIMARY KEY,
	role              TEXT NOT NULL,
	issuer_pub        TEXT NOT NULL,
	subject_pub       TEXT NOT NULL,
	ontology_id       TEXT NOT NULL,
	deal_id           TEXT NOT NULL,
	deal_receipt_hash TEXT NOT NULL,
	token             TEXT NOT NULL,
	evidence_hash     TEXT NOT NULL DEFAULT '',
	acc_bp            INTEGER NOT NULL DEFAULT 0,
	cons_bp           INTEGER NOT NULL DEFAULT 0,
	disp_bp           INTEGER NOT NULL DEFAULT 0,
	pred_bp           INTEGER NOT NULL DEFAULT 0,
	collab_bp         INTEGER NOT NULL DEFAULT 0,
	prov_bp           INTEGER NOT NULL DEFAULT 0,
	composite_bp      INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	UNIQUE (issuer_pub, subject_pub, deal_receipt_hash, role)
);
CREATE INDEX IF NOT EXISTS idx_rep_att_ont ON reputation_attestations(ontology_id, created_at);
CREATE INDEX IF NOT EXISTS idx_rep_att_subj ON reputation_attestations(subject_pub, ontology_id);

CREATE TABLE IF NOT EXISTS reputation_6d (
	subject_pub    TEXT NOT NULL,
	ontology_id    TEXT NOT NULL,
	acc_bp         INTEGER NOT NULL DEFAULT 0,
	cons_bp        INTEGER NOT NULL DEFAULT 0,
	disp_bp        INTEGER NOT NULL DEFAULT 0,
	pred_bp        INTEGER NOT NULL DEFAULT 0,
	collab_bp      INTEGER NOT NULL DEFAULT 0,
	prov_bp        INTEGER NOT NULL DEFAULT 0,
	eigentrust_bp  INTEGER NOT NULL DEFAULT 0,
	composite_bp   INTEGER NOT NULL DEFAULT 0,
	direct_deals   INTEGER NOT NULL DEFAULT 0,
	attest_count   INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL,
	PRIMARY KEY (subject_pub, ontology_id)
);
CREATE INDEX IF NOT EXISTS idx_rep6d_ont_comp ON reputation_6d(ontology_id, composite_bp DESC);

CREATE TABLE IF NOT EXISTS deal_outcomes (
	deal_id     TEXT PRIMARY KEY,
	subject_pub TEXT NOT NULL,
	ontology_id TEXT NOT NULL,
	exit_code   INTEGER NOT NULL,
	note        TEXT NOT NULL DEFAULT '',
	recorded_at INTEGER NOT NULL
);
`
	_, err := tx.Exec(ddl)
	return err
}

// SaveReputationAttestation persists a verified bilateral nektarrep1.* attestation (idempotent per deal+role).
func (d *DB) SaveReputationAttestation(r ReputationAttestationRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT INTO reputation_attestations (
			attest_hash, role, issuer_pub, subject_pub, ontology_id, deal_id, deal_receipt_hash,
			token, evidence_hash, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp, composite_bp, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(issuer_pub, subject_pub, deal_receipt_hash, role) DO UPDATE SET
			attest_hash = excluded.attest_hash,
			token = excluded.token,
			evidence_hash = excluded.evidence_hash,
			acc_bp = excluded.acc_bp,
			cons_bp = excluded.cons_bp,
			disp_bp = excluded.disp_bp,
			pred_bp = excluded.pred_bp,
			collab_bp = excluded.collab_bp,
			prov_bp = excluded.prov_bp,
			composite_bp = excluded.composite_bp,
			created_at = excluded.created_at`,
		strings.ToLower(strings.TrimSpace(r.AttestHash)),
		r.Role,
		strings.ToLower(strings.TrimSpace(r.IssuerPub)),
		strings.ToLower(strings.TrimSpace(r.SubjectPub)),
		strings.ToLower(strings.TrimSpace(r.OntologyID)),
		r.DealID,
		strings.ToLower(strings.TrimSpace(r.DealReceiptHash)),
		r.Token,
		strings.ToLower(strings.TrimSpace(r.EvidenceHash)),
		r.AccBP, r.ConsBP, r.DispBP, r.PredBP, r.CollabBP, r.ProvBP, r.CompositeBP, r.CreatedAt,
	)
	return err
}

// ListReputationAttestations returns verified attestations filtered by ontologyID (empty = all).
func (d *DB) ListReputationAttestations(ontologyID string, limit int) ([]ReputationAttestationRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	var rows *sql.Rows
	var err error
	ont := strings.ToLower(strings.TrimSpace(ontologyID))
	if ont != "" && ont != "global" {
		rows, err = d.sql.Query(`
			SELECT attest_hash, role, issuer_pub, subject_pub, ontology_id, deal_id, deal_receipt_hash,
				token, evidence_hash, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp, composite_bp, created_at
			FROM reputation_attestations WHERE ontology_id = ? ORDER BY created_at DESC LIMIT ?`, ont, limit)
	} else {
		rows, err = d.sql.Query(`
			SELECT attest_hash, role, issuer_pub, subject_pub, ontology_id, deal_id, deal_receipt_hash,
				token, evidence_hash, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp, composite_bp, created_at
			FROM reputation_attestations ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReputationAttestationRow
	for rows.Next() {
		var r ReputationAttestationRow
		if err := rows.Scan(
			&r.AttestHash, &r.Role, &r.IssuerPub, &r.SubjectPub, &r.OntologyID, &r.DealID, &r.DealReceiptHash,
			&r.Token, &r.EvidenceHash, &r.AccBP, &r.ConsBP, &r.DispBP, &r.PredBP, &r.CollabBP, &r.ProvBP, &r.CompositeBP, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertReputation6D saves or replaces a computed 6D reputation vector for (subject_pub, ontology_id).
func (d *DB) UpsertReputation6D(r Reputation6DRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.UpdatedAt == 0 {
		r.UpdatedAt = time.Now().Unix()
	}
	ont := strings.ToLower(strings.TrimSpace(r.OntologyID))
	if ont == "" {
		ont = "global"
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO reputation_6d (
			subject_pub, ontology_id, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp,
			eigentrust_bp, composite_bp, direct_deals, attest_count, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.ToLower(strings.TrimSpace(r.SubjectPub)), ont,
		r.AccBP, r.ConsBP, r.DispBP, r.PredBP, r.CollabBP, r.ProvBP,
		r.EigenTrustBP, r.CompositeBP, r.DirectDeals, r.AttestCount, r.UpdatedAt,
	)
	return err
}

// GetReputation6D fetches the 6D reputation vector for (subjectPub, ontologyID).
func (d *DB) GetReputation6D(subjectPub, ontologyID string) (Reputation6DRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ont := strings.ToLower(strings.TrimSpace(ontologyID))
	if ont == "" {
		ont = "global"
	}
	var r Reputation6DRow
	err := d.sql.QueryRow(`
		SELECT subject_pub, ontology_id, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp,
			eigentrust_bp, composite_bp, direct_deals, attest_count, updated_at
		FROM reputation_6d WHERE subject_pub = ? AND ontology_id = ?`,
		strings.ToLower(strings.TrimSpace(subjectPub)), ont).Scan(
		&r.SubjectPub, &r.OntologyID, &r.AccBP, &r.ConsBP, &r.DispBP, &r.PredBP, &r.CollabBP, &r.ProvBP,
		&r.EigenTrustBP, &r.CompositeBP, &r.DirectDeals, &r.AttestCount, &r.UpdatedAt,
	)
	return r, err
}

// ListReputation6D lists computed 6D vectors filtered by ontologyID (empty = all), highest composite first.
func (d *DB) ListReputation6D(ontologyID string, limit int) ([]Reputation6DRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	ont := strings.ToLower(strings.TrimSpace(ontologyID))
	if ont != "" {
		rows, err = d.sql.Query(`
			SELECT subject_pub, ontology_id, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp,
				eigentrust_bp, composite_bp, direct_deals, attest_count, updated_at
			FROM reputation_6d WHERE ontology_id = ? ORDER BY composite_bp DESC, eigentrust_bp DESC LIMIT ?`, ont, limit)
	} else {
		rows, err = d.sql.Query(`
			SELECT subject_pub, ontology_id, acc_bp, cons_bp, disp_bp, pred_bp, collab_bp, prov_bp,
				eigentrust_bp, composite_bp, direct_deals, attest_count, updated_at
			FROM reputation_6d ORDER BY composite_bp DESC, eigentrust_bp DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reputation6DRow
	for rows.Next() {
		var r Reputation6DRow
		if err := rows.Scan(
			&r.SubjectPub, &r.OntologyID, &r.AccBP, &r.ConsBP, &r.DispBP, &r.PredBP, &r.CollabBP, &r.ProvBP,
			&r.EigenTrustBP, &r.CompositeBP, &r.DirectDeals, &r.AttestCount, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordDealOutcome records a downstream task outcome linked to a deal.
func (d *DB) RecordDealOutcome(row DealOutcomeRow) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if row.RecordedAt == 0 {
		row.RecordedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT OR REPLACE INTO deal_outcomes (deal_id, subject_pub, ontology_id, exit_code, note, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		row.DealID, strings.ToLower(strings.TrimSpace(row.SubjectPub)),
		strings.ToLower(strings.TrimSpace(row.OntologyID)), row.ExitCode, row.Note, row.RecordedAt,
	)
	return err
}

// ListDealOutcomes returns recent deal outcome records newest first.
func (d *DB) ListDealOutcomes(limit int) ([]DealOutcomeRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.sql.Query(`
		SELECT deal_id, subject_pub, ontology_id, exit_code, note, recorded_at
		FROM deal_outcomes ORDER BY recorded_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DealOutcomeRow
	for rows.Next() {
		var r DealOutcomeRow
		if err := rows.Scan(&r.DealID, &r.SubjectPub, &r.OntologyID, &r.ExitCode, &r.Note, &r.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
