package wallet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/brc29"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/entity"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/storage"
	toolboxwallet "github.com/bsv-blockchain/go-wallet-toolbox/pkg/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk/primitives"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/guard"
	"gorm.io/gorm"
)

type toolboxTables struct {
	outputs      string
	transactions string
	userUTXOs    string
	knownTxs     string
}

// Engine is Nektar's pure-Go BRC-100 embedded wallet powered by the official BSV Association
// go-wallet-toolbox (pkg/wallet + pkg/storage + pkg/brc29) running zero-CGO over modernc.org/sqlite
// via storage.WithGORM and Nektar's bcommon wdk.Services adapter (zero Kubernetes/Teranode deps).
type Engine struct {
	cfg        Config
	profile    FuelProfile
	fuelDenom  uint64
	fanoutMax  uint64
	rootKey    *ec.PrivateKey
	keyDeriver *sdkwallet.CachedKeyDeriver
	fundKey    *ec.PrivateKey
	fundAddr   string
	fundScript *script.Script
	signer     *bwallet.Signer

	sqlDB    *sql.DB
	readDB   *sql.DB
	gormDB   *gorm.DB
	tables   toolboxTables
	txCache  *txLRU
	svc      *bcommonServices
	provider *storage.Provider
	tb       *toolboxwallet.Wallet
	userID   int

	fuel    *FuelKeeper
	monitor *Monitor

	absorbMu sync.Mutex
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
	closeMu  sync.Mutex
	closed   bool
}

var _ sdkwallet.Interface = (*Engine)(nil)

// Open initializes identity.json, pure-Go modernc.org/sqlite + GORM, go-wallet-toolbox StorageProvider
// (with StrategyThroughput enabled), and *toolboxwallet.Wallet, and migrates any legacy wallet.json.
func Open(cfg Config) (*Engine, error) {
	if cfg.Originator == "" {
		cfg.Originator = "nektar"
	}
	if cfg.Profile.FundProtocol.Protocol == "" {
		cfg.Profile = bwallet.Profile{
			FundProtocol:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar fund"},
			FundKeyID:      "nektar-fund",
			FundBasket:     BasketFunding,
			Version:        "0.4.0",
			LegacyPoolFile: "nektar-pool.json",
		}
	}
	if cfg.MaxUnprovenDepth <= 0 {
		cfg.MaxUnprovenDepth = DefaultMaxUnprovenDepth
	}
	if cfg.ReservationTTL <= 0 {
		cfg.ReservationTTL = DefaultReservationTTL
	}
	if cfg.TxCacheSize <= 0 {
		cfg.TxCacheSize = DefaultTxCacheSize
	}
	if cfg.FeeSource == nil {
		cfg.FeeSource = feepolicy.Static(DefaultNetworkFees)
	}
	prof := StandardFuelProfile()
	if cfg.FuelProfile != nil {
		prof = *cfg.FuelProfile
	}

	rootKey, err := LoadOrCreateIdentity(cfg.Dir)
	if err != nil {
		return nil, err
	}
	deriver := sdkwallet.NewCachedKeyDeriver(rootKey, 256)

	// Derive Nektar's canonical BRC-43 static P2PKH funding key (identical to bcommon/bwallet).
	fundKey, err := deriver.DerivePrivateKey(
		cfg.Profile.FundProtocol,
		cfg.Profile.FundKeyID,
		sdkwallet.Counterparty{Type: sdkwallet.CounterpartyTypeSelf},
	)
	if err != nil {
		return nil, fmt.Errorf("wallet: derive BRC-43 fund key: %w", err)
	}
	fundAddrObj, err := script.NewAddressFromPublicKey(fundKey.PubKey(), cfg.Mainnet)
	if err != nil {
		return nil, fmt.Errorf("wallet: derive fund address: %w", err)
	}
	fundScript, err := p2pkh.Lock(fundAddrObj)
	if err != nil {
		return nil, fmt.Errorf("wallet: derive fund locking script: %w", err)
	}

	var dbPath string
	if cfg.Dir != "" && !cfg.InMemoryStore {
		dbPath = filepath.Join(cfg.Dir, "wallet.sqlite")
	}
	sqlDB, readDB, gormDB, err := openPureGoGORM(dbPath, cfg.InMemoryStore || dbPath == "")
	if err != nil {
		return nil, err
	}
	closeDBs := func() {
		if readDB != nil && readDB != sqlDB {
			_ = readDB.Close()
		}
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}

	txCache := newTxLRU(cfg.TxCacheSize)
	svc := newBcommonServices(cfg, txCache)

	chainNet := defs.NetworkTestnet
	if cfg.Mainnet {
		chainNet = defs.NetworkMainnet
	}

	feeSatPerKB := initialFeeRateSatPerKB(cfg.FeeSource)
	utxoCfg := buildUTXOManagementConfig(prof, feeSatPerKB)
	minAgeSec := uint64(cfg.ReservationTTL.Seconds())
	if minAgeSec == 0 {
		minAgeSec = 1
	}

	tbLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	provider, err := storage.NewGORMProvider(
		chainNet,
		svc,
		storage.WithGORM(gormDB),
		storage.WithLogger(tbLogger),
		storage.WithFeeModel(defs.FeeModel{Type: defs.SatPerKB, Value: int64(feeSatPerKB)}),
		storage.WithUTXOManagement(utxoCfg),
		storage.WithChangeBasket(defs.ChangeBasket{
			NumberOfDesiredUTXOs:    8,
			MinimumDesiredUTXOValue: 1000,
			MaxChangeOutputsPerTx:   1,
		}),
		storage.WithFailAbandoned(defs.FailAbandoned{
			MinTransactionAgeSeconds: uint(minAgeSec),
		}),
		storage.WithSynchronizeTxStatuses(defs.SynchronizeTxStatuses{
			MaxAttempts:            10,
			BlocksDelay:            0,
			CheckNoSendPeriodHours: 24,
		}),
	)
	if err != nil {
		closeDBs()
		return nil, fmt.Errorf("wallet: init go-wallet-toolbox storage provider: %w", err)
	}

	initCtx := context.Background()
	storageIDKey := hex.EncodeToString(rootKey.PubKey().Compressed())
	if _, err := provider.Migrate(initCtx, "nektar-sqlite", storageIDKey); err != nil {
		provider.Stop(initCtx)
		closeDBs()
		return nil, fmt.Errorf("wallet: migrate go-wallet-toolbox storage: %w", err)
	}
	tables := discoverAndIndexToolboxTables(sqlDB)

	// Ensure user + baskets exist in storage right away so Balance/ListUTXOs work immediately on fresh DBs.
	userResp, err := provider.FindOrInsertUser(initCtx, storageIDKey)
	if err != nil {
		provider.Stop(initCtx)
		closeDBs()
		return nil, fmt.Errorf("wallet: init toolbox user: %w", err)
	}

	tbWallet, err := toolboxwallet.New(
		chainNet,
		rootKey,
		provider,
		toolboxwallet.WithServices(svc),
		toolboxwallet.WithLogger(tbLogger),
		toolboxwallet.WithAutoKnownTxids(false), // always return complete self-contained AtomicBEEFs to counterparties
	)
	if err != nil {
		provider.Stop(initCtx)
		closeDBs()
		return nil, fmt.Errorf("wallet: init go-wallet-toolbox wallet: %w", err)
	}

	e := &Engine{
		cfg:        cfg,
		profile:    prof,
		fuelDenom:  utxoCfg.Throughput.DenominationSatoshis,
		fanoutMax:  utxoCfg.Throughput.FanoutOutputsPerTx,
		rootKey:    rootKey,
		keyDeriver: deriver,
		fundKey:    fundKey,
		fundAddr:   fundAddrObj.AddressString,
		fundScript: fundScript,
		sqlDB:      sqlDB,
		readDB:     readDB,
		gormDB:     gormDB,
		tables:     tables,
		txCache:    txCache,
		svc:        svc,
		provider:   provider,
		tb:         tbWallet,
		userID:     userResp.User.UserID,
	}
	e.signer = &bwallet.Signer{
		Interface:  e,
		Identity:   rootKey.PubKey(),
		Originator: cfg.Originator,
		Mainnet:    cfg.Mainnet,
		Profile:    cfg.Profile,
	}
	e.fuel = newFuelKeeper(e)
	e.monitor = newMonitor(e)
	svc.ownedOutputSum = e.ownedOutputSumForTx

	// Reclaim any orphaned NoSend transactions left behind by a previous process crash.
	if !cfg.InMemoryStore {
		if n, rErr := e.ReconcileOrphanedNoSend(initCtx); rErr == nil && n > 0 {
			e.logf("wallet: reclaimed %d orphaned NoSend transaction(s) on boot", n)
		}
	}

	// Migrate legacy bcommon/bwallet wallet.json or nektar-pool.json into go-wallet-toolbox once.
	if cfg.Dir != "" && !cfg.InMemoryStore {
		if n, mErr := e.migrateLegacyPool(initCtx, cfg.Dir, cfg.Profile.LegacyPoolFile); mErr != nil {
			e.logf("wallet: legacy pool migration warning: %v", mErr)
		} else if n > 0 {
			e.logf("wallet: migrated %d UTXOs from legacy pool into go-wallet-toolbox", n)
		}
	}

	return e, nil
}

func initialFeeRateSatPerKB(src feepolicy.Source) uint64 {
	if src == nil {
		return 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	f, err := src.Fees(ctx)
	if err == nil && !f.Rate.IsZero() && f.Rate.Bytes > 0 {
		rate := (f.Rate.Sats*1000 + f.Rate.Bytes - 1) / f.Rate.Bytes
		if rate >= 1 {
			return rate
		}
	}
	return 100
}

func buildUTXOManagementConfig(prof FuelProfile, feeSatPerKB uint64) defs.UTXOManagement {
	cfg := defs.DefaultUTXOManagement()
	cfg.Strategy = defs.StrategyThroughput
	denom := prof.FuelSats
	minFloor := (148*feeSatPerKB + 999) / 1000
	if denom <= minFloor {
		denom = minFloor + 1000
	}
	cfg.Throughput.DenominationSatoshis = denom
	cfg.Throughput.TargetTPS = 10
	target := uint64(prof.FuelTarget)
	if target == 0 {
		target = 24
	}
	cfg.Throughput.TargetPoolSize = target
	lowWater := uint64(prof.LowWaterPct)
	if lowWater == 0 || lowWater > 100 {
		lowWater = 30
	}
	cfg.Throughput.LowWaterPercent = lowWater
	cfg.Throughput.HighWaterPercent = 100
	cfg.Throughput.SpendPolicy = defs.SpendPolicyAny
	cfg.Throughput.PoolBasket = BasketFuel
	cfg.Throughput.ReserveBasket = BasketReserve
	maxOut := uint64(prof.MaxFanOutOutputs)
	if maxOut == 0 {
		maxOut = 64
	}
	cfg.Throughput.FanoutOutputsPerTx = maxOut
	cfg.Throughput.FanoutMaxTxsPerRound = 16
	cfg.Throughput.FanoutTreeDepth = 2
	cfg.Throughput.ConsolidationInputsPerTx = 256
	cfg.Throughput.TopUp.Enabled = false // Nektar FuelKeeper drives FanOutFuel cleanly
	return cfg
}

// StartBackground launches the FuelKeeper and Monitor loops until ctx is canceled or Close is called.
func (e *Engine) StartBackground(ctx context.Context) {
	e.closeMu.Lock()
	if e.closed || e.bgCancel != nil {
		e.closeMu.Unlock()
		return
	}
	bgCtx, cancel := context.WithCancel(ctx)
	e.bgCancel = cancel
	e.closeMu.Unlock()

	e.bgWG.Add(2)
	go func() {
		defer e.bgWG.Done()
		e.fuel.run(bgCtx)
	}()
	go func() {
		defer e.bgWG.Done()
		e.monitor.run(bgCtx)
	}()
}

// Close stops background workers, drains go-wallet-toolbox's storage provider, and closes SQLite.
func (e *Engine) Close() error {
	e.closeMu.Lock()
	if e.closed {
		e.closeMu.Unlock()
		return nil
	}
	e.closed = true
	cancel := e.bgCancel
	e.closeMu.Unlock()

	if cancel != nil {
		cancel()
	}
	e.bgWG.Wait()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if e.tb != nil {
		e.tb.Close()
	}
	if e.provider != nil {
		e.provider.Stop(stopCtx)
	}
	if e.readDB != nil && e.readDB != e.sqlDB {
		_ = e.readDB.Close()
	}
	if e.sqlDB != nil {
		// Reclaim WAL pages cleanly on shutdown so wallet.sqlite-wal never lingers bloated on disk.
		_, _ = e.sqlDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		return e.sqlDB.Close()
	}
	return nil
}

// discoverAndIndexToolboxTables inspects sqlite_master once after Migrate to locate the exact
// GORM table names (honoring defs.DefaultTablePrefix) and adds covering partial indexes for
// O(unspent) Balance, ListUTXOs, and ListRecentActions queries without altering toolbox schema.
func discoverAndIndexToolboxTables(db *sql.DB) toolboxTables {
	t := toolboxTables{
		outputs:      defs.DefaultTablePrefix + "outputs",
		transactions: defs.DefaultTablePrefix + "transactions",
		userUTXOs:    defs.DefaultTablePrefix + "user_utxos",
		knownTxs:     defs.DefaultTablePrefix + "known_txes",
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) != nil {
				continue
			}
			switch {
			case name == t.outputs || strings.HasSuffix(name, "_outputs") || name == "outputs":
				t.outputs = name
			case name == t.transactions || strings.HasSuffix(name, "_transactions") || name == "transactions":
				t.transactions = name
			case name == t.userUTXOs || strings.HasSuffix(name, "_user_utxos") || name == "user_utxos":
				t.userUTXOs = name
			case name == t.knownTxs || strings.HasSuffix(name, "_known_txes") || strings.HasSuffix(name, "_known_txs") || name == "known_txes" || name == "known_txs":
				t.knownTxs = name
			}
		}
	}
	// Covering partial indexes for O(unspent) reads and O(1) per-tx fee aggregation.
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_outputs_unspent ON "%s"(user_id, spendable, basket_name) WHERE deleted_at IS NULL AND spent_by IS NULL`, t.outputs))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_outputs_tx_fee ON "%s"(transaction_id, satoshis) WHERE deleted_at IS NULL`, t.outputs))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_outputs_spent_fee ON "%s"(spent_by, satoshis) WHERE deleted_at IS NULL AND spent_by IS NOT NULL`, t.outputs))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_txs_user_id_desc ON "%s"(user_id, id DESC) WHERE deleted_at IS NULL`, t.transactions))
	_, _ = db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_nektar_txs_user_status ON "%s"(user_id, status) WHERE deleted_at IS NULL`, t.transactions))
	// Durable pre-broadcast journal for BRC-29 desktop fund/sweep transfers (prevents orphaned funds on crash).
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS nektar_pending_brc29 (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			direction          TEXT NOT NULL,
			wire_url           TEXT NOT NULL DEFAULT '',
			counterparty_pub   TEXT NOT NULL,
			derivation_prefix  TEXT NOT NULL,
			derivation_suffix  TEXT NOT NULL,
			locking_script_hex TEXT NOT NULL DEFAULT '',
			satoshis           INTEGER NOT NULL DEFAULT 0,
			tx_id              TEXT NOT NULL DEFAULT '',
			output_index       INTEGER NOT NULL DEFAULT 0,
			atomic_beef        BLOB,
			status             TEXT NOT NULL DEFAULT 'pending',
			created_at         INTEGER NOT NULL,
			updated_at         INTEGER NOT NULL
		)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_nektar_pending_brc29_status ON nektar_pending_brc29(status, id)`)
	return t
}

func (e *Engine) logf(format string, args ...any) {
	if e.cfg.Logger != nil {
		e.cfg.Logger(format, args...)
	}
}

// Public accessors.
func (e *Engine) Toolbox() *toolboxwallet.Wallet { return e.tb }
func (e *Engine) Provider() *storage.Provider    { return e.provider }
func (e *Engine) IdentityKey() *ec.PublicKey     { return e.rootKey.PubKey() }
func (e *Engine) IdentityPubHex() string         { return hex.EncodeToString(e.rootKey.PubKey().Compressed()) }
func (e *Engine) FundAddress() string            { return e.fundAddr }
func (e *Engine) FundScript() *script.Script     { return e.fundScript }
func (e *Engine) Signer() *bwallet.Signer        { return e.signer }
func (e *Engine) FuelKeeper() *FuelKeeper        { return e.fuel }
func (e *Engine) Monitor() *Monitor              { return e.monitor }
func (e *Engine) FuelDenom() uint64              { return e.fuelDenom }

func (e *Engine) ownedOutputSumForTx(txid string) (uint64, bool) {
	if e.readDB == nil || txid == "" {
		return 0, false
	}
	q := fmt.Sprintf(`
		SELECT t.id, COALESCE((
			SELECT SUM(o.satoshis)
			FROM "%s" o
			WHERE o.transaction_id = t.id
			  AND o.deleted_at IS NULL
			  AND o.basket_name IN ('default','reserve','fuel','nektar funding')
		), 0)
		FROM "%s" t
		WHERE t.tx_id = ?
		  AND t.user_id = ?
		  AND t.deleted_at IS NULL
		LIMIT 1`,
		e.tables.outputs, e.tables.transactions,
	)
	var tid, sum int64
	if err := e.readDB.QueryRow(q, txid, e.userID).Scan(&tid, &sum); err != nil {
		return 0, false
	}
	if sum < 0 {
		sum = 0
	}
	return uint64(sum), true
}

func (e *Engine) countDustUTXOs(ctx context.Context, belowSats uint64) int {
	if e.readDB == nil || belowSats == 0 {
		return 0
	}
	q := fmt.Sprintf(`
		SELECT COUNT(1)
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
		WHERE o.user_id = ?
		  AND o.deleted_at IS NULL
		  AND o.spent_by IS NULL
		  AND o.spendable = 1
		  AND u.reserved_by_id IS NULL
		  AND o.satoshis > 0
		  AND o.satoshis <= ?
		  AND o.basket_name IN ('default','fuel')
		  AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed','submission_unknown')`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs,
	)
	var cnt int
	_ = e.readDB.QueryRowContext(ctx, q, e.userID, belowSats).Scan(&cnt)
	return cnt
}

// SavePendingBRC29 persists BRC-29 derivation nonces BEFORE broadcasting a desktop fund/sweep tx.
func (e *Engine) SavePendingBRC29(ctx context.Context, rec PendingBRC29Record) (int64, error) {
	if e.sqlDB == nil {
		return 0, nil
	}
	now := time.Now().Unix()
	if rec.Status == "" {
		rec.Status = "pending"
	}
	res, err := e.sqlDB.ExecContext(ctx, `
		INSERT INTO nektar_pending_brc29
		(direction, wire_url, counterparty_pub, derivation_prefix, derivation_suffix, locking_script_hex, satoshis, tx_id, output_index, atomic_beef, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Direction, rec.WireURL, rec.CounterpartyPub, rec.DerivationPrefix, rec.DerivationSuffix,
		rec.LockingScriptHex, rec.Satoshis, rec.TxID, rec.OutputIndex, rec.AtomicBEEF, rec.Status, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePendingBRC29Tx attaches the broadcast txid, outputIndex, and AtomicBEEF to a pending BRC-29 row.
func (e *Engine) UpdatePendingBRC29Tx(ctx context.Context, id int64, txid string, outIdx uint32, beef []byte, sats uint64) error {
	if e.sqlDB == nil || id <= 0 {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `
		UPDATE nektar_pending_brc29
		SET tx_id = ?, output_index = ?, atomic_beef = ?, satoshis = CASE WHEN ? > 0 THEN ? ELSE satoshis END, updated_at = ?
		WHERE id = ?`,
		txid, outIdx, beef, sats, sats, time.Now().Unix(), id)
	return err
}

// MarkPendingBRC29Done marks a pending BRC-29 transfer internalized and complete.
func (e *Engine) MarkPendingBRC29Done(ctx context.Context, id int64) error {
	if e.sqlDB == nil || id <= 0 {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `UPDATE nektar_pending_brc29 SET status = 'done', updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// MarkPendingBRC29DoneByTx marks a pending BRC-29 transfer complete by its broadcast txid and vout.
func (e *Engine) MarkPendingBRC29DoneByTx(ctx context.Context, txid string, outIdx uint32) error {
	if e.sqlDB == nil || txid == "" {
		return nil
	}
	_, err := e.sqlDB.ExecContext(ctx, `UPDATE nektar_pending_brc29 SET status = 'done', updated_at = ? WHERE tx_id = ? AND output_index = ?`, time.Now().Unix(), txid, outIdx)
	return err
}

// ListPendingBRC29 returns all incomplete pending BRC-29 transfers ordered oldest first.
func (e *Engine) ListPendingBRC29(ctx context.Context) ([]PendingBRC29Record, error) {
	if e.readDB == nil {
		return nil, nil
	}
	rows, err := e.readDB.QueryContext(ctx, `
		SELECT id, direction, wire_url, counterparty_pub, derivation_prefix, derivation_suffix, locking_script_hex, satoshis, tx_id, output_index, atomic_beef, status, created_at, updated_at
		FROM nektar_pending_brc29 WHERE status = 'pending' ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingBRC29Record
	for rows.Next() {
		var r PendingBRC29Record
		if err := rows.Scan(&r.ID, &r.Direction, &r.WireURL, &r.CounterpartyPub, &r.DerivationPrefix, &r.DerivationSuffix,
			&r.LockingScriptHex, &r.Satoshis, &r.TxID, &r.OutputIndex, &r.AtomicBEEF, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FeeRateSatPerKB returns the live Arcade /v1/policy sat/kB rate (or static fallback).
func (e *Engine) FeeRateSatPerKB(ctx context.Context) uint64 {
	if e.cfg.FeeSource != nil {
		if f, err := e.cfg.FeeSource.Fees(ctx); err == nil && !f.Rate.IsZero() && f.Rate.Bytes > 0 {
			return (f.Rate.Sats*1000 + f.Rate.Bytes - 1) / f.Rate.Bytes
		}
	}
	return 100
}

func (e *Engine) normalizeOrig(orig string) string {
	if strings.TrimSpace(orig) == "" {
		return e.cfg.Originator
	}
	return strings.TrimSpace(orig)
}

func ensureDesc(s, fallback string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 5 {
		return s
	}
	if s == "" {
		return fallback
	}
	return s + " action"
}

// -----------------------------------------------------------------------------
// BRC-100 sdkwallet.Interface (28 methods delegated to go-wallet-toolbox)
// -----------------------------------------------------------------------------

func (e *Engine) CreateAction(ctx context.Context, args sdkwallet.CreateActionArgs, originator string) (*sdkwallet.CreateActionResult, error) {
	return e.createActionGuarded(ctx, args, originator, false, false)
}

func (e *Engine) createActionGuarded(ctx context.Context, args sdkwallet.CreateActionArgs, originator string, overrideGuard, skipGuard bool) (*sdkwallet.CreateActionResult, error) {
	orig := e.normalizeOrig(originator)
	args.Description = ensureDesc(args.Description, "nektar wallet action")
	for i := range args.Outputs {
		args.Outputs[i].OutputDescription = ensureDesc(args.Outputs[i].OutputDescription, "nektar output")
	}
	for i := range args.Inputs {
		args.Inputs[i].InputDescription = ensureDesc(args.Inputs[i].InputDescription, "nektar input")
	}

	if args.Options == nil {
		args.Options = &sdkwallet.CreateActionOptions{}
	}
	if args.Options.AcceptDelayedBroadcast == nil {
		d := e.profile.DefaultAcceptDelayed
		args.Options.AcceptDelayedBroadcast = &d
	}

	isNoSend := args.Options.NoSend != nil && *args.Options.NoSend
	isSignLater := args.Options.SignAndProcess != nil && !*args.Options.SignAndProcess
	isPureSendWith := len(args.Inputs) == 0 && len(args.Outputs) == 0 && len(args.Options.SendWith) > 0

	var outTotal uint64
	hasSweepAll := false
	for _, o := range args.Outputs {
		if o.Satoshis == primitives.SweepMaxSatoshis {
			hasSweepAll = true
		} else {
			outTotal += o.Satoshis
		}
	}

	// Read-only SpendGuard pre-check for fast fail (zero phantom deduction; real deduction happens
	// once post-signing inside bcommonServices.PostFromBEEF with exact fee + external net spend).
	if !skipGuard && !overrideGuard && !isNoSend && !isSignLater && !isPureSendWith && e.cfg.SpendGuardCheck != nil {
		estFee := e.estimateFeeSats(ctx, max(1, len(args.Inputs)), len(args.Outputs)+1)
		if err := e.cfg.SpendGuardCheck(estFee, outTotal+estFee); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, err)
		}
	}
	if overrideGuard || skipGuard {
		ctx = WithOverrideGuard(ctx)
	} else if !isPureSendWith {
		if hasSweepAll {
			ctx = withSweepAllExternalSpend(ctx)
		} else {
			ctx = withExternalSpend(ctx, outTotal)
		}
	}

	ctx, errSink := withPostErrSink(ctx)
	res, err := e.tb.CreateAction(ctx, args, orig)
	if err != nil && isNotEnoughFunds(err) && !isPureSendWith && e.svc.hasBroadcaster() {
		retried := false
		// 1. If static FundAddress() deposits are waiting in BasketFunding, absorb into native BRC-29 "default".
		if absorbed, aErr := e.AbsorbFundingBasket(ctx); aErr == nil && absorbed > 0 {
			res, err = e.tb.CreateAction(ctx, args, orig)
			retried = true
		}
		// 2. If funds are split across StrategyThroughput baskets (default + reserve + fuel) such that no
		// single basket covers this action alone, reclaim just enough reserve/fuel into "default" and retry!
		if err != nil && isNotEnoughFunds(err) {
			var outTotal uint64
			for _, o := range args.Outputs {
				if o.Satoshis != primitives.SweepMaxSatoshis {
					outTotal += o.Satoshis
				}
			}
			if reclaimed, rErr := e.reclaimPoolToDefault(ctx, outTotal+e.estimateFeeSats(ctx, 4, len(args.Outputs)+1)); rErr == nil && reclaimed > 0 {
				res, err = e.tb.CreateAction(ctx, args, orig)
				retried = true
			}
		}
		_ = retried
	}
	if err != nil {
		if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
			if errors.Is(sinkErr, ErrBroadcastUnknown) && sinkTxID != "" {
				_ = e.MarkSubmissionUnknown(ctx, sinkTxID)
			}
			return nil, fmt.Errorf("%w (%v)", sinkErr, err)
		}
		if errors.Is(err, ErrBroadcastUnknown) && res != nil && !res.Txid.IsEqual(&chainhash.Hash{}) {
			_ = e.MarkSubmissionUnknown(ctx, res.Txid.String())
		}
		return nil, err
	}

	if res != nil && isNoSend && !res.Txid.IsEqual(&chainhash.Hash{}) {
		e.svc.setPlannedSpend(res.Txid.String(), outTotal)
	}
	if res != nil && len(res.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(res.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	if !isSignLater && !isNoSend {
		e.fuel.Wake()
	}
	return res, nil
}

func (e *Engine) SignAction(ctx context.Context, args sdkwallet.SignActionArgs, originator string) (*sdkwallet.SignActionResult, error) {
	if args.Options == nil {
		args.Options = &sdkwallet.SignActionOptions{}
	}
	if args.Options.AcceptDelayedBroadcast == nil {
		d := e.profile.DefaultAcceptDelayed
		args.Options.AcceptDelayedBroadcast = &d
	}
	ctx, errSink := withPostErrSink(ctx)
	res, err := e.tb.SignAction(ctx, args, e.normalizeOrig(originator))
	if err != nil {
		if sinkTxID, sinkErr := errSink.get(); sinkErr != nil {
			if errors.Is(sinkErr, ErrBroadcastUnknown) && sinkTxID != "" {
				_ = e.MarkSubmissionUnknown(ctx, sinkTxID)
			}
			return nil, fmt.Errorf("%w (%v)", sinkErr, err)
		}
		return nil, err
	}
	if res != nil && len(res.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(res.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	e.fuel.Wake()
	return res, nil
}

func (e *Engine) AbortAction(ctx context.Context, args sdkwallet.AbortActionArgs, originator string) (*sdkwallet.AbortActionResult, error) {
	return e.tb.AbortAction(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ListActions(ctx context.Context, args sdkwallet.ListActionsArgs, originator string) (*sdkwallet.ListActionsResult, error) {
	return e.tb.ListActions(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) InternalizeAction(ctx context.Context, args sdkwallet.InternalizeActionArgs, originator string) (*sdkwallet.InternalizeActionResult, error) {
	args.Description = ensureDesc(args.Description, "nektar internalize action")
	res, err := e.tb.InternalizeAction(ctx, args, e.normalizeOrig(originator))
	if err != nil {
		return nil, err
	}
	if _, parsed, _, pErr := guard.ParseBEEF(args.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
		e.txCache.Put(parsed.TxID().String(), parsed)
	}
	return res, nil
}

func (e *Engine) ListOutputs(ctx context.Context, args sdkwallet.ListOutputsArgs, originator string) (*sdkwallet.ListOutputsResult, error) {
	res, err := e.tb.ListOutputs(ctx, args, e.normalizeOrig(originator))
	if err == nil && res != nil && res.TotalOutputs == 0 && args.Basket == BasketDefault && e.svc.hasBroadcaster() {
		if absorbed, aErr := e.AbsorbFundingBasket(ctx); aErr == nil && absorbed > 0 {
			return e.tb.ListOutputs(ctx, args, e.normalizeOrig(originator))
		}
	}
	return res, err
}

func (e *Engine) RelinquishOutput(ctx context.Context, args sdkwallet.RelinquishOutputArgs, originator string) (*sdkwallet.RelinquishOutputResult, error) {
	return e.tb.RelinquishOutput(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetPublicKey(ctx context.Context, args sdkwallet.GetPublicKeyArgs, originator string) (*sdkwallet.GetPublicKeyResult, error) {
	return e.tb.GetPublicKey(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RevealCounterpartyKeyLinkage(ctx context.Context, args sdkwallet.RevealCounterpartyKeyLinkageArgs, originator string) (*sdkwallet.RevealCounterpartyKeyLinkageResult, error) {
	return e.tb.RevealCounterpartyKeyLinkage(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RevealSpecificKeyLinkage(ctx context.Context, args sdkwallet.RevealSpecificKeyLinkageArgs, originator string) (*sdkwallet.RevealSpecificKeyLinkageResult, error) {
	return e.tb.RevealSpecificKeyLinkage(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) Encrypt(ctx context.Context, args sdkwallet.EncryptArgs, originator string) (*sdkwallet.EncryptResult, error) {
	return e.tb.Encrypt(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) Decrypt(ctx context.Context, args sdkwallet.DecryptArgs, originator string) (*sdkwallet.DecryptResult, error) {
	return e.tb.Decrypt(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) CreateHMAC(ctx context.Context, args sdkwallet.CreateHMACArgs, originator string) (*sdkwallet.CreateHMACResult, error) {
	return e.tb.CreateHMAC(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) VerifyHMAC(ctx context.Context, args sdkwallet.VerifyHMACArgs, originator string) (*sdkwallet.VerifyHMACResult, error) {
	return e.tb.VerifyHMAC(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) CreateSignature(ctx context.Context, args sdkwallet.CreateSignatureArgs, originator string) (*sdkwallet.CreateSignatureResult, error) {
	return e.tb.CreateSignature(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) VerifySignature(ctx context.Context, args sdkwallet.VerifySignatureArgs, originator string) (*sdkwallet.VerifySignatureResult, error) {
	return e.tb.VerifySignature(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) AcquireCertificate(ctx context.Context, args sdkwallet.AcquireCertificateArgs, originator string) (*sdkwallet.Certificate, error) {
	return e.tb.AcquireCertificate(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ListCertificates(ctx context.Context, args sdkwallet.ListCertificatesArgs, originator string) (*sdkwallet.ListCertificatesResult, error) {
	return e.tb.ListCertificates(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) ProveCertificate(ctx context.Context, args sdkwallet.ProveCertificateArgs, originator string) (*sdkwallet.ProveCertificateResult, error) {
	return e.tb.ProveCertificate(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) RelinquishCertificate(ctx context.Context, args sdkwallet.RelinquishCertificateArgs, originator string) (*sdkwallet.RelinquishCertificateResult, error) {
	return e.tb.RelinquishCertificate(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) DiscoverByIdentityKey(ctx context.Context, args sdkwallet.DiscoverByIdentityKeyArgs, originator string) (*sdkwallet.DiscoverCertificatesResult, error) {
	return e.tb.DiscoverByIdentityKey(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) DiscoverByAttributes(ctx context.Context, args sdkwallet.DiscoverByAttributesArgs, originator string) (*sdkwallet.DiscoverCertificatesResult, error) {
	return e.tb.DiscoverByAttributes(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) IsAuthenticated(ctx context.Context, args any, originator string) (*sdkwallet.AuthenticatedResult, error) {
	return e.tb.IsAuthenticated(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) WaitForAuthentication(ctx context.Context, args any, originator string) (*sdkwallet.AuthenticatedResult, error) {
	return e.tb.WaitForAuthentication(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetHeight(ctx context.Context, args any, originator string) (*sdkwallet.GetHeightResult, error) {
	return e.tb.GetHeight(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetHeaderForHeight(ctx context.Context, args sdkwallet.GetHeaderArgs, originator string) (*sdkwallet.GetHeaderResult, error) {
	return e.tb.GetHeaderForHeight(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetNetwork(ctx context.Context, args any, originator string) (*sdkwallet.GetNetworkResult, error) {
	return e.tb.GetNetwork(ctx, args, e.normalizeOrig(originator))
}

func (e *Engine) GetVersion(_ context.Context, _ any, _ string) (*sdkwallet.GetVersionResult, error) {
	return &sdkwallet.GetVersionResult{Version: WalletVersion}, nil
}

// -----------------------------------------------------------------------------
// NoSend broadcast bridge (for chain.Port BuildFundedTx -> Broadcast callers)
// -----------------------------------------------------------------------------

// SendNoSendTx broadcasts a previously created NoSend transaction via go-wallet-toolbox SendWith.
func (e *Engine) SendNoSendTx(ctx context.Context, txid string) error {
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return fmt.Errorf("wallet: invalid txid %q: %w", txid, err)
	}
	acceptDelayed := false
	res, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "broadcast nektar funded transaction",
		Options: &sdkwallet.CreateActionOptions{
			SendWith:               []chainhash.Hash{*h},
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		if lastErr := e.svc.takeLastErr(txid); lastErr != nil {
			if errors.Is(lastErr, ErrBroadcastUnknown) {
				_ = e.MarkSubmissionUnknown(ctx, txid)
			}
			return lastErr
		}
		if errors.Is(err, ErrBroadcastUnknown) {
			_ = e.MarkSubmissionUnknown(ctx, txid)
		}
		return err
	}
	for _, sw := range res.SendWithResults {
		if sw.Txid.String() == txid && sw.Status == sdkwallet.ActionResultStatusFailed {
			if lastErr := e.svc.takeLastErr(txid); lastErr != nil {
				if errors.Is(lastErr, ErrBroadcastUnknown) {
					_ = e.MarkSubmissionUnknown(ctx, txid)
				}
				return lastErr
			}
			return fmt.Errorf("wallet: broadcast failed for tx %s", txid)
		}
	}
	e.svc.takeLastErr(txid)
	e.fuel.Wake()
	return nil
}

// AbortNoSendByTxID looks up the true go-wallet-toolbox action reference for txid and aborts it,
// atomically releasing reserved inputs so UTXOs never remain locked after pre-broadcast refusal.
func (e *Engine) AbortNoSendByTxID(ctx context.Context, txid string) error {
	if e.readDB == nil || txid == "" {
		return nil
	}
	var refStr string
	q := fmt.Sprintf(`SELECT COALESCE(reference, '') FROM "%s" WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL LIMIT 1`, e.tables.transactions)
	if err := e.readDB.QueryRowContext(ctx, q, e.userID, txid).Scan(&refStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if refStr == "" {
		return nil
	}
	// go-wallet-toolbox stores transactions.reference as base64-encoded bytes.
	refBytes, err := base64.StdEncoding.DecodeString(refStr)
	if err != nil || len(refBytes) == 0 {
		refBytes = []byte(refStr)
	}
	if _, err := e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: refBytes}, e.cfg.Originator); err != nil {
		// Fall back to raw string bytes if toolbox stored literal reference bytes.
		if _, err2 := e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: []byte(refStr)}, e.cfg.Originator); err2 != nil {
			return err
		}
	}
	return nil
}

// MarkSubmissionUnknown transitions a transaction whose broadcast outcome is uncertain (timeout/EOF/5xx)
// to status='submission_unknown' so inputs remain reserved until Monitor.UnFail confirms or proves it.
func (e *Engine) MarkSubmissionUnknown(ctx context.Context, txid string) error {
	if e.sqlDB == nil || txid == "" {
		return nil
	}
	q := fmt.Sprintf(`UPDATE "%s" SET status = 'submission_unknown', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND tx_id = ? AND deleted_at IS NULL AND status NOT IN ('completed','failed','aborted')`, e.tables.transactions)
	_, err := e.sqlDB.ExecContext(ctx, q, e.userID, txid)
	return err
}

// ReconcileOrphanedNoSend aborts stale 'nosend' transactions older than ReservationTTL on boot
// so inputs reserved before a process crash are never permanently locked.
func (e *Engine) ReconcileOrphanedNoSend(ctx context.Context) (int, error) {
	if e.readDB == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-e.cfg.ReservationTTL)
	q := fmt.Sprintf(`SELECT COALESCE(tx_id, '') FROM "%s" WHERE user_id = ? AND deleted_at IS NULL AND status = 'nosend' AND updated_at < ? AND tx_id != ''`, e.tables.transactions)
	rows, err := e.readDB.QueryContext(ctx, q, e.userID, cutoff)
	if err != nil {
		return 0, err
	}
	var txids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			txids = append(txids, id)
		}
	}
	_ = rows.Close()
	aborted := 0
	for _, id := range txids {
		if err := e.AbortNoSendByTxID(ctx, id); err == nil {
			aborted++
		}
	}
	return aborted, nil
}

// -----------------------------------------------------------------------------
// Static FundAddress() -> Native BRC-29 Absorption & Legacy Migration
// -----------------------------------------------------------------------------

// AbsorbFundingBasket sweeps any spendable static-P2PKH outputs in "nektar funding"
// (from FundAddress() deposits, faucets, testchain coinbases, or legacy wallet.json)
// into go-wallet-toolbox's native BRC-29 "default" change basket via a single BRC-100 action.
func (e *Engine) AbsorbFundingBasket(ctx context.Context) (int, error) {
	if !e.svc.hasBroadcaster() {
		return 0, nil
	}
	e.absorbMu.Lock()
	defer e.absorbMu.Unlock()

	outs, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		BasketName().Equals(BasketFunding).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil || len(outs) == 0 {
		return 0, err
	}

	// Filter to unspent outputs with a txid and positive satoshis.
	var eligible []*entity.Output
	for _, o := range outs {
		if o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || o.SpentBy != nil {
			continue
		}
		if o.TxStatus == wdk.TxStatusFailed || o.TxStatus == wdk.TxStatusAborted {
			continue
		}
		eligible = append(eligible, o)
	}
	if len(eligible) == 0 {
		return 0, nil
	}

	absorbed := 0
	const chunkSize = 100
	for start := 0; start < len(eligible); start += chunkSize {
		end := min(start+chunkSize, len(eligible))
		chunk := eligible[start:end]
		if err := e.absorbFundingChunk(ctx, chunk); err != nil {
			return absorbed, err
		}
		absorbed += len(chunk)
	}
	return absorbed, nil
}

func (e *Engine) absorbFundingChunk(ctx context.Context, chunk []*entity.Output) error {
	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(chunk))
	seenTx := make(map[string]bool)

	for _, o := range chunk {
		txid := *o.TxID
		if !seenTx[txid] {
			if err := e.mergeTxIntoBeef(ctx, combinedBeef, txid); err != nil {
				return fmt.Errorf("wallet: absorb load parent %s: %w", txid, err)
			}
			seenTx[txid] = true
		}
		h, err := chainhash.NewHashFromHex(txid)
		if err != nil {
			return err
		}
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "absorb static fund address deposit",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}

	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return fmt.Errorf("wallet: encode absorb inputBEEF: %w", err)
	}

	signAndProc := false
	acceptDelayed := false
	created, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "absorb funding deposits into default basket",
		Labels:      []string{"nektar", "absorb"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs:     nil, // 100% net value returns to go-wallet-toolbox's native BRC-29 "default" change basket!
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		return fmt.Errorf("wallet: absorb CreateAction: %w", err)
	}
	if created.SignableTransaction == nil {
		return errors.New("wallet: absorb CreateAction did not return SignableTransaction")
	}

	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return fmt.Errorf("wallet: absorb parse signable tx: %w", err)
	}

	unlocker, err := p2pkh.Unlock(e.fundKey, nil)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return err
	}
	spends := make(map[uint32]sdkwallet.SignActionSpend, len(inputs))
	for idx := range inputs {
		vin := uint32(idx)
		if int(vin) >= len(tx.Inputs) {
			break
		}
		us, sErr := unlocker.Sign(tx, vin)
		if sErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return fmt.Errorf("wallet: absorb sign input %d: %w", vin, sErr)
		}
		seqFinal := uint32(0xffffffff)
		spends[vin] = sdkwallet.SignActionSpend{
			UnlockingScript: *us,
			SequenceNumber:  &seqFinal,
		}
	}

	// Internal self-move from static FundAddress into default basket: external spend is 0 (fee-only).
	signCtx := withExternalSpend(ctx, 0)
	signed, err := e.tb.SignAction(signCtx, sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    spends,
		Options: &sdkwallet.SignActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		return fmt.Errorf("wallet: absorb SignAction: %w", err)
	}
	if signed != nil && len(signed.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(signed.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	return nil
}

// reclaimPoolToDefault moves just enough spendable UTXOs from "reserve" (first) and "fuel" (second)
// into "default" so a single large action whose target exceeds both "fuel" and "default" individually
// (while still <= total wallet balance) succeeds cleanly.
func (e *Engine) reclaimPoolToDefault(ctx context.Context, neededTotal uint64) (int, error) {
	if !e.svc.hasBroadcaster() {
		return 0, nil
	}
	e.absorbMu.Lock()
	defer e.absorbMu.Unlock()

	bal, err := e.Balance(ctx)
	if err != nil {
		return 0, err
	}
	if bal.ReserveSats+bal.FuelSats == 0 {
		return 0, nil
	}
	var shortfall uint64
	if neededTotal > bal.DefaultSats {
		shortfall = neededTotal - bal.DefaultSats
	}
	if shortfall == 0 {
		shortfall = 1000
	}
	if bal.ReserveSats+bal.FuelSats < shortfall {
		return 0, nil
	}

	outs, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil {
		return 0, err
	}
	utxos, _ := e.provider.UserUTXOEntity().Read().UserID(e.userID).Find(ctx)
	reserved := make(map[uint]bool, len(utxos))
	for _, u := range utxos {
		if u.ReservedByID != nil {
			reserved[u.OutputID] = true
		}
	}

	var reserveOuts, fuelOuts []*entity.Output
	for _, o := range outs {
		if o.SpentBy != nil || o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || reserved[o.ID] {
			continue
		}
		if o.TxStatus == wdk.TxStatusFailed || o.TxStatus == wdk.TxStatusAborted || o.TxStatus == wdk.TxStatusUnsigned || o.TxStatus == wdk.TxStatusUnprocessed {
			continue
		}
		if o.BasketName == nil {
			continue
		}
		switch *o.BasketName {
		case BasketReserve:
			reserveOuts = append(reserveOuts, o)
		case BasketFuel:
			fuelOuts = append(fuelOuts, o)
		}
	}

	// Prefer reclaiming larger reserve chunks first, then fuel UTXOs only for the remaining shortfall.
	sort.Slice(reserveOuts, func(i, j int) bool { return reserveOuts[i].Satoshis > reserveOuts[j].Satoshis })
	sort.Slice(fuelOuts, func(i, j int) bool { return fuelOuts[i].Satoshis > fuelOuts[j].Satoshis })
	candidates := append(reserveOuts, fuelOuts...)

	var picked []*entity.Output
	var pickedSum uint64
	target := shortfall + e.estimateFeeSats(ctx, 8, 2) + 500
	for _, o := range candidates {
		picked = append(picked, o)
		pickedSum += uint64(o.Satoshis)
		if pickedSum >= target || len(picked) >= 100 {
			break
		}
	}
	if len(picked) == 0 {
		return 0, nil
	}

	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(picked))
	seenTx := make(map[string]bool)
	for _, o := range picked {
		id := *o.TxID
		if !seenTx[id] {
			if mErr := e.mergeTxIntoBeef(ctx, combinedBeef, id); mErr != nil {
				return 0, mErr
			}
			seenTx[id] = true
		}
		h, _ := chainhash.NewHashFromHex(id)
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "reclaim throughput pool UTXO to default",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}
	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return 0, err
	}

	signAndProc := false
	acceptDelayed := false
	created, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "reclaim throughput pool UTXOs into default basket",
		Labels:      []string{"nektar", "reclaim"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs:     nil,
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		return 0, err
	}
	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return 0, err
	}
	spends := make(map[uint32]sdkwallet.SignActionSpend, len(picked))
	for idx, o := range picked {
		vin := uint32(idx)
		unlocker, uErr := e.unlockerForEntity(o)
		if uErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return 0, uErr
		}
		us, sErr := unlocker.Sign(tx, vin)
		if sErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return 0, sErr
		}
		seqFinal := uint32(0xffffffff)
		spends[vin] = sdkwallet.SignActionSpend{UnlockingScript: *us, SequenceNumber: &seqFinal}
	}
	// Internal self-move from reserve/fuel into default basket: external spend is 0 (fee-only).
	signed, err := e.tb.SignAction(withExternalSpend(ctx, 0), sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    spends,
		Options:   &sdkwallet.SignActionOptions{AcceptDelayedBroadcast: &acceptDelayed},
	}, e.cfg.Originator)
	if err != nil {
		return 0, err
	}
	if signed != nil && len(signed.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(signed.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			e.txCache.Put(parsed.TxID().String(), parsed)
		}
	}
	return len(picked), nil
}

func (e *Engine) mergeTxIntoBeef(ctx context.Context, dst *transaction.Beef, txid string) error {
	if beef, err := e.provider.GetBeefForTransaction(ctx, txid, wdk.StorageGetBeefOptions{}); err == nil && beef != nil {
		return dst.MergeBeef(beef)
	}
	if tx, err := e.LoadParsedTx(ctx, txid); err == nil && tx != nil {
		_, err = dst.MergeTransaction(tx)
		return err
	}
	return fmt.Errorf("tx %s not available for InputBEEF", txid)
}

// migrateLegacyPool imports existing bcommon/bwallet wallet.json or nektar-pool.json UTXOs
// into go-wallet-toolbox's "nektar funding" basket once and renames the legacy file to .migrated.
func (e *Engine) migrateLegacyPool(ctx context.Context, walletDir, legacyFile string) (int, error) {
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

	// Only migrate when wallet has no existing outputs so restarts never duplicate coins.
	existing, err := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).Count(ctx)
	if err != nil || existing > 0 {
		return 0, err
	}

	pool, err := bwallet.LoadPool(poolPath)
	if err != nil {
		return 0, fmt.Errorf("load %s: %w", poolPath, err)
	}
	outs := pool.Outputs()
	if len(outs) == 0 {
		_ = os.Rename(poolPath, poolPath+".migrated")
		return 0, nil
	}

	beefDir := filepath.Join(walletDir, "txbeef")
	imported := 0
	for _, o := range outs {
		if o.TxID == "" || o.Satoshis == 0 {
			continue
		}
		var tx *transaction.Transaction
		var atomicBeef []byte

		if rawBeef, rErr := os.ReadFile(filepath.Join(beefDir, o.TxID+".beef")); rErr == nil && len(rawBeef) > 0 {
			if _, parsed, _, pErr := guard.ParseBEEF(rawBeef, guard.DefaultBound); pErr == nil && parsed != nil {
				tx = parsed
				atomicBeef = rawBeef
			}
		}
		if tx == nil && o.Raw != "" {
			if parsed, pErr := transaction.NewTransactionFromHex(strings.TrimSpace(o.Raw)); pErr == nil {
				tx = parsed
			}
		}
		if tx == nil {
			continue
		}
		if o.Bump != "" && tx.MerklePath == nil {
			if bumpBytes, bErr := hex.DecodeString(strings.TrimSpace(o.Bump)); bErr == nil {
				if mp, mpErr := transaction.NewMerklePathFromBinary(bumpBytes); mpErr == nil {
					tx.MerklePath = mp
				}
			}
		}
		if len(atomicBeef) == 0 {
			var aErr error
			atomicBeef, aErr = SafeAtomicBEEF(tx)
			if aErr != nil {
				continue
			}
		}
		e.txCache.Put(o.TxID, tx)
		_, iErr := e.tb.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
			Tx:          atomicBeef,
			Description: "migrated from legacy wallet.json",
			Labels:      []string{"migrated"},
			Outputs: []sdkwallet.InternalizeOutput{{
				OutputIndex: o.Vout,
				Protocol:    sdkwallet.InternalizeProtocolBasketInsertion,
				InsertionRemittance: &sdkwallet.BasketInsertion{
					Basket: BasketFunding,
					Tags:   []string{"migrated", "fund"},
				},
			}},
		}, e.cfg.Originator)
		if iErr != nil {
			e.logf("wallet: migrate output %s:%d skipped: %v", o.TxID, o.Vout, iErr)
			continue
		}
		imported++
	}
	_ = os.Rename(poolPath, poolPath+".migrated")
	return imported, nil
}

// -----------------------------------------------------------------------------
// Balance, UTXO & Action Telemetry (via go-wallet-toolbox typed CRUD)
// -----------------------------------------------------------------------------

// Balance computes confirmed, unproven, reserved, and per-basket telemetry in O(unspent) time
// using a partial-index-backed SQL UNION ALL GROUP BY over the dedicated read-only WAL pool
// (zero writer contention, zero O(N_history) scans!).
func (e *Engine) Balance(ctx context.Context) (BalanceBreakdown, error) {
	var b BalanceBreakdown
	// Branch 1 hits partial index idx_nektar_outputs_unspent (WHERE deleted_at IS NULL AND spent_by IS NULL).
	// Branch 2 hits idx_nektar_txs_user_status + idx_nektar_outputs_spent_fee only when unsigned/unprocessed txs exist.
	q := fmt.Sprintf(`
		SELECT basket, is_reserved, is_confirmed, COUNT(1) AS cnt, COALESCE(SUM(sats), 0) AS total_sats
		FROM (
			SELECT
				COALESCE(o.basket_name, '') AS basket,
				CASE WHEN u.reserved_by_id IS NOT NULL OR o.spendable = 0 THEN 1 ELSE 0 END AS is_reserved,
				CASE WHEN owner.status = 'completed' OR u.utxo_status = 'mined' THEN 1 ELSE 0 END AS is_confirmed,
				o.satoshis AS sats
			FROM "%s" o
			INNER JOIN "%s" owner ON owner.id = o.transaction_id
			LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
			WHERE o.user_id = ?
			  AND o.deleted_at IS NULL
			  AND o.spent_by IS NULL
			  AND o.satoshis > 0
			  AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed','submission_unknown')
			UNION ALL
			SELECT
				COALESCE(o.basket_name, '') AS basket,
				1 AS is_reserved,
				0 AS is_confirmed,
				o.satoshis AS sats
			FROM "%s" sp
			INNER JOIN "%s" o ON o.spent_by = sp.id AND o.deleted_at IS NULL
			INNER JOIN "%s" owner ON owner.id = o.transaction_id
			WHERE sp.user_id = ?
			  AND sp.deleted_at IS NULL
			  AND sp.status IN ('unsigned','unprocessed','submission_unknown')
			  AND o.user_id = ?
			  AND o.satoshis > 0
			  AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed','submission_unknown')
		)
		GROUP BY basket, is_reserved, is_confirmed`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs,
		e.tables.transactions, e.tables.outputs, e.tables.transactions,
	)

	rows, err := e.readDB.QueryContext(ctx, q, e.userID, e.userID, e.userID)
	if err != nil {
		return b, err
	}
	defer rows.Close()

	for rows.Next() {
		var basket string
		var isReserved, isConfirmed, cnt int
		var sumSats int64
		if err := rows.Scan(&basket, &isReserved, &isConfirmed, &cnt, &sumSats); err != nil {
			return b, err
		}
		if sumSats <= 0 || cnt <= 0 {
			continue
		}
		if !IsSpendingBasket(basket) {
			if isReserved == 0 {
				b.CustomCount += cnt
			}
			continue
		}
		sats := uint64(sumSats)
		b.UTXOCount += cnt
		b.TotalSats += sats
		if isReserved != 0 {
			b.ReservedSats += sats
			continue
		}
		b.SpendableSats += sats
		b.SpendableCount += cnt
		if isConfirmed != 0 {
			b.ConfirmedSats += sats
		} else {
			b.UnprovenSats += sats
		}
		switch basket {
		case BasketFuel:
			b.FuelCount += cnt
			b.FuelSats += sats
		case BasketReserve:
			b.ReserveCount += cnt
			b.ReserveSats += sats
		case BasketDefault:
			b.DefaultCount += cnt
			b.DefaultSats += sats
		case BasketFunding:
			b.FundingCount += cnt
			b.FundingSats += sats
		}
	}
	if err := rows.Err(); err != nil {
		return b, err
	}

	pendingQ := fmt.Sprintf(`SELECT COUNT(1) FROM "%s" WHERE user_id = ? AND deleted_at IS NULL AND status IN ('unsigned','unprocessed','sending','submission_unknown')`, e.tables.transactions)
	_ = e.readDB.QueryRowContext(ctx, pendingQ, e.userID).Scan(&b.PendingTxCount)
	b.MaxUnprovenDepth = e.computeMaxUnprovenDepth(ctx)
	return b, nil
}

func (e *Engine) computeMaxUnprovenDepth(ctx context.Context) int {
	if e.readDB == nil {
		return 0
	}
	q := fmt.Sprintf(`
		SELECT DISTINCT COALESCE(owner.tx_id, '')
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		WHERE o.user_id = ?
		  AND o.deleted_at IS NULL
		  AND o.spent_by IS NULL
		  AND o.satoshis > 0
		  AND owner.status IN ('unproven','sending','nosend')
		  AND owner.tx_id IS NOT NULL
		  AND owner.tx_id != ''`,
		e.tables.outputs, e.tables.transactions,
	)
	rows, err := e.readDB.QueryContext(ctx, q, e.userID)
	if err != nil {
		return 0
	}
	var unproved []string
	for rows.Next() {
		var txid string
		if rows.Scan(&txid) == nil && txid != "" {
			unproved = append(unproved, txid)
		}
	}
	_ = rows.Close()
	if len(unproved) == 0 {
		return 0
	}

	// Build parent-tx status lookup for fast bounded DFS.
	maxCap := e.cfg.MaxUnprovenDepth + 4
	if maxCap < 16 {
		maxCap = 16
	}
	memo := make(map[string]int, len(unproved)*2)
	var depthOf func(txid string, seen map[string]bool) int
	depthOf = func(txid string, seen map[string]bool) int {
		if d, ok := memo[txid]; ok {
			return d
		}
		if seen[txid] || len(seen) > maxCap {
			return 0
		}
		tx, err := e.LoadParsedTx(ctx, txid)
		if err != nil || tx == nil {
			memo[txid] = 1
			return 1
		}
		if tx.MerklePath != nil {
			memo[txid] = 0
			return 0
		}
		seen[txid] = true
		best := 1
		for _, in := range tx.Inputs {
			if in.SourceTXID == nil {
				continue
			}
			pid := in.SourceTXID.String()
			pd := depthOf(pid, seen)
			if pd+1 > best {
				best = pd + 1
			}
		}
		delete(seen, txid)
		memo[txid] = best
		return best
	}

	maxDepth := 0
	for _, id := range unproved {
		d := depthOf(id, make(map[string]bool))
		if d > maxDepth {
			maxDepth = d
		}
	}
	return maxDepth
}

// ListUTXOs returns unspent UTXO records filtered by basket ("all", "default", "reserve", "fuel", etc.)
// directly from the read-only WAL connection pool in O(limit) with SQL-level filtering & pagination.
func (e *Engine) ListUTXOs(ctx context.Context, basket string, spendableOnly bool, limit, offset int) ([]OutputRecord, int, error) {
	if limit <= 0 {
		limit = 100
	}
	where := fmt.Sprintf(`
		o.user_id = ?
		AND o.deleted_at IS NULL
		AND o.spent_by IS NULL
		AND owner.status NOT IN ('failed','aborted','unsigned','unprocessed')`)
	args := []any{e.userID}
	if basket != "" && basket != "all" {
		where += ` AND o.basket_name = ?`
		args = append(args, basket)
	}
	if spendableOnly {
		where += ` AND o.spendable = 1 AND u.reserved_by_id IS NULL`
	}

	countQ := fmt.Sprintf(`
		SELECT COUNT(1)
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
		WHERE %s`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs, where,
	)
	var total int
	if err := e.readDB.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || offset >= total {
		return []OutputRecord{}, total, nil
	}

	selQ := fmt.Sprintf(`
		SELECT
			o.id,
			COALESCE(owner.tx_id, ''),
			o.vout,
			o.satoshis,
			o.locking_script,
			COALESCE(o.basket_name, ''),
			o.spendable,
			CASE WHEN u.reserved_by_id IS NOT NULL THEN 1 ELSE 0 END,
			COALESCE(o.derivation_prefix, ''),
			COALESCE(o.derivation_suffix, ''),
			COALESCE(o.sender_identity_key, ''),
			COALESCE(o.custom_instructions, ''),
			COALESCE(o.description, ''),
			o.created_at,
			COALESCE(owner.status, '')
		FROM "%s" o
		INNER JOIN "%s" owner ON owner.id = o.transaction_id
		LEFT JOIN "%s" u ON u.output_id = o.id AND u.user_id = o.user_id
		WHERE %s
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT ? OFFSET ?`,
		e.tables.outputs, e.tables.transactions, e.tables.userUTXOs, where,
	)
	qArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := e.readDB.QueryContext(ctx, selQ, qArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]OutputRecord, 0, min(limit, total-offset))
	for rows.Next() {
		var (
			id                              int64
			txid, bName, pre, suf, snd, ci  string
			desc, pStatus                   string
			vout                            uint32
			sats                            int64
			lockScript                      []byte
			spendableInt, reservedInt       int
			createdAt                       time.Time
		)
		if err := rows.Scan(&id, &txid, &vout, &sats, &lockScript, &bName, &spendableInt, &reservedInt,
			&pre, &suf, &snd, &ci, &desc, &createdAt, &pStatus); err != nil {
			return nil, 0, err
		}
		keyKind := KeyKindCustom
		if bName == BasketFunding {
			keyKind = KeyKindFund
		} else if pre != "" {
			keyKind = KeyKindBRC29
		}
		out = append(out, OutputRecord{
			ID:                 id,
			TxID:               txid,
			Vout:               vout,
			Satoshis:           uint64(max(0, sats)),
			LockingScript:      lockScript,
			LockingScriptHex:   hex.EncodeToString(lockScript),
			Basket:             bName,
			Spendable:          spendableInt != 0 && reservedInt == 0,
			Reserved:           reservedInt != 0,
			KeyKind:            keyKind,
			DerivationPrefix:   pre,
			DerivationSuffix:   suf,
			SenderIdentityKey:  snd,
			CustomInstructions: ci,
			OutputDescription:  desc,
			CreatedAt:          createdAt.Unix(),
			ParentStatus:       pStatus,
		})
	}
	return out, total, rows.Err()
}

// ListRecentActions returns recent wallet transactions ordered newest first in ONE indexed SQL query
// over the dedicated read-only WAL pool (eliminating the previous N+1 KnownTx + BEEF decode loop!).
func (e *Engine) ListRecentActions(ctx context.Context, limit, offset int) ([]TxRecord, int, error) {
	if limit <= 0 {
		limit = 50
	}
	countQ := fmt.Sprintf(`SELECT COUNT(1) FROM "%s" WHERE user_id = ? AND deleted_at IS NULL`, e.tables.transactions)
	var total int
	if err := e.readDB.QueryRowContext(ctx, countQ, e.userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 || offset >= total {
		return []TxRecord{}, total, nil
	}

	selQ := fmt.Sprintf(`
		SELECT
			COALESCE(t.tx_id, ''),
			COALESCE(t.reference, ''),
			COALESCE(t.status, ''),
			t.is_outgoing,
			t.satoshis,
			COALESCE(t.description, ''),
			t.version,
			t.lock_time,
			COALESCE(k.block_height, 0),
			t.created_at,
			t.updated_at,
			COALESCE((SELECT SUM(satoshis) FROM "%s" WHERE spent_by = t.id AND deleted_at IS NULL), 0) AS in_sats,
			COALESCE((SELECT SUM(satoshis) FROM "%s" WHERE transaction_id = t.id AND deleted_at IS NULL), 0) AS out_sats
		FROM "%s" t
		LEFT JOIN "%s" k ON k.tx_id = t.tx_id
		WHERE t.user_id = ? AND t.deleted_at IS NULL
		ORDER BY t.id DESC
		LIMIT ? OFFSET ?`,
		e.tables.outputs, e.tables.outputs, e.tables.transactions, e.tables.knownTxs,
	)
	rows, err := e.readDB.QueryContext(ctx, selQ, e.userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]TxRecord, 0, min(limit, total-offset))
	for rows.Next() {
		var (
			txid, ref, status, desc string
			isOutInt                int
			sats, inSats, outSats   int64
			ver, lockTime, height   uint32
			createdAt, updatedAt    time.Time
		)
		if err := rows.Scan(&txid, &ref, &status, &isOutInt, &sats, &desc, &ver, &lockTime,
			&height, &createdAt, &updatedAt, &inSats, &outSats); err != nil {
			return nil, 0, err
		}
		var feeSats uint64
		if isOutInt != 0 {
			if inSats > outSats {
				feeSats = uint64(inSats - outSats)
			} else if txid != "" {
				// Fallback to in-memory LRU only (0 SQL!) if tx spent external non-wallet inputs.
				if cached, ok := e.txCache.Get(txid); ok && cached != nil {
					feeSats = computeTxFee(cached)
				}
			}
		}
		out = append(out, TxRecord{
			TxID:        txid,
			Reference:   ref,
			Status:      status,
			IsOutgoing:  isOutInt != 0,
			Satoshis:    sats,
			FeeSats:     feeSats,
			Description: desc,
			Version:     ver,
			LockTime:    lockTime,
			BlockHeight: height,
			CreatedAt:   createdAt.Unix(),
			UpdatedAt:   updatedAt.Unix(),
		})
	}
	return out, total, rows.Err()
}

// LoadParsedTx resolves a transaction by txid from LRU cache or go-wallet-toolbox KnownTx storage.
func (e *Engine) LoadParsedTx(ctx context.Context, txid string) (*transaction.Transaction, error) {
	if tx, ok := e.txCache.Get(txid); ok && tx != nil {
		return tx, nil
	}
	if beef, err := e.provider.GetBeefForTransaction(ctx, txid, wdk.StorageGetBeefOptions{}); err == nil && beef != nil {
		if tx := beef.FindTransaction(txid); tx != nil {
			hydrateInputs(tx, beef, e.txCache, 0)
			e.txCache.Put(txid, tx)
			return tx, nil
		}
	}
	ktxs, err := e.provider.KnownTxEntity().Read().TxID(txid).Find(ctx)
	if err == nil && len(ktxs) > 0 {
		ktx := ktxs[0]
		if len(ktx.RawTx) > 0 {
			if tx, pErr := transaction.NewTransactionFromBytes(ktx.RawTx); pErr == nil {
				if len(ktx.MerklePath) > 0 {
					if mp, mpErr := transaction.NewMerklePathFromBinary(ktx.MerklePath); mpErr == nil {
						tx.MerklePath = mp
					}
				}
				e.txCache.Put(txid, tx)
				return tx, nil
			}
		}
	}
	return nil, fmt.Errorf("wallet: tx %s not found", txid)
}

func (e *Engine) CacheTx(tx *transaction.Transaction) {
	if tx != nil {
		e.txCache.Put(tx.TxID().String(), tx)
	}
}

func (e *Engine) HasTx(ctx context.Context, txid string) bool {
	if _, ok := e.txCache.Get(txid); ok {
		return true
	}
	n, err := e.provider.KnownTxEntity().Read().TxID(txid).Count(ctx)
	return err == nil && n > 0
}

// TxFeeSats computes the exact fee paid by a wallet transaction from its hydrated inputs/outputs.
func (e *Engine) TxFeeSats(ctx context.Context, txid string) uint64 {
	tx, err := e.LoadParsedTx(ctx, txid)
	if err != nil || tx == nil {
		return 0
	}
	return computeTxFee(tx)
}

func computeTxFee(tx *transaction.Transaction) uint64 {
	if tx == nil {
		return 0
	}
	var inTotal, outTotal uint64
	for _, in := range tx.Inputs {
		if in.SourceTransaction == nil || int(in.SourceTxOutIndex) >= len(in.SourceTransaction.Outputs) {
			return 0
		}
		inTotal += in.SourceTransaction.Outputs[in.SourceTxOutIndex].Satoshis
	}
	for _, o := range tx.Outputs {
		outTotal += o.Satoshis
	}
	if inTotal > outTotal {
		return inTotal - outTotal
	}
	return 0
}

func (e *Engine) estimateFeeSats(ctx context.Context, inputs, outputs int) uint64 {
	rate := e.FeeRateSatPerKB(ctx)
	bytesEst := uint64(10 + inputs*148 + outputs*34)
	fee := (bytesEst*rate + 999) / 1000
	if fee < 1 {
		fee = 1
	}
	return fee
}

// -----------------------------------------------------------------------------
// High-Level Operators: Send, Sweep, Consolidate, ImportBEEF, ImportTxID
// -----------------------------------------------------------------------------

// Send sends BSV to a P2PKH address OR a BRC-100 identity public key (BRC-29).
func (e *Engine) Send(ctx context.Context, args SendArgs) (*SendResult, error) {
	if args.Satoshis == 0 {
		return nil, fmt.Errorf("%w: send satoshis must be > 0", ErrInvalidArgs)
	}
	if (args.ToAddress == "" && args.ToIdentityKey == "") || (args.ToAddress != "" && args.ToIdentityKey != "") {
		return nil, fmt.Errorf("%w: specify exactly one of toAddress or toIdentityKey", ErrInvalidArgs)
	}

	var lockScript *script.Script
	var prefixB64, suffixB64, senderHex string
	var err error
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("%w: invalid address %q: %v", ErrInvalidArgs, args.ToAddress, aErr)
		}
		lockScript, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else {
		var preRaw, sufRaw [16]byte
		_, _ = rand.Read(preRaw[:])
		_, _ = rand.Read(sufRaw[:])
		prefixB64 = base64.StdEncoding.EncodeToString(preRaw[:])
		suffixB64 = base64.StdEncoding.EncodeToString(sufRaw[:])
		senderHex = e.IdentityPubHex()

		recipPub, pErr := guard.ParsePubKeyHex(strings.TrimSpace(args.ToIdentityKey))
		if pErr != nil {
			return nil, fmt.Errorf("%w: invalid toIdentityKey: %v", ErrInvalidArgs, pErr)
		}
		if !e.cfg.Mainnet {
			lockScript, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: prefixB64,
				DerivationSuffix: suffixB64,
			}, recipPub, brc29.WithTestNet())
		} else {
			lockScript, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: prefixB64,
				DerivationSuffix: suffixB64,
			}, recipPub)
		}
		if err != nil {
			return nil, fmt.Errorf("wallet: derive BRC-29 payment script: %w", err)
		}
	}

	desc := ensureDesc(args.Description, "nektar wallet send")
	noSend := args.NoSend
	acceptDelayed := args.AcceptDelayed
	randomize := false
	caRes, err := e.createActionGuarded(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      args.Labels,
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *lockScript,
			Satoshis:          args.Satoshis,
			OutputDescription: desc,
		}},
		Options: &sdkwallet.CreateActionOptions{
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, e.cfg.Originator, args.OverrideGuard, false)
	if err != nil {
		return nil, err
	}

	txid := caRes.Txid.String()
	var parsed *transaction.Transaction
	if len(caRes.Tx) > 0 {
		_, parsed, _, _ = guard.ParseBEEF(caRes.Tx, guard.DefaultBound)
	}
	if parsed == nil {
		parsed, _ = e.LoadParsedTx(ctx, txid)
	}
	outIdx := uint32(0)
	inputCount := 0
	var feeSats uint64
	if parsed != nil {
		inputCount = len(parsed.Inputs)
		feeSats = computeTxFee(parsed)
		for idx, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) && o.Satoshis == args.Satoshis {
				outIdx = uint32(idx)
				break
			}
		}
	}
	return &SendResult{
		TxID:              txid,
		Satoshis:          args.Satoshis,
		FeeSats:           feeSats,
		InputCount:        inputCount,
		BEEF:              caRes.Tx,
		BEEFHex:           hex.EncodeToString(caRes.Tx),
		Tx:                parsed,
		DerivationPrefix:  prefixB64,
		DerivationSuffix:  suffixB64,
		SenderIdentityKey: senderHex,
		OutputIndex:       outIdx,
	}, nil
}

// Sweep sweeps 100% of spendable funds across ALL spending baskets (default, reserve, fuel, and
// nektar funding) to a P2PKH address or BRC-100 identity public key (BRC-29) via go-wallet-toolbox.
func (e *Engine) Sweep(ctx context.Context, args SweepArgs) (*SweepResult, error) {
	if (args.ToAddress == "" && args.ToIdentityKey == "") || (args.ToAddress != "" && args.ToIdentityKey != "") {
		return nil, fmt.Errorf("%w: specify exactly one of toAddress or toIdentityKey", ErrInvalidArgs)
	}
	maxPerTx := args.MaxPerTx
	if maxPerTx <= 0 {
		maxPerTx = DefaultSweepChunkInputs
	}

	// Collect all unspent spendable outputs across default, reserve, fuel, and funding baskets.
	allOuts, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	utxos, _ := e.provider.UserUTXOEntity().Read().UserID(e.userID).Find(ctx)
	reservedByOut := make(map[uint]bool, len(utxos))
	for _, u := range utxos {
		if u.ReservedByID != nil {
			reservedByOut[u.OutputID] = true
		}
	}

	var defaultCount int
	var explicit []*entity.Output
	var totalAvail uint64
	for _, o := range allOuts {
		if o.SpentBy != nil || o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || reservedByOut[o.ID] {
			continue
		}
		if o.TxStatus == wdk.TxStatusFailed || o.TxStatus == wdk.TxStatusAborted || o.TxStatus == wdk.TxStatusUnsigned || o.TxStatus == wdk.TxStatusUnprocessed {
			continue
		}
		bName := ""
		if o.BasketName != nil {
			bName = *o.BasketName
		}
		if !IsSpendingBasket(bName) {
			continue
		}
		totalAvail += uint64(o.Satoshis)
		if bName == BasketDefault {
			defaultCount++
		} else {
			explicit = append(explicit, o)
		}
	}
	if defaultCount == 0 && len(explicit) == 0 {
		return nil, ErrInsufficient
	}

	if e.cfg.SpendGuardCheck != nil && !args.OverrideGuard {
		estFee := e.estimateFeeSats(ctx, defaultCount+len(explicit), 1)
		if err := e.cfg.SpendGuardCheck(estFee, totalAvail); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, err)
		}
	}
	if args.OverrideGuard {
		ctx = WithOverrideGuard(ctx)
	} else {
		ctx = withSweepAllExternalSpend(ctx)
	}

	var recipPub *ec.PublicKey
	var staticLock *script.Script
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("%w: invalid address %q: %v", ErrInvalidArgs, args.ToAddress, aErr)
		}
		staticLock, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else {
		recipPub, err = guard.ParsePubKeyHex(strings.TrimSpace(args.ToIdentityKey))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid toIdentityKey: %v", ErrInvalidArgs, err)
		}
	}

	makeDestLock := func() (*script.Script, string, string, error) {
		if staticLock != nil {
			return staticLock, "", "", nil
		}
		var preRaw, sufRaw [16]byte
		_, _ = rand.Read(preRaw[:])
		_, _ = rand.Read(sufRaw[:])
		preB64 := base64.StdEncoding.EncodeToString(preRaw[:])
		sufB64 := base64.StdEncoding.EncodeToString(sufRaw[:])
		var ls *script.Script
		if !e.cfg.Mainnet {
			ls, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
			}, recipPub, brc29.WithTestNet())
		} else {
			ls, err = brc29.LockForCounterparty(e.rootKey, brc29.KeyID{
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
			}, recipPub)
		}
		return ls, preB64, sufB64, err
	}

	desc := ensureDesc(args.Description, "nektar wallet sweep")
	out := &SweepResult{}
	if recipPub != nil {
		out.SenderIdentityKey = e.IdentityPubHex()
	}

	// Step 1: Sweep all explicit non-default outputs (fuel, reserve, nektar funding) in chunks!
	// On the final chunk, if defaultCount > 0 and chunk + defaultCount <= maxPerTx, we include
	// SweepMaxSatoshis so go-wallet-toolbox sweeps "default" into the exact same single tx!
	defaultSwept := false
	for start := 0; start < len(explicit); start += maxPerTx {
		end := min(start+maxPerTx, len(explicit))
		chunk := explicit[start:end]
		isLast := end == len(explicit)
		includeDefault := isLast && defaultCount > 0 && len(chunk)+defaultCount <= maxPerTx

		lockScript, preB64, sufB64, lErr := makeDestLock()
		if lErr != nil {
			return out, lErr
		}
		var pendingID int64
		if preB64 != "" {
			pendingID, _ = e.SavePendingBRC29(ctx, PendingBRC29Record{
				Direction:        "sweep",
				WireURL:          args.WireURL,
				CounterpartyPub:  strings.TrimSpace(args.ToIdentityKey),
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
				LockingScriptHex: hex.EncodeToString(*lockScript),
			})
		}
		txid, beef, parsed, sweptSats, feeSats, inputsUsed, sErr := e.sweepExplicitChunk(ctx, chunk, includeDefault, lockScript, desc)
		if sErr != nil {
			return out, sErr
		}
		if includeDefault {
			defaultSwept = true
		}
		e.appendSweepChunkResult(out, txid, beef, parsed, sweptSats, feeSats, inputsUsed, lockScript, preB64, sufB64)
		if pendingID > 0 {
			outIdx := out.OutputIndices[len(out.OutputIndices)-1]
			_ = e.UpdatePendingBRC29Tx(ctx, pendingID, txid, outIdx, beef, sweptSats)
		}
	}

	// Step 2: Sweep remaining "default" basket via go-wallet-toolbox's native SweepMaxSatoshis!
	if defaultCount > 0 && !defaultSwept {
		lockScript, preB64, sufB64, lErr := makeDestLock()
		if lErr != nil {
			return out, lErr
		}
		var pendingID int64
		if preB64 != "" {
			pendingID, _ = e.SavePendingBRC29(ctx, PendingBRC29Record{
				Direction:        "sweep",
				WireURL:          args.WireURL,
				CounterpartyPub:  strings.TrimSpace(args.ToIdentityKey),
				DerivationPrefix: preB64,
				DerivationSuffix: sufB64,
				LockingScriptHex: hex.EncodeToString(*lockScript),
			})
		}
		acceptDelayed := false
		randomize := false
		caRes, cErr := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
			Description: desc,
			Labels:      []string{"nektar", "sweep"},
			Outputs: []sdkwallet.CreateActionOutput{{
				LockingScript:     *lockScript,
				Satoshis:          primitives.SweepMaxSatoshis,
				OutputDescription: desc,
			}},
			Options: &sdkwallet.CreateActionOptions{
				AcceptDelayedBroadcast: &acceptDelayed,
				RandomizeOutputs:       &randomize,
			},
		}, e.cfg.Originator)
		if cErr != nil {
			return out, fmt.Errorf("wallet: sweep default basket: %w", cErr)
		}
		txid := caRes.Txid.String()
		var parsed *transaction.Transaction
		if len(caRes.Tx) > 0 {
			_, parsed, _, _ = guard.ParseBEEF(caRes.Tx, guard.DefaultBound)
		}
		if parsed == nil {
			parsed, _ = e.LoadParsedTx(ctx, txid)
		}
		var sweptSats, feeSats uint64
		var inputsUsed int
		if parsed != nil {
			inputsUsed = len(parsed.Inputs)
			feeSats = computeTxFee(parsed)
			for _, o := range parsed.Outputs {
				if o.LockingScript != nil && string(*o.LockingScript) == string(*lockScript) {
					sweptSats += o.Satoshis
				}
			}
			e.txCache.Put(txid, parsed)
		}
		e.appendSweepChunkResult(out, txid, caRes.Tx, parsed, sweptSats, feeSats, inputsUsed, lockScript, preB64, sufB64)
		if pendingID > 0 {
			outIdx := out.OutputIndices[len(out.OutputIndices)-1]
			_ = e.UpdatePendingBRC29Tx(ctx, pendingID, txid, outIdx, caRes.Tx, sweptSats)
		}
	}

	return out, nil
}

func (e *Engine) sweepExplicitChunk(ctx context.Context, chunk []*entity.Output, includeDefault bool, destLock *script.Script, desc string) (
	txid string, beef []byte, parsed *transaction.Transaction, sweptSats, feeSats uint64, inputsUsed int, err error,
) {
	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(chunk))
	seenTx := make(map[string]bool)
	var chunkSum uint64

	for _, o := range chunk {
		id := *o.TxID
		if !seenTx[id] {
			if mErr := e.mergeTxIntoBeef(ctx, combinedBeef, id); mErr != nil {
				return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep load parent %s: %w", id, mErr)
			}
			seenTx[id] = true
		}
		h, hErr := chainhash.NewHashFromHex(id)
		if hErr != nil {
			return "", nil, nil, 0, 0, 0, hErr
		}
		chunkSum += uint64(o.Satoshis)
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "nektar sweep input",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}
	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return "", nil, nil, 0, 0, 0, err
	}

	outSats := uint64(primitives.SweepMaxSatoshis)
	if !includeDefault {
		// Compute exact fee for this pure explicit-input chunk so zero funds are drawn from "default".
		estFee := e.estimateFeeSats(ctx, len(inputs), 1)
		if chunkSum <= estFee {
			return "", nil, nil, 0, 0, 0, ErrInsufficient
		}
		outSats = chunkSum - estFee
	}

	signAndProc := false
	acceptDelayed := false
	randomize := false
	created, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      []string{"nektar", "sweep"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs: []sdkwallet.CreateActionOutput{{
			LockingScript:     *destLock,
			Satoshis:          outSats,
			OutputDescription: desc,
		}},
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, e.cfg.Originator)
	if err != nil {
		return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep CreateAction: %w", err)
	}
	if created.SignableTransaction == nil {
		return "", nil, nil, 0, 0, 0, errors.New("wallet: sweep did not return SignableTransaction")
	}

	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return "", nil, nil, 0, 0, 0, err
	}

	spends := make(map[uint32]sdkwallet.SignActionSpend, len(chunk))
	for idx, o := range chunk {
		vin := uint32(idx)
		unlocker, uErr := e.unlockerForEntity(o)
		if uErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep unlocker input %d: %w", vin, uErr)
		}
		us, sErr := unlocker.Sign(tx, vin)
		if sErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep sign input %d: %w", vin, sErr)
		}
		seqFinal := uint32(0xffffffff)
		spends[vin] = sdkwallet.SignActionSpend{
			UnlockingScript: *us,
			SequenceNumber:  &seqFinal,
		}
	}

	signed, err := e.tb.SignAction(ctx, sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    spends,
		Options: &sdkwallet.SignActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		return "", nil, nil, 0, 0, 0, fmt.Errorf("wallet: sweep SignAction: %w", err)
	}

	txid = signed.Txid.String()
	beef = signed.Tx
	if len(beef) > 0 {
		_, parsed, _, _ = guard.ParseBEEF(beef, guard.DefaultBound)
	}
	if parsed == nil {
		parsed, _ = e.LoadParsedTx(ctx, txid)
	}
	if parsed != nil {
		inputsUsed = len(parsed.Inputs)
		feeSats = computeTxFee(parsed)
		for _, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*destLock) {
				sweptSats += o.Satoshis
			}
		}
		e.txCache.Put(txid, parsed)
	}
	return txid, beef, parsed, sweptSats, feeSats, inputsUsed, nil
}

func (e *Engine) unlockerForEntity(o *entity.Output) (transaction.UnlockingScriptTemplate, error) {
	bName := ""
	if o.BasketName != nil {
		bName = *o.BasketName
	}
	if bName == BasketFunding {
		return p2pkh.Unlock(e.fundKey, nil)
	}
	if o.DerivationPrefix != nil && *o.DerivationPrefix != "" && o.DerivationSuffix != nil && *o.DerivationSuffix != "" {
		senderPub := e.rootKey.PubKey()
		if o.SenderIdentityKey != nil && *o.SenderIdentityKey != "" {
			if parsed, err := guard.ParsePubKeyHex(*o.SenderIdentityKey); err == nil && parsed != nil {
				senderPub = parsed
			}
		}
		return brc29.Unlock(senderPub, brc29.KeyID{
			DerivationPrefix: *o.DerivationPrefix,
			DerivationSuffix: *o.DerivationSuffix,
		}, e.rootKey)
	}
	return p2pkh.Unlock(e.fundKey, nil)
}

func (e *Engine) appendSweepChunkResult(out *SweepResult, txid string, beef []byte, parsed *transaction.Transaction, sweptSats, feeSats uint64, inputsUsed int, destLock *script.Script, preB64, sufB64 string) {
	outIdx := uint32(0)
	if parsed != nil {
		for i, o := range parsed.Outputs {
			if o.LockingScript != nil && string(*o.LockingScript) == string(*destLock) {
				outIdx = uint32(i)
				break
			}
		}
	}
	out.TxIDs = append(out.TxIDs, txid)
	out.SweptSats += sweptSats
	out.TotalFeeSats += feeSats
	out.InputsSwept += inputsUsed
	out.BEEFs = append(out.BEEFs, beef)
	out.Txs = append(out.Txs, parsed)
	out.OutputIndices = append(out.OutputIndices, outIdx)
	if preB64 != "" {
		if out.DerivationPrefix == "" {
			out.DerivationPrefix = preB64
			out.DerivationSuffix = sufB64
		}
		out.Remittances = append(out.Remittances, PaymentRemittance{
			TxID:              txid,
			OutputIndex:       outIdx,
			DerivationPrefix:  preB64,
			DerivationSuffix:  sufB64,
			SenderIdentityKey: e.IdentityPubHex(),
		})
	}
}

// Consolidate merges fragmented small UTXOs (below args.BelowSats) into one clean output in targetBasket.
func (e *Engine) Consolidate(ctx context.Context, args ConsolidateArgs) (*ConsolidateResult, error) {
	_, _ = e.AbsorbFundingBasket(ctx)
	below := args.BelowSats
	if below == 0 {
		below = 10_000
	}
	maxIn := args.MaxInputs
	if maxIn <= 0 {
		maxIn = 100
	}
	targetBasket := args.TargetBasket
	if targetBasket == "" {
		targetBasket = BasketReserve
	}

	outs, err := e.provider.OutputsEntity().Read().
		UserID().Equals(e.userID).
		Spendable().Equals(true).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	utxos, _ := e.provider.UserUTXOEntity().Read().UserID(e.userID).Find(ctx)
	reserved := make(map[uint]bool, len(utxos))
	for _, u := range utxos {
		if u.ReservedByID != nil {
			reserved[u.OutputID] = true
		}
	}

	var small []*entity.Output
	for _, o := range outs {
		if o.SpentBy != nil || o.TxID == nil || *o.TxID == "" || o.Satoshis <= 0 || reserved[o.ID] {
			continue
		}
		if o.TxStatus == wdk.TxStatusFailed || o.TxStatus == wdk.TxStatusAborted || o.TxStatus == wdk.TxStatusUnsigned || o.TxStatus == wdk.TxStatusUnprocessed {
			continue
		}
		bName := ""
		if o.BasketName != nil {
			bName = *o.BasketName
		}
		if bName != BasketDefault && bName != BasketFuel {
			continue
		}
		if uint64(o.Satoshis) <= below {
			small = append(small, o)
		}
	}
	if len(small) < 2 {
		return &ConsolidateResult{TargetBasket: targetBasket}, nil
	}
	if len(small) > maxIn {
		small = small[:maxIn]
	}

	var totalIn uint64
	combinedBeef := transaction.NewBeef()
	inputs := make([]sdkwallet.CreateActionInput, 0, len(small))
	seenTx := make(map[string]bool)
	for _, o := range small {
		id := *o.TxID
		if !seenTx[id] {
			if mErr := e.mergeTxIntoBeef(ctx, combinedBeef, id); mErr != nil {
				return nil, mErr
			}
			seenTx[id] = true
		}
		h, _ := chainhash.NewHashFromHex(id)
		totalIn += uint64(o.Satoshis)
		seqFinal := uint32(0xffffffff)
		inputs = append(inputs, sdkwallet.CreateActionInput{
			Outpoint:              transaction.Outpoint{Txid: *h, Index: o.Vout},
			InputDescription:      "nektar consolidate small UTXO",
			UnlockingScriptLength: 107,
			SequenceNumber:        &seqFinal,
		})
	}
	estFee := e.estimateFeeSats(ctx, len(inputs), 1)
	if totalIn <= estFee+100 {
		return &ConsolidateResult{TargetBasket: targetBasket}, nil
	}
	inputBeefBytes, err := combinedBeef.Bytes()
	if err != nil {
		return nil, err
	}

	// Consolidate cleanly into native BRC-29 "default" change basket (or shaped reserve via FanOutFuel).
	signAndProc := false
	acceptDelayed := false
	created, err := e.tb.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "nektar UTXO consolidation",
		Labels:      []string{"nektar", "consolidate"},
		InputBEEF:   inputBeefBytes,
		Inputs:      inputs,
		Outputs:     nil,
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet: consolidate CreateAction: %w", err)
	}
	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
		return nil, err
	}
	spends := make(map[uint32]sdkwallet.SignActionSpend, len(small))
	for idx, o := range small {
		vin := uint32(idx)
		unlocker, uErr := e.unlockerForEntity(o)
		if uErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return nil, uErr
		}
		us, sErr := unlocker.Sign(tx, vin)
		if sErr != nil {
			_, _ = e.tb.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, e.cfg.Originator)
			return nil, sErr
		}
		seqFinal := uint32(0xffffffff)
		spends[vin] = sdkwallet.SignActionSpend{UnlockingScript: *us, SequenceNumber: &seqFinal}
	}
	// Internal self-move from reserve/fuel into default basket: external spend is 0 (fee-only).
	signed, err := e.tb.SignAction(withExternalSpend(ctx, 0), sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    spends,
		Options:   &sdkwallet.SignActionOptions{AcceptDelayedBroadcast: &acceptDelayed},
	}, e.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("wallet: consolidate SignAction: %w", err)
	}
	txid := signed.Txid.String()
	var feeSats, outSats uint64
	if len(signed.Tx) > 0 {
		if _, parsed, _, pErr := guard.ParseBEEF(signed.Tx, guard.DefaultBound); pErr == nil && parsed != nil {
			feeSats = computeTxFee(parsed)
			for _, o := range parsed.Outputs {
				outSats += o.Satoshis
			}
			e.txCache.Put(txid, parsed)
		}
	}
	return &ConsolidateResult{
		TxID:         txid,
		MergedCount:  len(small),
		OutputSats:   outSats,
		FeeSats:      feeSats,
		TargetBasket: BasketDefault,
	}, nil
}

// ImportBEEF verifies a BEEF/AtomicBEEF payload and internalizes any outputs paying FundAddress().
func (e *Engine) ImportBEEF(ctx context.Context, beefBytes []byte) (*ImportResult, error) {
	if len(beefBytes) == 0 {
		return nil, fmt.Errorf("%w: empty BEEF bytes", ErrInvalidArgs)
	}
	_, tx, _, err := guard.ParseBEEF(beefBytes, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("wallet: parse BEEF: %v", err)
	}
	return e.importTxToFundingBasket(ctx, tx, beefBytes, "imported BEEF payment")
}

// ImportTxID fetches a mined transaction + proof from Arcade/Chain and internalizes outputs paying FundAddress().
func (e *Engine) ImportTxID(ctx context.Context, txid string) (*ImportResult, error) {
	txid = strings.TrimSpace(txid)
	if len(txid) != 64 {
		return nil, fmt.Errorf("%w: txid must be 64 hex chars", ErrInvalidArgs)
	}
	var tx *transaction.Transaction
	if cached, ok := e.txCache.Get(txid); ok && cached != nil {
		tx = cached
	} else if e.cfg.Chain != nil {
		raw, err := e.cfg.Chain.TxRaw(ctx, txid)
		if err != nil {
			return nil, fmt.Errorf("wallet: fetch tx %s: %w", txid, err)
		}
		tx, err = transaction.NewTransactionFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("wallet: parse tx %s: %w", txid, err)
		}
	} else {
		return nil, fmt.Errorf("wallet: no chain client configured to fetch tx %s", txid)
	}
	if tx.MerklePath == nil {
		if mp, _, ok, _ := e.svc.lookupProof(ctx, txid); ok && mp != nil {
			tx.MerklePath = mp
		}
	}
	atomicBeef, err := SafeAtomicBEEF(tx)
	if err != nil {
		return nil, fmt.Errorf("wallet: encode AtomicBEEF for %s: %w", txid, err)
	}
	return e.importTxToFundingBasket(ctx, tx, atomicBeef, "imported txid payment")
}

// AdoptReceivedTx credits any outputs in a verified counterparty payment tx paying FundAddress().
func (e *Engine) AdoptReceivedTx(ctx context.Context, tx *transaction.Transaction) (uint64, error) {
	if tx == nil {
		return 0, nil
	}
	atomicBeef, err := SafeAtomicBEEF(tx)
	if err != nil {
		return 0, err
	}
	res, err := e.importTxToFundingBasket(ctx, tx, atomicBeef, "adopted received payment")
	if err != nil {
		return 0, err
	}
	return res.Sats, nil
}

// SeedCoinbaseOutput records a mature coinbase output (paying FundAddress) into go-wallet-toolbox storage.
func (e *Engine) SeedCoinbaseOutput(tx *transaction.Transaction, vout uint32, mp *transaction.MerklePath, height uint32) error {
	if tx == nil || int(vout) >= len(tx.Outputs) {
		return fmt.Errorf("%w: invalid coinbase output", ErrInvalidArgs)
	}
	if mp != nil {
		tx.MerklePath = mp
	}
	atomicBeef, err := SafeAtomicBEEF(tx)
	if err != nil {
		return err
	}
	txid := tx.TxID().String()
	e.txCache.Put(txid, tx)

	// Avoid duplicate internalization if already stored.
	if existing, fErr := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).TxID().Equals(txid).Vout().Equals(vout).Count(context.Background()); fErr == nil && existing > 0 {
		return nil
	}

	_, err = e.tb.InternalizeAction(context.Background(), sdkwallet.InternalizeActionArgs{
		Tx:          atomicBeef,
		Description: "seeded mature coinbase output",
		Labels:      []string{"coinbase", "seed"},
		Outputs: []sdkwallet.InternalizeOutput{{
			OutputIndex: vout,
			Protocol:    sdkwallet.InternalizeProtocolBasketInsertion,
			InsertionRemittance: &sdkwallet.BasketInsertion{
				Basket: BasketFunding,
				Tags:   []string{"coinbase", "fund"},
			},
		}},
	}, e.cfg.Originator)
	return err
}

func (e *Engine) importTxToFundingBasket(ctx context.Context, tx *transaction.Transaction, atomicBeef []byte, desc string) (*ImportResult, error) {
	txid := tx.TxID().String()
	fundBytes := []byte(*e.fundScript)
	var outputs []sdkwallet.InternalizeOutput
	var totalSats uint64
	for idx, o := range tx.Outputs {
		if o.LockingScript == nil || o.Satoshis == 0 {
			continue
		}
		if string(*o.LockingScript) == string(fundBytes) {
			outputs = append(outputs, sdkwallet.InternalizeOutput{
				OutputIndex: uint32(idx),
				Protocol:    sdkwallet.InternalizeProtocolBasketInsertion,
				InsertionRemittance: &sdkwallet.BasketInsertion{
					Basket: BasketFunding,
					Tags:   []string{"fund"},
				},
			})
			totalSats += o.Satoshis
		}
	}
	if len(outputs) == 0 {
		return &ImportResult{TxID: txid}, nil
	}
	e.txCache.Put(txid, tx)
	if len(atomicBeef) == 0 {
		var err error
		atomicBeef, err = SafeAtomicBEEF(tx)
		if err != nil {
			return nil, err
		}
	}
	_, err := e.tb.InternalizeAction(ctx, sdkwallet.InternalizeActionArgs{
		Tx:          atomicBeef,
		Description: ensureDesc(desc, "nektar import payment"),
		Labels:      []string{"import"},
		Outputs:     outputs,
	}, e.cfg.Originator)
	if err != nil {
		return nil, err
	}
	mined := tx.MerklePath != nil
	var height uint32
	if mined {
		height = tx.MerklePath.BlockHeight
	}
	return &ImportResult{
		TxID:    txid,
		Outputs: len(outputs),
		Sats:    totalSats,
		Mined:   mined,
		Height:  height,
	}, nil
}

// PruneUnrecognizedTxs marks any persisted outputs whose txid is not in keep as unspendable
// (used when re-seeding an ephemeral testchain).
func (e *Engine) PruneUnrecognizedTxs(ctx context.Context, keep func(txid string) bool) error {
	outs, err := e.provider.OutputsEntity().Read().UserID().Equals(e.userID).Spendable().Equals(true).Find(ctx)
	if err != nil {
		return err
	}
	f := false
	for _, o := range outs {
		if o.TxID == nil || *o.TxID == "" || keep(*o.TxID) {
			continue
		}
		_ = e.provider.OutputsEntity().Update(ctx, &entity.OutputUpdateSpecification{
			ID:        o.ID,
			Spendable: &f,
		})
		e.txCache.Delete(*o.TxID)
	}
	return nil
}

// SafeAtomicBEEF encodes tx as strict BRC-95 AtomicBEEF (allowPartial=false), falling back
// to allowPartial=true only when spending an external unconfirmed faucet output whose remote
// ancestor BUMP has not yet mined.
func SafeAtomicBEEF(tx *transaction.Transaction) ([]byte, error) {
	if tx == nil {
		return nil, errors.New("wallet: nil transaction")
	}
	if b, err := tx.AtomicBEEF(false); err == nil && len(b) > 0 {
		return b, nil
	}
	return tx.AtomicBEEF(true)
}
