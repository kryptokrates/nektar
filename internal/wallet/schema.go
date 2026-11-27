package wallet

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
)

// Transaction lifecycle statuses aligned with BRC-100 / go-wallet-toolbox.
const (
	StatusSubmissionUnknown = "submission_unknown"
	StatusUnproven          = "unproven"
	StatusCompleted         = "completed"
	StatusFailed            = "failed"
	StatusAborted           = "aborted"

	// Canonical SQL status sets shared across Balance, ListUTXOs, countDustUTXOs, PendingTxCount,
	// and computeUnprovenDepthBounds so spendable/pending telemetry never drifts across call sites.
	sqlNonSpendableParentStatuses = `'failed','aborted','unsigned','unprocessed','nosend','sending','submission_unknown'`
	sqlPendingTxStatuses          = `'unsigned','unprocessed','nosend','sending','submission_unknown'`
	sqlSpendableUnprovenStatuses  = `'unproven'`
)

// Standard spending baskets managed by go-wallet-toolbox StrategyThroughput plus Nektar's static funding basket.
// Any custom application basket is never auto-spent for fees (per BRC-46 / BRC-100).
const (
	BasketDefault = "default"
	BasketReserve = "reserve"
	BasketFuel    = "fuel"
	BasketFunding = "nektar funding"
)

// Key kinds exposed in UTXO listings.
const (
	KeyKindFund   = "fund"   // derived under Nektar Profile (nektar fund / nektar-fund, self)
	KeyKindBRC29  = "brc29"  // derived under BRC-29 (wallet payment / go-wallet-toolbox change)
	KeyKindCustom = "custom" // basket insertion or external script not auto-signed by wallet
)

const (
	// DefaultMaxUnprovenDepth caps reported/monitored 0-conf chain depth.
	DefaultMaxUnprovenDepth = 15
	// DefaultSweepChunkInputs caps explicit inputs per sweep chunk transaction.
	DefaultSweepChunkInputs = 300
	// DefaultReservationTTL is how long an unsigned two-phase action lives before AbortAbandoned reclaims it.
	DefaultReservationTTL = 5 * time.Minute
	// DefaultTxCacheSize bounds the in-memory parsed transaction LRU cache.
	DefaultTxCacheSize = 512
	// WalletVersion is returned by BRC-100 GetVersion.
	WalletVersion = "nektar-toolbox-0.1.0"
)

// Standard errors returned by the wallet engine.
var (
	ErrInsufficient     = errors.New("wallet: insufficient spendable funds")
	ErrNoChain          = errors.New("wallet: no chain tracker configured")
	ErrNoSettler        = errors.New("wallet: no transaction broadcaster configured")
	ErrInvalidArgs      = errors.New("wallet: invalid arguments")
	ErrNetworkMismatch  = errors.New("wallet: network mismatch between local and remote wallet")
	ErrSpendRejected    = errors.New("wallet: spend rejected by policy")
	ErrBroadcastUnknown = errors.New("wallet: broadcast outcome unknown (timeout or transport failure)")
	ErrThrottled        = errors.New("wallet: sync throttled")
	ErrDepthExceeded    = errors.New("wallet: unconfirmed ancestor chain depth limit exceeded")
)

type ctxKeyOverrideGuard struct{}
type ctxKeyExternalSpend struct{}
type ctxKeyPostErrSink struct{}
type ctxKeyIncomingInternalize struct{}

type postErrSink struct {
	mu           sync.Mutex
	txid         string
	err          error
	preBroadcast bool
}

func (p *postErrSink) record(txid string, err error) {
	if p == nil || err == nil {
		return
	}
	p.mu.Lock()
	p.txid = txid
	p.err = err
	p.preBroadcast = false
	p.mu.Unlock()
}

func (p *postErrSink) recordPreBroadcast(txid string, err error) {
	if p == nil || err == nil {
		return
	}
	p.mu.Lock()
	p.txid = txid
	p.err = err
	p.preBroadcast = true
	p.mu.Unlock()
}

func (p *postErrSink) get() (string, error) {
	if p == nil {
		return "", nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.txid, p.err
}

func (p *postErrSink) isPreBroadcast() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preBroadcast
}

func withPostErrSink(ctx context.Context) (context.Context, *postErrSink) {
	s := &postErrSink{}
	return context.WithValue(ctx, ctxKeyPostErrSink{}, s), s
}

func postErrSinkFromCtx(ctx context.Context) *postErrSink {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(ctxKeyPostErrSink{}).(*postErrSink)
	return s
}

type externalSpendHint struct {
	set      bool
	sweepAll bool
	sats     uint64
}

// WithOverrideGuard marks a context as an explicit operator treasury sweep allowed to bypass SpendGuard.
func WithOverrideGuard(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyOverrideGuard{}, true)
}

func isOverrideGuard(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyOverrideGuard{}).(bool)
	return v
}

// WithIncomingInternalize marks a context as internalizing a verified incoming third-party transaction
// so go-wallet-toolbox's BackgroundBroadcast does not debit buyer change against the receiver's SpendGuard.
func WithIncomingInternalize(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyIncomingInternalize{}, true)
}

func isIncomingInternalize(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyIncomingInternalize{}).(bool)
	return v
}

func withExternalSpend(ctx context.Context, sats uint64) context.Context {
	return context.WithValue(ctx, ctxKeyExternalSpend{}, externalSpendHint{set: true, sats: sats})
}

func withSweepAllExternalSpend(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyExternalSpend{}, externalSpendHint{set: true, sweepAll: true})
}

func externalSpendFromCtx(ctx context.Context) externalSpendHint {
	if ctx == nil {
		return externalSpendHint{}
	}
	h, _ := ctx.Value(ctxKeyExternalSpend{}).(externalSpendHint)
	return h
}

// DefaultNetworkFees matches BSV's 100 sat/kB rate with 1 sat floor and 100 sat dust threshold.
var DefaultNetworkFees = mint.Fees{
	Rate:    mint.Rate{Sats: 100, Bytes: 1000},
	Floor:   1,
	Dust:    100,
	MaxRate: mint.Rate{Sats: 500, Bytes: 1000},
}

// FuelProfile tunes go-wallet-toolbox StrategyThroughput + FuelKeeper FanOutFuel rounds.
type FuelProfile struct {
	Name                   string
	FuelTarget             int    // desired spendable fuel UTXOs
	FuelSats               uint64 // satoshis per fuel UTXO
	LowWaterPct            int    // replenish when fuel count < FuelTarget * LowWaterPct / 100
	ReserveTarget          int    // desired reserve UTXOs
	ReserveSats            uint64 // satoshis per reserve UTXO
	MaxFanOutOutputs       int    // max outputs per single FanOutFuel tx
	DustThresholdSats      uint64 // outputs below this (in default/fuel) are eligible for consolidation
	DustConsolidateTrigger int    // trigger idle dust consolidation when dust count >= this
	DefaultAcceptDelayed   bool   // throughput hint when caller leaves AcceptDelayedBroadcast nil
}

// StandardFuelProfile is tuned for ordinary agent nodes.
func StandardFuelProfile() FuelProfile {
	return FuelProfile{
		Name:                   "standard",
		FuelTarget:             24,
		FuelSats:               2000,
		LowWaterPct:            30,
		ReserveTarget:          4,
		ReserveSats:            50_000,
		MaxFanOutOutputs:       64,
		DustThresholdSats:      500,
		DustConsolidateTrigger: 20,
		DefaultAcceptDelayed:   false,
	}
}

// ThroughputFuelProfile is tuned for busy Hub or market-maker nodes (high concurrency burst).
func ThroughputFuelProfile() FuelProfile {
	return FuelProfile{
		Name:                   "throughput",
		FuelTarget:             128,
		FuelSats:               1500,
		LowWaterPct:            40,
		ReserveTarget:          8,
		ReserveSats:            100_000,
		MaxFanOutOutputs:       256,
		DustThresholdSats:      500,
		DustConsolidateTrigger: 32,
		DefaultAcceptDelayed:   false,
	}
}

// FuelProfileByName returns ThroughputFuelProfile for "throughput", else StandardFuelProfile.
func FuelProfileByName(name string) FuelProfile {
	if name == "throughput" || name == "high" || name == "hub" {
		return ThroughputFuelProfile()
	}
	return StandardFuelProfile()
}

// SpendGuardCheck is invoked before broadcasting an outgoing transaction.
// overrideGuard is true only for explicit operator treasury sweeps.
type SpendGuardCheck func(txid string, feeSats, spendSats uint64, overrideGuard bool) error

// Broadcaster is the single streamlined transaction broadcasting and proof-lookup interface
// used across Nektar (backed on live networks by github.com/bsv-blockchain/go-sdk/transaction/broadcaster).
type Broadcaster interface {
	Name() string
	Broadcast(ctx context.Context, tx *transaction.Transaction) error
	Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error)
}

// BroadcastFunc submits a signed transaction to the network (used for in-memory testchain/tests).
type BroadcastFunc func(ctx context.Context, tx *transaction.Transaction) error

// ProofLookupFunc resolves a mined MerklePath + block height for txid if available.
type ProofLookupFunc func(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error)

// HeaderBytesFunc returns the 80-byte raw block header for height (optional, for BRC-100 GetHeaderForHeight).
type HeaderBytesFunc func(ctx context.Context, height uint32) ([]byte, error)

// MedianTimePastFunc returns the consensus 11-block median time past at the current chain tip.
type MedianTimePastFunc func(ctx context.Context) (uint32, error)

// TxMinedHook is notified when Monitor proves a transaction mined with a verified MerklePath.
type TxMinedHook func(txid string, height uint32, mp *transaction.MerklePath)

// Config configures a pure-Go BRC-100 Engine backed by go-wallet-toolbox + modernc.org/sqlite.
type Config struct {
	Dir                         string                                // directory holding identity.json & wallet.sqlite (empty => in-memory)
	InMemoryStore               bool                                  // keep identity.json in Dir, run SQLite in-memory (ideal for ephemeral memory testchain)
	Mainnet                     bool                                  // true for mainnet, false for testnet/regtest/memory
	Originator                  string                                // BRC-100 originator (default "nektar")
	Profile                     bwallet.Profile                       // BRC-43 static funding derivation profile
	Tracker                     chaintracker.ChainTracker             // SPV header verifier & height source
	HeaderBytes                 HeaderBytesFunc                       // optional raw 80-byte header getter
	MedianTimePast              MedianTimePastFunc                    // optional consensus 11-block MTP getter
	FeeSource                   feepolicy.Source                      // live BSV Broadcaster policy or static rate
	Broadcaster                 Broadcaster                           // single live BSV Broadcaster / RPC skeleton
	Chain                       nodeapi.Chain                         // optional Tx/Proof/Spend view (WoC / node)
	WoC                         *nodeapi.WoC                          // optional WhatsOnChain client for external address sync
	Broadcast                   BroadcastFunc                         // optional custom broadcaster (overrides Broadcaster; used for testchain)
	ProofLookup                 ProofLookupFunc                       // optional custom proof lookup (used for testchain)
	SpendGuard                  SpendGuardCheck                       // optional fail-closed spend guard hook (enforced post-signing in PostFromBEEF)
	SpendGuardCheck             func(feeSats, spendSats uint64) error // optional read-only pre-build cap check
	SpendGuardRemainingInterval func() time.Duration                  // optional min-interval remaining time for FuelKeeper pacing
	OnTxMined                   TxMinedHook                           // optional callback invoked when Monitor verifies a mined BUMP
	SettleDepth                 int                                   // confirmations before finalizing proofs / BlocksDelay (>=1 on live)
	FuelProfile                 *FuelProfile                          // nil => StandardFuelProfile()
	MaxUnprovenDepth            int                                   // 0 => DefaultMaxUnprovenDepth (15)
	ReservationTTL              time.Duration                         // 0 => DefaultReservationTTL (5m)
	TxCacheSize                 int                                   // 0 => DefaultTxCacheSize (512)
	Logger                      func(format string, args ...any)
}

// TxRecord is one transaction record returned by ListRecentActions / HTTP API / CLI.
type TxRecord struct {
	TxID        string   `json:"txid"`
	Reference   string   `json:"reference,omitempty"`
	Status      string   `json:"status"`
	IsOutgoing  bool     `json:"isOutgoing"`
	Satoshis    int64    `json:"satoshis"`
	FeeSats     uint64   `json:"feeSats"`
	Description string   `json:"description"`
	Labels      []string `json:"labels,omitempty"`
	Version     uint32   `json:"version"`
	LockTime    uint32   `json:"lockTime"`
	BlockHeight uint32   `json:"blockHeight"`
	CreatedAt   int64    `json:"createdAt"`
	UpdatedAt   int64    `json:"updatedAt"`
}

// OutputRecord is one UTXO record returned by ListUTXOs / HTTP API / CLI.
type OutputRecord struct {
	ID                 int64    `json:"id"`
	TxID               string   `json:"txid"`
	Vout               uint32   `json:"vout"`
	Satoshis           uint64   `json:"satoshis"`
	LockingScript      []byte   `json:"-"`
	LockingScriptHex   string   `json:"lockingScriptHex,omitempty"`
	Basket             string   `json:"basket"`
	Spendable          bool     `json:"spendable"`
	Reserved           bool     `json:"reserved,omitempty"`
	KeyKind            string   `json:"keyKind"`
	DerivationPrefix   string   `json:"derivationPrefix,omitempty"`
	DerivationSuffix   string   `json:"derivationSuffix,omitempty"`
	SenderIdentityKey  string   `json:"senderIdentityKey,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	CustomInstructions string   `json:"customInstructions,omitempty"`
	OutputDescription  string   `json:"outputDescription,omitempty"`
	CreatedAt          int64    `json:"createdAt"`
	ParentStatus       string   `json:"parentStatus,omitempty"`
}

// BalanceBreakdown gives full visibility into confirmed, unproven, reserved, and basketed UTXOs.
type BalanceBreakdown struct {
	ConfirmedSats     uint64 `json:"confirmedSats"`
	UnprovenSats      uint64 `json:"unprovenSats"`
	ReservedSats      uint64 `json:"reservedSats"`
	TotalSats         uint64 `json:"totalSats"`
	SpendableSats     uint64 `json:"spendableSats"`
	UTXOCount         int    `json:"utxoCount"`
	SpendableCount    int    `json:"spendableCount"`
	FuelCount         int    `json:"fuelCount"`
	FuelSats          uint64 `json:"fuelSats"`
	ProvenFuelCount   int    `json:"provenFuelCount"`
	UnprovenFuelCount int    `json:"unprovenFuelCount"`
	ProvenFuelSats    uint64 `json:"provenFuelSats"`
	ReserveCount      int    `json:"reserveCount"`
	ReserveSats       uint64 `json:"reserveSats"`
	DefaultCount      int    `json:"defaultCount"`
	DefaultSats       uint64 `json:"defaultSats"`
	FundingCount      int    `json:"fundingCount,omitempty"`
	FundingSats       uint64 `json:"fundingSats,omitempty"`
	CustomCount       int    `json:"customCount"`
	MaxUnprovenDepth  int    `json:"maxUnprovenDepth"`
	PendingTxCount    int    `json:"pendingTxCount"`
}

// SendArgs sends BSV to either a P2PKH address OR a BRC-100 identity public key (BRC-29).
type SendArgs struct {
	ToAddress        string   `json:"toAddress,omitempty"`
	ToIdentityKey    string   `json:"toIdentityKey,omitempty"`
	DerivationPrefix string   `json:"derivationPrefix,omitempty"` // optional server-issued BRC-105 prefix
	DerivationSuffix string   `json:"derivationSuffix,omitempty"` // optional caller-supplied suffix
	Satoshis         uint64   `json:"satoshis"`
	Description      string   `json:"description,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	AcceptDelayed    bool     `json:"acceptDelayed,omitempty"`
	NoSend           bool     `json:"noSend,omitempty"`
	OverrideGuard    bool     `json:"overrideGuard,omitempty"`
}

// SendResult returns the signed tx, AtomicBEEF, and BRC-29 remittance (when sent to an identity key).
type SendResult struct {
	TxID              string                   `json:"txid"`
	Satoshis          uint64                   `json:"satoshis"`
	FeeSats           uint64                   `json:"feeSats"`
	InputCount        int                      `json:"inputCount"`
	BEEF              []byte                   `json:"-"`
	BEEFHex           string                   `json:"beefHex,omitempty"`
	Tx                *transaction.Transaction `json:"-"`
	DerivationPrefix  string                   `json:"derivationPrefix,omitempty"`
	DerivationSuffix  string                   `json:"derivationSuffix,omitempty"`
	SenderIdentityKey string                   `json:"senderIdentityKey,omitempty"`
	OutputIndex       uint32                   `json:"outputIndex"`
}

// SweepArgs sweeps 100% of spendable UTXOs across default, reserve, fuel, and funding baskets.
type SweepArgs struct {
	ToAddress     string `json:"toAddress,omitempty"`
	ToIdentityKey string `json:"toIdentityKey,omitempty"`
	WireURL       string `json:"wireUrl,omitempty"`
	Description   string `json:"description,omitempty"`
	MaxPerTx      int    `json:"maxPerTx,omitempty"`
	OverrideGuard bool   `json:"overrideGuard,omitempty"`
}

// PendingBRC29Record durably persists BRC-29 derivation nonces before broadcasting desktop fund/sweep txs
// so a crash between CreateAction and InternalizeAction never orphans funds.
type PendingBRC29Record struct {
	ID               int64  `json:"id"`
	Direction        string `json:"direction"` // "fund" | "sweep"
	WireURL          string `json:"wireUrl,omitempty"`
	CounterpartyPub  string `json:"counterpartyPub"`
	DerivationPrefix string `json:"derivationPrefix"`
	DerivationSuffix string `json:"derivationSuffix"`
	LockingScriptHex string `json:"lockingScriptHex,omitempty"`
	Satoshis         uint64 `json:"satoshis"`
	TxID             string `json:"txid,omitempty"`
	OutputIndex      uint32 `json:"outputIndex"`
	AtomicBEEF       []byte `json:"-"`
	Status           string `json:"status"` // "pending" | "done" | "failed"
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// PaymentRemittance holds BRC-29 derivation metadata for one payment output.
type PaymentRemittance struct {
	TxID              string `json:"txid"`
	OutputIndex       uint32 `json:"outputIndex"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
}

// SweepResult summarizes one or more sweep transactions.
type SweepResult struct {
	TxIDs             []string                   `json:"txids"`
	SweptSats         uint64                     `json:"sweptSats"`
	TotalFeeSats      uint64                     `json:"totalFeeSats"`
	InputsSwept       int                        `json:"inputsSwept"`
	BEEFs             [][]byte                   `json:"-"`
	Txs               []*transaction.Transaction `json:"-"`
	DerivationPrefix  string                     `json:"derivationPrefix,omitempty"`
	DerivationSuffix  string                     `json:"derivationSuffix,omitempty"`
	SenderIdentityKey string                     `json:"senderIdentityKey,omitempty"`
	OutputIndices     []uint32                   `json:"outputIndices,omitempty"`
	Remittances       []PaymentRemittance        `json:"remittances,omitempty"`
}

// ConsolidateArgs merges fragmented small UTXOs into clean reserve outputs.
type ConsolidateArgs struct {
	BelowSats    uint64 `json:"belowSats,omitempty"`
	MaxInputs    int    `json:"maxInputs,omitempty"`
	TargetBasket string `json:"targetBasket,omitempty"`
}

// ConsolidateResult reports a UTXO consolidation pass.
type ConsolidateResult struct {
	TxID         string `json:"txid,omitempty"`
	MergedCount  int    `json:"mergedCount"`
	OutputSats   uint64 `json:"outputSats"`
	FeeSats      uint64 `json:"feeSats"`
	TargetBasket string `json:"targetBasket"`
}

// ImportResult summarizes a BEEF or txid import.
type ImportResult struct {
	TxID    string `json:"txid"`
	Outputs int    `json:"outputs"`
	Sats    uint64 `json:"sats"`
	Mined   bool   `json:"mined"`
	Height  uint32 `json:"height"`
}

// SyncReport summarizes a Monitor reconciliation pass.
type SyncReport struct {
	AddedOutputs int `json:"addedOutputs"`
	ProvedTxs    int `json:"provedTxs"`
	SentWaiting  int `json:"sentWaiting"`
	AbortedStale int `json:"abortedStale"`
	PrunedSpent  int `json:"prunedSpent"`
}

// IsSpendingBasket reports whether basket is part of the wallet's spendable balance.
func IsSpendingBasket(basket string) bool {
	return basket == BasketDefault || basket == BasketReserve || basket == BasketFuel || basket == BasketFunding
}
