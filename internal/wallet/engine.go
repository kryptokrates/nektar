package wallet

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/storage"
	toolboxwallet "github.com/bsv-blockchain/go-wallet-toolbox/pkg/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"gorm.io/gorm"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/faststore"
)

type toolboxTables struct {
	outputs      string
	transactions string
	userUTXOs    string
	knownTxs     string
}

// chronicleScriptsVerifier upgrades go-wallet-toolbox's ScriptsVerifier from pre-Chronicle
// (WithAfterGenesis) to full post-Chronicle BSV consensus (WithAfterChronicle) so Rúnar
// v1.0.0-rc.1 covenants using OP_2MUL/OP_2DIV/OP_RSHIFTNUM verify natively on createAction and internalizeAction.
type chronicleScriptsVerifier struct{}

func (chronicleScriptsVerifier) VerifyScripts(ctx context.Context, tx *transaction.Transaction) (bool, error) {
	if tx == nil {
		return false, errors.New("nil transaction")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for vin, input := range tx.Inputs {
		sourceOutput := input.SourceTxOutput()
		if sourceOutput == nil {
			return false, fmt.Errorf("failed to verify scripts for tx: %s, err: missing source output: input %d", tx.TxID().String(), vin)
		}
		if err := interpreter.NewEngine().Execute(
			interpreter.WithTx(tx, vin, sourceOutput),
			interpreter.WithForkID(),
			interpreter.WithAfterChronicle(),
			interpreter.WithContext(ctx),
		); err != nil {
			return false, fmt.Errorf("failed to verify scripts for tx: %s, err: script verification failed: %w", tx.TxID().String(), err)
		}
	}
	return true, nil
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

	fuel     *FuelKeeper
	monitor  *Monitor
	fastLane *FastLanePool

	absorbMu      sync.Mutex
	actionMu      sync.Mutex
	importStripes [64]sync.Mutex
	brc29Backoff  *core.KeyBackoff[int64]
	bgCancel     context.CancelFunc
	bgWG         sync.WaitGroup
	closeMu      sync.Mutex
	closed       bool
}

var _ sdkwallet.Interface = (*Engine)(nil)

// Open initializes identity.json, pure-Go modernc.org/sqlite + GORM, go-wallet-toolbox StorageProvider
// (with StrategyThroughput enabled), and *toolboxwallet.Wallet.
func Open(cfg Config) (*Engine, error) {
	if cfg.Originator == "" {
		cfg.Originator = "nektar"
	}
	if cfg.Profile.FundProtocol.Protocol == "" {
		cfg.Profile = bwallet.Profile{
			FundProtocol:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar fund"},
			FundKeyID:      "nektar-fund",
			FundBasket:     BasketFunding,
			Version:        "0.1.0",
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

	// Attach first-party BUMPs immediately on the first mined block (D6) so fuel stock, anchor proofs,
	// and change UTXOs never stall for extra blocks; value-final SettleDepth is enforced separately at
	// counterparty settlement seams (VerifyCounterpartyPaymentBEEF, ZeroConfWatch, Broker, Hub bounties).
	blocksDelay := uint(0)
	if !cfg.InMemoryStore {
		blocksDelay = 1
	}

	tbLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	provider, err := storage.NewGORMProvider(
		chainNet,
		svc,
		storage.WithGORM(gormDB),
		storage.WithScriptsVerifier(chronicleScriptsVerifier{}),
		storage.WithLogger(tbLogger),
		storage.WithFeeModel(defs.FeeModel{Type: defs.SatPerKB, Value: int64(feeSatPerKB)}),
		storage.WithUTXOManagement(utxoCfg),
		storage.WithChangeBasket(defs.ChangeBasket{
			NumberOfDesiredUTXOs:    32,
			MinimumDesiredUTXOValue: 1000,
			MaxChangeOutputsPerTx:   4,
		}),
		storage.WithFailAbandoned(defs.FailAbandoned{
			MinTransactionAgeSeconds: uint(minAgeSec),
		}),
		storage.WithSynchronizeTxStatuses(defs.SynchronizeTxStatuses{
			MaxAttempts:            10,
			MaxRebroadcastAttempts: 5,
			BlocksDelay:            blocksDelay,
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
		cfg:          cfg,
		profile:      prof,
		fuelDenom:    utxoCfg.Throughput.DenominationSatoshis,
		fanoutMax:    utxoCfg.Throughput.FanoutOutputsPerTx,
		rootKey:      rootKey,
		keyDeriver:   deriver,
		fundKey:      fundKey,
		fundAddr:     fundAddrObj.AddressString,
		fundScript:   fundScript,
		sqlDB:        sqlDB,
		readDB:       readDB,
		gormDB:       gormDB,
		tables:       tables,
		txCache:      txCache,
		svc:          svc,
		provider:     provider,
		tb:           tbWallet,
		userID:       userResp.User.UserID,
		brc29Backoff: core.NewKeyBackoff[int64](16384, 30*time.Second, 15*time.Minute),
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
	svc.isIncomingTx = e.isIncomingTx
	svc.loadTxFromDB = e.loadTxFromKnownTxs
	svc.loadBeefFromDB = e.loadBeefFromStorage
	svc.onAsyncBroadcastFailure = func(txid string) {
		_ = e.MarkSubmissionUnknown(context.Background(), txid)
	}

	// Reclaim any orphaned NoSend transactions left behind by a previous process crash.
	if !cfg.InMemoryStore {
		if n, rErr := e.ReconcileOrphanedNoSend(initCtx); rErr == nil && n > 0 {
			e.logf("wallet: reclaimed %d orphaned NoSend transaction(s) on boot", n)
		}
	}

	// Initialize pure-Go Pebble WAL 64-shard FastLanePool when requested or in throughput profile.
	if cfg.FastStore != nil || cfg.EnableFastLane || os.Getenv("NEKTAR_FAST_WALLET") == "1" || prof.Name == "throughput" {
		fs := cfg.FastStore
		ownsFS := false
		if fs == nil {
			var fsDir string
			if cfg.Dir != "" && !cfg.InMemoryStore {
				fsDir = filepath.Join(cfg.Dir, "fastlane.pebble")
			}
			if opened, fErr := faststore.Open(faststore.Options{
				Dir:        fsDir,
				InMemory:   cfg.InMemoryStore || fsDir == "",
				SyncWrites: !cfg.InMemoryStore,
			}); fErr == nil {
				fs = opened
				ownsFS = true
			}
		}
		if fs != nil {
			e.fastLane = newFastLanePool(e, fs, ownsFS)
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
	if prof.Name == "throughput" {
		cfg.Throughput.TargetTPS = 100
	} else {
		cfg.Throughput.TargetTPS = 25
	}
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
	// SpendPolicyPreferMined prefers mined/proven UTXOs and never spends TxStatusSending outputs.
	cfg.Throughput.SpendPolicy = defs.SpendPolicyPreferMined
	cfg.Throughput.PoolBasket = BasketFuel
	cfg.Throughput.ReserveBasket = BasketReserve
	maxOut := uint64(prof.MaxFanOutOutputs)
	if maxOut == 0 {
		maxOut = 64
	}
	cfg.Throughput.FanoutOutputsPerTx = maxOut
	cfg.Throughput.FanoutMaxTxsPerRound = 32
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
	if e.fastLane != nil {
		_ = e.fastLane.Close()
	}
	if e.readDB != nil && e.readDB != e.sqlDB {
		_ = e.readDB.Close()
	}
	if e.sqlDB != nil {
		// Reclaim WAL pages cleanly on shutdown so wallet.sqlite-wal never lingers bloated on disk.
		_, _ = e.sqlDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		err := e.sqlDB.Close()
		zeroECKey(e.rootKey)
		zeroECKey(e.fundKey)
		return err
	}
	zeroECKey(e.rootKey)
	zeroECKey(e.fundKey)
	return nil
}

func zeroECKey(k *ec.PrivateKey) {
	if k != nil && k.D != nil {
		k.D.SetInt64(0)
	}
}

func (e *Engine) logf(format string, args ...any) {
	if e.cfg.Logger != nil {
		e.cfg.Logger(format, args...)
	}
}

// Public accessors.
func (e *Engine) Tracker() chaintracker.ChainTracker { return e.cfg.Tracker }
func (e *Engine) IdentityKey() *ec.PublicKey         { return e.rootKey.PubKey() }
func (e *Engine) IdentityPubHex() string             { return hex.EncodeToString(e.rootKey.PubKey().Compressed()) }
func (e *Engine) FundPubKey() *ec.PublicKey          { return e.fundKey.PubKey() }
func (e *Engine) FundAddress() string                { return e.fundAddr }
func (e *Engine) FundScript() *script.Script         { return e.fundScript }

// SignIdentityHash signs a 32-byte digest with the embedded wallet's root identity key
// without ever exposing raw private key bytes outside internal/wallet.
func (e *Engine) SignIdentityHash(digest []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, errors.New("wallet: SignIdentityHash requires 32-byte digest")
	}
	sig, err := e.rootKey.Sign(digest)
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}

// SignFundHash signs a 32-byte digest with the embedded wallet's BRC-43 funding key
// without ever exposing raw private key bytes outside internal/wallet.
func (e *Engine) SignFundHash(digest []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, errors.New("wallet: SignFundHash requires 32-byte digest")
	}
	sig, err := e.fundKey.Sign(digest)
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}
func (e *Engine) Signer() *bwallet.Signer { return e.signer }
func (e *Engine) FuelKeeper() *FuelKeeper { return e.fuel }
func (e *Engine) Monitor() *Monitor       { return e.monitor }
func (e *Engine) FuelDenom() uint64       { return e.fuelDenom }

// FeeRateSatPerKB returns the live Arcade /v1/policy sat/kB rate (or static fallback).
func (e *Engine) FeeRateSatPerKB(ctx context.Context) uint64 {
	if e.cfg.FeeSource != nil {
		fCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if f, err := e.cfg.FeeSource.Fees(fCtx); err == nil && !f.Rate.IsZero() && f.Rate.Bytes > 0 {
			return (f.Rate.Sats*1000 + f.Rate.Bytes - 1) / f.Rate.Bytes
		}
	}
	return 100
}

// SnapshotTo writes an atomic, WAL-safe point-in-time snapshot of wallet.sqlite to dstPath
// using SQLite VACUUM INTO without ever exposing raw private keys (which remain solely in identity.json).
func (e *Engine) SnapshotTo(ctx context.Context, dstPath string) error {
	if e == nil {
		return errors.New("wallet: nil engine")
	}
	if dstPath == "" {
		return errors.New("wallet: snapshot destination path required")
	}
	e.closeMu.Lock()
	closed := e.closed
	db := e.sqlDB
	e.closeMu.Unlock()
	if closed || db == nil {
		return errors.New("wallet: database closed")
	}
	_, err := db.ExecContext(ctx, `VACUUM INTO ?`, filepath.Clean(dstPath))
	return err
}
