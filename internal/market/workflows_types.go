package market

import (
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
)

// DealState records the durable lifecycle of one buyer or seller deal.
type DealState struct {
	DealID          string                    `json:"dealId"`
	Role            string                    `json:"role"`   // buyer | seller
	Status          string                    `json:"status"` // open | approval_pending | collecting | offered | payment_pending | paid_awaiting_reveal | claimed | settled | failed | expired
	Want            bstack.WantAd             `json:"want"`
	ChosenOffer     *bstack.OfferAd           `json:"chosenOffer,omitempty"`
	MatchAudit      *MatchAudit               `json:"matchAudit,omitempty"`
	DealReceipt     *DealReceipt              `json:"dealReceipt,omitempty"`
	ChirpURL        string                    `json:"chirpUrl,omitempty"`
	SaleOffer       *contracts.KeyedSaleOffer `json:"saleOffer,omitempty"`
	PaymentTxID     string                    `json:"paymentTxid,omitempty"`
	ClaimTxID       string                    `json:"claimTxid,omitempty"`
	ClaimPromise    string                    `json:"claimPromise,omitempty"`
	ApprovalID      string                    `json:"approvalId,omitempty"`
	AwakeableID     string                    `json:"awakeableId,omitempty"`
	StandingOrderID string                    `json:"standingOrderId,omitempty"`
	EscrowTxID      string                    `json:"escrowTxid,omitempty"`
	Shard           string                    `json:"shard,omitempty"`
	DeliveredTxt    string                    `json:"deliveredText,omitempty"`
	Error           string                    `json:"error,omitempty"`
	UpdatedAt       int64                     `json:"updatedAt"`
}

// ApprovalDecision resolves a suspended BuyerDealWorkflow approval signal channel.
type ApprovalDecision struct {
	Approved  bool   `json:"approved"`
	DecidedBy string `json:"decidedBy,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// BuyerDealInput starts a durable BuyerDealWorkflow keyed by WantID.
type BuyerDealInput struct {
	Want               bstack.WantAd    `json:"want"`
	Offers             []bstack.OfferAd `json:"offers,omitempty"`
	TargetShard        string           `json:"targetShard,omitempty"`
	CollectTimeout     time.Duration    `json:"collectTimeout,omitempty"`
	WaitTimeout        time.Duration    `json:"waitTimeout,omitempty"`
	ApprovalTimeout    time.Duration    `json:"approvalTimeout,omitempty"`
	StandingTTL        time.Duration    `json:"standingTtl,omitempty"`
	ApprovedOverrideID string           `json:"approvedOverrideId,omitempty"`
}

// SellerDealInput starts a durable SellerDealWorkflow keyed by "sell-"+OfferID.
type SellerDealInput struct {
	Want            bstack.WantAd             `json:"want"`
	Shard           string                    `json:"shard"`
	Seq             int64                     `json:"seq"`
	PriceSats       uint64                    `json:"priceSats"`
	BuyerPub        string                    `json:"buyerPub"`
	OfferTTL        time.Duration             `json:"offerTtl,omitempty"`
	ConfirmTimeout  time.Duration             `json:"confirmTimeout,omitempty"`
	PreCreatedOffer *contracts.KeyedSaleOffer `json:"preCreatedOffer,omitempty"`
	RevealKeyHex    string                    `json:"revealKeyHex,omitempty"`
	ChirpURL        string                    `json:"chirpUrl,omitempty"`
	ProofBundle     *contracts.ProofBundle    `json:"proofBundle,omitempty"`
}

// PaymentSignal delivers a buyer's payment txid + optional verified BUMP proof to SellerDealWorkflow.
type PaymentSignal struct {
	TxID          string `json:"txid"`
	AmountSats    uint64 `json:"amountSats"`
	Confirmed     bool   `json:"confirmed"`
	BlockHeight   uint32 `json:"blockHeight,omitempty"`
	MerklePathHex string `json:"merklePathHex,omitempty"`
}

// ClaimSignal delivers the seller's BRC-369 revealed content key k to BuyerDealWorkflow.
type ClaimSignal struct {
	ClaimTxID      string                 `json:"claimTxid"`
	ClaimTxBEEFHex string                 `json:"claimTxBeefHex,omitempty"`
	ContentKeyHex  string                 `json:"contentKeyHex"`
	CiphertextHex  string                 `json:"ciphertextHex,omitempty"`
	ChirpURL       string                 `json:"chirpUrl,omitempty"`
	ProofBundle    *contracts.ProofBundle `json:"proofBundle,omitempty"`
	DealReceipt    *DealReceipt           `json:"dealReceipt,omitempty"`
}

// CustodyReserveInput serializes global spend reservation atomically in SQLite.
type CustodyReserveInput struct {
	OperationID     string   `json:"operationId"`
	Attempt         int      `json:"attempt"`
	SellerPub       string   `json:"sellerPub,omitempty"`
	Domains         []string `json:"domains,omitempty"`
	AmountSats      uint64   `json:"amountSats"`
	BypassPolicy    bool     `json:"bypassPolicy,omitempty"`
	ApprovedCapSats uint64   `json:"approvedCapSats,omitempty"`
}

// CustodyReleaseInput releases a reserved (OperationID, Attempt) on definitive pre-broadcast failure.
type CustodyReleaseInput struct {
	OperationID string `json:"operationId"`
	Attempt     int    `json:"attempt"`
}

// CustodyReserveResult reports a serialized reservation from Custody.
type CustodyReserveResult struct {
	Approved        bool   `json:"approved"`
	AlreadyReserved bool   `json:"alreadyReserved,omitempty"`
	BalanceSats     uint64 `json:"balanceSats"`
	Reason          string `json:"reason,omitempty"`
}

// TxEventInput records a durable transaction lifecycle transition.
type TxEventInput struct {
	Kind          string `json:"kind"` // prepared | submission_unknown | broadcast | mempool | mined | reorged | failed | aborted
	Shard         string `json:"shard,omitempty"`
	BlockHeight   uint32 `json:"blockHeight,omitempty"`
	MerklePathHex string `json:"merklePathHex,omitempty"`
	Detail        string `json:"detail,omitempty"`
	ObservedAt    int64  `json:"observedAt,omitempty"`
}

// TxState is the durable state stored per txid.
type TxState struct {
	TxID        string         `json:"txid"`
	Status      string         `json:"status"`
	BlockHeight uint32         `json:"blockHeight,omitempty"`
	Events      []TxEventInput `json:"events"`
	UpdatedAt   int64          `json:"updatedAt"`
}

// AnchorCadenceInput triggers a durable shard anchor check.
type AnchorCadenceInput struct {
	IntervalSeconds int64 `json:"intervalSeconds,omitempty"`
}

// AnchorCadenceState summarizes the latest durable anchor cadence run for a shard.
type AnchorCadenceState struct {
	Shard          string `json:"shard"`
	LastTxID       string `json:"lastTxid,omitempty"`
	LastRunAt      int64  `json:"lastRunAt"`
	AnchorRuns     int64  `json:"anchorRuns"`
	NextSlot       int64  `json:"nextSlot,omitempty"`
	DeferredReason string `json:"deferredReason,omitempty"`
}

// RuntimeTxEmitInput dispatches a Tx event.
type RuntimeTxEmitInput struct {
	TxID  string       `json:"txid"`
	Event TxEventInput `json:"event"`
}

// RuntimeAnchorTriggerInput dispatches an AnchorCadence poke.
type RuntimeAnchorTriggerInput struct {
	Shard           string `json:"shard"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	Slot            int64  `json:"slot,omitempty"`
}
