package chain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"

	"github.com/kryptokrates/nektar/internal/core"
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
	HeaderSource             string // comma-separated independent PoW header witnesses (e.g. woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2)
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

// Port is the single chain + BRC-100 custody abstraction consumed by the entire Nektar Go runtime.
// In embedded mode it wraps internal/wallet.Engine (pure-Go WAL SQLite, CanonicalChangeSelector,
// FuelKeeper, Monitor); in client mode it speaks standard BRC-100 binary wire (bcommon/wirewallet)
// to BSV Desktop, Metanet Desktop, or an external go-wallet-toolbox instance.
type Port struct {
	mu            sync.Mutex
	tcMu          *sync.Mutex
	network       string
	mainnet       bool
	walletMode    string
	wireURL       string
	dataDir       string
	clientTxDir   string
	settleDepth   int
	zeroConfMax   uint64
	zeroConfAgg   uint64
	maxUnconfAnc  int
	guard         *core.SpendGuard
	wallet        sdkwallet.Interface
	engine        *nwallet.Engine // non-nil in embedded mode
	signer        *bwallet.Signer
	tracker       chaintracker.ChainTracker
	witnessed     *WitnessedTracker
	testChain       *testchain.Chain
	nodeChain       *nodeapi.Sources
	woc             *nodeapi.WoC
	broadcaster     nwallet.Broadcaster
	broadcasterURLs []string
	feeSource       feepolicy.Source
	httpClient    *http.Client
	zeroConfWatch *ZeroConfWatch

	reorgMu      sync.RWMutex
	reorgHooks   []ReorgHandler
	txMinedMu    sync.RWMutex
	txMinedHooks []nwallet.TxMinedHook

	// Lightweight bookkeeping for BuildFundedTx -> Broadcast split callers & client-mode txs.
	pendingNoSend    map[string]string                   // txid -> action reference (embedded BuildFundedTx)
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
	walletMode := strings.ToLower(strings.TrimSpace(cfg.WalletMode))
	if walletMode == "" {
		walletMode = "embedded"
	}

	dir := cfg.DataDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "nektar-wallet-*")
		if err != nil {
			return nil, err
		}
	}
	walletDir := filepath.Join(dir, "wallet")
	if walletMode != "client" {
		if err := os.MkdirAll(walletDir, 0o700); err != nil {
			return nil, fmt.Errorf("chain: mkdir wallet: %w", err)
		}
	}

	guardOpts := cfg.SpendGuard
	if guardOpts.StatePath == "" && cfg.DataDir != "" {
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

	httpCli := &http.Client{Timeout: 20 * time.Second}
	if strings.TrimSpace(cfg.WoCKey) == "" && netName != "memory" && netName != "regtest" {
		httpCli.Transport = newWoCPacingTransport(http.DefaultTransport, 400*time.Millisecond)
	}
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
		pendingNoSend:    make(map[string]string),
		clientBuilt:      make(map[string]*transaction.Transaction),
		memCovenantTxs:   make(map[string]*transaction.Transaction),
		memCovenantSpent: make(map[string]string),
	}

	// 1. Configure multi-witness chain tracker, chain view (WoC/node), settler (Arcade/RPC/TCP), and live feepolicy!
	switch netName {
	case "memory":
		if cfg.SharedMemoryChain != nil {
			p.tcMu = &cfg.SharedMemoryChain.mu
			p.testChain = cfg.SharedMemoryChain.chain
			p.tracker = cfg.SharedMemoryChain
		} else {
			tc := testchain.New(1)
			p.testChain = tc
			p.tracker = tc
		}
	default:
		spec := strings.TrimSpace(cfg.HeaderSource)
		if spec == "" {
			spec = DefaultHeaderSources(netName)
		}
		chainSpec := strings.TrimSpace(cfg.ChainSpec)
		if chainSpec == "" {
			switch netName {
			case "main":
				chainSpec = "woc:main"
			case "testnet":
				chainSpec = "woc:test"
			}
		}
		settleSpec := NormalizeSettleSpec(cfg.SettleSpec)
		if settleSpec == "" {
			settleSpec = DefaultSettleSpec(netName)
		}

		if cfg.TestTracker != nil {
			p.tracker = cfg.TestTracker
		} else {
			wt, err := NewWitnessedTrackerFromSpec(netName, spec, chainSpec, settleSpec, p.httpClient)
			if err != nil {
				return nil, fmt.Errorf("chain: witnessed header tracker: %w", err)
			}
			p.witnessed = wt
			p.tracker = wt
			wt.OnReorg(p.dispatchReorg)
		}

		if netName == "testnet" {
			if w, wErr := nodeapi.NewWoC("test", cfg.WoCKey); wErr == nil {
				w.Client = p.httpClient
				p.woc = w
			}
		} else if netName == "main" {
			if w, wErr := nodeapi.NewWoC("main", cfg.WoCKey); wErr == nil {
				w.Client = p.httpClient
				p.woc = w
			}
		}
		if chainSpec != "" {
			src, err := nodeapi.ParseChain(chainSpec, nodeapi.ChainOptions{
				WoCKey:  cfg.WoCKey,
				Client:  p.httpClient,
				Headers: p.tracker,
			})
			if err != nil {
				return nil, fmt.Errorf("chain: chain spec %q: %w", chainSpec, err)
			}
			p.nodeChain = src
		}
		if settleSpec != "" {
			bcast, bcastURLs, err := ParseBroadcaster(settleSpec, BroadcasterOptions{
				APIKey:  cfg.ArcadeKey,
				RPCUser: cfg.RPCUser,
				RPCPass: cfg.RPCPass,
				Client:  p.httpClient,
			})
			if err != nil {
				return nil, fmt.Errorf("chain: settle spec %q: %w", settleSpec, err)
			}
			p.broadcaster = bcast
			p.broadcasterURLs = bcastURLs
		}
	}

	// Build fee policy: live BSV Broadcaster /policy (cached 5m, clamped [100, 500] sat/kB) or static fallback/override.
	baseFees := nwallet.DefaultNetworkFees
	if cfg.FeeRateSatPerKB > 0 {
		baseFees.Rate = mint.Rate{Sats: cfg.FeeRateSatPerKB, Bytes: 1000}
		if baseFees.Rate.Cmp(baseFees.MaxRate) > 0 {
			baseFees.MaxRate = baseFees.Rate
		}
		p.feeSource = feepolicy.Static(baseFees)
	} else if len(p.broadcasterURLs) > 0 {
		p.feeSource = &feepolicy.ARC{
			URLs: p.broadcasterURLs,
			Key:  cfg.ArcadeKey,
			Base: baseFees,
			Min:  mint.Rate{Sats: 100, Bytes: 1000},
			Max:  mint.Rate{Sats: 500, Bytes: 1000},
		}
	} else {
		p.feeSource = feepolicy.Static(baseFees)
	}

	// 2. Open custody: external BRC-100 desktop wallet (NEKTAR_WALLET=client) OR embedded pure-Go Engine.
	if walletMode == "client" {
		if netName == "regtest" || netName == "memory" {
			return nil, errors.New("chain: client wallet custody is for testnet/main only")
		}
		wireURL := cfg.WalletWireURL
		if wireURL == "" {
			wireURL = nwallet.DefaultDesktopWireURL
		}
		p.wireURL = wireURL
		if err := wirewallet.CheckURL(wireURL); err != nil {
			return nil, fmt.Errorf("chain: BRC-100 wire URL refused: %w", err)
		}
		w, err := nwallet.DialDesktopWallet(ctx, Originator, wireURL, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("chain: dial BRC-100 wallet at %s: %w", wireURL, err)
		}
		netRes, err := w.GetNetwork(ctx, nil, Originator)
		if err != nil {
			return nil, fmt.Errorf("chain: BRC-100 wallet network check failed: %w", err)
		}
		wantNet := sdkwallet.NetworkTestnet
		if mainnet {
			wantNet = sdkwallet.NetworkMainnet
		}
		if netRes == nil || netRes.Network != wantNet {
			gotNet := sdkwallet.Network("")
			if netRes != nil {
				gotNet = netRes.Network
			}
			return nil, fmt.Errorf("chain: BRC-100 client wallet network mismatch: Nektar requires %q, desktop wallet is on %q", wantNet, gotNet)
		}
		idPub, err := wirewallet.IdentityKeyOf(ctx, w, Originator)
		if err != nil {
			return nil, fmt.Errorf("chain: BRC-100 wallet identity key (is BSV Desktop unlocked?): %w", err)
		}
		p.wallet = w
		p.signer = &bwallet.Signer{
			Interface:  w,
			Identity:   idPub,
			Originator: Originator,
			Mainnet:    mainnet,
			Profile:    NektarWalletProfile,
		}
	} else {
		prof := nwallet.FuelProfileByName(cfg.WalletProfile)
		wCfg := nwallet.Config{
			Dir:           walletDir,
			InMemoryStore: netName == "memory",
			Mainnet:       mainnet,
			Originator:    Originator,
			Profile:       NektarWalletProfile,
			Tracker:       p.tracker,
			FeeSource:     p.feeSource,
			Broadcaster:   p.broadcaster,
			WoC:           p.woc,
			FuelProfile:   &prof,
			SpendGuard: func(txid string, feeSats, spendSats uint64, overrideGuard bool) error {
				if overrideGuard {
					return nil
				}
				return p.guard.CheckAndRecord(txid, feeSats, spendSats)
			},
			SpendGuardCheck:             p.guard.Check,
			SpendGuardRefund:            p.guard.Refund,
			SpendGuardRemainingInterval: p.guard.RemainingInterval,
			OnTxMined:                   p.dispatchTxMined,
			SettleDepth:                 p.settleDepth,
		}
		if p.nodeChain != nil {
			wCfg.Chain = p.nodeChain
		}
		if p.witnessed != nil {
			wCfg.HeaderBytes = p.witnessed.HeaderBytes
			wCfg.MedianTimePast = p.witnessed.MedianTimePast
		}
		if p.testChain != nil {
			wCfg.Broadcast = func(_ context.Context, tx *transaction.Transaction) error {
				p.tcMu.Lock()
				defer p.tcMu.Unlock()
				if err := p.testChain.Send(tx); err != nil {
					return fmt.Errorf("chain: testchain broadcast: %w", err)
				}
				if mp, _, ok := p.testChain.Proof(tx.TxID().String()); ok {
					tx.MerklePath = mp
				}
				return nil
			}
			wCfg.ProofLookup = func(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
				p.tcMu.Lock()
				defer p.tcMu.Unlock()
				mp, h, ok := p.testChain.Proof(txid)
				return mp, h, ok, nil
			}
		}
		eng, err := nwallet.Open(wCfg)
		if err != nil {
			return nil, fmt.Errorf("chain: open embedded wallet engine: %w", err)
		}
		p.engine = eng
		p.wallet = eng
		p.signer = eng.Signer()
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

// Close stops background workers cleanly and closes wallet.sqlite.
func (p *Port) Close() error {
	if p.engine != nil {
		return p.engine.Close()
	}
	return nil
}

// seedTestChainCoins mines mature coinbases to the embedded wallet's funding address
// and reconciles wallet.sqlite against the fresh ephemeral testchain.
func (p *Port) seedTestChainCoins(_ context.Context) error {
	if p.testChain == nil || p.engine == nil {
		return nil
	}
	addr := p.engine.FundAddress()
	lockHex := p.engine.FundScript().String()
	type seedCoinbase struct {
		tx *transaction.Transaction
		mp *transaction.MerklePath
		h  uint32
	}
	var toSeed []seedCoinbase
	known := make(map[string]bool)

	p.tcMu.Lock()
	// Generate 105 blocks so the earliest coinbases mature past testchain.Maturity (100).
	if _, err := p.testChain.Generate(testchain.Maturity+5, addr); err != nil {
		p.tcMu.Unlock()
		return err
	}
	tip := p.testChain.Height()
	for _, txid := range p.testChain.Txids() {
		known[txid] = true
		mp, h, ok := p.testChain.Proof(txid)
		if !ok || h+testchain.Maturity > tip {
			continue
		}
		cbTx := p.testChain.Tx(txid)
		if cbTx == nil || len(cbTx.Outputs) == 0 {
			continue
		}
		out0 := cbTx.Outputs[0]
		if out0.LockingScript == nil || out0.LockingScript.String() != lockHex || out0.Satoshis == 0 {
			continue
		}
		toSeed = append(toSeed, seedCoinbase{tx: cbTx, mp: mp, h: h})
	}
	p.tcMu.Unlock()

	// Prune outputs from previous ephemeral testchain sessions on the same DataDir (unlocked so tracker re-entry never deadlocks).
	if err := p.engine.PruneUnrecognizedTxs(context.Background(), func(txid string) bool {
		return known[txid]
	}); err != nil {
		return err
	}
	for _, cb := range toSeed {
		if err := p.engine.SeedCoinbaseOutput(cb.tx, 0, cb.mp, cb.h); err != nil {
			return err
		}
	}

	// Absorb seeded static coinbases into go-wallet-toolbox's native BRC-29 default basket in 1 tx.
	if _, err := p.engine.AbsorbFundingBasket(context.Background()); err != nil {
		return err
	}
	if p.BalanceSats() == 0 {
		return errors.New("testchain seeded zero mature coinbase outputs")
	}
	return nil
}

func (p *Port) Network() string                      { return p.network }
func (p *Port) Mainnet() bool                        { return p.mainnet }
func (p *Port) WalletMode() string                   { return p.walletMode }
func (p *Port) Tracker() chaintracker.ChainTracker   { return p.tracker }
func (p *Port) WitnessedTracker() *WitnessedTracker  { return p.witnessed }
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
func (p *Port) FeeSource() feepolicy.Source          { return p.feeSource }

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
	if p.zeroConfWatch != nil && mp != nil {
		if p.settleDepth <= 1 {
			p.zeroConfWatch.MarkSettled(txid)
		} else if p.tracker != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if tip, err := p.tracker.CurrentHeight(ctx); err == nil && tip >= height && int(tip-height)+1 >= p.settleDepth {
				p.zeroConfWatch.MarkSettled(txid)
			}
			cancel()
		}
	}
	p.txMinedMu.RLock()
	hooks := append([]nwallet.TxMinedHook(nil), p.txMinedHooks...)
	p.txMinedMu.RUnlock()
	for _, fn := range hooks {
		fn(txid, height, mp)
	}
}
