package wallet

import (
	"container/list"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
	_ "modernc.org/sqlite"
)

// identityFile mirrors bcommon/bwallet's identity.json schema ({"wif":"..."}) so existing
// Nektar wallets preserve their exact root key, identity pubkey, and BRC-43 funding address.
type identityFile struct {
	WIF         string `json:"wif,omitempty"`
	IdentityPub string `json:"identityPub,omitempty"`
}

// LoadOrCreateIdentity loads or atomically creates identity.json under dir.
// When dir is empty, an ephemeral in-memory root key is returned.
func LoadOrCreateIdentity(dir string) (*ec.PrivateKey, error) {
	if dir == "" {
		return ec.NewPrivateKey()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wallet: mkdir %s: %w", dir, err)
	}
	idPath := filepath.Join(dir, "identity.json")
	if priv, err := loadIdentityFile(idPath); err == nil {
		return priv, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	priv, err := ec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("wallet: generate root key: %w", err)
	}
	f := identityFile{
		WIF: priv.Wif(),
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	defer clear(b)
	// Atomic O_CREATE|O_EXCL prevents concurrent first-boot processes from overwriting a live root key.
	fd, err := os.OpenFile(idPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			zeroECKey(priv)
			return loadIdentityFile(idPath)
		}
		return nil, fmt.Errorf("wallet: create %s: %w", idPath, err)
	}
	_, wErr := fd.Write(b)
	sErr := fd.Sync()
	cErr := fd.Close()
	if wErr != nil || sErr != nil || cErr != nil {
		_ = os.Remove(idPath)
		if wErr != nil {
			return nil, fmt.Errorf("wallet: write %s: %w", idPath, wErr)
		}
		if sErr != nil {
			return nil, fmt.Errorf("wallet: sync %s: %w", idPath, sErr)
		}
		return nil, fmt.Errorf("wallet: close %s: %w", idPath, cErr)
	}
	if err := hardenFileACL(idPath); err != nil {
		return nil, fmt.Errorf("wallet: harden ACL on %s: %w", idPath, err)
	}
	return priv, nil
}

func loadIdentityFile(idPath string) (*ec.PrivateKey, error) {
	raw, err := os.ReadFile(idPath)
	if err != nil {
		return nil, fmt.Errorf("wallet: read %s: %w", idPath, err)
	}
	defer clear(raw)
	if err := hardenFileACL(idPath); err != nil {
		return nil, fmt.Errorf("wallet: harden ACL on %s: %w", idPath, err)
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("wallet: parse %s: %w", idPath, err)
	}
	if f.WIF != "" {
		priv, wErr := ec.PrivateKeyFromWif(f.WIF)
		if wErr != nil {
			return nil, fmt.Errorf("wallet: decode wif in %s: %w", idPath, wErr)
		}
		return priv, nil
	}
	return nil, fmt.Errorf("wallet: %s missing wif", idPath)
}

// openPureGoGORM opens a pure-Go modernc.org/sqlite writer connection (MaxOpenConns=1 for
// contention-free GORM writes) plus a dedicated read-only WAL reader pool (MaxOpenConns=4)
// for lock-free concurrent Balance / ListUTXOs / ListRecentActions queries under heavy write load.
func openPureGoGORM(dbPath string, inMemory bool) (*sql.DB, *sql.DB, *gorm.DB, error) {
	var dsn string
	isMem := inMemory || dbPath == "" || dbPath == ":memory:"
	if isMem {
		dsn = ":memory:?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	} else {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
			return nil, nil, nil, fmt.Errorf("wallet: mkdir db dir: %w", err)
		}
		dsn = fmt.Sprintf(
			"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(256)&_pragma=journal_size_limit(16777216)",
			filepath.ToSlash(dbPath),
		)
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wallet: open modernc sqlite: %w", err)
	}
	// SQLite allows one writer at a time; pinning MaxOpenConns(1) prevents SQLITE_BUSY across
	// concurrent goroutines inside go-wallet-toolbox's GORM transactions (where dbretry only
	// retries Postgres class-40 conflicts).
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, nil, nil, fmt.Errorf("wallet: ping modernc sqlite: %w", err)
	}
	if !isMem {
		if err := hardenFileACL(dbPath); err != nil {
			_ = sqlDB.Close()
			return nil, nil, nil, fmt.Errorf("wallet: harden ACL on %s: %w", dbPath, err)
		}
	}

	gormDB, err := gorm.Open(sqlite.Dialector{
		DriverName: "sqlite",
		Conn:       sqlDB,
	}, &gorm.Config{
		Logger: glogger.Discard,
		NamingStrategy: schema.NamingStrategy{
			TablePrefix:   defs.DefaultTablePrefix,
			SingularTable: false,
		},
		TranslateError: true,
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, nil, fmt.Errorf("wallet: gorm open over modernc sqlite: %w", err)
	}

	readDB := sqlDB
	if !isMem {
		roDSN := fmt.Sprintf(
			"file:%s?mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(5000)",
			filepath.ToSlash(dbPath),
		)
		if rdb, rErr := sql.Open("sqlite", roDSN); rErr == nil {
			rdb.SetMaxOpenConns(4)
			rdb.SetMaxIdleConns(4)
			if rdb.Ping() == nil {
				readDB = rdb
			} else {
				_ = rdb.Close()
			}
		}
	}
	return sqlDB, readDB, gormDB, nil
}

// txLRU is a thread-safe bounded LRU cache for parsed transactions.
type txLRU struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	items map[string]*list.Element
}

type txLRUEntry struct {
	key string
	tx  *transaction.Transaction
}

func newTxLRU(capacity int) *txLRU {
	if capacity <= 0 {
		capacity = DefaultTxCacheSize
	}
	return &txLRU{
		cap:   capacity,
		ll:    list.New(),
		items: make(map[string]*list.Element, capacity),
	}
}

func (c *txLRU) Get(txid string) (*transaction.Transaction, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[txid]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*txLRUEntry).tx, true
	}
	return nil, false
}

func (c *txLRU) Put(txid string, tx *transaction.Transaction) {
	if tx == nil || txid == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[txid]; ok {
		el.Value.(*txLRUEntry).tx = tx
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&txLRUEntry{key: txid, tx: tx})
	c.items[txid] = el
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.ll.Remove(back)
		delete(c.items, back.Value.(*txLRUEntry).key)
	}
}

func (c *txLRU) Delete(txid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[txid]; ok {
		c.ll.Remove(el)
		delete(c.items, txid)
	}
}

// EvictFromHeight removes any cached transactions claiming a MerklePath at or above forkHeight
// so reorged proofs are never served stale from memory.
func (c *txLRU) EvictFromHeight(forkHeight uint32) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	evicted := 0
	for txid, el := range c.items {
		entry := el.Value.(*txLRUEntry)
		if entry != nil && entry.tx != nil && entry.tx.MerklePath != nil && entry.tx.MerklePath.BlockHeight >= forkHeight {
			c.ll.Remove(el)
			delete(c.items, txid)
			evicted++
		}
	}
	return evicted
}
