package chain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/faststore"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const (
	Originator = "nektar"
)

// NektarWalletProfile is the canonical BRC-43/BRC-100 profile for Nektar v0.1.
// All funding UTXOs derive cleanly from this profile with zero raw private keys leaving the wallet engine.
var NektarWalletProfile = bwallet.Profile{
	FundProtocol:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar fund"},
	FundKeyID:      "nektar-fund",
	FundBasket:     "nektar funding",
	Version:        "0.1.0",
	LegacyPoolFile: "nektar-pool.json",
}

// Config configures a ChainPort for memory/testchain, regtest, testnet, or main.
type Config struct {
	Network                  string // memory | regtest | testnet | main
	DataDir                  string
	WalletMode               string // embedded (default) | client (BRC-100 wire on localhost:3321)
	WalletWireURL            string // default http://127.0.0.1:3321
	WalletProfile            string // standard (default) | throughput
	FeeRateSatPerKB          uint64 // optional static sat/kB override (0 => live BSV Broadcaster /policy clamped [100,500] fallback 100)
	HeaderSource             string // comma-separated independent PoW header witnesses (default 2-of-3 on main: woc:main,arcade:https://arcade-v2-us-1.bsvblockchain.tech,arcade:https://arcade-eu-1.bsvblockchain.tech)
	ChainSpec                string // nodeapi.ParseChain spec (e.g. woc:main, teranode:http://...)
	SettleSpec               string // comma-separated BSV Broadcaster spec (e.g. broadcaster:https://arcade-v2-us-1.bsvblockchain.tech, rpc:http://...)
	SettleDepth              int    // confirmations required before value-final settlement (0 => network default)
	ZeroConfMaxSats          uint64 // optional per-tx 0-conf cap for direct paywalls (0 => require settleDepth)
	ZeroConfMaxAggregateSats uint64 // optional aggregate unconfirmed exposure cap
	MaxUnconfirmedAncestors  int    // optional counterparty 0-conf ancestor depth cap (0 => DefaultMaxUnconfirmedAncestors)
	ArcadeKey                string
	RPCUser                  string
	RPCPass                  string
	WoCKey                   string
	MainnetConfirm           string // must equal "I_UNDERSTAND" when Network == "main"
	SpendGuard               core.SpendGuardOpts
	// TestTracker optionally overrides the ChainTracker strictly for offline unit tests.
	TestTracker chaintracker.ChainTracker
	// FastBroadcaster optionally overrides the broadcaster (e.g. parallel non-blocking mock ARC in scale benchmarks).
	FastBroadcaster nwallet.Broadcaster
	// FastStore optionally supplies a shared Pebble WAL store for the embedded wallet's 64-shard FastLanePool.
	FastStore *faststore.DB
	// EnableFastLane enables the zero-GORM 64-shard BRC-29 FastLanePool inside internal/wallet.
	EnableFastLane bool
	// SharedMemoryChain optionally shares one in-process testchain across multiple memory-network nodes
	// so cross-node BRC-29 SPV verification runs against a single synchronized consensus chain.
	SharedMemoryChain *SharedMemoryChain
}

// SharedMemoryChain wraps a bcommon/testchain.Chain with a shared mutex so multiple
// in-process memory-network nodes can verify real cross-node BRC-29 SPV BUMPs race-free.
type SharedMemoryChain struct {
	mu    sync.Mutex
	chain *testchain.Chain
}

// NewSharedMemoryChain creates a synchronized in-process testchain for multi-node SPV tests.
func NewSharedMemoryChain() *SharedMemoryChain {
	return &SharedMemoryChain{chain: testchain.New(1)}
}

func (s *SharedMemoryChain) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chain.IsValidRootForHeight(ctx, root, height)
}

func (s *SharedMemoryChain) CurrentHeight(ctx context.Context) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chain.CurrentHeight(ctx)
}

// pebbleSpendJournal persists SpendGuard NDJSON entries inside Pebble WAL (zero NTFS per-tx Open/Close).
type pebbleSpendJournal struct {
	db  *faststore.DB
	seq atomic.Uint64
}

func newPebbleSpendJournal(db *faststore.DB) *pebbleSpendJournal {
	return &pebbleSpendJournal{db: db}
}

func (j *pebbleSpendJournal) Load() ([][]byte, error) {
	var lines [][]byte
	var maxSeq uint64
	err := j.db.ScanPrefix([]byte("sg:j:"), func(k, v []byte) bool {
		if len(k) == 5+8 {
			s := binary.BigEndian.Uint64(k[5:])
			if s > maxSeq {
				maxSeq = s
			}
		}
		lines = append(lines, append([]byte(nil), v...))
		return true
	})
	if err != nil {
		return nil, err
	}
	j.seq.Store(maxSeq)
	return lines, nil
}

func (j *pebbleSpendJournal) Append(line []byte) error {
	s := j.seq.Add(1)
	var k [5 + 8]byte
	copy(k[:5], "sg:j:")
	binary.BigEndian.PutUint64(k[5:], s)
	return j.db.Set(k[:], line)
}

func (j *pebbleSpendJournal) Compact(lines [][]byte) error {
	var oldKeys [][]byte
	_ = j.db.ScanPrefix([]byte("sg:j:"), func(k, _ []byte) bool {
		oldKeys = append(oldKeys, append([]byte(nil), k...))
		return true
	})
	batch := j.db.NewBatch()
	for _, k := range oldKeys {
		_ = batch.Delete(k, nil)
	}
	var maxSeq uint64
	for i, ln := range lines {
		s := uint64(i + 1)
		maxSeq = s
		var k [5 + 8]byte
		copy(k[:5], "sg:j:")
		binary.BigEndian.PutUint64(k[5:], s)
		_ = batch.Set(k[:], ln, nil)
	}
	if err := j.db.CommitBatch(batch); err != nil {
		return err
	}
	j.seq.Store(maxSeq)
	return nil
}

// Port is the single chain + BRC-100 custody abstraction consumed by the entire Nektar Go runtime.
// In embedded mode it wraps internal/wallet.Engine (pure-Go WAL SQLite, CanonicalChangeSelector,
// FuelKeeper, Monitor); in client mode it speaks standard BRC-100 binary wire (bcommon/wirewallet)
// to BSV Desktop, Metanet Desktop, or an external go-wallet-toolbox instance.
type Port struct {
	mu              sync.Mutex
	tcMu            *sync.Mutex
	network         string
	mainnet         bool
	walletMode      string
	wireURL         string
	dataDir         string
	clientTxDir     string
	settleDepth     int
	zeroConfMax     uint64
	zeroConfAgg     uint64
	maxUnconfAnc    int
	guard           *core.SpendGuard
	wallet          sdkwallet.Interface
	engine          *nwallet.Engine // non-nil in embedded mode
	signer          *bwallet.Signer
	tracker         chaintracker.ChainTracker
	witnessed       *WitnessedTracker
	testChain       *testchain.Chain
	nodeChain       nodeapi.Chain
	woc             *nodeapi.WoC
	broadcaster     nwallet.Broadcaster
	broadcasterURLs []string
	feeSource       feepolicy.Source
	httpClient      *http.Client
	zeroConfWatch   *ZeroConfWatch

	reorgMu      sync.RWMutex
	reorgHooks   []ReorgHandler
	txMinedMu    sync.RWMutex
	txMinedHooks []nwallet.TxMinedHook

	proofCacheMu   sync.RWMutex
	proofCache     map[string]*transaction.MerklePath // reorg-evicting cache of verified BUMPs (bounded 64k FIFO)
	proofCacheKeys []string
	proofCacheHead int

	// Lightweight bookkeeping for client-mode txs & memory-mode locktime-final covenant txs.
	clientBuilt      map[string]*transaction.Transaction // hot cache backed durably by clientTxDir
	memCovenantTxs   map[string]*transaction.Transaction // memory-mode locktime-final covenant txs (real IsFinalTx + Chronicle VM)
	memCovenantSpent map[string]string                   // memory-mode "txid:vout" -> spending txid double-spend guard
}

// Open constructs a Port for any network:
//   - "memory" (default for tests): in-process bcommon/testchain + pure-Go SQLite embedded wallet
//   - "regtest" | "testnet" | "main": bcommon/headers + bcommon/nodeapi + embedded wallet (or BRC-100 wirewallet)
func Open(ctx context.Context, cfg Config) (*Port, error) {
	netName := cfg.Network
	if netName == "" {
		netName = "memory"
	}
	if netName == "main" && cfg.MainnetConfirm != "I_UNDERSTAND" {
		return nil, errors.New("chain: mainnet requires NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND")
	}
	mainnet := netName == "main"

	dir := cfg.DataDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "nektar-wallet-*")
		if err != nil {
			return nil, err
		}
	}
	wireURL := strings.TrimSpace(cfg.WalletWireURL)
	if wireURL == "" {
		wireURL = nwallet.DefaultDesktopWireURL
	}
	walletMode, stickyMode, err := resolveWalletMode(ctx, dir, netName, cfg.WalletMode, wireURL)
	if err != nil {
		return nil, err
	}
	walletDir := filepath.Join(dir, "wallet")
	if walletMode != "client" {
		if err := os.MkdirAll(walletDir, 0o700); err != nil {
			return nil, fmt.Errorf("chain: mkdir wallet: %w", err)
		}
	}

	guardOpts := cfg.SpendGuard
	if cfg.FastStore != nil && guardOpts.Journal == nil {
		guardOpts.Journal = newPebbleSpendJournal(cfg.FastStore)
	}
	if guardOpts.StatePath == "" && cfg.DataDir != "" && guardOpts.Journal == nil {
		guardOpts.StatePath = filepath.Join(cfg.DataDir, "spend-guard.json")
	}
	guardInst := core.NewSpendGuard(guardOpts)

	settleDepth := cfg.SettleDepth
	if settleDepth <= 0 {
		settleDepth = DefaultSettleDepth(netName)
	}
	maxUnconfAnc := cfg.MaxUnconfirmedAncestors
	if maxUnconfAnc <= 0 {
		maxUnconfAnc = DefaultMaxUnconfirmedAncestors
	}
	clientTxDir := filepath.Join(dir, "client-txs")

	var wrap func(http.RoundTripper) http.RoundTripper
	if strings.TrimSpace(cfg.WoCKey) == "" && netName != "memory" && netName != "regtest" {
		wrap = func(base http.RoundTripper) http.RoundTripper {
			return newWoCPacingTransport(base, 400*time.Millisecond)
		}
	}
	httpCli := core.SafeHTTPClientForNetworkWithWrapper(netName, 20*time.Second, wrap)
	p := &Port{
		tcMu:             &sync.Mutex{},
		network:          netName,
		mainnet:          mainnet,
		walletMode:       walletMode,
		wireURL:          cfg.WalletWireURL,
		dataDir:          dir,
		clientTxDir:      clientTxDir,
		settleDepth:      settleDepth,
		zeroConfMax:      cfg.ZeroConfMaxSats,
		zeroConfAgg:      cfg.ZeroConfMaxAggregateSats,
		maxUnconfAnc:     maxUnconfAnc,
		guard:            guardInst,
		httpClient:       httpCli,
		zeroConfWatch:    NewZeroConfWatch(),
		clientBuilt:      make(map[string]*transaction.Transaction),
		memCovenantTxs:   make(map[string]*transaction.Transaction),
		memCovenantSpent: make(map[string]string),
	}

	// 1. Configure multi-witness chain tracker, chain view (WoC/node), settler (Arcade/RPC/TCP), and live feepolicy!
	if err := p.initNetwork(cfg); err != nil {
		return nil, err
	}
	p.initFeeSource(cfg)

	// 2. Open custody: external BRC-100 desktop wallet (NEKTAR_WALLET=client) OR embedded pure-Go Engine.
	if walletMode == "client" {
		if err := p.openClientWallet(ctx, cfg, dir, stickyMode); err != nil {
			return nil, err
		}
	} else {
		if err := p.openEmbeddedWallet(cfg, dir, walletDir); err != nil {
			return nil, err
		}
	}

	// 3. Seed mature test coins when running in memory mode.
	if p.testChain != nil && p.engine != nil {
		if err := p.seedTestChainCoins(ctx); err != nil {
			_ = p.engine.Close()
			return nil, fmt.Errorf("chain: seed testchain: %w", err)
		}
	}

	return p, nil
}

// StartBackground starts the FuelKeeper and Monitor background loops (embedded mode).
func (p *Port) StartBackground(ctx context.Context) {
	if p.engine != nil {
		p.engine.StartBackground(ctx)
	}
}

// Close stops background workers cleanly and closes wallet.sqlite and SpendGuard journal handle.
func (p *Port) Close() error {
	var firstErr error
	if p.engine != nil {
		firstErr = p.engine.Close()
	}
	if p.guard != nil {
		if err := p.guard.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Port) Network() string                      { return p.network }
func (p *Port) Mainnet() bool                        { return p.mainnet }
func (p *Port) WalletMode() string                   { return p.walletMode }
func (p *Port) Tracker() chaintracker.ChainTracker   { return p.tracker }
func (p *Port) SettleDepth() int                     { return p.settleDepth }
func (p *Port) ZeroConfWatch() *ZeroConfWatch        { return p.zeroConfWatch }
func (p *Port) ZeroConfMaxSats() uint64              { return p.zeroConfMax }
func (p *Port) ZeroConfMaxAggregateSats() uint64     { return p.zeroConfAgg }
func (p *Port) MaxUnconfirmedAncestors() int         { return p.maxUnconfAnc }
func (p *Port) SpendGuard() *core.SpendGuard         { return p.guard }
func (p *Port) WalletInterface() sdkwallet.Interface { return p.wallet }
func (p *Port) Engine() *nwallet.Engine              { return p.engine }
func (p *Port) Signer() *bwallet.Signer              { return p.signer }
func (p *Port) TestChain() *testchain.Chain          { return p.testChain }
func (p *Port) IdentityPubHex() string               { return p.signer.IdentityHex() }

// OnReorg registers a callback invoked when the witnessed header chain reorganizes.
func (p *Port) OnReorg(fn ReorgHandler) {
	if fn == nil {
		return
	}
	p.reorgMu.Lock()
	p.reorgHooks = append(p.reorgHooks, fn)
	p.reorgMu.Unlock()
}

// OnTxMined registers a callback invoked when Monitor proves a transaction mined with a verified BUMP.
func (p *Port) OnTxMined(fn nwallet.TxMinedHook) {
	if fn == nil {
		return
	}
	p.txMinedMu.Lock()
	p.txMinedHooks = append(p.txMinedHooks, fn)
	p.txMinedMu.Unlock()
}

func (p *Port) dispatchReorg(forkHeight uint32, orphanedHashes []string) {
	p.evictProofCacheFrom(forkHeight)
	if p.zeroConfWatch != nil {
		p.zeroConfWatch.ClearMinedFrom(forkHeight)
	}
	if p.engine != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = p.engine.HandleReorg(ctx, forkHeight, orphanedHashes)
		cancel()
	}
	p.reorgMu.RLock()
	hooks := append([]ReorgHandler(nil), p.reorgHooks...)
	p.reorgMu.RUnlock()
	for _, fn := range hooks {
		fn(forkHeight, orphanedHashes)
	}
}

func (p *Port) dispatchTxMined(txid string, height uint32, mp *transaction.MerklePath) {
	if mp != nil {
		p.putProofCache(txid, mp)
	}
	if p.zeroConfWatch != nil && mp != nil {
		if p.settleDepth <= 1 {
			p.zeroConfWatch.MarkSettled(txid)
		} else {
			p.zeroConfWatch.MarkMined(txid, height)
			if p.tracker != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if tip, err := p.tracker.CurrentHeight(ctx); err == nil {
					p.zeroConfWatch.SweepSettled(tip, p.settleDepth)
				}
				cancel()
			}
		}
	}
	p.txMinedMu.RLock()
	hooks := append([]nwallet.TxMinedHook(nil), p.txMinedHooks...)
	p.txMinedMu.RUnlock()
	for _, fn := range hooks {
		fn(txid, height, mp)
	}
}
