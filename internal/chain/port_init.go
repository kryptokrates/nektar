package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func (p *Port) initNetwork(cfg Config) error {
	netName := p.network
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
		if cfg.TestTracker != nil {
			p.tracker = cfg.TestTracker
		}
		return nil
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
				return fmt.Errorf("chain: witnessed header tracker: %w", err)
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
				return fmt.Errorf("chain: chain spec %q: %w", chainSpec, err)
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
				return fmt.Errorf("chain: settle spec %q: %w", settleSpec, err)
			}
			p.broadcaster = bcast
			p.broadcasterURLs = bcastURLs
		}
		return nil
	}
}

func (p *Port) initFeeSource(cfg Config) {
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
}

func (p *Port) openClientWallet(ctx context.Context, cfg Config, dir string, stickyMode bool) error {
	if p.network == "regtest" || p.network == "memory" {
		return errors.New("chain: client wallet custody is for testnet/main only")
	}
	wireURL := cfg.WalletWireURL
	if wireURL == "" {
		wireURL = nwallet.DefaultDesktopWireURL
	}
	p.wireURL = wireURL
	if err := wirewallet.CheckURL(wireURL); err != nil {
		return fmt.Errorf("chain: BRC-100 wire URL refused: %w", err)
	}
	w, err := nwallet.DialDesktopWallet(ctx, Originator, wireURL, 30*time.Second)
	if err != nil {
		return fmt.Errorf("chain: dial BRC-100 wallet at %s: %w", wireURL, err)
	}
	netRes, err := w.GetNetwork(ctx, nil, Originator)
	if err != nil {
		if stickyMode {
			return fmt.Errorf("chain: this node is bound to BSV Desktop (client custody at %s), but BSV Desktop is not reachable — open & unlock BSV Desktop or use a fresh --data-dir with --wallet embedded (%w)", wireURL, err)
		}
		return fmt.Errorf("chain: BRC-100 wallet network check failed: %w", err)
	}
	wantNet := sdkwallet.NetworkTestnet
	if p.mainnet {
		wantNet = sdkwallet.NetworkMainnet
	}
	if netRes == nil || netRes.Network != wantNet {
		gotNet := sdkwallet.Network("")
		if netRes != nil {
			gotNet = netRes.Network
		}
		return fmt.Errorf("chain: BRC-100 client wallet network mismatch: Nektar requires %q, desktop wallet is on %q", wantNet, gotNet)
	}
	idPub, err := wirewallet.IdentityKeyOf(ctx, w, Originator)
	if err != nil {
		return fmt.Errorf("chain: BRC-100 wallet identity key (is BSV Desktop unlocked?): %w", err)
	}
	p.wallet = w
	p.signer = &bwallet.Signer{
		Interface:  w,
		Identity:   idPub,
		Originator: Originator,
		Mainnet:    p.mainnet,
		Profile:    NektarWalletProfile,
	}
	persistWalletModeMarker(dir, "client", wireURL, p.network)
	return nil
}

func (p *Port) openEmbeddedWallet(cfg Config, dir, walletDir string) error {
	prof := nwallet.FuelProfileByName(cfg.WalletProfile)
	wCfg := nwallet.Config{
		Dir:           walletDir,
		InMemoryStore: p.network == "memory",
		Mainnet:       p.mainnet,
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
		SpendGuardRemainingInterval: p.guard.RemainingInterval,
		OnTxMined:                   p.dispatchTxMined,
		SettleDepth:                 p.settleDepth,
		FastStore:                   cfg.FastStore,
		EnableFastLane:              cfg.EnableFastLane,
	}
	if cfg.FastBroadcaster != nil {
		p.broadcaster = cfg.FastBroadcaster
		wCfg.Broadcaster = cfg.FastBroadcaster
	}
	if p.nodeChain != nil {
		wCfg.Chain = p.nodeChain
	}
	if p.witnessed != nil {
		wCfg.HeaderBytes = p.witnessed.HeaderBytes
		wCfg.MedianTimePast = p.witnessed.MedianTimePast
	}
	if p.testChain != nil && cfg.FastBroadcaster == nil {
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
		return fmt.Errorf("chain: open embedded wallet engine: %w", err)
	}
	p.engine = eng
	p.wallet = eng
	p.signer = eng.Signer()
	persistWalletModeMarker(dir, "embedded", "", p.network)
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
