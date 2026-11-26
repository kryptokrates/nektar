package market

import (
	"crypto/subtle"
	"encoding/json"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	restate "github.com/restatedev/sdk-go"
	restateserver "github.com/restatedev/sdk-go/server"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

const internalTokenHeader = "x-nektar-internal-token"

// DealState records the durable lifecycle of one buyer or seller deal.
type DealState struct {
	DealID       string                    `json:"dealId"`
	Role         string                    `json:"role"`   // buyer | seller
	Status       string                    `json:"status"` // open | approval_pending | collecting | offered | payment_pending | paid_awaiting_reveal | claimed | settled | failed | expired
	Want         bstack.WantAd             `json:"want"`
	ChosenOffer  *bstack.OfferAd           `json:"chosenOffer,omitempty"`
	MatchAudit   *MatchAudit               `json:"matchAudit,omitempty"`
	DealReceipt  *DealReceipt              `json:"dealReceipt,omitempty"`
	ChirpURL     string                    `json:"chirpUrl,omitempty"`
	SaleOffer    *contracts.KeyedSaleOffer `json:"saleOffer,omitempty"`
	PaymentTxID  string                    `json:"paymentTxid,omitempty"`
	ClaimTxID    string                    `json:"claimTxid,omitempty"`
	ApprovalID      string                    `json:"approvalId,omitempty"`
	AwakeableID     string                    `json:"awakeableId,omitempty"`
	StandingOrderID string                    `json:"standingOrderId,omitempty"`
	EscrowTxID      string                    `json:"escrowTxid,omitempty"`
	Shard           string                    `json:"shard,omitempty"`
	DeliveredTxt string                    `json:"deliveredText,omitempty"`
	Error        string                    `json:"error,omitempty"`
	UpdatedAt    int64                     `json:"updatedAt"`
}

// ApprovalDecision resolves a suspended BuyerDeal approval awakeable.
type ApprovalDecision struct {
	Approved  bool   `json:"approved"`
	DecidedBy string `json:"decidedBy,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// BuyerDealInput starts a Restate BuyerDeal workflow keyed by WantID.
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

// SellerDealInput starts a Restate SellerDeal workflow keyed by OfferID.
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

// PaymentSignal delivers a buyer's payment txid + optional verified BUMP proof to a waiting SellerDeal workflow.
type PaymentSignal struct {
	TxID          string `json:"txid"`
	AmountSats    uint64 `json:"amountSats"`
	Confirmed     bool   `json:"confirmed"`
	BlockHeight   uint32 `json:"blockHeight,omitempty"`
	MerklePathHex string `json:"merklePathHex,omitempty"`
}

// ClaimSignal delivers the seller's BRC-369 revealed content key k to a waiting BuyerDeal workflow.
type ClaimSignal struct {
	ClaimTxID      string                 `json:"claimTxid"`
	ClaimTxBEEFHex string                 `json:"claimTxBeefHex,omitempty"`
	ContentKeyHex  string                 `json:"contentKeyHex"`
	CiphertextHex  string                 `json:"ciphertextHex,omitempty"`
	ChirpURL       string                 `json:"chirpUrl,omitempty"`
	ProofBundle    *contracts.ProofBundle `json:"proofBundle,omitempty"`
	DealReceipt    *DealReceipt           `json:"dealReceipt,omitempty"`
}

// CustodyReserveInput serializes global spend reservation through the private Restate Custody Virtual Object.
type CustodyReserveInput struct {
	OperationID  string   `json:"operationId"`
	Attempt      int      `json:"attempt"`
	SellerPub    string   `json:"sellerPub,omitempty"`
	Domains      []string `json:"domains,omitempty"`
	AmountSats   uint64   `json:"amountSats"`
	BypassPolicy bool     `json:"bypassPolicy,omitempty"`
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

// TxEventInput records a durable transaction lifecycle transition on the private Restate Tx Virtual Object.
type TxEventInput struct {
	Kind          string `json:"kind"` // prepared | submission_unknown | broadcast | mempool | mined | reorged | failed | aborted
	Shard         string `json:"shard,omitempty"`
	BlockHeight   uint32 `json:"blockHeight,omitempty"`
	MerklePathHex string `json:"merklePathHex,omitempty"`
	Detail        string `json:"detail,omitempty"`
	ObservedAt    int64  `json:"observedAt,omitempty"`
}

// TxState is the durable state stored per txid inside the Restate Tx Virtual Object.
type TxState struct {
	TxID        string         `json:"txid"`
	Status      string         `json:"status"`
	BlockHeight uint32         `json:"blockHeight,omitempty"`
	Events      []TxEventInput `json:"events"`
	UpdatedAt   int64          `json:"updatedAt"`
}

// AnchorCadenceInput triggers a durable shard anchor check on the private AnchorCadence Virtual Object.
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

// RuntimeTxEmitInput dispatches a Tx event from the authenticated NektarRuntime gateway to the private Tx VO.
type RuntimeTxEmitInput struct {
	TxID  string       `json:"txid"`
	Event TxEventInput `json:"event"`
}

// RuntimeAnchorTriggerInput dispatches an AnchorCadence poke from NektarRuntime to the private AnchorCadence VO.
type RuntimeAnchorTriggerInput struct {
	Shard           string `json:"shard"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	Slot            int64  `json:"slot,omitempty"`
}

// Engine coordinates Nektar v0.1 market deals & the active Restate Go SDK durable execution plane.
// SQLite (`deals`, `approvals`, `pending_sales`, `spend_reservations`) is the single verdict authority;
// Restate provides durable execution, retries, awakeables, and timers.
type Engine struct {
	mu            sync.RWMutex
	mem           *memory.Service
	policy        *Policy
	saleKey       *ec.PrivateKey // persistent node key signing DisclosureSale offers
	broker        *Broker
	matcher       Matcher
	internalToken string
}

func NewEngine(mem *memory.Service, pol *Policy) *Engine {
	if pol == nil {
		pol = DefaultPolicy()
	}
	var sk *ec.PrivateKey
	if mem != nil {
		if raw, err := mem.LoadOrCreateProtectedKey("node_market_key_v1", func() ([]byte, error) {
			k, err := ec.NewPrivateKey()
			if err != nil {
				return nil, err
			}
			return k.Serialize(), nil
		}); err == nil && len(raw) == 32 {
			sk, _ = ec.PrivateKeyFromBytes(raw)
			clear(raw)
		}
	}
	if sk == nil {
		var err error
		sk, err = ec.NewPrivateKey()
		if err != nil {
			panic("market: generate fallback sale key: " + err.Error())
		}
	}
	if mem != nil && mem.DB() != nil {
		pol.AttachDB(mem.DB())
	}
	return &Engine{
		mem:     mem,
		policy:  pol,
		saleKey: sk,
	}
}

func (e *Engine) Policy() *Policy { return e.policy }

// SetBroker attaches the live market Broker so Restate BuyerDeal/SellerDeal handlers can publish
// WantAds, collect live SSE offers, and deliver direct/NAT-bbox purchases inside restate.Run.
func (e *Engine) SetBroker(b *Broker) {
	e.mu.Lock()
	e.broker = b
	e.mu.Unlock()
}

func (e *Engine) getBroker() *Broker {
	e.mu.RLock()
	b := e.broker
	e.mu.RUnlock()
	return b
}

// SetMatcher attaches an optional Jev System One semantic Matcher for Restate workflows.
func (e *Engine) SetMatcher(m Matcher) {
	e.mu.Lock()
	e.matcher = m
	e.mu.Unlock()
}

// Matcher returns the attached semantic Matcher (or nil if deterministic-only).
func (e *Engine) Matcher() Matcher {
	e.mu.RLock()
	m := e.matcher
	e.mu.RUnlock()
	return m
}

// SetInternalToken configures an optional shared secret checked on NektarRuntime ingress calls.
func (e *Engine) SetInternalToken(tok string) {
	e.mu.Lock()
	e.internalToken = strings.TrimSpace(tok)
	e.mu.Unlock()
}

func (e *Engine) checkInternalAuth(ctx restate.Context) error {
	return e.checkInternalAuthReq(ctx.Request())
}

func (e *Engine) checkInternalAuthReq(req *restate.Request) error {
	e.mu.RLock()
	want := e.internalToken
	e.mu.RUnlock()
	if want == "" {
		return restate.TerminalErrorf("nektar: internal restate auth token not configured (fail-closed)")
	}
	got := ""
	if req != nil && req.Headers != nil {
		for k, v := range req.Headers.Iter() {
			if strings.EqualFold(k, internalTokenHeader) {
				got = strings.TrimSpace(v)
				break
			}
		}
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return restate.TerminalErrorf("nektar: unauthorized internal restate call")
	}
	return nil
}

// ListDeals reads durable deal records directly from SQLite (single verdict authority — zero RAM shadow store).
func (e *Engine) ListDeals() []DealState {
	if e.mem == nil || e.mem.DB() == nil {
		return nil
	}
	recs, err := e.mem.DB().ListDeals(100)
	if err != nil {
		return nil
	}
	out := make([]DealState, 0, len(recs))
	for _, r := range recs {
		out = append(out, dealStateFromRecord(r))
	}
	return out
}

func dealStateFromRecord(r store.DealRecord) DealState {
	st := DealState{
		DealID:       r.DealID,
		Role:         r.Role,
		Status:       r.Status,
		ChirpURL:     r.ChirpURL,
		PaymentTxID:  r.PaymentTxID,
		Shard:        r.Shard,
		DeliveredTxt: r.DeliveredText,
		Error:        r.Error,
		UpdatedAt:    r.UpdatedAt,
	}
	if len(r.WantJSON) > 0 {
		_ = json.Unmarshal(r.WantJSON, &st.Want)
	}
	if len(r.ChosenOfferJSON) > 0 {
		var off bstack.OfferAd
		if json.Unmarshal(r.ChosenOfferJSON, &off) == nil {
			st.ChosenOffer = &off
		}
	}
	if len(r.MatchAuditJSON) > 0 {
		var ma MatchAudit
		if json.Unmarshal(r.MatchAuditJSON, &ma) == nil {
			st.MatchAudit = &ma
		}
	}
	if len(r.DealReceiptJSON) > 0 {
		var dr DealReceipt
		if json.Unmarshal(r.DealReceiptJSON, &dr) == nil {
			st.DealReceipt = &dr
		}
	}
	return st
}

// persistDealRecord performs CAS-protected update on existing SQLite deals or inserts if new,
// enforcing monotonic terminal protection so a 'settled' deal never regresses to 'offered'/'expired'.
func (e *Engine) persistDealRecord(rec store.DealRecord) {
	if e.mem == nil || e.mem.DB() == nil || rec.DealID == "" {
		return
	}
	existing, err := e.mem.DB().GetDeal(rec.DealID)
	if err != nil {
		_ = e.mem.DB().SaveDeal(rec)
		return
	}
	// Monotonicity guard: once a deal is settled, never regress status back to pre-settlement states
	// EXCEPT on explicit verified chain reorg rollback ("reorged").
	if existing.Status == "settled" && rec.Status != "settled" && rec.Status != "reorged" {
		rec.Status = "settled"
		if rec.Error != "" && existing.Error == "" {
			rec.Error = ""
		}
	}
	// Preserve fields already committed in earlier stages if not overwritten.
	if len(rec.WantJSON) == 0 {
		rec.WantJSON = existing.WantJSON
	}
	if len(rec.ChosenOfferJSON) == 0 {
		rec.ChosenOfferJSON = existing.ChosenOfferJSON
	}
	if len(rec.MatchAuditJSON) == 0 {
		rec.MatchAuditJSON = existing.MatchAuditJSON
	}
	if len(rec.DealReceiptJSON) == 0 {
		rec.DealReceiptJSON = existing.DealReceiptJSON
	}
	if rec.PaymentTxID == "" {
		rec.PaymentTxID = existing.PaymentTxID
	}
	if len(rec.PaymentBEEF) == 0 {
		rec.PaymentBEEF = existing.PaymentBEEF
	}
	if rec.PaymentDerivPrefix == "" {
		rec.PaymentOutputIndex = existing.PaymentOutputIndex
		rec.PaymentDerivPrefix = existing.PaymentDerivPrefix
		rec.PaymentDerivSuffix = existing.PaymentDerivSuffix
		rec.PaymentSenderKey = existing.PaymentSenderKey
	}
	if rec.ClaimKeyHex == "" {
		rec.ClaimKeyHex = existing.ClaimKeyHex
	}
	if rec.ChirpURL == "" {
		rec.ChirpURL = existing.ChirpURL
	}
	if rec.CounterpartyPub == "" {
		rec.CounterpartyPub = existing.CounterpartyPub
	}
	if rec.PriceSats == 0 {
		rec.PriceSats = existing.PriceSats
	}
	if rec.FirstOfferMs == 0 {
		rec.FirstOfferMs = existing.FirstOfferMs
	}
	if rec.Shard == "" {
		rec.Shard = existing.Shard
	}
	rec.CreatedAt = existing.CreatedAt
	rec.Revision = existing.Revision
	if ok, casErr := e.mem.DB().UpdateDealCAS(rec); casErr != nil || !ok {
		// Re-read once on concurrent modification and retry CAS cleanly.
		if latest, gErr := e.mem.DB().GetDeal(rec.DealID); gErr == nil {
			if latest.Status == "settled" && rec.Status != "settled" && rec.Status != "reorged" {
				rec.Status = "settled"
			}
			rec.Revision = latest.Revision
			rec.CreatedAt = latest.CreatedAt
			_, _ = e.mem.DB().UpdateDealCAS(rec)
		}
	}
}

// NewRestateServer creates the dedicated h2c Restate service endpoint with optional Request Identity v1 JWT verification.
func (e *Engine) NewRestateServer(identityKeys ...string) *restateserver.Restate {
	srv := restateserver.NewRestate()
	var clean []string
	for _, k := range identityKeys {
		if k = strings.TrimSpace(k); k != "" {
			clean = append(clean, k)
		}
	}
	if len(clean) > 0 {
		srv = srv.WithIdentityV1(clean...)
	}
	return e.BindRestate(srv)
}

// BindRestate registers ALL Nektar durable services onto restatedev/sdk-go/server.Restate:
//   - Workflow "BuyerDeal"       — durable buyer flow (policy -> awakeable approval -> rank -> Custody -> pay -> claim -> verify -> settle)
//   - Workflow "SellerDeal"      — durable seller flow (BRC-369 seal -> pending_sales -> bounded payment & confirmation wait -> settle)
//   - Virtual Object "Custody"   — private serialized global spend reservation (SQLite atomic tx, zero network I/O)
//   - Virtual Object "Tx"        — private monotonic deduplicated tx lifecycle event journal per txid
//   - Virtual Object "AnchorCadence" — private quantized slot-keyed shard anchoring cadence with PauseOnMaxAttempts
//   - Service "NektarRuntime"    — authenticated ingress gateway to private Tx & AnchorCadence objects
func (e *Engine) BindRestate(srv *restateserver.Restate) *restateserver.Restate {
	dealRetry := restate.WithInvocationRetryPolicy(
		restate.WithInitialRetryInterval(1*time.Second),
		restate.WithRetryIntervalFactor(2.0),
		restate.WithMaxRetryInterval(30*time.Second),
		restate.WithMaxRetryAttempts(8),
		restate.PauseOnMaxAttempts(),
	)
	floorRetry := restate.WithInvocationRetryPolicy(
		restate.WithInitialRetryInterval(2*time.Second),
		restate.WithRetryIntervalFactor(2.0),
		restate.WithMaxRetryInterval(60*time.Second),
		restate.WithMaxRetryAttempts(5),
		restate.PauseOnMaxAttempts(),
	)

	buyerWF := restate.NewWorkflow("BuyerDeal",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateBuyerRun)).
		Handler("deliverKey", restate.NewWorkflowSharedHandler(e.restateBuyerDeliverKey)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateDealStatus))

	sellerWF := restate.NewWorkflow("SellerDeal",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateSellerRun)).
		Handler("deliverPayment", restate.NewWorkflowSharedHandler(e.restateSellerDeliverPayment)).
		Handler("confirmPayment", restate.NewWorkflowSharedHandler(e.restateSellerConfirmPayment)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateDealStatus))

	custodyObj := restate.NewObject("Custody",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("reserve", restate.NewObjectHandler(e.restateCustodyReserve)).
		Handler("release", restate.NewObjectHandler(e.restateCustodyRelease)).
		Handler("balance", restate.NewObjectSharedHandler(e.restateCustodyBalance))

	txObj := restate.NewObject("Tx",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("emit", restate.NewObjectHandler(e.restateTxEmit)).
		Handler("state", restate.NewObjectSharedHandler(e.restateTxState))

	cadenceObj := restate.NewObject("AnchorCadence",
		restate.WithIngressPrivate(true),
		restate.WithInactivityTimeout(3*time.Minute),
		floorRetry,
	).
		Handler("trigger", restate.NewObjectHandler(e.restateAnchorCadenceTrigger)).
		Handler("status", restate.NewObjectSharedHandler(e.restateAnchorCadenceStatus))

	runtimeSvc := restate.NewService("NektarRuntime",
		restate.WithInactivityTimeout(1*time.Minute),
		dealRetry,
	).
		Handler("emitTx", restate.NewServiceHandler(e.restateRuntimeEmitTx)).
		Handler("txState", restate.NewServiceHandler(e.restateRuntimeTxState)).
		Handler("triggerAnchor", restate.NewServiceHandler(e.restateRuntimeTriggerAnchor)).
		Handler("anchorStatus", restate.NewServiceHandler(e.restateRuntimeAnchorStatus))

	standingWF := restate.NewWorkflow("StandingOrder",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateStandingOrderRun)).
		Handler("onOffer", restate.NewWorkflowSharedHandler(e.restateStandingOrderOnOffer)).
		Handler("steer", restate.NewWorkflowSharedHandler(e.restateStandingOrderSteer)).
		Handler("cancel", restate.NewWorkflowSharedHandler(e.restateStandingOrderCancel)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateStandingOrderStatus))

	guardianWF := restate.NewWorkflow("EscrowGuardian",
		restate.WithInactivityTimeout(2*time.Minute),
		dealRetry,
	).
		Handler("run", restate.NewWorkflowHandler(e.restateEscrowGuardianRun)).
		Handler("markClaimed", restate.NewWorkflowSharedHandler(e.restateEscrowGuardianMarkClaimed)).
		Handler("status", restate.NewWorkflowSharedHandler(e.restateEscrowGuardianStatus))

	return srv.
		Bind(buyerWF).
		Bind(sellerWF).
		Bind(custodyObj).
		Bind(txObj).
		Bind(cadenceObj).
		Bind(runtimeSvc).
		Bind(standingWF).
		Bind(guardianWF)
}

func (e *Engine) restateDealStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (DealState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return DealState{}, err
	}
	return restate.Get[DealState](ctx, "state")
}
