package wallet

import (
	"context"
	"errors"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// Transaction lifecycle statuses (aligned with BRC-100 / go-wallet-toolbox utxo-lifecycle.md).
const (
	StatusUnsigned    = "unsigned"
	StatusUnprocessed = "unprocessed"
	StatusSending     = "sending"
	StatusUnproven    = "unproven"
	StatusCompleted   = "completed"
	StatusNoSend      = "nosend"
	StatusFailed      = "failed"
)

// Standard spending baskets (only these three are eligible for automatic fee/funding selection).
// Any custom application basket is never auto-spent for fees (per BRC-46 / BRC-100).
const (
	BasketDefault = "default"
	BasketReserve = "reserve"
	BasketFuel    = "fuel"
)

// Key kinds tracked per output so Sweep / CreateAction / Consolidate know how to sign any UTXO.
const (
	KeyKindFund   = "fund"   // derived under Nektar Profile (nektar fund / nektar-fund, self)
	KeyKindBRC29  = "brc29"  // derived under BRC-29 (2-3241645161d8-<prefix> <suffix>, sender)
	KeyKindCustom = "custom" // basket insertion or external script not auto-signed by wallet
)

const (
	// CoinbaseMaturity matches BSV consensus coinbase maturity (100 blocks).
	CoinbaseMaturity = 100
	// DefaultMaxUnprovenDepth caps 0-conf ancestry chain depth to bound BEEF size and mempool risk.
	DefaultMaxUnprovenDepth = 15
	// DefaultMaxInputsPerTx caps automatic multi-input coin selection per single transaction.
	DefaultMaxInputsPerTx = 256
	// DefaultSweepChunkInputs caps inputs per sweep chunk transaction to stay well within policy limits.
	DefaultSweepChunkInputs = 400
	// DefaultReservationTTL is how long an unsigned two-phase action holds reserved UTXOs before AbortAbandoned reclaims them.
	DefaultReservationTTL = 5 * time.Minute
	// DefaultTxCacheSize bounds the in-memory parsed transaction LRU cache.
	DefaultTxCacheSize = 512
	// WalletVersion is returned by BRC-100 GetVersion.
	WalletVersion = "nektar-wallet-0.4.0"
)

// Standard errors returned by the wallet engine.
var (
	ErrNotSupported    = errors.New("wallet: method not supported by embedded wallet")
	ErrInsufficient    = errors.New("wallet: insufficient spendable funds")
	ErrNoChain         = errors.New("wallet: no chain tracker configured")
	ErrNoSettler       = errors.New("wallet: no transaction broadcaster configured")
	ErrActionNotFound  = errors.New("wallet: action reference not found")
	ErrInvalidArgs     = errors.New("wallet: invalid arguments")
	ErrNetworkMismatch = errors.New("wallet: network mismatch between local and remote wallet")
	ErrSpendRejected   = errors.New("wallet: spend rejected by policy")
)

// DefaultNetworkFees matches BSV's 100 sat/kB rate with 1 sat floor and 100 sat dust threshold
// (avoiding sub-100-sat change outputs while paying ~23 sats for typical 1-in/2-out P2PKH txs).
var DefaultNetworkFees = mint.Fees{
	Rate:    mint.Rate{Sats: 100, Bytes: 1000},
	Floor:   1,
	Dust:    100,
	MaxRate: mint.Rate{Sats: 500, Bytes: 1000},
}

// FuelProfile tunes the background two-tier FuelKeeper (Reserve -> Fuel) for standard or high-throughput nodes.
type FuelProfile struct {
	Name                    string
	FuelTarget              int    // desired spendable fuel UTXOs
	FuelSats                uint64 // satoshis per fuel UTXO
	LowWaterPct             int    // replenish when fuel count < FuelTarget * LowWaterPct / 100
	ReserveTarget           int    // desired reserve UTXOs
	ReserveSats             uint64 // satoshis per reserve UTXO
	MaxFanOutOutputs        int    // max outputs per single fan-out tx
	DustThresholdSats       uint64 // outputs below this (in default/fuel) are eligible for idle consolidation
	DustConsolidateTrigger  int    // trigger idle dust consolidation when dust count >= this
	DefaultAcceptDelayed    bool   // throughput hint when caller leaves AcceptDelayedBroadcast nil
}

// StandardFuelProfile is tuned for ordinary agent nodes (~25-100 TPS burst on WAL SQLite).
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

// BroadcastFunc submits a signed transaction to the network (Settler or testchain).
type BroadcastFunc func(ctx context.Context, tx *transaction.Transaction) error

// ProofLookupFunc resolves a mined MerklePath + block height for txid if available.
type ProofLookupFunc func(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error)

// HeaderBytesFunc returns the 80-byte raw block header for height (optional, for BRC-100 GetHeaderForHeight).
type HeaderBytesFunc func(ctx context.Context, height uint32) ([]byte, error)

// Config configures a pure-Go BRC-100 Engine.
type Config struct {
	Dir              string                    // directory holding identity.json & wallet.sqlite (empty => in-memory)
	Mainnet          bool                      // true for mainnet, false for testnet/regtest/memory
	Originator       string                    // BRC-100 originator (default "nektar")
	Profile          bwallet.Profile           // BRC-43 funding derivation profile
	Tracker          chaintracker.ChainTracker // SPV header verifier & height source
	HeaderBytes      HeaderBytesFunc           // optional raw 80-byte header getter
	FeeSource        feepolicy.Source          // live ARC/Arcade policy or static rate
	Settler          publish.Settler           // optional live broadcaster (Arcade / RPC / TCP)
	Arcade           *publish.Arcade           // optional Arcade client for fast proof lookup
	Chain            nodeapi.Chain             // optional Tx/Proof/Spend view (WoC / node)
	WoC              *nodeapi.WoC              // optional WhatsOnChain client for external address sync
	Broadcast        BroadcastFunc             // optional custom broadcaster (overrides Settler; used for testchain)
	ProofLookup      ProofLookupFunc           // optional custom proof lookup (used for testchain)
	SpendGuard       SpendGuardCheck           // optional fail-closed spend guard hook
	FuelProfile      *FuelProfile              // nil => StandardFuelProfile()
	MaxUnprovenDepth int                       // 0 => DefaultMaxUnprovenDepth (15)
	MaxInputsPerTx   int                       // 0 => DefaultMaxInputsPerTx (256)
	ReservationTTL   time.Duration             // 0 => DefaultReservationTTL (5m)
	TxCacheSize      int                       // 0 => DefaultTxCacheSize (512)
	Logger           func(format string, args ...any)
}

// TxRecord is one persisted transaction row in wallet_txs.
type TxRecord struct {
	TxID              string   `json:"txid"`
	Reference         string   `json:"reference,omitempty"`
	Status            string   `json:"status"`
	IsOutgoing        bool     `json:"isOutgoing"`
	Satoshis          int64    `json:"satoshis"`
	FeeSats           uint64   `json:"feeSats"`
	Description       string   `json:"description"`
	Labels            []string `json:"labels,omitempty"`
	Version           uint32   `json:"version"`
	LockTime          uint32   `json:"lockTime"`
	RawTx             []byte   `json:"-"`
	BEEF              []byte   `json:"-"`
	MerklePath        []byte   `json:"-"`
	BlockHeight       uint32   `json:"blockHeight"`
	UnprovenDepth     int      `json:"unprovenDepth"`
	BroadcastAttempts int      `json:"broadcastAttempts"`
	LastError         string   `json:"lastError,omitempty"`
	CreatedAt         int64    `json:"createdAt"`
	UpdatedAt         int64    `json:"updatedAt"`
}

// OutputRecord is one persisted UTXO row in wallet_outputs.
type OutputRecord struct {
	ID                 int64    `json:"id"`
	TxID               string   `json:"txid"`
	Vout               uint32   `json:"vout"`
	Satoshis           uint64   `json:"satoshis"`
	LockingScript      []byte   `json:"-"`
	LockingScriptHex   string   `json:"lockingScriptHex,omitempty"`
	Basket             string   `json:"basket"`
	Spendable          bool     `json:"spendable"`
	SpentByTxID        string   `json:"spentByTxid,omitempty"`
	ReservedByRef      string   `json:"reservedByRef,omitempty"`
	ReservedUntil      int64    `json:"reservedUntil,omitempty"`
	KeyKind            string   `json:"keyKind"`
	DerivationPrefix   string   `json:"derivationPrefix,omitempty"`
	DerivationSuffix   string   `json:"derivationSuffix,omitempty"`
	SenderIdentityKey  string   `json:"senderIdentityKey,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	CustomInstructions string   `json:"customInstructions,omitempty"`
	OutputDescription  string   `json:"outputDescription,omitempty"`
	Coinbase           bool     `json:"coinbase,omitempty"`
	BlockHeight        uint32   `json:"blockHeight"`
	CreatedAt          int64    `json:"createdAt"`
	// Joined parent tx metadata populated by selector/listing queries.
	ParentStatus        string `json:"parentStatus,omitempty"`
	ParentUnprovenDepth int    `json:"parentUnprovenDepth,omitempty"`
}

// InputRecord tracks an input spent by a wallet transaction for BRC-100 ListActions.
type InputRecord struct {
	TxID                string `json:"txid"`
	Vin                 uint32 `json:"vin"`
	SourceTxID          string `json:"sourceTxid"`
	SourceVout          uint32 `json:"sourceVout"`
	SourceSatoshis      uint64 `json:"sourceSatoshis"`
	SourceLockingScript []byte `json:"-"`
	UnlockingScript     []byte `json:"-"`
	InputDescription    string `json:"inputDescription"`
	SequenceNumber      uint32 `json:"sequenceNumber"`
}

// BalanceBreakdown gives full visibility into confirmed, unproven, reserved, and basketed UTXOs.
type BalanceBreakdown struct {
	ConfirmedSats    uint64 `json:"confirmedSats"`
	UnprovenSats     uint64 `json:"unprovenSats"`
	ReservedSats     uint64 `json:"reservedSats"`
	TotalSats        uint64 `json:"totalSats"`
	SpendableSats    uint64 `json:"spendableSats"`
	UTXOCount        int    `json:"utxoCount"`
	SpendableCount   int    `json:"spendableCount"`
	FuelCount        int    `json:"fuelCount"`
	FuelSats         uint64 `json:"fuelSats"`
	ReserveCount     int    `json:"reserveCount"`
	ReserveSats      uint64 `json:"reserveSats"`
	DefaultCount     int    `json:"defaultCount"`
	DefaultSats      uint64 `json:"defaultSats"`
	CustomCount      int    `json:"customCount"`
	MaxUnprovenDepth int    `json:"maxUnprovenDepth"`
	PendingTxCount   int    `json:"pendingTxCount"`
}

// SendArgs sends BSV to either a P2PKH address OR a BRC-100 identity public key (BRC-29).
type SendArgs struct {
	ToAddress      string   `json:"toAddress,omitempty"`      // base58 P2PKH address
	ToIdentityKey  string   `json:"toIdentityKey,omitempty"`  // compressed 66-hex secp256k1 pubkey (derives BRC-29 output!)
	Satoshis       uint64   `json:"satoshis"`
	Description    string   `json:"description,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	AcceptDelayed  bool     `json:"acceptDelayed,omitempty"`
	NoSend         bool     `json:"noSend,omitempty"`
	OverrideGuard  bool     `json:"overrideGuard,omitempty"`
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
	DerivationPrefix  string                   `json:"derivationPrefix,omitempty"`  // base64 (BRC-29)
	DerivationSuffix  string                   `json:"derivationSuffix,omitempty"`  // base64 (BRC-29)
	SenderIdentityKey string                   `json:"senderIdentityKey,omitempty"` // compressed hex
	OutputIndex       uint32                   `json:"outputIndex"`
}

// SweepArgs sweeps 100% of spendable UTXOs across default, reserve, and fuel baskets.
type SweepArgs struct {
	ToAddress     string `json:"toAddress,omitempty"`
	ToIdentityKey string `json:"toIdentityKey,omitempty"`
	Description   string `json:"description,omitempty"`
	MaxPerTx      int    `json:"maxPerTx,omitempty"` // default DefaultSweepChunkInputs (400)
	OverrideGuard bool   `json:"overrideGuard,omitempty"`
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
}

// ConsolidateArgs merges fragmented small UTXOs into clean reserve outputs.
type ConsolidateArgs struct {
	BelowSats   uint64 `json:"belowSats,omitempty"` // 0 => consolidate all small UTXOs (default 10000)
	MaxInputs   int    `json:"maxInputs,omitempty"` // default 100
	TargetBasket string `json:"targetBasket,omitempty"` // default "reserve"
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
	AddedOutputs  int `json:"addedOutputs"`
	ProvedTxs     int `json:"provedTxs"`
	SentWaiting   int `json:"sentWaiting"`
	AbortedStale  int `json:"abortedStale"`
	PrunedSpent   int `json:"prunedSpent"`
}

// IsSpendingBasket reports whether basket is eligible for automatic fee/funding coin selection.
func IsSpendingBasket(basket string) bool {
	return basket == BasketDefault || basket == BasketReserve || basket == BasketFuel
}

// isWalletStatusAccepted maps a wallet_txs status to BRC-100 ActionStatus.
func toBRC100ActionStatus(status string) sdkwallet.ActionStatus {
	switch status {
	case StatusCompleted:
		return sdkwallet.ActionStatusCompleted
	case StatusUnprocessed:
		return sdkwallet.ActionStatusUnprocessed
	case StatusSending:
		return sdkwallet.ActionStatusSending
	case StatusUnproven:
		return sdkwallet.ActionStatusUnproven
	case StatusUnsigned:
		return sdkwallet.ActionStatusUnsigned
	case StatusNoSend:
		return sdkwallet.ActionStatusNoSend
	default:
		return sdkwallet.ActionStatusNonFinal
	}
}

const walletSchemaSQL = `
CREATE TABLE IF NOT EXISTS wallet_txs (
	txid               TEXT PRIMARY KEY,
	reference          TEXT UNIQUE,
	status             TEXT NOT NULL,
	is_outgoing        INTEGER NOT NULL DEFAULT 1,
	satoshis           INTEGER NOT NULL DEFAULT 0,
	fee_sats           INTEGER NOT NULL DEFAULT 0,
	description        TEXT NOT NULL DEFAULT '',
	labels_json        TEXT NOT NULL DEFAULT '[]',
	version            INTEGER NOT NULL DEFAULT 1,
	lock_time          INTEGER NOT NULL DEFAULT 0,
	raw_tx             BLOB,
	beef               BLOB,
	merkle_path        BLOB,
	block_height       INTEGER NOT NULL DEFAULT 0,
	unproven_depth     INTEGER NOT NULL DEFAULT 0,
	broadcast_attempts INTEGER NOT NULL DEFAULT 0,
	last_error         TEXT NOT NULL DEFAULT '',
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wallet_txs_status ON wallet_txs(status, updated_at);

CREATE TABLE IF NOT EXISTS wallet_outputs (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	txid                TEXT NOT NULL,
	vout                INTEGER NOT NULL,
	satoshis            INTEGER NOT NULL,
	locking_script      BLOB NOT NULL,
	basket              TEXT NOT NULL DEFAULT 'default',
	spendable           INTEGER NOT NULL DEFAULT 1,
	spent_by_txid       TEXT NOT NULL DEFAULT '',
	reserved_by_ref     TEXT NOT NULL DEFAULT '',
	reserved_until      INTEGER NOT NULL DEFAULT 0,
	key_kind            TEXT NOT NULL DEFAULT 'fund',
	derivation_prefix   TEXT NOT NULL DEFAULT '',
	derivation_suffix   TEXT NOT NULL DEFAULT '',
	sender_identity_key TEXT NOT NULL DEFAULT '',
	tags_json           TEXT NOT NULL DEFAULT '[]',
	custom_instructions TEXT NOT NULL DEFAULT '',
	output_description  TEXT NOT NULL DEFAULT '',
	coinbase            INTEGER NOT NULL DEFAULT 0,
	block_height        INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL,
	UNIQUE(txid, vout)
);
CREATE INDEX IF NOT EXISTS idx_wallet_outputs_select
	ON wallet_outputs(basket, spendable, reserved_by_ref, satoshis, id);
CREATE INDEX IF NOT EXISTS idx_wallet_outputs_reserved
	ON wallet_outputs(reserved_by_ref, reserved_until);
CREATE INDEX IF NOT EXISTS idx_wallet_outputs_txid
	ON wallet_outputs(txid);

CREATE TABLE IF NOT EXISTS wallet_inputs (
	txid                  TEXT NOT NULL,
	vin                   INTEGER NOT NULL,
	source_txid           TEXT NOT NULL,
	source_vout           INTEGER NOT NULL,
	source_satoshis       INTEGER NOT NULL DEFAULT 0,
	source_locking_script BLOB,
	unlocking_script      BLOB,
	input_description     TEXT NOT NULL DEFAULT '',
	sequence_number       INTEGER NOT NULL DEFAULT 4294967295,
	PRIMARY KEY (txid, vin)
);

CREATE TABLE IF NOT EXISTS wallet_pending_actions (
	reference  TEXT PRIMARY KEY,
	state_json BLOB NOT NULL,
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
`
