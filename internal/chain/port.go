package chain

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/headers"
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
	Network         string // memory | regtest | testnet | main
	DataDir         string
	WalletMode      string // embedded (default) | client (BRC-100 wire on localhost:3321)
	WalletWireURL   string // default http://127.0.0.1:3321
	WalletProfile   string // standard (default) | throughput
	FeeRateSatPerKB uint64 // optional static sat/kB override (0 => live Arcade /v1/policy clamped [100,500] fallback 100)
	HeaderSource    string // headers.NewSource spec (e.g. woc:main, chaintracks:https://..., native:http://...)
	ChainSpec       string // nodeapi.ParseChain spec (e.g. woc:main, teranode:http://...)
	SettleSpec      string // publish.ParseSettler spec (e.g. arcade:https://..., rpc:http://...)
	ArcadeKey       string
	RPCUser         string
	RPCPass         string
	WoCKey          string
	MainnetConfirm  string // must equal "I_UNDERSTAND" when Network == "main"
	SpendGuard      core.SpendGuardOpts
}

// Port is the single chain + BRC-100 custody abstraction consumed by the entire Nektar Go runtime.
// In embedded mode it wraps internal/wallet.Engine (pure-Go WAL SQLite, CanonicalChangeSelector,
// FuelKeeper, Monitor); in client mode it speaks standard BRC-100 binary wire (bcommon/wirewallet)
// to BSV Desktop, Metanet Desktop, or an external go-wallet-toolbox instance.
type Port struct {
	mu           sync.Mutex
	tcMu         sync.Mutex
	network      string
	mainnet      bool
	walletMode   string
	wireURL      string
	dataDir      string
	guard        *core.SpendGuard
	wallet       sdkwallet.Interface
	engine       *nwallet.Engine // non-nil in embedded mode
	signer       *bwallet.Signer
	tracker      chaintracker.ChainTracker
	headerClient *headers.Client
	testChain    *testchain.Chain
	nodeChain    *nodeapi.Sources
	woc          *nodeapi.WoC
	settler      publish.Settler
	arcade       *publish.Arcade
	feeSource    feepolicy.Source
	httpClient   *http.Client

	// Lightweight bookkeeping for BuildFundedTx -> Broadcast split callers & client-mode txs.
	pendingNoSend map[string]string                   // txid -> action reference (embedded BuildFundedTx)
	clientBuilt   map[string]*transaction.Transaction // bounded cache for client-mode txs already broadcast via CreateAction
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

	p := &Port{
		network:       netName,
		mainnet:       mainnet,
		walletMode:    walletMode,
		wireURL:       cfg.WalletWireURL,
		dataDir:       dir,
		guard:         guardInst,
		httpClient:    &http.Client{Timeout: 20 * time.Second},
		pendingNoSend: make(map[string]string),
		clientBuilt:   make(map[string]*transaction.Transaction),
	}

	// 1. Configure chain tracker, chain view (WoC/node), settler (Arcade/RPC/TCP), and live feepolicy!
	switch netName {
	case "memory":
		tc := testchain.New(1)
		p.testChain = tc
		p.tracker = tc
	default:
		spec := cfg.HeaderSource
		if spec == "" {
			switch netName {
			case "main":
				spec = "woc:main"
			case "testnet":
				spec = "woc:test"
			default:
				spec = "native:http://127.0.0.1:8080"
			}
		}
		hc, err := headers.NewSource(spec)
		if err != nil {
			return nil, fmt.Errorf("chain: header source %q: %w", spec, err)
		}
		p.headerClient = hc
		p.tracker = hc

		chainSpec := cfg.ChainSpec
		if chainSpec == "" {
			switch netName {
			case "main":
				chainSpec = "woc:main"
			case "testnet":
				chainSpec = "woc:test"
			}
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
		settleSpec := cfg.SettleSpec
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
			p.settler = settler
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
			SpendGuardCheck:  p.guard.Check,
			SpendGuardRefund: p.guard.Refund,
		}
		if p.headerClient != nil {
			wCfg.HeaderBytes = func(hCtx context.Context, height uint32) ([]byte, error) {
				hdr, err := p.headerClient.HeaderAt(hCtx, height)
				if err != nil {
					return nil, err
				}
				if hdr == nil {
					return nil, fmt.Errorf("chain: nil block header at height %d", height)
				}
				raw := make([]byte, 80)
				binary.LittleEndian.PutUint32(raw[0:4], hdr.Version)
				copy(raw[4:36], hdr.Prev[:])
				copy(raw[36:68], hdr.MerkleRoot[:])
				binary.LittleEndian.PutUint32(raw[68:72], hdr.Time)
				binary.LittleEndian.PutUint32(raw[72:76], hdr.Bits)
				binary.LittleEndian.PutUint32(raw[76:80], hdr.Nonce)
				return raw, nil
			}
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

func (p *Port) Network() string                       { return p.network }
func (p *Port) Mainnet() bool                         { return p.mainnet }
func (p *Port) WalletMode() string                    { return p.walletMode }
func (p *Port) Tracker() chaintracker.ChainTracker    { return p.tracker }
func (p *Port) SpendGuard() *core.SpendGuard          { return p.guard }
func (p *Port) WalletInterface() sdkwallet.Interface  { return p.wallet }
func (p *Port) Engine() *nwallet.Engine               { return p.engine }
func (p *Port) Signer() *bwallet.Signer               { return p.signer }
func (p *Port) TestChain() *testchain.Chain           { return p.testChain }
func (p *Port) IdentityPubHex() string                { return p.signer.IdentityHex() }
func (p *Port) FeeSource() feepolicy.Source           { return p.feeSource }

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
	// Client mode: query remote BRC-100 wallet via ListOutputs!
	if p.wallet == nil {
		return nwallet.BalanceBreakdown{}, nil
	}
	limit := uint32(1000)
	res, err := p.wallet.ListOutputs(ctx, sdkwallet.ListOutputsArgs{
		Basket: nwallet.BasketDefault,
		Limit:  &limit,
	}, Originator)
	if err != nil {
		return nwallet.BalanceBreakdown{}, err
	}
	var b nwallet.BalanceBreakdown
	for _, o := range res.Outputs {
		b.UTXOCount++
		b.TotalSats += o.Satoshis
		if o.Spendable {
			b.SpendableCount++
			b.SpendableSats += o.Satoshis
			b.ConfirmedSats += o.Satoshis
			b.DefaultCount++
			b.DefaultSats += o.Satoshis
		} else {
			b.ReservedSats += o.Satoshis
		}
	}
	return b, nil
}

// BalanceSats returns total spendable satoshis (works in both embedded and client modes!).
func (p *Port) BalanceSats() uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0
	}
	return bal.SpendableSats
}

// UTXOCount returns total spendable UTXOs (works in both embedded and client modes!).
func (p *Port) UTXOCount() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bal, err := p.Balance(ctx)
	if err != nil {
		return 0
	}
	return bal.SpendableCount
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
	if !overrideGuard {
		// Read-only cap pre-check before asking remote BRC-100 wallet to sign/broadcast.
		if err := p.guard.Check(estFee, outTotal+estFee); err != nil {
			return nil, err
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
		return nil, fmt.Errorf("chain: client wallet CreateAction: %w", err)
	}
	txid := actRes.Txid.String()
	var parsedTx *transaction.Transaction
	actualFee := estFee
	if len(actRes.Tx) > 0 {
		_, parsedTx, _, _ = guard.ParseBEEF(actRes.Tx, guard.DefaultBound)
	}
	if parsedTx != nil {
		p.rememberClientTx(txid, parsedTx)
		if f := computeParsedTxFee(parsedTx); f > 0 {
			actualFee = f
		}
	}
	if !overrideGuard {
		// Record real txid & fee after client wallet signs/broadcasts (idempotent on txid).
		_ = p.guard.CheckAndRecord(txid, actualFee, outTotal+actualFee)
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

func (p *Port) rememberClientTx(txid string, tx *transaction.Transaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clientBuilt) > 256 {
		// Evict arbitrary old entries to keep client-mode cache bounded.
		for k := range p.clientBuilt {
			delete(p.clientBuilt, k)
			if len(p.clientBuilt) <= 128 {
				break
			}
		}
	}
	p.clientBuilt[txid] = tx
}

// SendToAddress builds, signs, SpendGuard-checks, and broadcasts a payment to a BSV P2PKH address.
// Works seamlessly in BOTH embedded and client (BSV Desktop / BRC-100 wire) modes!
func (p *Port) SendToAddress(ctx context.Context, address string, sats uint64) (*transaction.Transaction, string, error) {
	if sats == 0 {
		return nil, "", errors.New("chain: send amount must be > 0")
	}
	for attempt := range 3 {
		res, err := p.Send(ctx, nwallet.SendArgs{
			ToAddress:   address,
			Satoshis:    sats,
			Description: "nektar payment",
		})
		if err != nil {
			if attempt < 2 && (strings.Contains(err.Error(), "UTXO_SPENT") || strings.Contains(err.Error(), "spent by another transaction")) {
				continue
			}
			return nil, "", err
		}
		return res.Tx, res.TxID, nil
	}
	return nil, "", errors.New("chain: failed to broadcast payment after pruning stale UTXOs")
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

// Height returns the current chain tip height.
func (p *Port) Height(ctx context.Context) (uint32, error) {
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		return p.testChain.Height(), nil
	}
	if p.walletMode == "client" && p.wallet != nil {
		if hRes, err := p.wallet.GetHeight(ctx, nil, Originator); err == nil && hRes != nil && hRes.Height > 0 {
			return hRes.Height, nil
		}
	}
	if p.tracker == nil {
		return 0, nwallet.ErrNoChain
	}
	return p.tracker.CurrentHeight(ctx)
}

// BuildFundedTx builds and signs a transaction paying outputs using multi-input CanonicalChangeSelector
// and dynamic feepolicy in embedded mode, OR delegates directly to wallet.CreateAction in client mode!
func (p *Port) BuildFundedTx(ctx context.Context, outputs []*transaction.TransactionOutput) (*transaction.Transaction, uint64, uint64, error) {
	if len(outputs) == 0 {
		return nil, 0, 0, errors.New("chain: BuildFundedTx requires at least one output")
	}

	// Client mode: delegate cleanly to remote BRC-100 wallet.CreateAction (fixes broken client mode!).
	if p.engine == nil {
		res, err := p.clientCreateAction(ctx, outputs, "nektar funded tx", false)
		if err != nil {
			return nil, 0, 0, err
		}
		if res.Tx == nil {
			return nil, 0, 0, fmt.Errorf("chain: client wallet did not return parseable AtomicBEEF for %s", res.TxID)
		}
		return res.Tx, res.FeeSats, res.Satoshis + res.FeeSats, nil
	}

	// Embedded mode: build & sign atomically as NoSend via Engine.CreateAction so inputs stay reserved
	// until Broadcast() runs SpendGuard and submits to chain!
	wOuts := make([]sdkwallet.CreateActionOutput, len(outputs))
	var outTotal uint64
	for i, o := range outputs {
		if o == nil || o.LockingScript == nil {
			return nil, 0, 0, fmt.Errorf("chain: output %d has nil locking script", i)
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
		return nil, 0, 0, err
	}
	txid := res.Txid.String()
	var tx *transaction.Transaction
	if len(res.Tx) > 0 {
		_, tx, _, _ = guard.ParseBEEF(res.Tx, guard.DefaultBound)
	}
	if tx == nil {
		tx, err = p.engine.LoadParsedTx(ctx, txid)
		if err != nil {
			return nil, 0, 0, err
		}
	}
	feeSats := p.engine.TxFeeSats(ctx, txid)
	p.mu.Lock()
	p.pendingNoSend[txid] = txid
	p.mu.Unlock()

	return tx, feeSats, outTotal + feeSats, nil
}

// Broadcast enforces SpendGuard caps, then broadcasts tx on testchain or configured live settler.
// On ANY failure before broadcast succeeds, reserved UTXOs are atomically rolled back via go-wallet-toolbox AbortAction!
func (p *Port) Broadcast(ctx context.Context, tx *transaction.Transaction, feeSats, spendSats uint64) (string, error) {
	if tx == nil {
		return "", errors.New("chain: nil transaction")
	}
	txid := tx.TxID().String()

	// Client mode: BuildFundedTx already broadcast via remote BRC-100 CreateAction (and ran SpendGuard).
	if p.engine == nil {
		p.mu.Lock()
		_, alreadyClient := p.clientBuilt[txid]
		p.mu.Unlock()
		if alreadyClient {
			return txid, nil
		}
	}

	p.mu.Lock()
	_, hadNoSend := p.pendingNoSend[txid]
	if hadNoSend {
		delete(p.pendingNoSend, txid)
	}
	p.mu.Unlock()

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
			// Genuine pre-broadcast refusal (SpendGuard cap, double spend, missing inputs): release UTXOs cleanly.
			rollback()
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
			p.guard.Refund(txid)
			return "", fmt.Errorf("chain: testchain broadcast: %w", err)
		}
		if p.engine != nil {
			p.engine.CacheTx(tx)
			p.engine.FuelKeeper().Wake()
		}
		return txid, nil
	}

	if p.settler == nil {
		p.guard.Refund(txid)
		return "", errors.New("chain: no live settler configured (set NEKTAR_SETTLE_SPEC)")
	}
	if err := p.settler.Submit(ctx, tx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
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

// Proof returns the verified MerklePath + height if txid is mined.
func (p *Port) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool) {
	if p.testChain != nil {
		p.tcMu.Lock()
		defer p.tcMu.Unlock()
		return p.testChain.Proof(txid)
	}
	if p.arcade != nil {
		if mp, h, err := p.arcade.Proof(ctx, txid); err == nil && mp != nil {
			return mp, h, true
		}
	}
	if p.nodeChain != nil {
		if mp, h, err := p.nodeChain.Proof(ctx, txid); err == nil && mp != nil {
			return mp, h, true
		}
	}
	if p.engine != nil {
		if tx, err := p.engine.LoadParsedTx(ctx, txid); err == nil && tx != nil && tx.MerklePath != nil {
			return tx.MerklePath, tx.MerklePath.BlockHeight, true
		}
	}
	return nil, 0, false
}

// Tx returns a known transaction by txid (from testchain, bounded SQLite LRU cache, or client cache).
func (p *Port) Tx(txid string) *transaction.Transaction {
	if p.testChain != nil {
		p.tcMu.Lock()
		tx := p.testChain.Tx(txid)
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clientBuilt[txid]
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
	_, _ = rand.Read(pre[:])
	_, _ = rand.Read(suf[:])
	return base64.StdEncoding.EncodeToString(pre[:]), base64.StdEncoding.EncodeToString(suf[:])
}
