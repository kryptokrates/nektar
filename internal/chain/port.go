package chain

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"

	"github.com/kryptokrates/nektar/internal/core"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const (
	Originator = "nektar"
)

// NektarWalletProfile is the canonical BRC-43/BRC-100 profile for Nektar v0.4.
// All funding UTXOs derive cleanly from this profile with zero raw private keys leaving the wallet engine.
var NektarWalletProfile = bwallet.Profile{
	FundProtocol:   sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar fund"},
	FundKeyID:      "nektar-fund",
	FundBasket:     "nektar funding",
	Version:        "0.4.0",
	LegacyPoolFile: "nektar-pool.json",
}

// Config configures a ChainPort for memory/testchain, regtest, testnet, or main.
type Config struct {
	Network                  string // memory | regtest | testnet | main
	DataDir                  string
	WalletMode               string // embedded (default) | client (BRC-100 wire on localhost:3321)
	WalletWireURL            string // default http://127.0.0.1:3321
	WalletProfile            string // standard (default) | throughput
	FeeRateSatPerKB          uint64 // optional static sat/kB override (0 => live Arcade /v1/policy clamped [100,500] fallback 100)
	HeaderSource             string // comma-separated independent PoW header witnesses (e.g. woc:main,arcade:https://arcade.gorillapool.io)
	ChainSpec                string // nodeapi.ParseChain spec (e.g. woc:main, teranode:http://...)
	SettleSpec               string // publish.ParseSettler spec (e.g. arcade:https://..., rpc:http://...)
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
}

// Port is the single chain + BRC-100 custody abstraction consumed by the entire Nektar Go runtime.
// In embedded mode it wraps internal/wallet.Engine (pure-Go WAL SQLite, CanonicalChangeSelector,
// FuelKeeper, Monitor); in client mode it speaks standard BRC-100 binary wire (bcommon/wirewallet)
// to BSV Desktop, Metanet Desktop, or an external go-wallet-toolbox instance.
type Port struct {
	mu            sync.Mutex
	tcMu          sync.Mutex
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
	testChain     *testchain.Chain
	nodeChain     *nodeapi.Sources
	woc           *nodeapi.WoC
	settler       publish.Settler
	arcade        *publish.Arcade
	feeSource     feepolicy.Source
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

// strictSettler wraps a bcommon publish.Settler so unacknowledged TCP writes and Arcade
// RECEIVED verdict timeouts fail closed with ErrBroadcastUnknown instead of returning nil.
type strictSettler struct {
	inner            publish.Settler
	arcade           *publish.Arcade
	unconfirmedTxids sync.Map // txid -> struct{}
}

func wrapStrictSettler(s publish.Settler, arcade *publish.Arcade) publish.Settler {
	if s == nil {
		return nil
	}
	ss := &strictSettler{inner: s, arcade: arcade}
	if arcade != nil {
		prevNote := arcade.Note
		arcade.Note = func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if strings.Contains(msg, "verdict had not arrived") && len(args) > 0 {
				if txid, ok := args[0].(string); ok && len(txid) == 64 {
					ss.unconfirmedTxids.Store(txid, struct{}{})
				}
			}
			if prevNote != nil {
				prevNote(format, args...)
			}
		}
	}
	return ss
}

func (s *strictSettler) Name() string {
	return s.inner.Name()
}

func (s *strictSettler) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("chain: nil transaction")
	}
	if _, isTCP := s.inner.(*publish.TCPIngress); isTCP {
		if err := s.inner.Submit(ctx, tx); err != nil {
			return err
		}
		// TCPIngress provides zero network acceptance acknowledgement: fail closed as unknown!
		return fmt.Errorf("%w: %s provides no acceptance acknowledgement", nwallet.ErrBroadcastUnknown, s.inner.Name())
	}
	txid := tx.TxID().String()
	s.unconfirmedTxids.Delete(txid)
	if err := s.inner.Submit(ctx, tx); err != nil {
		return err
	}
	if s.arcade != nil {
		if _, timedOut := s.unconfirmedTxids.LoadAndDelete(txid); timedOut {
			st, stErr := s.arcade.Status(ctx, txid)
			if stErr != nil {
				return fmt.Errorf("%w: arcade verdict timed out for %s (%v)", nwallet.ErrBroadcastUnknown, txid, stErr)
			}
			if st.Refused() {
				return fmt.Errorf("chain: arcade refused %s: %s", txid, st.Why())
			}
			if !st.Accepted() && !st.Mined() {
				return fmt.Errorf("%w: arcade holds %s as %s (network verdict unconfirmed)", nwallet.ErrBroadcastUnknown, txid, st.TxStatus)
			}
		}
	}
	return nil
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
	if err := os.MkdirAll(walletDir, 0o700); err != nil {
		return nil, fmt.Errorf("chain: mkdir wallet: %w", err)
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

	p := &Port{
		network:       netName,
		mainnet:       mainnet,
		walletMode:    walletMode,
		wireURL:       cfg.WalletWireURL,
		dataDir:       dir,
		clientTxDir:   clientTxDir,
		settleDepth:   settleDepth,
		zeroConfMax:   cfg.ZeroConfMaxSats,
		zeroConfAgg:   cfg.ZeroConfMaxAggregateSats,
		maxUnconfAnc:  maxUnconfAnc,
		guard:         guardInst,
		httpClient:    &http.Client{Timeout: 20 * time.Second},
		zeroConfWatch: NewZeroConfWatch(),
		pendingNoSend:    make(map[string]string),
		clientBuilt:      make(map[string]*transaction.Transaction),
		memCovenantTxs:   make(map[string]*transaction.Transaction),
		memCovenantSpent: make(map[string]string),
	}

	// 1. Configure multi-witness chain tracker, chain view (WoC/node), settler (Arcade/RPC/TCP), and live feepolicy!
	switch netName {
	case "memory":
		tc := testchain.New(1)
		p.testChain = tc
		p.tracker = tc
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
		settleSpec := strings.TrimSpace(cfg.SettleSpec)
		if settleSpec == "" {
			switch netName {
			case "testnet":
				settleSpec = "arcade:test"
			case "main":
				settleSpec = "arcade:main"
			default:
				if def, err := publish.DefaultSettle(netName); err == nil {
					settleSpec = def
				}
			}
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
				p.woc = w
			}
		} else if netName == "main" {
			if w, wErr := nodeapi.NewWoC("main", cfg.WoCKey); wErr == nil {
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
			sOpts := publish.SettleOptions{
				Key:     cfg.ArcadeKey,
				RPCUser: cfg.RPCUser,
				RPCPass: cfg.RPCPass,
				Client:  p.httpClient,
			}
			if p.nodeChain != nil {
				sOpts.Spends = p.nodeChain
			}
			settler, arcade, err := publish.ParseSettler(settleSpec, sOpts)
			if err != nil {
				return nil, fmt.Errorf("chain: settle spec %q: %w", settleSpec, err)
			}
			p.settler = wrapStrictSettler(settler, arcade)
			p.arcade = arcade
		}
	}

	// Build fee policy: live Arcade /v1/policy (cached 5m, clamped [100, 500] sat/kB) or static fallback/override.
	baseFees := nwallet.DefaultNetworkFees
	if cfg.FeeRateSatPerKB > 0 {
		baseFees.Rate = mint.Rate{Sats: cfg.FeeRateSatPerKB, Bytes: 1000}
		if baseFees.Rate.Cmp(baseFees.MaxRate) > 0 {
			baseFees.MaxRate = baseFees.Rate
		}
		p.feeSource = feepolicy.Static(baseFees)
	} else if p.arcade != nil && p.arcade.Base != "" {
		p.feeSource = &feepolicy.ARC{
			URLs: []string{p.arcade.Base},
			Key:  p.arcade.Key,
			Base: baseFees,
			Min:  mint.Rate{Sats: 100, Bytes: 1000},
			Max:  mint.Rate{Sats: 500, Bytes: 1000},
		}
	} else {
		p.feeSource = feepolicy.Static(baseFees)
	}

	// 2. Open custody: external BRC-100 wirewallet (NEKTAR_WALLET=client) OR embedded pure-Go Engine.
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
		w, err := wirewallet.Dial(Originator, wireURL, 30*time.Second)
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
			Originator:  Originator,
			Profile:     NektarWalletProfile,
			Tracker:     p.tracker,
			FeeSource:   p.feeSource,
			Settler:     p.settler,
			Arcade:      p.arcade,
			Chain:       p.nodeChain,
			WoC:         p.woc,
			FuelProfile: &prof,
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
	p.tcMu.Lock()
	addr := p.engine.FundAddress()
	lockHex := p.engine.FundScript().String()
	// Generate 105 blocks so the earliest coinbases mature past testchain.Maturity (100).
	if _, err := p.testChain.Generate(testchain.Maturity+5, addr); err != nil {
		p.tcMu.Unlock()
		return err
	}
	// Prune outputs from previous ephemeral testchain sessions on the same DataDir.
	if err := p.engine.PruneUnrecognizedTxs(context.Background(), func(txid string) bool {
		return p.testChain.Tx(txid) != nil
	}); err != nil {
		p.tcMu.Unlock()
		return err
	}
	tip := p.testChain.Height()
	for _, txid := range p.testChain.Txids() {
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
		if err := p.engine.SeedCoinbaseOutput(cbTx, 0, mp, h); err != nil {
			p.tcMu.Unlock()
			return err
		}
	}
	p.tcMu.Unlock()

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

// VerifyPaymentBEEF verifies an incoming counterparty payment BEEF against this Port's witnessed
// ChainTracker, settleDepth, and ZeroConfWatch policy.
func (p *Port) VerifyPaymentBEEF(ctx context.Context, beefBytes []byte, opts PaymentVerifyOpts) (*PaymentVerifyResult, error) {
	tracker := p.tracker
	if tracker == nil && p.engine != nil {
		tracker = p.engine.Tracker()
	}
	if opts.SettleDepth <= 0 {
		opts.SettleDepth = p.settleDepth
	}
	if opts.MaxUnconfirmedAncestors <= 0 {
		opts.MaxUnconfirmedAncestors = p.maxUnconfAnc
	}
	if opts.ConflictWatch == nil {
		opts.ConflictWatch = p.zeroConfWatch
	}
	return VerifyCounterpartyPaymentBEEF(ctx, beefBytes, tracker, opts)
}

func (p *Port) FundAddress() (string, error) {
	if p.engine != nil {
		return p.engine.FundAddress(), nil
	}
	return p.signer.FundAddress(p.mainnet)
}

// FeeRateSatPerKB returns the current fee rate in sat/kB (live Arcade policy or static fallback).
func (p *Port) FeeRateSatPerKB(ctx context.Context) uint64 {
	if p.engine != nil {
		return p.engine.FeeRateSatPerKB(ctx)
	}
	if p.feeSource != nil {
		if f, err := p.feeSource.Fees(ctx); err == nil && !f.Rate.IsZero() && f.Rate.Bytes > 0 {
			return (f.Rate.Sats*1000 + f.Rate.Bytes - 1) / f.Rate.Bytes
		}
	}
	return 100
}

// Balance returns structured confirmed / unproven / reserved / per-basket telemetry in both embedded and client modes.
func (p *Port) Balance(ctx context.Context) (nwallet.BalanceBreakdown, error) {
	if p.engine != nil {
		return p.engine.Balance(ctx)
	}
	// Client mode: request full transaction BEEF via ListOutputs so ConfirmedSats ONLY counts
	// outputs backed by a BUMP verified against our witnessed ChainTracker (never trusting bare spendable flags).
	if p.wallet == nil {
		return nwallet.BalanceBreakdown{}, errors.New("chain: client wallet not initialized")
	}
	limit := uint32(1000)
	res, err := p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
		Basket:  nwallet.BasketDefault,
		Include: sdkwallet.OutputIncludeEntireTransactions,
		Limit:   &limit,
	}, Originator)
	if err != nil {
		// Some remote wallets refuse entire transactions on large baskets; retry metadata-only and
		// report all spendable outputs honestly as UnprovenSats (never ConfirmedSats).
		res, err = p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
			Basket: nwallet.BasketDefault,
			Limit:  &limit,
		}, Originator)
		if err != nil {
			return nwallet.BalanceBreakdown{}, err
		}
	}
	var beef *transaction.Beef
	if len(res.BEEF) > 0 {
		if parsedBeef, _, _, pErr := guard.ParseBEEF(res.BEEF, guard.DefaultBound); pErr == nil {
			beef = parsedBeef
		}
	}
	verifiedMined := make(map[string]bool)
	isTxConfirmed := func(txid string) bool {
		if txid == "" || beef == nil || p.tracker == nil {
			return false
		}
		if v, ok := verifiedMined[txid]; ok {
			return v
		}
		tx := beef.FindTransaction(txid)
		if tx == nil || tx.MerklePath == nil {
			verifiedMined[txid] = false
			return false
		}
		txHash, hErr := chainhash.NewHashFromHex(txid)
		if hErr != nil {
			verifiedMined[txid] = false
			return false
		}
		root, rErr := tx.MerklePath.ComputeRoot(txHash)
		if rErr != nil || root == nil {
			verifiedMined[txid] = false
			return false
		}
		valid, vErr := p.tracker.IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight)
		ok := vErr == nil && valid
		verifiedMined[txid] = ok
		return ok
	}

	var b nwallet.BalanceBreakdown
	for _, o := range res.Outputs {
		b.UTXOCount++
		b.TotalSats += o.Satoshis
		if o.Spendable {
			b.SpendableCount++
			b.SpendableSats += o.Satoshis
			if isTxConfirmed(o.Outpoint.Txid.String()) {
				b.ConfirmedSats += o.Satoshis
			} else {
				b.UnprovenSats += o.Satoshis
			}
			b.DefaultCount++
			b.DefaultSats += o.Satoshis
		} else {
			b.ReservedSats += o.Satoshis
		}
	}
	return b, nil
}

// BalanceChecked returns total spendable satoshis and propagates any DB/RPC/timeout error
// distinctly from a true zero balance.
func (p *Port) BalanceChecked(ctx context.Context) (uint64, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0, err
	}
	return bal.SpendableSats, nil
}

// UTXOCountChecked returns total spendable UTXOs and propagates any DB/RPC/timeout error.
func (p *Port) UTXOCountChecked(ctx context.Context) (int, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0, err
	}
	return bal.SpendableCount, nil
}

// BalanceSats returns total spendable satoshis (best-effort; prefer BalanceChecked when errors matter).
func (p *Port) BalanceSats() uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sats, err := p.BalanceChecked(ctx)
	if err != nil {
		return 0
	}
	return sats
}

// UTXOCount returns total spendable UTXOs (best-effort; prefer UTXOCountChecked when errors matter).
func (p *Port) UTXOCount() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cnt, err := p.UTXOCountChecked(ctx)
	if err != nil {
		return 0
	}
	return cnt
}

// EnsureSplitCoins warms up the fuel basket via FuelKeeper (embedded mode; no-op in client mode).
func (p *Port) EnsureSplitCoins(ctx context.Context, targetCount int, splitSats uint64) error {
	if p.engine == nil || targetCount <= 1 || splitSats == 0 {
		return nil
	}
	_, err := p.engine.FuelKeeper().WarmFuel(ctx, targetCount, splitSats)
	return err
}

// Send pays to a P2PKH address or BRC-100 identity key (BRC-29) in either embedded or client mode.
func (p *Port) Send(ctx context.Context, args nwallet.SendArgs) (*nwallet.SendResult, error) {
	if p.engine != nil {
		return p.engine.Send(ctx, args)
	}
	// Client mode: execute directly through remote BRC-100 wallet.CreateAction!
	if p.wallet == nil {
		return nil, errors.New("chain: wallet not initialized")
	}
	if args.Satoshis == 0 {
		return nil, errors.New("chain: send amount must be > 0")
	}
	var lockScript *script.Script
	var err error
	if args.ToAddress != "" {
		addr, aErr := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if aErr != nil {
			return nil, fmt.Errorf("chain: invalid address %q: %w", args.ToAddress, aErr)
		}
		lockScript, err = p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
	} else if args.ToIdentityKey != "" {
		preB64, sufB64 := randomBRC29Nonce()
		lockScript, err = p.signer.PaymentDestination(ctx, strings.TrimSpace(args.ToIdentityKey), preB64, sufB64)
		if err != nil {
			return nil, err
		}
		res, sErr := p.clientCreateAction(ctx, []*transaction.TransactionOutput{{Satoshis: args.Satoshis, LockingScript: lockScript}}, args.Description, args.OverrideGuard)
		if sErr != nil {
			return nil, sErr
		}
		if res.Tx == nil {
			return nil, fmt.Errorf("chain: client wallet returned unparseable BRC-29 BEEF for broadcast tx %s (outputIndex unknown)", res.TxID)
		}
		matched := false
		for idx, o := range res.Tx.Outputs {
			if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *lockScript) && o.Satoshis == args.Satoshis {
				res.OutputIndex = uint32(idx)
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("chain: client wallet broadcast tx %s did not contain expected BRC-29 output script", res.TxID)
		}
		res.DerivationPrefix = preB64
		res.DerivationSuffix = sufB64
		res.SenderIdentityKey = p.IdentityPubHex()
		return res, nil
	} else {
		return nil, errors.New("chain: toAddress or toIdentityKey required")
	}
	return p.clientCreateAction(ctx, []*transaction.TransactionOutput{{Satoshis: args.Satoshis, LockingScript: lockScript}}, args.Description, args.OverrideGuard)
}

func (p *Port) clientCreateAction(ctx context.Context, outputs []*transaction.TransactionOutput, desc string, overrideGuard bool) (*nwallet.SendResult, error) {
	var outTotal uint64
	for _, o := range outputs {
		outTotal += o.Satoshis
	}
	estFee := uint64(200)
	if f, fErr := p.feeSource.Fees(ctx); fErr == nil {
		if ef, eErr := f.For(250 + len(outputs)*34); eErr == nil {
			estFee = ef
		}
	}
	var resID string
	if !overrideGuard {
		// Atomically reserve daily SpendGuard budget BEFORE asking remote BRC-100 wallet to broadcast.
		var rErr error
		resID, rErr = p.guard.Reserve(estFee, outTotal+estFee)
		if rErr != nil {
			return nil, rErr
		}
	}
	if desc == "" {
		desc = "nektar client wallet action"
	}
	wOuts := make([]sdkwallet.CreateActionOutput, len(outputs))
	for i, o := range outputs {
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: desc,
		}
	}
	acceptDelayed := false
	randomize := false
	actRes, err := p.wallet.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      []string{"nektar"},
		Outputs:     wOuts,
		Options: &sdkwallet.CreateActionOptions{
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, Originator)
	if err != nil {
		lower := strings.ToLower(err.Error())
		uncertain := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			strings.Contains(lower, "timeout") || strings.Contains(lower, "eof") ||
			strings.Contains(lower, "connection reset") || strings.Contains(lower, "http 5") ||
			strings.Contains(lower, "status 5")
		if uncertain {
			// Remote wallet may already have broadcast: keep SpendGuard reservation intact and return ErrBroadcastUnknown.
			return nil, fmt.Errorf("chain: client wallet CreateAction: %w: %w", nwallet.ErrBroadcastUnknown, err)
		}
		if resID != "" {
			p.guard.CancelReservation(resID)
		}
		return nil, fmt.Errorf("chain: client wallet CreateAction: %w", err)
	}
	txid := actRes.Txid.String()
	var parsedTx *transaction.Transaction
	var parseErr error
	actualFee := estFee
	if len(actRes.Tx) > 0 {
		_, parsedTx, _, parseErr = guard.ParseBEEF(actRes.Tx, guard.DefaultBound)
		p.rememberClientTxBytes(txid, actRes.Tx, parsedTx)
	} else {
		p.rememberClientTxBytes(txid, nil, nil)
	}
	if parsedTx != nil {
		if f := computeParsedTxFee(parsedTx); f > 0 {
			actualFee = f
		}
	}
	if !overrideGuard {
		// Unconditionally commit real broadcast spend so spentToday never under-counts.
		if cErr := p.guard.CommitReservation(resID, txid, actualFee, outTotal+actualFee); cErr != nil {
			return nil, fmt.Errorf("chain: persist SpendGuard record for broadcast tx %s: %w", txid, cErr)
		}
	}
	if parseErr != nil {
		return nil, fmt.Errorf("chain: client wallet returned malformed AtomicBEEF for broadcast tx %s: %w", txid, parseErr)
	}
	return &nwallet.SendResult{
		TxID:     txid,
		Satoshis: outTotal,
		FeeSats:  actualFee,
		BEEF:     actRes.Tx,
		BEEFHex:  hex.EncodeToString(actRes.Tx),
		Tx:       parsedTx,
	}, nil
}

func computeParsedTxFee(tx *transaction.Transaction) uint64 {
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

func (p *Port) rememberClientTxBytes(txid string, rawBEEF []byte, tx *transaction.Transaction) {
	if txid == "" {
		return
	}
	// Persist durably under dataDir/client-txs so RAM eviction never breaks Broadcast idempotency or Port.Tx.
	if p.clientTxDir != "" && len(txid) == 64 {
		if err := os.MkdirAll(p.clientTxDir, 0o700); err == nil {
			path := filepath.Join(p.clientTxDir, txid+".beef")
			payload := rawBEEF
			if len(payload) == 0 && tx != nil {
				if b, err := tx.AtomicBEEF(false); err == nil && len(b) > 0 {
					payload = b
				}
			}
			if len(payload) > 0 {
				_ = os.WriteFile(path, payload, 0o600)
			} else {
				_ = os.WriteFile(filepath.Join(p.clientTxDir, txid+".sent"), []byte(txid), 0o600)
			}
		}
	}
	if tx == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clientBuilt) > 256 {
		for k := range p.clientBuilt {
			delete(p.clientBuilt, k)
			if len(p.clientBuilt) <= 128 {
				break
			}
		}
	}
	p.clientBuilt[txid] = tx
}

func (p *Port) hasClientTx(txid string) bool {
	if txid == "" {
		return false
	}
	p.mu.Lock()
	_, ok := p.clientBuilt[txid]
	p.mu.Unlock()
	if ok {
		return true
	}
	if p.clientTxDir != "" && len(txid) == 64 {
		if _, err := os.Stat(filepath.Join(p.clientTxDir, txid+".beef")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(p.clientTxDir, txid+".sent")); err == nil {
			return true
		}
	}
	return false
}

func (p *Port) loadClientTx(txid string) *transaction.Transaction {
	if txid == "" {
		return nil
	}
	p.mu.Lock()
	tx := p.clientBuilt[txid]
	p.mu.Unlock()
	if tx != nil {
		return tx
	}
	if p.clientTxDir != "" && len(txid) == 64 {
		if raw, err := os.ReadFile(filepath.Join(p.clientTxDir, txid+".beef")); err == nil && len(raw) > 0 {
			if _, parsed, _, pErr := guard.ParseBEEF(raw, guard.DefaultBound); pErr == nil && parsed != nil {
				p.mu.Lock()
				p.clientBuilt[txid] = parsed
				p.mu.Unlock()
				return parsed
			}
		}
	}
	return nil
}

// SendToAddress builds, signs, SpendGuard-checks, and broadcasts a payment to a BSV P2PKH address.
// Works seamlessly in BOTH embedded and client (BSV Desktop / BRC-100 wire) modes.
// Never blindly retries a second payment on broadcaster error strings.
func (p *Port) SendToAddress(ctx context.Context, address string, sats uint64) (*transaction.Transaction, string, error) {
	if sats == 0 {
		return nil, "", errors.New("chain: send amount must be > 0")
	}
	res, err := p.Send(ctx, nwallet.SendArgs{
		ToAddress:   address,
		Satoshis:    sats,
		Description: "nektar payment",
	})
	if err != nil {
		return nil, "", err
	}
	return res.Tx, res.TxID, nil
}

// Sweep sweeps 100% of spendable funds in embedded mode to a P2PKH address or BRC-100 identity key.
func (p *Port) Sweep(ctx context.Context, args nwallet.SweepArgs) (*nwallet.SweepResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: sweep is supported in embedded wallet mode (manage funds directly in BSV Desktop in client mode)")
	}
	return p.engine.Sweep(ctx, args)
}

// Consolidate merges fragmented small UTXOs into clean reserve outputs (embedded mode).
func (p *Port) Consolidate(ctx context.Context, args nwallet.ConsolidateArgs) (*nwallet.ConsolidateResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: consolidate is supported in embedded wallet mode")
	}
	return p.engine.Consolidate(ctx, args)
}

// ImportBEEF verifies and imports a BEEF/AtomicBEEF payment into the embedded wallet.
func (p *Port) ImportBEEF(ctx context.Context, beefBytes []byte) (*nwallet.ImportResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: import BEEF is supported in embedded wallet mode")
	}
	return p.engine.ImportBEEF(ctx, beefBytes)
}

// ImportTxID fetches, SPV-verifies, and imports a mined txid into the embedded wallet.
func (p *Port) ImportTxID(ctx context.Context, txid string) (*nwallet.ImportResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: import txid is supported in embedded wallet mode")
	}
	return p.engine.ImportTxID(ctx, txid)
}

// FundFromDesktop performs 1-click BRC-29 funding from local BSV Desktop / BRC-100 wire into Nektar.
func (p *Port) FundFromDesktop(ctx context.Context, wireURL string, sats uint64) (*nwallet.DesktopFundResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: fund-from-desktop is for embedded wallet mode (already using client wallet)")
	}
	if wireURL == "" {
		wireURL = p.wireURL
	}
	return nwallet.FundFromWireWallet(ctx, wireURL, sats, p.engine)
}

// SweepToDesktop sweeps 100% of spendable funds from Nektar into local BSV Desktop via BRC-29 + InternalizeAction.
func (p *Port) SweepToDesktop(ctx context.Context, wireURL string, overrideGuard bool) (*nwallet.DesktopSweepResult, error) {
	if p.engine == nil {
		return nil, errors.New("chain: sweep-to-desktop is for embedded wallet mode")
	}
	if wireURL == "" {
		wireURL = p.wireURL
	}
	return nwallet.SweepToWireWallet(ctx, wireURL, p.engine, overrideGuard)
}

// Height returns the witnessed chain tip height (never delegating trust to a remote client wallet).
func (p *Port) Height(ctx context.Context) (uint32, error) {
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		return p.testChain.Height(), nil
	}
	if p.tracker == nil {
		return 0, nwallet.ErrNoChain
	}
	return p.tracker.CurrentHeight(ctx)
}

// BuildFundedTx builds and signs a transaction paying outputs using multi-input CanonicalChangeSelector
// and dynamic feepolicy in embedded mode, OR delegates directly to wallet.CreateAction in client mode!
func (p *Port) BuildFundedTx(ctx context.Context, outputs []*transaction.TransactionOutput) (*transaction.Transaction, uint64, uint64, error) {
	tx, _, fee, spend, err := p.BuildFundedTxWithBEEF(ctx, outputs)
	return tx, fee, spend, err
}

// BuildFundedTxWithBEEF preserves the strict AtomicBEEF bytes produced by go-wallet-toolbox directly
// without discarding and re-encoding through an allowPartial=true fallback.
func (p *Port) BuildFundedTxWithBEEF(ctx context.Context, outputs []*transaction.TransactionOutput) (*transaction.Transaction, []byte, uint64, uint64, error) {
	if len(outputs) == 0 {
		return nil, nil, 0, 0, errors.New("chain: BuildFundedTx requires at least one output")
	}

	// Client mode: delegate cleanly to remote BRC-100 wallet.CreateAction.
	if p.engine == nil {
		res, err := p.clientCreateAction(ctx, outputs, "nektar funded tx", false)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		if res.Tx == nil || len(res.BEEF) == 0 {
			return nil, nil, 0, 0, fmt.Errorf("chain: client wallet did not return parseable AtomicBEEF for %s", res.TxID)
		}
		return res.Tx, res.BEEF, res.FeeSats, res.Satoshis + res.FeeSats, nil
	}

	// Embedded mode: build & sign atomically as NoSend via Engine.CreateAction so inputs stay reserved
	// until Broadcast() runs SpendGuard and submits to chain!
	wOuts := make([]sdkwallet.CreateActionOutput, len(outputs))
	var outTotal uint64
	for i, o := range outputs {
		if o == nil || o.LockingScript == nil {
			return nil, nil, 0, 0, fmt.Errorf("chain: output %d has nil locking script", i)
		}
		outTotal += o.Satoshis
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: "nektar funded output",
		}
	}
	noSend := true
	randomize := false
	res, err := p.engine.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: "nektar funded tx",
		Labels:      []string{"funded"},
		Outputs:     wOuts,
		Options: &sdkwallet.CreateActionOptions{
			NoSend:           &noSend,
			RandomizeOutputs: &randomize,
		},
	}, Originator)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	txid := res.Txid.String()
	var tx *transaction.Transaction
	beef := res.Tx
	if len(beef) > 0 {
		var pErr error
		_, tx, _, pErr = guard.ParseBEEF(beef, guard.DefaultBound)
		if pErr != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, nil, 0, 0, fmt.Errorf("chain: parse toolbox AtomicBEEF for %s: %w", txid, pErr)
		}
	}
	if tx == nil {
		tx, err = p.engine.LoadParsedTx(ctx, txid)
		if err != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, nil, 0, 0, err
		}
	}
	if len(beef) == 0 {
		var bErr error
		beef, bErr = SafeAtomicBEEF(tx)
		if bErr != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, nil, 0, 0, bErr
		}
	}
	feeSats := p.engine.TxFeeSats(ctx, txid)
	p.mu.Lock()
	p.pendingNoSend[txid] = txid
	p.mu.Unlock()

	return tx, beef, feeSats, outTotal + feeSats, nil
}

// BuildBRC29Tx constructs a signed (NoSend in embedded mode) BRC-29 payment transaction and returns
// its remittance metadata BEFORE broadcasting so durable workflows can persist txid + BEEF + nonces
// atomically prior to Broadcast (eliminating double-pay and stranded remittance on crash/retry).
// In client mode, where the remote wallet signs and broadcasts atomically, it executes Send directly
// and sets alreadyBroadcast=true.
func (p *Port) BuildBRC29Tx(ctx context.Context, toIdentityKey string, sats uint64, desc string) (tx *transaction.Transaction, fee, spend uint64, res *nwallet.SendResult, alreadyBroadcast bool, err error) {
	if p.engine == nil {
		sr, sErr := p.Send(ctx, nwallet.SendArgs{
			ToIdentityKey: toIdentityKey,
			Satoshis:      sats,
			Description:   desc,
			Labels:        []string{"market", "purchase"},
		})
		if sErr != nil {
			return nil, 0, 0, nil, false, sErr
		}
		return sr.Tx, sr.FeeSats, sr.Satoshis + sr.FeeSats, sr, true, nil
	}
	preB64, sufB64 := randomBRC29Nonce()
	lockScript, err := p.signer.PaymentDestination(ctx, strings.TrimSpace(toIdentityKey), preB64, sufB64)
	if err != nil {
		return nil, 0, 0, nil, false, err
	}
	tx, beef, fee, spend, err := p.BuildFundedTxWithBEEF(ctx, []*transaction.TransactionOutput{{Satoshis: sats, LockingScript: lockScript}})
	if err != nil {
		return nil, 0, 0, nil, false, err
	}
	var outIdx uint32
	matched := false
	if tx != nil && lockScript != nil {
		for idx, o := range tx.Outputs {
			if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *lockScript) && o.Satoshis == sats {
				outIdx = uint32(idx)
				matched = true
				break
			}
		}
	}
	if !matched {
		_ = p.engine.AbortNoSendByTxID(ctx, tx.TxID().String())
		return nil, 0, 0, nil, false, errors.New("chain: built BRC-29 tx missing expected destination output")
	}
	if len(beef) == 0 {
		var bErr error
		beef, bErr = SafeAtomicBEEF(tx)
		if bErr != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, tx.TxID().String())
			return nil, 0, 0, nil, false, bErr
		}
	}
	return tx, fee, spend, &nwallet.SendResult{
		TxID:              tx.TxID().String(),
		Satoshis:          sats,
		FeeSats:           fee,
		BEEF:              beef,
		BEEFHex:           hex.EncodeToString(beef),
		Tx:                tx,
		OutputIndex:       outIdx,
		DerivationPrefix:  preB64,
		DerivationSuffix:  sufB64,
		SenderIdentityKey: p.IdentityPubHex(),
	}, false, nil
}

// Broadcast enforces SpendGuard caps, then broadcasts tx on testchain or configured live settler.
// On ANY failure before broadcast succeeds, reserved UTXOs are atomically rolled back via go-wallet-toolbox AbortAction!
func (p *Port) Broadcast(ctx context.Context, tx *transaction.Transaction, feeSats, spendSats uint64) (string, error) {
	if tx == nil {
		return "", errors.New("chain: nil transaction")
	}
	txid := tx.TxID().String()

	// Client mode: BuildFundedTx already broadcast via remote BRC-100 CreateAction (and ran SpendGuard).
	if p.engine == nil && p.hasClientTx(txid) {
		return txid, nil
	}

	p.mu.Lock()
	_, hadNoSend := p.pendingNoSend[txid]
	if hadNoSend {
		delete(p.pendingNoSend, txid)
	}
	p.mu.Unlock()
	// Consult durable wallet.sqlite status in addition to RAM pendingNoSend so a restart/retry
	// between BuildFundedTx and Broadcast NEVER bypasses SendNoSendTx into raw settler.Submit.
	if p.engine != nil && !hadNoSend {
		if st, ok := p.engine.TxStatus(ctx, txid); ok && st == "nosend" {
			hadNoSend = true
		}
	}

	rollback := func() {
		if p.engine == nil || !hadNoSend {
			return
		}
		_ = p.engine.AbortNoSendByTxID(ctx, txid)
	}

	// If tx was created via BuildFundedTx (NoSend=true in go-wallet-toolbox), broadcast it via
	// go-wallet-toolbox SendWith so PostFromBEEF runs the single exact SpendGuard chokepoint
	// and storage status, KnownTx, and UTXO lifecycle stay 100% consistent!
	if p.engine != nil && hadNoSend {
		if err := p.engine.SendNoSendTx(ctx, txid); err != nil {
			if errors.Is(err, nwallet.ErrBroadcastUnknown) {
				// Broadcast outcome uncertain: NEVER abort/unlock UTXOs! Mark submission_unknown for Monitor.UnFail.
				_ = p.engine.MarkSubmissionUnknown(ctx, txid)
				return "", fmt.Errorf("chain: broadcast %s: %w", txid, err)
			}
			// Only release UTXOs locally on genuine pre-broadcast refusal (SpendGuard cap);
			// post-submit broadcaster error strings leave the tx in submission_unknown until reconciled.
			if errors.Is(err, nwallet.ErrSpendRejected) {
				rollback()
			} else {
				_ = p.engine.MarkSubmissionUnknown(ctx, txid)
			}
			return "", fmt.Errorf("chain: broadcast %s: %w", txid, err)
		}
		if p.testChain != nil {
			p.tcMu.Lock()
			if mp, _, ok := p.testChain.Proof(txid); ok && mp != nil {
				tx.MerklePath = mp
			}
			p.tcMu.Unlock()
		}
		p.engine.CacheTx(tx)
		return txid, nil
	}

	if err := p.guard.CheckAndRecord(txid, feeSats, spendSats); err != nil {
		rollback()
		return "", err
	}

	if p.testChain != nil {
		p.tcMu.Lock()
		err := p.testChain.Send(tx)
		if err == nil {
			if pmp, _, ok := p.testChain.Proof(txid); ok {
				tx.MerklePath = pmp
			}
		}
		p.tcMu.Unlock()
		if err != nil {
			if rErr := p.guard.Refund(txid); rErr != nil {
				return "", fmt.Errorf("chain: testchain broadcast: %w (spendguard refund failed: %v)", err, rErr)
			}
			return "", fmt.Errorf("chain: testchain broadcast: %w", err)
		}
		if p.engine != nil {
			p.engine.CacheTx(tx)
			p.engine.FuelKeeper().Wake()
		}
		return txid, nil
	}

	if p.settler == nil {
		if rErr := p.guard.Refund(txid); rErr != nil {
			return "", fmt.Errorf("chain: no live settler configured (spendguard refund failed: %w)", rErr)
		}
		return "", errors.New("chain: no live settler configured (set NEKTAR_SETTLE_SPEC)")
	}
	if err := p.settler.Submit(ctx, tx); err != nil {
		// Never refund SpendGuard on post-submit broadcaster error strings: treat all post-submit
		// failures conservatively (ErrBroadcastUnknown on transport/timeout, error hint otherwise).
		lower := strings.ToLower(err.Error())
		if errors.Is(err, nwallet.ErrBroadcastUnknown) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			strings.Contains(lower, "timeout") || strings.Contains(lower, "eof") ||
			strings.Contains(lower, "http 5") || strings.Contains(lower, "connection refused") {
			return "", fmt.Errorf("chain: %s broadcast: %w: %w", p.settler.Name(), nwallet.ErrBroadcastUnknown, err)
		}
		return "", fmt.Errorf("chain: %s broadcast: %w", p.settler.Name(), err)
	}
	if p.engine != nil {
		p.engine.CacheTx(tx)
		p.engine.FuelKeeper().Wake()
	}
	return txid, nil
}

// AdoptChange is preserved for backwards compatibility with callers that run BuildFundedTx -> Broadcast -> AdoptChange.
// Wallet-built transactions already persist change outputs atomically in go-wallet-toolbox storage.
func (p *Port) AdoptChange(tx *transaction.Transaction) error {
	if p.engine == nil || tx == nil {
		return nil
	}
	if p.engine.HasTx(context.Background(), tx.TxID().String()) {
		return nil
	}
	_, err := p.engine.AdoptReceivedTx(context.Background(), tx)
	return err
}

// ProofChecked returns the witnessed-header-verified MerklePath + height if txid is mined,
// returning explicit errors on tracker/upstream outages instead of collapsing outages to false.
func (p *Port) ProofChecked(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		mp, h, ok := p.testChain.Proof(txid)
		return mp, h, ok, nil
	}
	if p.tracker == nil {
		return nil, 0, false, fmt.Errorf("%w: no chain tracker configured", ErrTrackerUnavailable)
	}
	txHash, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, 0, false, fmt.Errorf("chain: invalid txid %q: %w", txid, err)
	}
	verifyProof := func(mp *transaction.MerklePath, h uint32) (bool, error) {
		if mp == nil {
			return false, nil
		}
		if h == 0 {
			h = mp.BlockHeight
		}
		root, rErr := mp.ComputeRoot(txHash)
		if rErr != nil || root == nil {
			return false, nil
		}
		valid, vErr := p.tracker.IsValidRootForHeight(ctx, root, h)
		if vErr != nil {
			return false, fmt.Errorf("%w: %w", ErrTrackerUnavailable, vErr)
		}
		return valid, nil
	}

	// 1. Check locally held verified proof FIRST (avoid unnecessary network refetch).
	if p.engine != nil {
		if tx, lErr := p.engine.LoadParsedTx(ctx, txid); lErr == nil && tx != nil && tx.MerklePath != nil {
			valid, vErr := verifyProof(tx.MerklePath, tx.MerklePath.BlockHeight)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				return tx.MerklePath, tx.MerklePath.BlockHeight, true, nil
			}
		}
	}

	var lastErr error
	// 2. Ask Arcade, then nodeChain (every proof verified against independent witnessed headers).
	if p.arcade != nil {
		mp, h, aErr := p.arcade.Proof(ctx, txid)
		if aErr == nil && mp != nil {
			valid, vErr := verifyProof(mp, h)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				if h == 0 {
					h = mp.BlockHeight
				}
				return mp, h, true, nil
			}
		} else if aErr != nil && !errors.Is(aErr, nodeapi.ErrNotMined) && !errors.Is(aErr, publish.ErrArcadeUnknown) && !errors.Is(aErr, publish.ErrArcadeRefused) {
			lastErr = aErr
		}
	}
	if p.nodeChain != nil {
		mp, h, cErr := p.nodeChain.Proof(ctx, txid)
		if cErr == nil && mp != nil {
			valid, vErr := verifyProof(mp, h)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				if h == 0 {
					h = mp.BlockHeight
				}
				return mp, h, true, nil
			}
		} else if cErr != nil && !errors.Is(cErr, nodeapi.ErrNotMined) {
			lastErr = cErr
		}
	}
	return nil, 0, false, lastErr
}

// Proof returns the header-verified MerklePath + height if txid is mined.
func (p *Port) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool) {
	mp, h, ok, _ := p.ProofChecked(ctx, txid)
	return mp, h, ok
}

// TxBroadcastStatus returns verified mining proof AND durable local wallet status for txid,
// enabling crash recovery (ReconcilePendingAnchors) to distinguish unbroadcast NoSend txs from live broadcasts.
func (p *Port) TxBroadcastStatus(ctx context.Context, txid string) (mined bool, walletStatus string, mp *transaction.MerklePath, height uint32) {
	mp, height, mined, _ = p.ProofChecked(ctx, txid)
	if p.engine != nil {
		if st, ok := p.engine.TxStatus(ctx, txid); ok {
			walletStatus = st
		}
	} else if mined {
		walletStatus = "completed"
	} else if p.Tx(txid) != nil {
		walletStatus = "unproven"
	}
	return mined, walletStatus, mp, height
}

// Tx returns a known transaction by txid (from testchain, bounded SQLite LRU cache, or durable client cache).
func (p *Port) Tx(txid string) *transaction.Transaction {
	if p.testChain != nil {
		p.tcMu.Lock()
		tx := p.testChain.Tx(txid)
		if tx == nil {
			tx = p.memCovenantTxs[txid]
		}
		p.tcMu.Unlock()
		if tx != nil {
			return tx
		}
	}
	if p.engine != nil {
		if tx, err := p.engine.LoadParsedTx(context.Background(), txid); err == nil && tx != nil {
			return tx
		}
	}
	return p.loadClientTx(txid)
}

// SignDigest signs a 32-byte digest under Nektar's BRC-43 identity key using wallet.Interface.
func (p *Port) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	res, err := p.signer.CreateSignature(ctx, sdkwallet.CreateSignatureArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar provenance"},
			KeyID:      "default",
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
		HashToDirectlySign: digest,
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.Signature.Serialize(), nil
}

// SigningPubKey returns the public key corresponding to SignDigest.
func (p *Port) SigningPubKey(ctx context.Context) (*ec.PublicKey, error) {
	res, err := p.signer.GetPublicKey(ctx, sdkwallet.GetPublicKeyArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar provenance"},
			KeyID:      "default",
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.PublicKey, nil
}

// RelayExternalTx broadcasts an incoming third-party (buyer) transaction to the configured live settler
// before releasing paid goods under zero-conf policy (does not consume the receiver's SpendGuard budget).
func (p *Port) RelayExternalTx(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("chain: nil external transaction")
	}
	if p.testChain != nil || p.network == "memory" {
		return nil
	}
	if p.settler == nil {
		return errors.New("chain: no live settler configured to broadcast zero-conf buyer payment")
	}
	return p.settler.Submit(ctx, tx)
}

// EncryptAtRest wraps sensitive local secrets (e.g. Borg epoch keys, market signing key)
// using the node's BRC-43 wallet key (CounterpartyTypeSelf) so raw secrets are never stored in plaintext SQLite.
func (p *Port) EncryptAtRest(ctx context.Context, keyID string, plaintext []byte) ([]byte, error) {
	if p == nil || p.wallet == nil {
		return nil, errors.New("chain: wallet unavailable for EncryptAtRest")
	}
	res, err := p.wallet.Encrypt(ctx, sdkwallet.EncryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar storage"},
			KeyID:      keyID,
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
		Plaintext: plaintext,
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.Ciphertext, nil
}

// DecryptAtRest unwraps a ciphertext created by EncryptAtRest.
func (p *Port) DecryptAtRest(ctx context.Context, keyID string, ciphertext []byte) ([]byte, error) {
	if p == nil || p.wallet == nil {
		return nil, errors.New("chain: wallet unavailable for DecryptAtRest")
	}
	res, err := p.wallet.Decrypt(ctx, sdkwallet.DecryptArgs{
		EncryptionArgs: sdkwallet.EncryptionArgs{
			ProtocolID: sdkwallet.Protocol{SecurityLevel: sdkwallet.SecurityLevelEveryApp, Protocol: "nektar storage"},
			KeyID:      keyID,
			Counterparty: sdkwallet.Counterparty{
				Type: sdkwallet.CounterpartyTypeSelf,
			},
		},
		Ciphertext: ciphertext,
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.Plaintext, nil
}

// AnchorOutput helper creates a 1-sat output locked to a NektarAnchor script.
func AnchorOutput(lockScript *script.Script) *transaction.TransactionOutput {
	return &transaction.TransactionOutput{
		Satoshis:      1,
		LockingScript: lockScript,
	}
}

func randomBRC29Nonce() (string, string) {
	var pre, suf [16]byte
	if _, err := rand.Read(pre[:]); err != nil {
		panic("chain: crypto/rand failed: " + err.Error())
	}
	if _, err := rand.Read(suf[:]); err != nil {
		panic("chain: crypto/rand failed: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(pre[:]), base64.StdEncoding.EncodeToString(suf[:])
}
