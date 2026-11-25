package chain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	Originator = "nektar"
)

// NektarWalletProfile is the canonical BRC-43/BRC-100 profile for Nektar v0.4.
// All funding UTXOs derive cleanly from this profile with zero raw private keys leaving bwallet.
var NektarWalletProfile = bwallet.Profile{
	FundProtocol:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "nektar fund"},
	FundKeyID:      "nektar-fund",
	FundBasket:     "nektar funding",
	Version:        "0.4.0",
	LegacyPoolFile: "nektar-pool.json",
}

// Config configures a ChainPort for memory/testchain, regtest, testnet, or main.
type Config struct {
	Network        string // memory | regtest | testnet | main
	DataDir        string
	WalletMode     string // embedded (default) | client (BRC-100 wire on localhost:3321)
	WalletWireURL  string // default http://127.0.0.1:3321
	HeaderSource   string // headers.NewSource spec (e.g. woc:main, chaintracks:https://..., native:http://...)
	ChainSpec      string // nodeapi.ParseChain spec (e.g. woc:main, teranode:http://...)
	SettleSpec     string // publish.ParseSettler spec (e.g. arcade:https://..., rpc:http://...)
	ArcadeKey      string
	RPCUser        string
	RPCPass        string
	WoCKey         string
	MainnetConfirm string // must equal "I_UNDERSTAND" when Network == "main"
	SpendGuard     core.SpendGuardOpts
}

// Port is the single chain + custody abstraction consumed by the entire Nektar Go runtime.
// Built directly on lightwebinc/bcommon/{bwallet,wirewallet,headers,nodeapi,publish,testchain}.
type Port struct {
	mu           sync.Mutex
	network      string
	mainnet      bool
	dataDir      string
	guard        *core.SpendGuard
	embedded     *bwallet.Embedded
	signer       *bwallet.Signer
	tracker      chaintracker.ChainTracker
	headerClient *headers.Client
	testChain    *testchain.Chain
	nodeChain    *nodeapi.Sources
	settler      publish.Settler
	httpClient   *http.Client
	txs          map[string]*transaction.Transaction
}

// Open constructs a Port for any network:
//   - "memory" (default for tests): in-process bcommon/testchain + bwallet embedded custody
//   - "regtest" | "testnet" | "main": bcommon/headers + bcommon/nodeapi + bwallet (or BRC-100 wirewallet)
func Open(ctx context.Context, cfg Config) (*Port, error) {
	net := cfg.Network
	if net == "" {
		net = "memory"
	}
	if net == "main" && cfg.MainnetConfirm != "I_UNDERSTAND" {
		return nil, errors.New("chain: mainnet requires NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND")
	}
	mainnet := net == "main"
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
	guard := core.NewSpendGuard(guardOpts)

	p := &Port{
		network:    net,
		mainnet:    mainnet,
		dataDir:    dir,
		guard:      guard,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		txs:        make(map[string]*transaction.Transaction),
	}

	// Open custody: remote BRC-100 wirewallet (NEKTAR_WALLET=client) or embedded bcommon/bwallet.
	if cfg.WalletMode == "client" {
		if net == "regtest" || net == "memory" {
			return nil, errors.New("chain: client wallet custody is for testnet/main only")
		}
		wireURL := cfg.WalletWireURL
		if wireURL == "" {
			wireURL = "http://127.0.0.1:3321"
		}
		if err := wirewallet.CheckURL(wireURL); err != nil {
			return nil, fmt.Errorf("chain: BRC-100 wire URL refused: %w", err)
		}
		w, err := wirewallet.Dial(Originator, wireURL, 15*time.Second)
		if err != nil {
			return nil, fmt.Errorf("chain: dial BRC-100 wallet at %s: %w", wireURL, err)
		}
		idPub, err := wirewallet.IdentityKeyOf(ctx, w, Originator)
		if err != nil {
			return nil, fmt.Errorf("chain: BRC-100 wallet identity key (is vault unlocked?): %w", err)
		}
		p.signer = &bwallet.Signer{
			Interface:  w,
			Identity:   idPub,
			Originator: Originator,
			Mainnet:    mainnet,
			Profile:    NektarWalletProfile,
		}
	} else {
		emb, err := bwallet.Open(walletDir, NektarWalletProfile)
		if err != nil {
			emb, err = bwallet.Create(walletDir, NektarWalletProfile)
			if err != nil {
				return nil, fmt.Errorf("chain: create embedded bwallet: %w", err)
			}
		}
		emb.Mainnet = mainnet
		emb.Originator = Originator
		p.embedded = emb
		p.signer = &bwallet.Signer{
			Interface:  emb,
			Identity:   emb.IdentityKey(),
			Originator: Originator,
			Mainnet:    mainnet,
			Profile:    NektarWalletProfile,
		}
	}

	switch net {
	case "memory":
		tc := testchain.New(1)
		p.testChain = tc
		p.tracker = tc
		// Seed spendable test coins directly into the embedded wallet pool so memory/test runs work immediately.
		if err := p.seedTestChainCoins(ctx); err != nil {
			return nil, fmt.Errorf("chain: seed testchain: %w", err)
		}
	default:
		spec := cfg.HeaderSource
		if spec == "" {
			switch net {
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
		if cfg.ChainSpec != "" {
			src, err := nodeapi.ParseChain(cfg.ChainSpec, nodeapi.ChainOptions{
				WoCKey:  cfg.WoCKey,
				Client:  p.httpClient,
				Headers: p.tracker,
			})
			if err != nil {
				return nil, fmt.Errorf("chain: chain spec %q: %w", cfg.ChainSpec, err)
			}
			p.nodeChain = src
		}
		settleSpec := cfg.SettleSpec
		if settleSpec == "" {
			if def, err := publish.DefaultSettle(net); err == nil {
				settleSpec = def
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
			settler, _, err := publish.ParseSettler(settleSpec, sOpts)
			if err != nil {
				return nil, fmt.Errorf("chain: settle spec %q: %w", settleSpec, err)
			}
			p.settler = settler
		}
	}

	return p, nil
}

// seedTestChainCoins mines mature coinbases to the embedded wallet's funding address
// and populates its durable bwallet.Pool with spendable outputs + MerklePaths.
func (p *Port) seedTestChainCoins(ctx context.Context) error {
	if p.testChain == nil || p.embedded == nil {
		return nil
	}
	addr, err := p.signer.FundAddress(p.mainnet)
	if err != nil {
		return err
	}
	lockScript, err := p.signer.FundScript()
	if err != nil {
		return err
	}
	lockHex := lockScript.String()
	// Generate 105 blocks so the earliest coinbases mature past testchain.Maturity (100).
	if _, err := p.testChain.Generate(testchain.Maturity+5, addr); err != nil {
		return err
	}
	// Reconcile persisted pool against this fresh in-memory testchain: prune outputs
	// from previous ephemeral sessions that do not exist on the new testchain.
	for _, existing := range p.embedded.Pool.Outputs() {
		if p.testChain.Tx(existing.TxID) == nil {
			_, _ = p.embedded.Pool.Remove(existing)
		}
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
		if _, err := p.embedded.Pool.Add(bwallet.Output{
			TxID:          txid,
			Vout:          0,
			Satoshis:      out0.Satoshis,
			LockingScript: lockHex,
			Height:        h,
			Coinbase:      true,
			Raw:           cbTx.Hex(),
			Bump:          hex.EncodeToString(mp.Bytes()),
		}); err != nil {
			return err
		}
	}
	if p.embedded.Pool.Balance() == 0 {
		return errors.New("testchain seeded zero mature coinbase outputs")
	}
	return nil
}

func (p *Port) Network() string                       { return p.network }
func (p *Port) Mainnet() bool                         { return p.mainnet }
func (p *Port) Tracker() chaintracker.ChainTracker    { return p.tracker }
func (p *Port) SpendGuard() *core.SpendGuard          { return p.guard }
func (p *Port) Wallet() *bwallet.Embedded             { return p.embedded }
func (p *Port) Signer() *bwallet.Signer               { return p.signer }
func (p *Port) TestChain() *testchain.Chain           { return p.testChain }
func (p *Port) IdentityPubHex() string                { return p.signer.IdentityHex() }
func (p *Port) FundAddress() (string, error)          { return p.signer.FundAddress(p.mainnet) }
func (p *Port) BalanceSats() uint64 {
	if p.embedded == nil || p.embedded.Pool == nil {
		return 0
	}
	return p.embedded.Pool.Balance()
}

// Height returns the current chain tip height.
func (p *Port) Height(ctx context.Context) (uint32, error) {
	if p.testChain != nil {
		return p.testChain.Height(), nil
	}
	return p.tracker.CurrentHeight(ctx)
}

// Broadcast enforces SpendGuard caps, then broadcasts tx on testchain or configured live node.
func (p *Port) Broadcast(ctx context.Context, tx *transaction.Transaction, feeSats, spendSats uint64) (string, error) {
	txid := tx.TxID().String()
	if err := p.guard.CheckAndRecord(txid, feeSats, spendSats); err != nil {
		return "", err
	}
	if p.testChain != nil {
		if err := p.testChain.Send(tx); err != nil {
			return "", fmt.Errorf("chain: testchain broadcast: %w", err)
		}
		if mp, _, ok := p.testChain.Proof(txid); ok {
			tx.MerklePath = mp
		}
		p.mu.Lock()
		p.txs[txid] = tx
		p.mu.Unlock()
		return txid, nil
	}
	// Live/regtest broadcast via bcommon/publish.Settler (Arcade / RPC / Teranode TCP ingress).
	if p.settler == nil {
		return "", errors.New("chain: no live settler configured (set NEKTAR_SETTLE_SPEC)")
	}
	if err := p.settler.Submit(ctx, tx); err != nil {
		return "", fmt.Errorf("chain: %s broadcast: %w", p.settler.Name(), err)
	}
	p.mu.Lock()
	p.txs[txid] = tx
	p.mu.Unlock()
	return txid, nil
}

// Proof returns the verified MerklePath + height if txid is mined.
func (p *Port) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool) {
	if p.testChain != nil {
		return p.testChain.Proof(txid)
	}
	if p.nodeChain != nil {
		if mp, h, err := p.nodeChain.Proof(ctx, txid); err == nil && mp != nil {
			return mp, h, true
		}
	}
	p.mu.Lock()
	tx := p.txs[txid]
	p.mu.Unlock()
	if tx != nil && tx.MerklePath != nil {
		return tx.MerklePath, tx.MerklePath.BlockHeight, true
	}
	return nil, 0, false
}

// Tx returns a known transaction by txid.
func (p *Port) Tx(txid string) *transaction.Transaction {
	if p.testChain != nil {
		if tx := p.testChain.Tx(txid); tx != nil {
			return tx
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.txs[txid]
}

// SignDigest signs a 32-byte digest under Nektar's BRC-43 identity key using bwallet/wallet.Interface.
func (p *Port) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	res, err := p.signer.CreateSignature(ctx, wallet.CreateSignatureArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "nektar provenance"},
			KeyID:      "default",
			Counterparty: wallet.Counterparty{
				Type: wallet.CounterpartyTypeSelf,
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
	res, err := p.signer.GetPublicKey(ctx, wallet.GetPublicKeyArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID: wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: "nektar provenance"},
			KeyID:      "default",
			Counterparty: wallet.Counterparty{
				Type: wallet.CounterpartyTypeSelf,
			},
		},
	}, Originator)
	if err != nil {
		return nil, err
	}
	return res.PublicKey, nil
}

// BuildFundedTx builds and signs a transaction paying outputs from bwallet.Pool with change back to FundScript.
func (p *Port) BuildFundedTx(ctx context.Context, outputs []*transaction.TransactionOutput) (*transaction.Transaction, uint64, uint64, error) {
	if p.embedded == nil || p.embedded.Pool == nil {
		return nil, 0, 0, errors.New("chain: embedded bwallet pool required to fund tx")
	}
	tip, err := p.Height(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	var outTotal uint64
	for _, o := range outputs {
		outTotal += o.Satoshis
	}
	const feeEstimate = uint64(300)
	coin, err := p.embedded.Pool.TakeAtLeast(tip, outTotal+feeEstimate, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("chain: take funding coin: %w", err)
	}
	srcTx, err := transaction.NewTransactionFromHex(coin.Raw)
	if err != nil {
		_ = p.embedded.Pool.Return(coin)
		return nil, 0, 0, fmt.Errorf("chain: decode funding parent tx: %w", err)
	}
	if coin.Bump != "" {
		bumpBytes, bErr := hex.DecodeString(coin.Bump)
		if bErr == nil {
			if mp, mErr := transaction.NewMerklePathFromBinary(bumpBytes); mErr == nil {
				srcTx.MerklePath = mp
			}
		}
	}

	tx := transaction.NewTransaction()
	unlocker := p.signer.FundUnlocker()
	tx.AddInputFromTx(srcTx, coin.Vout, unlocker)
	for _, o := range outputs {
		tx.AddOutput(o)
	}
	changeScript, err := p.signer.FundScript()
	if err != nil {
		_ = p.embedded.Pool.Return(coin)
		return nil, 0, 0, err
	}
	changeSats := coin.Satoshis - outTotal - feeEstimate
	if changeSats > 0 {
		tx.AddOutput(&transaction.TransactionOutput{
			Satoshis:      changeSats,
			LockingScript: changeScript,
		})
	}
	if err := tx.Sign(); err != nil {
		_ = p.embedded.Pool.Return(coin)
		return nil, 0, 0, fmt.Errorf("chain: sign tx: %w", err)
	}

	// Re-add change output to bwallet.Pool once broadcast succeeds (caller calls AdoptChange).
	_ = p2pkh.Lock
	_ = chainhash.Hash{}
	return tx, feeEstimate, outTotal + feeEstimate, nil
}

// AdoptChange adds the change output of a broadcast tx back into bwallet.Pool so the coin book stays self-replenishing.
func (p *Port) AdoptChange(tx *transaction.Transaction) error {
	if p.embedded == nil || p.embedded.Pool == nil {
		return nil
	}
	fundScript, err := p.signer.FundScript()
	if err != nil {
		return err
	}
	fundHex := fundScript.String()
	txid := tx.TxID().String()
	var bumpHex string
	var height uint32
	if tx.MerklePath != nil {
		bumpHex = hex.EncodeToString(tx.MerklePath.Bytes())
		height = tx.MerklePath.BlockHeight
	}
	for vout, out := range tx.Outputs {
		if out.LockingScript != nil && out.LockingScript.String() == fundHex && out.Satoshis > 0 {
			_, err := p.embedded.Pool.Add(bwallet.Output{
				TxID:          txid,
				Vout:          uint32(vout),
				Satoshis:      out.Satoshis,
				LockingScript: fundHex,
				Height:        height,
				Raw:           tx.Hex(),
				Bump:          bumpHex,
				Unproven:      tx.MerklePath == nil,
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// AnchorOutput helper creates a 1-sat output locked to a NektarAnchor script.
func AnchorOutput(lockScript *script.Script) *transaction.TransactionOutput {
	return &transaction.TransactionOutput{
		Satoshis:      1,
		LockingScript: lockScript,
	}
}
