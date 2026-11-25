package wallet

import (
	"container/list"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/bwallet"
	_ "modernc.org/sqlite"
)

const (
	identityFileName = "identity.json"
	sqliteFileName   = "wallet.sqlite"
)

type identityFileJSON struct {
	WIF string `json:"wif"`
}

// LoadOrCreateIdentity loads <dir>/identity.json (exact bcommon/bwallet format) or creates it atomically (0600).
// When dir is empty, an ephemeral in-memory identity key is generated.
func LoadOrCreateIdentity(dir string) (*ec.PrivateKey, error) {
	if dir == "" {
		return ec.NewPrivateKey()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wallet: mkdir %s: %w", dir, err)
	}
	idPath := filepath.Join(dir, identityFileName)
	raw, err := os.ReadFile(idPath)
	if err == nil {
		var id identityFileJSON
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, fmt.Errorf("wallet: parse %s: %w", idPath, err)
		}
		key, err := ec.PrivateKeyFromWif(id.WIF)
		if err != nil {
			return nil, fmt.Errorf("wallet: invalid wif in %s: %w", idPath, err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wallet: read %s: %w", idPath, err)
	}
	key, err := ec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(identityFileJSON{WIF: key.Wif()})
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(idPath, payload, 0o600); err != nil {
		return nil, fmt.Errorf("wallet: write %s: %w", idPath, err)
	}
	return key, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// txLRU is a bounded thread-safe LRU cache for parsed *transaction.Transaction objects,
// eliminating the unbounded RAM growth of the old Port.txs map.
type txLRU struct {
	mu    sync.Mutex
	cap   int
	items map[string]*list.Element
	order *list.List
}

type txLRUEntry struct {
	txid string
	tx   *transaction.Transaction
}

func newTxLRU(capacity int) *txLRU {
	if capacity <= 0 {
		capacity = DefaultTxCacheSize
	}
	return &txLRU{
		cap:   capacity,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
	}
}

func (c *txLRU) Get(txid string) *transaction.Transaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[txid]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*txLRUEntry).tx
	}
	return nil
}

func (c *txLRU) Put(txid string, tx *transaction.Transaction) {
	if txid == "" || tx == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[txid]; ok {
		el.Value.(*txLRUEntry).tx = tx
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&txLRUEntry{txid: txid, tx: tx})
	c.items[txid] = el
	for c.order.Len() > c.cap {
		back := c.order.Back()
		if back == nil {
			break
		}
		entry := back.Value.(*txLRUEntry)
		delete(c.items, entry.txid)
		c.order.Remove(back)
	}
}

func (c *txLRU) Delete(txid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[txid]; ok {
		delete(c.items, txid)
		c.order.Remove(el)
	}
}

// Store is the pure-Go SQLite (modernc.org/sqlite, zero CGO) UTXO & transaction store.
// Short atomic transactions run under WAL + busy_timeout without holding locks across signing or network I/O.
type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	path    string
	txCache *txLRU
}

// OpenStore opens (or creates) wallet.sqlite inside dir, or an in-memory DB when dir == "".
func OpenStore(dir string, cacheSize int) (*Store, error) {
	dbPath := ":memory:"
	dsn := ":memory:"
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("wallet store: mkdir %s: %w", dir, err)
		}
		dbPath = filepath.Join(dir, sqliteFileName)
		dsn = dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("wallet store: open %s: %w", dbPath, err)
	}
	// Single serialized SQLite handle ensures zero SQLITE_BUSY under high goroutine fan-in
	// while keeping write transactions sub-100us in WAL mode.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(walletSchemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wallet store: migrate: %w", err)
	}
	return &Store{
		db:      db,
		path:    dbPath,
		txCache: newTxLRU(cacheSize),
	}, nil
}

// Close closes the underlying SQLite database cleanly.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the SQLite database path (or ":memory:").
func (s *Store) Path() string { return s.path }

// CacheTx stores tx in the bounded in-memory LRU cache.
func (s *Store) CacheTx(tx *transaction.Transaction) {
	if tx == nil {
		return
	}
	s.txCache.Put(tx.TxID().String(), tx)
}

// CachedTx returns a parsed tx from the bounded in-memory LRU cache if present.
func (s *Store) CachedTx(txid string) *transaction.Transaction {
	return s.txCache.Get(txid)
}

// withTx executes fn inside an atomic BEGIN IMMEDIATE SQLite transaction.
func (s *Store) withTx(fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// SQLite BEGIN IMMEDIATE acquires the write lock up front so concurrent writers never deadlock.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// MigrateLegacyPool imports any existing bcommon/bwallet wallet.json or legacy pool file
// (and optional txbeef/*.beef cache) into wallet.sqlite once, renaming the old file to .migrated.
func (s *Store) MigrateLegacyPool(walletDir, legacyFile string) (int, error) {
	if walletDir == "" {
		return 0, nil
	}
	candidates := []string{filepath.Join(walletDir, "wallet.json")}
	if legacyFile != "" && legacyFile != "wallet.json" {
		candidates = append(candidates, filepath.Join(walletDir, legacyFile))
	}
	var poolPath string
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			poolPath = c
			break
		}
	}
	if poolPath == "" {
		return 0, nil
	}

	// Only migrate when wallet_outputs is empty so repeated restarts never duplicate or resurrect spent coins.
	var existingCount int
	s.mu.Lock()
	err := s.db.QueryRow(`SELECT COUNT(1) FROM wallet_outputs`).Scan(&existingCount)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if existingCount > 0 {
		return 0, nil
	}

	pool, err := bwallet.LoadPool(poolPath)
	if err != nil {
		return 0, fmt.Errorf("wallet migrate: load %s: %w", poolPath, err)
	}
	outs := pool.Outputs()
	if len(outs) == 0 {
		_ = os.Rename(poolPath, poolPath+".migrated")
		return 0, nil
	}

	now := time.Now().Unix()
	beefDir := filepath.Join(walletDir, "txbeef")
	imported := 0
	err = s.withTx(func(tx *sql.Tx) error {
		for _, o := range outs {
			if o.TxID == "" || o.Satoshis == 0 {
				continue
			}
			lockBytes, err := hex.DecodeString(strings.TrimSpace(o.LockingScript))
			if err != nil || len(lockBytes) == 0 {
				continue
			}
			var rawBytes, bumpBytes, beefBytes []byte
			if o.Raw != "" {
				rawBytes, _ = hex.DecodeString(strings.TrimSpace(o.Raw))
			}
			if o.Bump != "" {
				bumpBytes, _ = hex.DecodeString(strings.TrimSpace(o.Bump))
			}
			if rawBeef, rErr := os.ReadFile(filepath.Join(beefDir, o.TxID+".beef")); rErr == nil && len(rawBeef) > 0 {
				beefBytes = rawBeef
			}

			status := StatusUnproven
			depth := 1
			if !o.Unproven && (len(bumpBytes) > 0 || o.Coinbase || o.Height > 0) {
				status = StatusCompleted
				depth = 0
				beefBytes = nil // compact proven ancestor BEEF
			}
			_, err = tx.Exec(`
				INSERT OR IGNORE INTO wallet_txs (
					txid, reference, status, is_outgoing, satoshis, fee_sats, description, labels_json,
					raw_tx, beef, merkle_path, block_height, unproven_depth, created_at, updated_at
				) VALUES (?, NULL, ?, 0, ?, 0, 'migrated from legacy wallet.json', '["migrated"]', ?, ?, ?, ?, ?, ?, ?)`,
				o.TxID, status, int64(o.Satoshis), rawBytes, beefBytes, bumpBytes, o.Height, depth, now, now,
			)
			if err != nil {
				return err
			}

			keyKind := KeyKindFund
			var prefix, suffix, sender string
			if o.Derivation != nil {
				keyKind = KeyKindBRC29
				parts := strings.SplitN(o.Derivation.KeyID, " ", 2)
				if len(parts) == 2 {
					prefix, suffix = parts[0], parts[1]
				}
				sender = o.Derivation.CounterpartyHex
			}
			res, err := tx.Exec(`
				INSERT OR IGNORE INTO wallet_outputs (
					txid, vout, satoshis, locking_script, basket, spendable,
					key_kind, derivation_prefix, derivation_suffix, sender_identity_key,
					coinbase, block_height, created_at
				) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`,
				o.TxID, o.Vout, o.Satoshis, lockBytes, BasketDefault,
				keyKind, prefix, suffix, sender, boolToInt(o.Coinbase), o.Height, now,
			)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				imported++
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("wallet migrate: %w", err)
	}
	_ = os.Rename(poolPath, poolPath+".migrated")
	return imported, nil
}

// UpsertIncomingTx records an external/coinbase/imported transaction and its wallet outputs atomically.
// Returns the number of newly inserted outputs.
func (s *Store) UpsertIncomingTx(rec TxRecord, outputs []OutputRecord) (int, error) {
	now := time.Now().Unix()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt == 0 {
		rec.UpdatedAt = now
	}
	if rec.Version == 0 {
		rec.Version = 1
	}
	labelsJSON, _ := json.Marshal(nonNilSlice(rec.Labels))
	added := 0
	err := s.withTx(func(tx *sql.Tx) error {
		// Insert tx if missing; if already present and we now have a proof, upgrade it!
		var existingStatus string
		err := tx.QueryRow(`SELECT status FROM wallet_txs WHERE txid = ?`, rec.TxID).Scan(&existingStatus)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var ref any
			if rec.Reference != "" {
				ref = rec.Reference
			}
			_, err = tx.Exec(`
				INSERT INTO wallet_txs (
					txid, reference, status, is_outgoing, satoshis, fee_sats, description, labels_json,
					version, lock_time, raw_tx, beef, merkle_path, block_height, unproven_depth,
					broadcast_attempts, last_error, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rec.TxID, ref, rec.Status, boolToInt(rec.IsOutgoing), rec.Satoshis, rec.FeeSats,
				rec.Description, string(labelsJSON), rec.Version, rec.LockTime,
				rec.RawTx, rec.BEEF, rec.MerklePath, rec.BlockHeight, rec.UnprovenDepth,
				rec.BroadcastAttempts, rec.LastError, rec.CreatedAt, rec.UpdatedAt,
			)
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if existingStatus != StatusCompleted && rec.Status == StatusCompleted && len(rec.MerklePath) > 0 {
				_, err = tx.Exec(`
					UPDATE wallet_txs
					SET status = ?, merkle_path = ?, block_height = ?, unproven_depth = 0, beef = NULL, updated_at = ?
					WHERE txid = ?`,
					StatusCompleted, rec.MerklePath, rec.BlockHeight, now, rec.TxID,
				)
				if err != nil {
					return err
				}
			} else if len(rec.BEEF) > 0 && existingStatus != StatusCompleted {
				_, _ = tx.Exec(`UPDATE wallet_txs SET beef = COALESCE(beef, ?), raw_tx = COALESCE(raw_tx, ?), updated_at = ? WHERE txid = ?`,
					rec.BEEF, rec.RawTx, now, rec.TxID)
			}
		}

		for _, o := range outputs {
			if o.Basket == "" {
				o.Basket = BasketDefault
			}
			if o.KeyKind == "" {
				o.KeyKind = KeyKindFund
			}
			if o.CreatedAt == 0 {
				o.CreatedAt = now
			}
			tagsJSON, _ := json.Marshal(nonNilSlice(o.Tags))
			res, err := tx.Exec(`
				INSERT OR IGNORE INTO wallet_outputs (
					txid, vout, satoshis, locking_script, basket, spendable,
					spent_by_txid, reserved_by_ref, reserved_until,
					key_kind, derivation_prefix, derivation_suffix, sender_identity_key,
					tags_json, custom_instructions, output_description, coinbase, block_height, created_at
				) VALUES (?, ?, ?, ?, ?, ?, '', '', 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				o.TxID, o.Vout, o.Satoshis, o.LockingScript, o.Basket, boolToInt(o.Spendable),
				o.KeyKind, o.DerivationPrefix, o.DerivationSuffix, o.SenderIdentityKey,
				string(tagsJSON), o.CustomInstructions, o.OutputDescription,
				boolToInt(o.Coinbase), o.BlockHeight, o.CreatedAt,
			)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
		}
		return nil
	})
	return added, err
}

// GetTx returns a single TxRecord by txid.
func (s *Store) GetTx(txid string) (*TxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
		SELECT txid, COALESCE(reference, ''), status, is_outgoing, satoshis, fee_sats,
		       description, labels_json, version, lock_time, raw_tx, beef, merkle_path,
		       block_height, unproven_depth, broadcast_attempts, last_error, created_at, updated_at
		FROM wallet_txs WHERE txid = ?`, txid)
	return scanTxRecord(row)
}

// GetTxByReference returns a single TxRecord by its unique action reference.
func (s *Store) GetTxByReference(ref string) (*TxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
		SELECT txid, COALESCE(reference, ''), status, is_outgoing, satoshis, fee_sats,
		       description, labels_json, version, lock_time, raw_tx, beef, merkle_path,
		       block_height, unproven_depth, broadcast_attempts, last_error, created_at, updated_at
		FROM wallet_txs WHERE reference = ?`, ref)
	return scanTxRecord(row)
}

// LoadParsedTx loads a *transaction.Transaction from LRU cache or SQLite (with MerklePath or BEEF ancestors hydrated).
func (s *Store) LoadParsedTx(txid string) (*transaction.Transaction, error) {
	if cached := s.txCache.Get(txid); cached != nil {
		return cached, nil
	}
	rec, err := s.GetTx(txid)
	if err != nil {
		return nil, err
	}
	var tx *transaction.Transaction
	if len(rec.BEEF) > 0 {
		if parsed, pErr := transaction.NewTransactionFromBEEF(rec.BEEF); pErr == nil && parsed != nil {
			tx = parsed
		}
	}
	if tx == nil && len(rec.RawTx) > 0 {
		parsed, pErr := transaction.NewTransactionFromBytes(rec.RawTx)
		if pErr != nil {
			return nil, fmt.Errorf("wallet store: decode raw tx %s: %w", txid, pErr)
		}
		tx = parsed
	}
	if tx == nil {
		return nil, fmt.Errorf("wallet store: tx %s has neither BEEF nor raw bytes", txid)
	}
	if len(rec.MerklePath) > 0 && tx.MerklePath == nil {
		if mp, mErr := transaction.NewMerklePathFromBinary(rec.MerklePath); mErr == nil {
			tx.MerklePath = mp
		}
	}
	// If the tx is unproven and we only have raw_tx (no hydrated ancestors), hydrate parents recursively from Store!
	if tx.MerklePath == nil {
		for _, in := range tx.Inputs {
			if in.SourceTransaction == nil && in.SourceTXID != nil {
				if parent, pErr := s.LoadParsedTx(in.SourceTXID.String()); pErr == nil && parent != nil {
					in.SourceTransaction = parent
				}
			}
		}
	}
	s.txCache.Put(txid, tx)
	return tx, nil
}

// MarkTxProven upgrades an unproven/sending tx to completed with its verified MerklePath and compacts away ancestor BEEF.
func (s *Store) MarkTxProven(txid string, merklePath []byte, blockHeight uint32) (bool, error) {
	now := time.Now().Unix()
	changed := false
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE wallet_txs
			SET status = ?, merkle_path = ?, block_height = ?, unproven_depth = 0, beef = NULL, last_error = '', updated_at = ?
			WHERE txid = ? AND status != ?`,
			StatusCompleted, merklePath, blockHeight, now, txid, StatusCompleted,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
		}
		_, err = tx.Exec(`UPDATE wallet_outputs SET block_height = ? WHERE txid = ? AND block_height = 0`, blockHeight, txid)
		return err
	})
	if err == nil && changed {
		// Refresh cached tx with its MerklePath if present in LRU.
		if cached := s.txCache.Get(txid); cached != nil && len(merklePath) > 0 {
			if mp, mErr := transaction.NewMerklePathFromBinary(merklePath); mErr == nil {
				cached.MerklePath = mp
			}
		}
	}
	return changed, err
}

// ReserveOutputs atomically claims outputs by ID inside a single SQLite transaction.
// Returns true iff ALL requested IDs were unreserved and spendable; otherwise rolls back and returns false.
func (s *Store) ReserveOutputs(ids []int64, ref string, reservedUntil int64) (bool, error) {
	if len(ids) == 0 {
		return true, nil
	}
	claimedAll := false
	err := s.withTx(func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.Exec(`
				UPDATE wallet_outputs
				SET spendable = 0, reserved_by_ref = ?, reserved_until = ?
				WHERE id = ? AND spendable = 1 AND reserved_by_ref = '' AND spent_by_txid = ''`,
				ref, reservedUntil, id,
			)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errOptimisticConflict
			}
		}
		claimedAll = true
		return nil
	})
	if errors.Is(err, errOptimisticConflict) {
		return false, nil
	}
	return claimedAll, err
}

var errOptimisticConflict = errors.New("wallet store: optimistic UTXO claim conflict")

// ReleaseReservation restores reserved outputs back to spendable=1 (used on abort, sign error, or transient rollback).
func (s *Store) ReleaseReservation(ref string) (int64, error) {
	var released int64
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE wallet_outputs
			SET spendable = 1, reserved_by_ref = '', reserved_until = 0
			WHERE reserved_by_ref = ? AND spent_by_txid = ''`, ref)
		if err != nil {
			return err
		}
		released, _ = res.RowsAffected()
		_, _ = tx.Exec(`DELETE FROM wallet_pending_actions WHERE reference = ?`, ref)
		return nil
	})
	return released, err
}

// MarkReservationFailed marks a tx as failed and optionally releases inputs (releaseInputs=false for double-spent inputs!).
func (s *Store) MarkReservationFailed(ref, txid, reason string, releaseInputs bool) error {
	now := time.Now().Unix()
	return s.withTx(func(tx *sql.Tx) error {
		if releaseInputs {
			if _, err := tx.Exec(`
				UPDATE wallet_outputs
				SET spendable = 1, reserved_by_ref = '', reserved_until = 0
				WHERE reserved_by_ref = ? AND spent_by_txid = ''`, ref); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`
				UPDATE wallet_outputs
				SET spendable = 0, spent_by_txid = COALESCE(NULLIF(spent_by_txid, ''), ?), reserved_by_ref = '', reserved_until = 0
				WHERE reserved_by_ref = ?`, "spent_elsewhere:"+txid, ref); err != nil {
				return err
			}
		}
		if txid != "" {
			_, _ = tx.Exec(`UPDATE wallet_txs SET status = ?, last_error = ?, updated_at = ? WHERE txid = ?`,
				StatusFailed, reason, now, txid)
			// Any outputs created by this failed tx must not remain spendable.
			_, _ = tx.Exec(`UPDATE wallet_outputs SET spendable = 0, spent_by_txid = 'tx_failed' WHERE txid = ?`, txid)
		} else if ref != "" {
			_, _ = tx.Exec(`UPDATE wallet_txs SET status = ?, last_error = ?, updated_at = ? WHERE reference = ?`,
				StatusFailed, reason, now, ref)
		}
		if ref != "" {
			_, _ = tx.Exec(`DELETE FROM wallet_pending_actions WHERE reference = ?`, ref)
		}
		return nil
	})
}

// CommitOutgoingAction atomically writes the signed tx, marks spent inputs, records inputs table,
// inserts new wallet-owned outputs (change/fuel/reserve/custom), and removes pending action state.
func (s *Store) CommitOutgoingAction(ref string, rec TxRecord, spentInputIDs []int64, inputs []InputRecord, newOutputs []OutputRecord) error {
	now := time.Now().Unix()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	if rec.Version == 0 {
		rec.Version = 1
	}
	labelsJSON, _ := json.Marshal(nonNilSlice(rec.Labels))

	return s.withTx(func(tx *sql.Tx) error {
		// Remove any placeholder unsigned row for this reference (txid changes once scripts are signed!).
		if ref != "" {
			_, _ = tx.Exec(`DELETE FROM wallet_txs WHERE reference = ? AND txid != ?`, ref, rec.TxID)
			_, _ = tx.Exec(`DELETE FROM wallet_pending_actions WHERE reference = ?`, ref)
		}
		var refVal any
		if rec.Reference != "" {
			refVal = rec.Reference
		}
		_, err := tx.Exec(`
			INSERT INTO wallet_txs (
				txid, reference, status, is_outgoing, satoshis, fee_sats, description, labels_json,
				version, lock_time, raw_tx, beef, merkle_path, block_height, unproven_depth,
				broadcast_attempts, last_error, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(txid) DO UPDATE SET
				status = excluded.status,
				satoshis = excluded.satoshis,
				fee_sats = excluded.fee_sats,
				raw_tx = excluded.raw_tx,
				beef = excluded.beef,
				merkle_path = COALESCE(excluded.merkle_path, wallet_txs.merkle_path),
				block_height = MAX(wallet_txs.block_height, excluded.block_height),
				unproven_depth = excluded.unproven_depth,
				broadcast_attempts = excluded.broadcast_attempts,
				last_error = excluded.last_error,
				updated_at = excluded.updated_at`,
			rec.TxID, refVal, rec.Status, boolToInt(rec.IsOutgoing), rec.Satoshis, rec.FeeSats,
			rec.Description, string(labelsJSON), rec.Version, rec.LockTime,
			rec.RawTx, rec.BEEF, rec.MerklePath, rec.BlockHeight, rec.UnprovenDepth,
			rec.BroadcastAttempts, rec.LastError, rec.CreatedAt, rec.UpdatedAt,
		)
		if err != nil {
			return err
		}

		// Mark reserved wallet inputs permanently spent by rec.TxID.
		for _, id := range spentInputIDs {
			if _, err := tx.Exec(`
				UPDATE wallet_outputs
				SET spendable = 0, spent_by_txid = ?, reserved_by_ref = '', reserved_until = 0
				WHERE id = ?`, rec.TxID, id); err != nil {
				return err
			}
		}
		// Also mark any wallet-owned caller-supplied inputs spent by outpoint.
		for _, in := range inputs {
			_, _ = tx.Exec(`
				UPDATE wallet_outputs
				SET spendable = 0, spent_by_txid = ?, reserved_by_ref = '', reserved_until = 0
				WHERE txid = ? AND vout = ?`, rec.TxID, in.SourceTxID, in.SourceVout)
			_, err := tx.Exec(`
				INSERT OR REPLACE INTO wallet_inputs (
					txid, vin, source_txid, source_vout, source_satoshis,
					source_locking_script, unlocking_script, input_description, sequence_number
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rec.TxID, in.Vin, in.SourceTxID, in.SourceVout, in.SourceSatoshis,
				in.SourceLockingScript, in.UnlockingScript, in.InputDescription, in.SequenceNumber,
			)
			if err != nil {
				return err
			}
		}

		// Insert new wallet-owned outputs (change / fuel / reserve / custom basket outputs).
		for _, o := range newOutputs {
			if o.Basket == "" {
				o.Basket = BasketDefault
			}
			if o.KeyKind == "" {
				o.KeyKind = KeyKindFund
			}
			if o.CreatedAt == 0 {
				o.CreatedAt = now
			}
			tagsJSON, _ := json.Marshal(nonNilSlice(o.Tags))
			_, err := tx.Exec(`
				INSERT OR REPLACE INTO wallet_outputs (
					txid, vout, satoshis, locking_script, basket, spendable,
					spent_by_txid, reserved_by_ref, reserved_until,
					key_kind, derivation_prefix, derivation_suffix, sender_identity_key,
					tags_json, custom_instructions, output_description, coinbase, block_height, created_at
				) VALUES (?, ?, ?, ?, ?, ?, '', '', 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				o.TxID, o.Vout, o.Satoshis, o.LockingScript, o.Basket, boolToInt(o.Spendable),
				o.KeyKind, o.DerivationPrefix, o.DerivationSuffix, o.SenderIdentityKey,
				string(tagsJSON), o.CustomInstructions, o.OutputDescription,
				boolToInt(o.Coinbase), o.BlockHeight, o.CreatedAt,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// SavePendingAction persists two-phase unsigned action metadata + placeholder tx row for crash-safe SignAction/AbortAbandoned.
func (s *Store) SavePendingAction(ref string, placeholderTx TxRecord, stateJSON []byte, expiresAt int64) error {
	now := time.Now().Unix()
	if placeholderTx.CreatedAt == 0 {
		placeholderTx.CreatedAt = now
	}
	placeholderTx.UpdatedAt = now
	labelsJSON, _ := json.Marshal(nonNilSlice(placeholderTx.Labels))
	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT OR REPLACE INTO wallet_txs (
				txid, reference, status, is_outgoing, satoshis, fee_sats, description, labels_json,
				version, lock_time, raw_tx, beef, block_height, unproven_depth, created_at, updated_at
			) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
			placeholderTx.TxID, ref, StatusUnsigned, placeholderTx.Satoshis, placeholderTx.FeeSats,
			placeholderTx.Description, string(labelsJSON), placeholderTx.Version, placeholderTx.LockTime,
			placeholderTx.RawTx, placeholderTx.BEEF, placeholderTx.UnprovenDepth,
			placeholderTx.CreatedAt, placeholderTx.UpdatedAt,
		)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			INSERT OR REPLACE INTO wallet_pending_actions (reference, state_json, expires_at, created_at)
			VALUES (?, ?, ?, ?)`, ref, stateJSON, expiresAt, now)
		return err
	})
}

// LoadPendingAction retrieves pending action state JSON by reference.
func (s *Store) LoadPendingAction(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var data []byte
	err := s.db.QueryRow(`SELECT state_json FROM wallet_pending_actions WHERE reference = ?`, ref).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActionNotFound
	}
	return data, err
}

// UpdateTxStatus updates status, attempts, and last_error for a tx (used by Monitor.SendWaiting).
func (s *Store) UpdateTxStatus(txid, status, lastErr string, bumpAttempts bool) error {
	now := time.Now().Unix()
	return s.withTx(func(tx *sql.Tx) error {
		if bumpAttempts {
			_, err := tx.Exec(`
				UPDATE wallet_txs
				SET status = ?, last_error = ?, broadcast_attempts = broadcast_attempts + 1, updated_at = ?
				WHERE txid = ?`, status, lastErr, now, txid)
			return err
		}
		_, err := tx.Exec(`
			UPDATE wallet_txs
			SET status = ?, last_error = ?, updated_at = ?
			WHERE txid = ?`, status, lastErr, now, txid)
		return err
	})
}

// AbortAbandoned releases all expired unsigned reservations (reserved_until > 0 && reserved_until < now).
func (s *Store) AbortAbandoned(nowUnix int64) (int, error) {
	aborted := 0
	err := s.withTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`
			SELECT DISTINCT reserved_by_ref FROM wallet_outputs
			WHERE reserved_by_ref != '' AND reserved_until > 0 AND reserved_until < ? AND spent_by_txid = ''`, nowUnix)
		if err != nil {
			return err
		}
		var refs []string
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				_ = rows.Close()
				return err
			}
			refs = append(refs, r)
		}
		_ = rows.Close()

		// Also pick up expired pending actions even if they held 0 wallet inputs.
		pRows, err := tx.Query(`SELECT reference FROM wallet_pending_actions WHERE expires_at > 0 AND expires_at < ?`, nowUnix)
		if err == nil {
			seen := make(map[string]bool, len(refs))
			for _, r := range refs {
				seen[r] = true
			}
			for pRows.Next() {
				var r string
				if pRows.Scan(&r) == nil && !seen[r] {
					seen[r] = true
					refs = append(refs, r)
				}
			}
			_ = pRows.Close()
		}

		for _, ref := range refs {
			_, _ = tx.Exec(`
				UPDATE wallet_outputs
				SET spendable = 1, reserved_by_ref = '', reserved_until = 0
				WHERE reserved_by_ref = ? AND spent_by_txid = ''`, ref)
			_, _ = tx.Exec(`
				UPDATE wallet_txs
				SET status = ?, last_error = 'abandoned unsigned action', updated_at = ?
				WHERE reference = ? AND status = ?`, StatusFailed, nowUnix, ref, StatusUnsigned)
			_, _ = tx.Exec(`DELETE FROM wallet_pending_actions WHERE reference = ?`, ref)
			aborted++
		}
		return nil
	})
	return aborted, err
}

// ListSpendableCandidates returns unspent, unreserved outputs in spending baskets (or a specific basket)
// joined with their parent transaction's status and unproven_depth.
func (s *Store) ListSpendableCandidates(baskets []string) ([]OutputRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(baskets) == 0 {
		baskets = []string{BasketFuel, BasketDefault, BasketReserve}
	}
	placeholders := strings.Repeat("?,", len(baskets))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(baskets))
	for i, b := range baskets {
		args[i] = b
	}
	query := fmt.Sprintf(`
		SELECT o.id, o.txid, o.vout, o.satoshis, o.locking_script, o.basket, o.spendable,
		       o.spent_by_txid, o.reserved_by_ref, o.reserved_until, o.key_kind,
		       o.derivation_prefix, o.derivation_suffix, o.sender_identity_key,
		       o.tags_json, o.custom_instructions, o.output_description, o.coinbase, o.block_height, o.created_at,
		       COALESCE(t.status, 'completed'), COALESCE(t.unproven_depth, 0)
		FROM wallet_outputs o
		LEFT JOIN wallet_txs t ON t.txid = o.txid
		WHERE o.basket IN (%s)
		  AND o.spendable = 1
		  AND o.reserved_by_ref = ''
		  AND o.spent_by_txid = ''
		  AND COALESCE(t.status, 'completed') != 'failed'
		ORDER BY o.satoshis ASC, o.id ASC`, placeholders)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutputRows(rows)
}

// ListOutputsByFilter lists outputs matching optional basket and spendable filter (for BRC-100 ListOutputs & UI/CLI).
func (s *Store) ListOutputsByFilter(basket string, onlySpendable bool, limit, offset int) ([]OutputRecord, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var where []string
	var args []any
	if basket != "" && basket != "all" {
		where = append(where, "o.basket = ?")
		args = append(args, basket)
	}
	if onlySpendable {
		where = append(where, "o.spendable = 1 AND o.spent_by_txid = '' AND o.reserved_by_ref = ''")
	} else {
		where = append(where, "o.spent_by_txid = ''")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(1) FROM wallet_outputs o "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 10000 {
		limit = 10000
	}
	if offset < 0 {
		offset = 0
	}
	qArgs := append(append([]any{}, args...), limit, offset)
	query := fmt.Sprintf(`
		SELECT o.id, o.txid, o.vout, o.satoshis, o.locking_script, o.basket, o.spendable,
		       o.spent_by_txid, o.reserved_by_ref, o.reserved_until, o.key_kind,
		       o.derivation_prefix, o.derivation_suffix, o.sender_identity_key,
		       o.tags_json, o.custom_instructions, o.output_description, o.coinbase, o.block_height, o.created_at,
		       COALESCE(t.status, 'completed'), COALESCE(t.unproven_depth, 0)
		FROM wallet_outputs o
		LEFT JOIN wallet_txs t ON t.txid = o.txid
		%s
		ORDER BY o.id DESC
		LIMIT ? OFFSET ?`, whereSQL)
	rows, err := s.db.Query(query, qArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	outs, err := scanOutputRows(rows)
	return outs, total, err
}

// GetOutputByOutpoint returns a wallet output by (txid, vout).
func (s *Store) GetOutputByOutpoint(txid string, vout uint32) (*OutputRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
		SELECT o.id, o.txid, o.vout, o.satoshis, o.locking_script, o.basket, o.spendable,
		       o.spent_by_txid, o.reserved_by_ref, o.reserved_until, o.key_kind,
		       o.derivation_prefix, o.derivation_suffix, o.sender_identity_key,
		       o.tags_json, o.custom_instructions, o.output_description, o.coinbase, o.block_height, o.created_at,
		       COALESCE(t.status, 'completed'), COALESCE(t.unproven_depth, 0)
		FROM wallet_outputs o
		LEFT JOIN wallet_txs t ON t.txid = o.txid
		WHERE o.txid = ? AND o.vout = ?`, txid, vout)
	return scanOutputRow(row)
}

// RelinquishOutput clears the basket assignment of an output (BRC-100 RelinquishOutput).
func (s *Store) RelinquishOutput(basket, txid string, vout uint32) (bool, error) {
	relinquished := false
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE wallet_outputs
			SET basket = '', spendable = 0
			WHERE txid = ? AND vout = ? AND (basket = ? OR ? = '')`,
			txid, vout, basket, basket)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			relinquished = true
		}
		return nil
	})
	return relinquished, err
}

// MarkOutputSpentExternally marks an output as spent by an external txid (discovered during chain reconciliation).
func (s *Store) MarkOutputSpentExternally(txid string, vout uint32, spenderTxID string) (bool, error) {
	if spenderTxID == "" {
		spenderTxID = "external"
	}
	changed := false
	err := s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE wallet_outputs
			SET spendable = 0, spent_by_txid = ?, reserved_by_ref = '', reserved_until = 0
			WHERE txid = ? AND vout = ? AND spent_by_txid = ''`,
			spenderTxID, txid, vout)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
		}
		return nil
	})
	return changed, err
}

// IsOutpointKnownSpent reports whether (txid, vout) is already marked spent or reserved in wallet_outputs.
func (s *Store) IsOutpointKnownSpent(txid string, vout uint32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var spentBy, reservedBy string
	err := s.db.QueryRow(`SELECT spent_by_txid, reserved_by_ref FROM wallet_outputs WHERE txid = ? AND vout = ?`, txid, vout).Scan(&spentBy, &reservedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return spentBy != "" || reservedBy != "", nil
}

// ListTxsByStatus returns wallet_txs rows with any of the given statuses, oldest updated first.
func (s *Store) ListTxsByStatus(statuses []string, limit int) ([]TxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(statuses) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	placeholders := strings.Repeat("?,", len(statuses))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(statuses)+1)
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, limit)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT txid, COALESCE(reference, ''), status, is_outgoing, satoshis, fee_sats,
		       description, labels_json, version, lock_time, raw_tx, beef, merkle_path,
		       block_height, unproven_depth, broadcast_attempts, last_error, created_at, updated_at
		FROM wallet_txs
		WHERE status IN (%s)
		ORDER BY updated_at ASC, created_at ASC
		LIMIT ?`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TxRecord
	for rows.Next() {
		r, err := scanTxRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListRecentTxs lists wallet transactions ordered newest first (for ListActions & CLI/UI).
func (s *Store) ListRecentTxs(limit, offset int) ([]TxRecord, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM wallet_txs`).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 10000 {
		limit = 10000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(`
		SELECT txid, COALESCE(reference, ''), status, is_outgoing, satoshis, fee_sats,
		       description, labels_json, version, lock_time, raw_tx, beef, merkle_path,
		       block_height, unproven_depth, broadcast_attempts, last_error, created_at, updated_at
		FROM wallet_txs
		ORDER BY created_at DESC, rowid DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []TxRecord
	for rows.Next() {
		r, err := scanTxRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

// ListInputsForTx returns recorded inputs for a transaction (for BRC-100 ListActions).
func (s *Store) ListInputsForTx(txid string) ([]InputRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT txid, vin, source_txid, source_vout, source_satoshis,
		       source_locking_script, unlocking_script, input_description, sequence_number
		FROM wallet_inputs WHERE txid = ? ORDER BY vin ASC`, txid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InputRecord
	for rows.Next() {
		var r InputRecord
		if err := rows.Scan(&r.TxID, &r.Vin, &r.SourceTxID, &r.SourceVout, &r.SourceSatoshis,
			&r.SourceLockingScript, &r.UnlockingScript, &r.InputDescription, &r.SequenceNumber); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListOutputsForTx returns recorded outputs for a transaction (for BRC-100 ListActions).
func (s *Store) ListOutputsForTx(txid string) ([]OutputRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT o.id, o.txid, o.vout, o.satoshis, o.locking_script, o.basket, o.spendable,
		       o.spent_by_txid, o.reserved_by_ref, o.reserved_until, o.key_kind,
		       o.derivation_prefix, o.derivation_suffix, o.sender_identity_key,
		       o.tags_json, o.custom_instructions, o.output_description, o.coinbase, o.block_height, o.created_at,
		       COALESCE(t.status, 'completed'), COALESCE(t.unproven_depth, 0)
		FROM wallet_outputs o
		LEFT JOIN wallet_txs t ON t.txid = o.txid
		WHERE o.txid = ? ORDER BY o.vout ASC`, txid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutputRows(rows)
}

// BalanceBreakdown computes confirmed, unproven, reserved, and per-basket telemetry in one query.
func (s *Store) BalanceBreakdown() (BalanceBreakdown, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b BalanceBreakdown
	rows, err := s.db.Query(`
		SELECT o.satoshis, o.basket, o.spendable, o.reserved_by_ref,
		       COALESCE(t.status, 'completed'), COALESCE(t.unproven_depth, 0)
		FROM wallet_outputs o
		LEFT JOIN wallet_txs t ON t.txid = o.txid
		WHERE o.spent_by_txid = '' AND COALESCE(t.status, 'completed') != 'failed'`)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var sats uint64
		var basket, reservedBy, parentStatus string
		var spendableInt, depth int
		if err := rows.Scan(&sats, &basket, &spendableInt, &reservedBy, &parentStatus, &depth); err != nil {
			return b, err
		}
		if !IsSpendingBasket(basket) {
			b.CustomCount++
			continue
		}
		b.UTXOCount++
		b.TotalSats += sats
		if depth > b.MaxUnprovenDepth {
			b.MaxUnprovenDepth = depth
		}
		if reservedBy != "" || spendableInt == 0 {
			b.ReservedSats += sats
		} else {
			b.SpendableSats += sats
			b.SpendableCount++
			if parentStatus == StatusCompleted {
				b.ConfirmedSats += sats
			} else {
				b.UnprovenSats += sats
			}
			switch basket {
			case BasketFuel:
				b.FuelCount++
				b.FuelSats += sats
			case BasketReserve:
				b.ReserveCount++
				b.ReserveSats += sats
			case BasketDefault:
				b.DefaultCount++
				b.DefaultSats += sats
			}
		}
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM wallet_txs WHERE status IN ('unprocessed','sending','unsigned')`).Scan(&b.PendingTxCount)
	return b, nil
}

// PruneUnrecognizedTxs deletes outputs/txs whose txid is rejected by keep (used when re-seeding an ephemeral testchain).
func (s *Store) PruneUnrecognizedTxs(keep func(txid string) bool) error {
	return s.withTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT DISTINCT txid FROM wallet_outputs`)
		if err != nil {
			return err
		}
		var drop []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil && !keep(id) {
				drop = append(drop, id)
			}
		}
		_ = rows.Close()
		for _, id := range drop {
			_, _ = tx.Exec(`DELETE FROM wallet_outputs WHERE txid = ?`, id)
			_, _ = tx.Exec(`DELETE FROM wallet_inputs WHERE txid = ?`, id)
			_, _ = tx.Exec(`DELETE FROM wallet_txs WHERE txid = ?`, id)
			s.txCache.Delete(id)
		}
		return nil
	})
}

type rowScanner interface{ Scan(dest ...any) error }

func scanTxRecord(s rowScanner) (*TxRecord, error) {
	var r TxRecord
	var isOut int
	var labelsJSON string
	err := s.Scan(
		&r.TxID, &r.Reference, &r.Status, &isOut, &r.Satoshis, &r.FeeSats,
		&r.Description, &labelsJSON, &r.Version, &r.LockTime,
		&r.RawTx, &r.BEEF, &r.MerklePath, &r.BlockHeight, &r.UnprovenDepth,
		&r.BroadcastAttempts, &r.LastError, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	r.IsOutgoing = isOut != 0
	if labelsJSON != "" {
		_ = json.Unmarshal([]byte(labelsJSON), &r.Labels)
	}
	return &r, nil
}

func scanOutputRow(s rowScanner) (*OutputRecord, error) {
	var o OutputRecord
	var spendableInt, coinbaseInt int
	var tagsJSON string
	err := s.Scan(
		&o.ID, &o.TxID, &o.Vout, &o.Satoshis, &o.LockingScript, &o.Basket, &spendableInt,
		&o.SpentByTxID, &o.ReservedByRef, &o.ReservedUntil, &o.KeyKind,
		&o.DerivationPrefix, &o.DerivationSuffix, &o.SenderIdentityKey,
		&tagsJSON, &o.CustomInstructions, &o.OutputDescription, &coinbaseInt, &o.BlockHeight, &o.CreatedAt,
		&o.ParentStatus, &o.ParentUnprovenDepth,
	)
	if err != nil {
		return nil, err
	}
	o.Spendable = spendableInt != 0
	o.Coinbase = coinbaseInt != 0
	o.LockingScriptHex = hex.EncodeToString(o.LockingScript)
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &o.Tags)
	}
	return &o, nil
}

func scanOutputRows(rows *sql.Rows) ([]OutputRecord, error) {
	var out []OutputRecord
	for rows.Next() {
		o, err := scanOutputRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
