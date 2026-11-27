package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenUpgradesLegacyFragmentsSchema verifies that opening an existing SQLite DB
// created before Memory Repo / epistemic columns were added to fragments (e.g. path,
// media_type, chirp_url, content_sha256, mpe_root, commit_hash, prev_leaf, chain_hash)
// cleanly migrates every missing column so AppendFragment and GetFragment succeed.
func TestOpenUpgradesLegacyFragmentsSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	legacyDDL := `
CREATE TABLE fragments (
	shard       TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	hdp         TEXT NOT NULL,
	leaf        TEXT NOT NULL,
	data        BLOB,
	ciphertext  BLOB,
	keyed_meta  BLOB,
	provenance  BLOB NOT NULL,
	level       INTEGER NOT NULL DEFAULT 0,
	class       TEXT NOT NULL DEFAULT 'general',
	batch_n     INTEGER NOT NULL DEFAULT -1,
	leaf_index  INTEGER NOT NULL DEFAULT -1,
	anchor_txid TEXT NOT NULL DEFAULT '',
	supersedes  INTEGER NOT NULL DEFAULT 0,
	forgotten   INTEGER NOT NULL DEFAULT 0,
	tainted     INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (shard, seq),
	UNIQUE (shard, hdp)
);
CREATE TABLE anchors (
	shard       TEXT NOT NULL,
	shard_id    TEXT NOT NULL,
	n           INTEGER NOT NULL,
	size        INTEGER NOT NULL,
	root_hex    TEXT NOT NULL,
	chained_hex TEXT NOT NULL,
	txid        TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (shard, n)
);
CREATE TABLE pending_anchors (
	shard       TEXT NOT NULL,
	n           INTEGER NOT NULL,
	seqs_json   BLOB NOT NULL,
	root_hex    TEXT NOT NULL,
	chained_hex TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (shard, n)
);
CREATE TABLE receipts (
	receipt_hash TEXT PRIMARY KEY,
	shard_seq    INTEGER NOT NULL,
	body_json    BLOB NOT NULL,
	created_at   INTEGER NOT NULL
);
INSERT INTO fragments (shard, seq, hdp, leaf, data, provenance, created_at)
VALUES ('__policies__', 1, 'legacy-hdp-1', 'legacy-leaf-1', X'706f6c696379', X'7b7d', 1700000000);
`
	if _, err := raw.Exec(legacyDDL); err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open on legacy schema failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Must be able to read the pre-existing row and append a new row using all evolved columns.
	oldRec, err := db.GetFragment("__policies__", 1)
	if err != nil {
		t.Fatalf("GetFragment(legacy row): %v", err)
	}
	if oldRec.HDP != "legacy-hdp-1" {
		t.Fatalf("unexpected legacy HDP %q", oldRec.HDP)
	}

	rec, dedup, err := db.AppendFragment(FragmentRecord{
		Shard:          "__policies__",
		HDP:            "upgraded-hdp-2",
		Leaf:           "upgraded-leaf-2",
		Data:           []byte("permit(principal, action, resource);"),
		Provenance:     []byte(`{"model":"test"}`),
		Path:           "policies/default.cedar",
		MediaType:      "text/plain",
		ContentSHA256:  "abcd",
		MPERoot:        "ef01",
		CommitHash:     "2345",
		TransitionHash: "6789",
		OntologyID:     "ops/policy",
		Distilled:      true,
		CreatedAt:      time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("AppendFragment after legacy upgrade failed: %v", err)
	}
	if dedup || rec.Seq != 2 || rec.Path != "policies/default.cedar" || rec.PrevLeaf != "legacy-leaf-1" {
		t.Fatalf("unexpected appended record: %+v dedup=%v", rec, dedup)
	}
}
