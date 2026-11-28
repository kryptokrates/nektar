package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-overlay-services/pkg/core/engine"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// BlobStore is the content-addressed BEEF blob store interface (PERF-5).
type BlobStore interface {
	Put(ctx context.Context, txid chainhash.Hash, data []byte) error
	Get(ctx context.Context, txid chainhash.Hash) ([]byte, error)
	Has(ctx context.Context, txid chainhash.Hash) (bool, error)
	Delete(ctx context.Context, txid chainhash.Hash) error
	Close() error
}

// FileBlobs stores one AtomicBEEF file per txid at `<root>/<ab>/<cd>/<txid>.beef` using atomic
// temp-file + rename (matching Lightweb's fileblobs layout while compiling cleanly on Windows, Linux, & macOS).
type FileBlobs struct {
	root string
}

// OpenFileBlobs opens a directory-sharded BEEF blob store at root.
func OpenFileBlobs(root string) (*FileBlobs, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("hub blobs: mkdir %s: %w", root, err)
	}
	return &FileBlobs{root: root}, nil
}

func (f *FileBlobs) path(txid chainhash.Hash) (dir, file string) {
	hx := txid.String()
	dir = filepath.Join(f.root, hx[:2], hx[2:4])
	return dir, filepath.Join(dir, hx+".beef")
}

func (f *FileBlobs) Put(_ context.Context, txid chainhash.Hash, data []byte) error {
	dir, dst := f.path(txid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".beef-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, wErr := tmp.Write(data); wErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return wErr
	}
	if cErr := tmp.Close(); cErr != nil {
		_ = os.Remove(tmpName)
		return cErr
	}
	if rErr := os.Rename(tmpName, dst); rErr != nil {
		_ = os.Remove(tmpName)
		return rErr
	}
	return nil
}

func (f *FileBlobs) Get(_ context.Context, txid chainhash.Hash) ([]byte, error) {
	_, dst := f.path(txid)
	raw, err := os.ReadFile(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func (f *FileBlobs) Has(_ context.Context, txid chainhash.Hash) (bool, error) {
	_, dst := f.path(txid)
	_, err := os.Stat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (f *FileBlobs) Delete(_ context.Context, txid chainhash.Hash) error {
	_, dst := f.path(txid)
	err := os.Remove(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (f *FileBlobs) Close() error { return nil }

// OverlayStore implements `go-overlay-services/pkg/core/engine.Storage` + `DeleteAppliedTransaction`
// using pure-Go `modernc.org/sqlite` WAL for the compact outpoint index and `FileBlobs` for BEEF bytes.
type OverlayStore struct {
	mu      sync.Mutex
	db      *sql.DB
	Blobs   *FileBlobs
	lastSeq atomic.Int64
}

var _ engine.Storage = (*OverlayStore)(nil)

func OpenOverlayStore(ctx context.Context, idxPath, blobsDir string) (*OverlayStore, error) {
	if err := os.MkdirAll(filepath.Dir(idxPath), 0o700); err != nil {
		return nil, fmt.Errorf("hub overlay store: mkdir: %w", err)
	}
	fb, err := OpenFileBlobs(blobsDir)
	if err != nil {
		return nil, err
	}
	dsn := idxPath + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(15000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("hub overlay store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	const schema = `
CREATE TABLE IF NOT EXISTS oe_outputs (
	topic            TEXT NOT NULL,
	txid             TEXT NOT NULL,
	vout             INTEGER NOT NULL,
	score            INTEGER NOT NULL,
	spent            INTEGER NOT NULL DEFAULT 0,
	spend_txid       TEXT NOT NULL DEFAULT '',
	block_height     INTEGER NOT NULL DEFAULT 0,
	block_idx        INTEGER NOT NULL DEFAULT 0,
	merkle_root      TEXT NOT NULL DEFAULT '',
	merkle_state     INTEGER NOT NULL DEFAULT 0,
	consumed_json    TEXT NOT NULL DEFAULT '[]',
	consumed_by_json TEXT NOT NULL DEFAULT '[]',
	ancillary_json   TEXT NOT NULL DEFAULT '[]',
	PRIMARY KEY (topic, txid, vout)
);
CREATE INDEX IF NOT EXISTS idx_oe_outputs_topic_spent_score ON oe_outputs(topic, spent, score);
CREATE INDEX IF NOT EXISTS idx_oe_outputs_txid ON oe_outputs(txid);
CREATE TABLE IF NOT EXISTS oe_applied (
	txid  TEXT NOT NULL,
	topic TEXT NOT NULL,
	at_us INTEGER NOT NULL,
	PRIMARY KEY (txid, topic)
);
CREATE TABLE IF NOT EXISTS oe_interactions (
	host  TEXT NOT NULL,
	topic TEXT NOT NULL,
	since REAL NOT NULL,
	PRIMARY KEY (host, topic)
);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hub overlay store: schema: %w", err)
	}
	st := &OverlayStore{db: db, Blobs: fb}
	st.lastSeq.Store(time.Now().UnixMicro())
	return st, nil
}

func (s *OverlayStore) nextScore() int64 {
	now := time.Now().UnixMicro()
	for {
		prev := s.lastSeq.Load()
		next := now
		if next <= prev {
			next = prev + 1
		}
		if s.lastSeq.CompareAndSwap(prev, next) {
			return next
		}
	}
}

func (s *OverlayStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

func encodeOutpointsJSON(ops []*transaction.Outpoint) string {
	if len(ops) == 0 {
		return "[]"
	}
	strs := make([]string, 0, len(ops))
	for _, op := range ops {
		if op != nil {
			strs = append(strs, op.String())
		}
	}
	b, _ := json.Marshal(strs)
	return string(b)
}

func decodeOutpointsJSON(raw string) []*transaction.Outpoint {
	var strs []string
	if raw == "" || raw == "[]" || json.Unmarshal([]byte(raw), &strs) != nil {
		return nil
	}
	out := make([]*transaction.Outpoint, 0, len(strs))
	for _, s := range strs {
		if op, err := parseOutpointString(s); err == nil {
			cp := op
			out = append(out, &cp)
		}
	}
	return out
}

func encodeHashesJSON(hs []*chainhash.Hash) string {
	if len(hs) == 0 {
		return "[]"
	}
	strs := make([]string, 0, len(hs))
	for _, h := range hs {
		if h != nil {
			strs = append(strs, h.String())
		}
	}
	b, _ := json.Marshal(strs)
	return string(b)
}

func decodeHashesJSON(raw string) []*chainhash.Hash {
	var strs []string
	if raw == "" || raw == "[]" || json.Unmarshal([]byte(raw), &strs) != nil {
		return nil
	}
	out := make([]*chainhash.Hash, 0, len(strs))
	for _, s := range strs {
		if h, err := chainhash.NewHashFromHex(s); err == nil && h != nil {
			out = append(out, h)
		}
	}
	return out
}

func (s *OverlayStore) InsertOutputs(ctx context.Context, topic string, txid *chainhash.Hash, outputs []uint32, outpointsConsumed []*transaction.Outpoint, beef *transaction.Beef, ancillaryTxids []*chainhash.Hash) error {
	if txid == nil || len(outputs) == 0 {
		return nil
	}
	if beef != nil {
		if raw, err := beef.AtomicBytes(txid); err == nil && len(raw) > 0 {
			if pErr := s.Blobs.Put(ctx, *txid, raw); pErr != nil {
				return pErr
			}
		}
	}
	var height uint32
	var mRoot string
	var mState engine.MerkleState
	if beef != nil {
		if tx := beef.FindTransactionByHash(txid); tx != nil && tx.MerklePath != nil {
			height = tx.MerklePath.BlockHeight
			if r, rErr := tx.MerklePath.ComputeRootHex(nil); rErr == nil {
				mRoot = r
				mState = engine.MerkleStateValidated
			}
		}
	}
	consJSON := encodeOutpointsJSON(outpointsConsumed)
	ancJSON := encodeHashesJSON(ancillaryTxids)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, vout := range outputs {
		score := s.nextScore()
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO oe_outputs (topic, txid, vout, score, spent, block_height, merkle_root, merkle_state, consumed_json, ancillary_json)
			VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
			topic, txid.String(), vout, score, height, mRoot, uint8(mState), consJSON, ancJSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *OverlayStore) hydrateRow(ctx context.Context, topic, txidHex string, vout uint32, score int64, spentInt int, height uint32, idx uint64, mRootHex string, mState uint8, consJSON, consByJSON, ancJSON string, includeBEEF bool) (*engine.Output, error) {
	h, err := chainhash.NewHashFromHex(txidHex)
	if err != nil || h == nil {
		return nil, err
	}
	out := &engine.Output{
		Outpoint:        transaction.Outpoint{Txid: *h, Index: vout},
		Topic:           topic,
		Spent:           spentInt != 0,
		OutputsConsumed: decodeOutpointsJSON(consJSON),
		ConsumedBy:      decodeOutpointsJSON(consByJSON),
		AncillaryTxids:  decodeHashesJSON(ancJSON),
		BlockHeight:     height,
		BlockIdx:        idx,
		Score:           float64(score),
		MerkleState:     engine.MerkleState(mState),
	}
	if mRootHex != "" {
		if mr, mErr := chainhash.NewHashFromHex(mRootHex); mErr == nil {
			out.MerkleRoot = mr
		}
	}
	if includeBEEF {
		if raw, gErr := s.Blobs.Get(ctx, *h); gErr == nil && len(raw) > 0 {
			if bf, _, bErr := transaction.NewBeefFromAtomicBytes(raw); bErr == nil {
				out.Beef = bf
			}
		}
	}
	return out, nil
}

func (s *OverlayStore) FindOutput(ctx context.Context, outpoint *transaction.Outpoint, topic *string, spent *bool, includeBEEF bool) (*engine.Output, error) {
	if outpoint == nil {
		return nil, nil
	}
	q := `SELECT topic, txid, vout, score, spent, block_height, block_idx, merkle_root, merkle_state, consumed_json, consumed_by_json, ancillary_json
	      FROM oe_outputs WHERE txid = ? AND vout = ?`
	args := []any{outpoint.Txid.String(), outpoint.Index}
	if topic != nil && *topic != "" {
		q += ` AND topic = ?`
		args = append(args, *topic)
	}
	if spent != nil {
		sp := 0
		if *spent {
			sp = 1
		}
		q += ` AND spent = ?`
		args = append(args, sp)
	}
	q += ` LIMIT 1`
	var tp, txHex, mrHex, cJSON, cbJSON, aJSON string
	var vout, height uint32
	var score int64
	var spentInt int
	var idx uint64
	var ms uint8
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&tp, &txHex, &vout, &score, &spentInt, &height, &idx, &mrHex, &ms, &cJSON, &cbJSON, &aJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.hydrateRow(ctx, tp, txHex, vout, score, spentInt, height, idx, mrHex, ms, cJSON, cbJSON, aJSON, includeBEEF)
}

func (s *OverlayStore) FindOutputs(ctx context.Context, outpoints []*transaction.Outpoint, topic string, spent *bool, includeBEEF bool) ([]*engine.Output, error) {
	var out []*engine.Output
	var tp *string
	if topic != "" {
		tp = &topic
	}
	for _, op := range outpoints {
		o, err := s.FindOutput(ctx, op, tp, spent, includeBEEF)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *OverlayStore) FindOutputsForTransaction(ctx context.Context, txid *chainhash.Hash, includeBEEF bool) ([]*engine.Output, error) {
	if txid == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT topic, txid, vout, score, spent, block_height, block_idx, merkle_root, merkle_state, consumed_json, consumed_by_json, ancillary_json
		FROM oe_outputs WHERE txid = ?`, txid.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*engine.Output
	for rows.Next() {
		var tp, txHex, mrHex, cJSON, cbJSON, aJSON string
		var vout, height uint32
		var score int64
		var spentInt int
		var idx uint64
		var ms uint8
		if err := rows.Scan(&tp, &txHex, &vout, &score, &spentInt, &height, &idx, &mrHex, &ms, &cJSON, &cbJSON, &aJSON); err != nil {
			return nil, err
		}
		o, err := s.hydrateRow(ctx, tp, txHex, vout, score, spentInt, height, idx, mrHex, ms, cJSON, cbJSON, aJSON, includeBEEF)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *OverlayStore) FindUTXOsForTopic(ctx context.Context, topic string, since float64, limit uint32, includeBEEF bool) ([]*engine.Output, error) {
	q := `SELECT topic, txid, vout, score, spent, block_height, block_idx, merkle_root, merkle_state, consumed_json, consumed_by_json, ancillary_json
	      FROM oe_outputs WHERE topic = ? AND spent = 0 AND score >= ? ORDER BY score ASC`
	args := []any{topic, int64(since)}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*engine.Output
	for rows.Next() {
		var tp, txHex, mrHex, cJSON, cbJSON, aJSON string
		var vout, height uint32
		var score int64
		var spentInt int
		var idx uint64
		var ms uint8
		if err := rows.Scan(&tp, &txHex, &vout, &score, &spentInt, &height, &idx, &mrHex, &ms, &cJSON, &cbJSON, &aJSON); err != nil {
			return nil, err
		}
		o, err := s.hydrateRow(ctx, tp, txHex, vout, score, spentInt, height, idx, mrHex, ms, cJSON, cbJSON, aJSON, includeBEEF)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *OverlayStore) DeleteOutput(ctx context.Context, outpoint *transaction.Outpoint, topic string) error {
	if outpoint == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM oe_outputs WHERE topic = ? AND txid = ? AND vout = ?`, topic, outpoint.Txid.String(), outpoint.Index)
	return err
}

func (s *OverlayStore) MarkUTXOsAsSpent(ctx context.Context, outpoints []*transaction.Outpoint, topic string, spendTxid *chainhash.Hash) error {
	if len(outpoints) == 0 {
		return nil
	}
	spHex := ""
	if spendTxid != nil {
		spHex = spendTxid.String()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, op := range outpoints {
		if op == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oe_outputs SET spent = 1, spend_txid = ? WHERE topic = ? AND txid = ? AND vout = ?`, spHex, topic, op.Txid.String(), op.Index); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *OverlayStore) UpdateConsumedBy(ctx context.Context, outpoint *transaction.Outpoint, topic string, consumedBy []*transaction.Outpoint) error {
	if outpoint == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE oe_outputs SET consumed_by_json = ? WHERE topic = ? AND txid = ? AND vout = ?`,
		encodeOutpointsJSON(consumedBy), topic, outpoint.Txid.String(), outpoint.Index)
	return err
}

func (s *OverlayStore) UpdateTransactionBEEF(ctx context.Context, txid *chainhash.Hash, beef *transaction.Beef) error {
	if txid == nil || beef == nil {
		return nil
	}
	raw, err := beef.AtomicBytes(txid)
	if err != nil {
		return err
	}
	return s.Blobs.Put(ctx, *txid, raw)
}

func (s *OverlayStore) UpdateOutputBlockHeight(ctx context.Context, outpoint *transaction.Outpoint, topic string, blockHeight uint32, blockIndex uint64) error {
	if outpoint == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE oe_outputs SET block_height = ?, block_idx = ? WHERE topic = ? AND txid = ? AND vout = ?`,
		blockHeight, blockIndex, topic, outpoint.Txid.String(), outpoint.Index)
	return err
}

func (s *OverlayStore) InsertAppliedTransaction(ctx context.Context, tx *overlay.AppliedTransaction) error {
	if tx == nil || tx.Txid == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO oe_applied (txid, topic, at_us) VALUES (?, ?, ?)`,
		tx.Txid.String(), tx.Topic, time.Now().UnixMicro())
	return err
}

func (s *OverlayStore) DoesAppliedTransactionExist(ctx context.Context, tx *overlay.AppliedTransaction) (bool, error) {
	if tx == nil || tx.Txid == nil {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM oe_applied WHERE txid = ? AND topic = ?`, tx.Txid.String(), tx.Topic).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *OverlayStore) DeleteAppliedTransaction(ctx context.Context, txid *chainhash.Hash, topic string) error {
	if txid == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM oe_applied WHERE txid = ? AND topic = ?`, txid.String(), topic)
	return err
}

func (s *OverlayStore) UpdateLastInteraction(ctx context.Context, host, topic string, since float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oe_interactions (host, topic, since) VALUES (?, ?, ?)
		ON CONFLICT(host, topic) DO UPDATE SET since = excluded.since`, host, topic, since)
	return err
}

func (s *OverlayStore) GetLastInteraction(ctx context.Context, host, topic string) (float64, error) {
	var since float64
	err := s.db.QueryRowContext(ctx, `SELECT since FROM oe_interactions WHERE host = ? AND topic = ?`, host, topic).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return since, err
}

func (s *OverlayStore) FindOutpointsByMerkleState(ctx context.Context, topic string, state engine.MerkleState, limit uint32) ([]*transaction.Outpoint, error) {
	q := `SELECT txid, vout FROM oe_outputs WHERE topic = ? AND merkle_state = ?`
	args := []any{topic, uint8(state)}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*transaction.Outpoint
	for rows.Next() {
		var txHex string
		var vout uint32
		if err := rows.Scan(&txHex, &vout); err != nil {
			return nil, err
		}
		if h, hErr := chainhash.NewHashFromHex(txHex); hErr == nil && h != nil {
			out = append(out, &transaction.Outpoint{Txid: *h, Index: vout})
		}
	}
	return out, rows.Err()
}

func (s *OverlayStore) ReconcileMerkleRoot(ctx context.Context, topic string, blockHeight uint32, merkleRoot *chainhash.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if merkleRoot == nil {
		return nil
	}
	rootHex := merkleRoot.String()
	_, err := s.db.ExecContext(ctx, `
		UPDATE oe_outputs
		SET merkle_state = CASE
			WHEN merkle_root = '' THEN ?
			WHEN merkle_root = ? THEN ?
			ELSE ?
		END
		WHERE topic = ? AND block_height = ?`,
		uint8(engine.MerkleStateUnmined), rootHex, uint8(engine.MerkleStateValidated), uint8(engine.MerkleStateInvalidated), topic, blockHeight)
	return err
}

func (s *OverlayStore) LoadAncillaryBeef(ctx context.Context, output *engine.Output) error {
	if output == nil || output.Beef == nil || len(output.AncillaryTxids) == 0 {
		return nil
	}
	for _, ah := range output.AncillaryTxids {
		if ah == nil {
			continue
		}
		if raw, err := s.Blobs.Get(ctx, *ah); err == nil && len(raw) > 0 {
			if abf, _, pErr := transaction.NewBeefFromAtomicBytes(raw); pErr == nil && abf != nil {
				_ = output.Beef.MergeBeef(abf)
			}
		}
	}
	return nil
}
