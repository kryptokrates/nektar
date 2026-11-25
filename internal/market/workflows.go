package market

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	restate "github.com/restatedev/sdk-go"
	restateserver "github.com/restatedev/sdk-go/server"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const internalTokenHeader = "x-nektar-internal-token"

// DealState records the durable lifecycle of one buyer or seller deal.
type DealState struct {
	DealID       string                    `json:"dealId"`
	Role         string                    `json:"role"`   // buyer | seller
	Status       string                    `json:"status"` // open | approval_pending | collecting | offered | payment_pending | paid_awaiting_reveal | claimed | settled | failed | expired
	Want         bstack.WantAd             `json:"want"`
	ChosenOffer  *bstack.OfferAd           `json:"chosenOffer,omitempty"`
	ChirpURL     string                    `json:"chirpUrl,omitempty"`
	SaleOffer    *contracts.KeyedSaleOffer `json:"saleOffer,omitempty"`
	PaymentTxID  string                    `json:"paymentTxid,omitempty"`
	ClaimTxID    string                    `json:"claimTxid,omitempty"`
	ApprovalID   string                    `json:"approvalId,omitempty"`
	AwakeableID  string                    `json:"awakeableId,omitempty"`
	Shard        string                    `json:"shard,omitempty"`
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

// PaymentSignal delivers a buyer's payment txid to a waiting SellerDeal workflow.
type PaymentSignal struct {
	TxID       string `json:"txid"`
	AmountSats uint64 `json:"amountSats"`
	Confirmed  bool   `json:"confirmed"`
}

// ClaimSignal delivers the seller's BRC-369 revealed content key k to a waiting BuyerDeal workflow.
type ClaimSignal struct {
	ClaimTxID     string                 `json:"claimTxid"`
	ContentKeyHex string                 `json:"contentKeyHex"`
	CiphertextHex string                 `json:"ciphertextHex,omitempty"`
	ChirpURL      string                 `json:"chirpUrl,omitempty"`
	ProofBundle   *contracts.ProofBundle `json:"proofBundle,omitempty"`
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
	Kind        string `json:"kind"` // prepared | submission_unknown | broadcast | mempool | mined | failed | aborted
	Shard       string `json:"shard,omitempty"`
	BlockHeight uint32 `json:"blockHeight,omitempty"`
	Detail      string `json:"detail,omitempty"`
	ObservedAt  int64  `json:"observedAt,omitempty"`
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
	Shard      string `json:"shard"`
	LastTxID   string `json:"lastTxid,omitempty"`
	LastRunAt  int64  `json:"lastRunAt"`
	AnchorRuns int64  `json:"anchorRuns"`
	NextSlot   int64  `json:"nextSlot,omitempty"`
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

// Engine coordinates Nektar v0.4 market deals & the active Restate Go SDK durable execution plane.
// SQLite (`deals`, `approvals`, `pending_sales`, `spend_reservations`) is the single verdict authority;
// Restate provides durable execution, retries, awakeables, and timers.
type Engine struct {
	mu            sync.RWMutex
	mem           *memory.Service
	policy        *Policy
	saleKey       *ec.PrivateKey // persistent node key signing DisclosureSale offers
	broker        *Broker
	internalToken string
}

func NewEngine(mem *memory.Service, pol *Policy) *Engine {
	if pol == nil {
		pol = DefaultPolicy()
	}
	var sk *ec.PrivateKey
	if mem != nil && mem.DB() != nil {
		if raw, err := mem.DB().LoadOrCreateStableKey("node_market_key_v1", func() ([]byte, error) {
			k, err := ec.NewPrivateKey()
			if err != nil {
				return nil, err
			}
			return k.Serialize(), nil
		}); err == nil && len(raw) == 32 {
			sk, _ = ec.PrivateKeyFromBytes(raw)
		}
	}
	if sk == nil {
		sk, _ = ec.NewPrivateKey()
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

// SetInternalToken configures an optional shared secret checked on NektarRuntime ingress calls.
func (e *Engine) SetInternalToken(tok string) {
	e.mu.Lock()
	e.internalToken = strings.TrimSpace(tok)
	e.mu.Unlock()
}

func (e *Engine) checkInternalAuth(ctx restate.Context) error {
	e.mu.RLock()
	want := e.internalToken
	e.mu.RUnlock()
	if want == "" {
		return nil
	}
	req := ctx.Request()
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
	return st
}

// persistDealRecord performs CAS-protected update on existing SQLite deals or inserts if new.
func (e *Engine) persistDealRecord(rec store.DealRecord) {
	if e.mem == nil || e.mem.DB() == nil || rec.DealID == "" {
		return
	}
	existing, err := e.mem.DB().GetDeal(rec.DealID)
	if err != nil {
		_ = e.mem.DB().SaveDeal(rec)
		return
	}
	// Preserve fields already committed in earlier stages if not overwritten.
	if len(rec.WantJSON) == 0 {
		rec.WantJSON = existing.WantJSON
	}
	if len(rec.ChosenOfferJSON) == 0 {
		rec.ChosenOfferJSON = existing.ChosenOfferJSON
	}
	if rec.PaymentTxID == "" {
		rec.PaymentTxID = existing.PaymentTxID
	}
	if len(rec.PaymentBEEF) == 0 {
		rec.PaymentBEEF = existing.PaymentBEEF
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

	return srv.
		Bind(buyerWF).
		Bind(sellerWF).
		Bind(custodyObj).
		Bind(txObj).
		Bind(cadenceObj).
		Bind(runtimeSvc)
}

type buyerPolicyEval struct {
	NowUnix        int64  `json:"nowUnix"`
	KillSwitch     bool   `json:"killSwitch"`
	NeedsApproval  bool   `json:"needsApproval"`
	ApprovalReason string `json:"approvalReason,omitempty"`
	BypassPolicy   bool   `json:"bypassPolicy"`
}

type buyerRankResult struct {
	NowUnix        int64           `json:"nowUnix"`
	NoOffers       bool            `json:"noOffers"`
	PolicyRejected bool            `json:"policyRejected"`
	Error          string          `json:"error,omitempty"`
	Chosen         *bstack.OfferAd `json:"chosen,omitempty"`
	FirstOfferMs   int64           `json:"firstOfferMs"`
}

type buyerPayResult struct {
	NowUnix         int64        `json:"nowUnix"`
	TxID            string       `json:"txid,omitempty"`
	PreBroadcastErr string       `json:"preBroadcastErr,omitempty"`
	ImmediateClaim  *ClaimSignal `json:"immediateClaim,omitempty"`
	DeliveryErr     string       `json:"deliveryErr,omitempty"`
}

type buyerVerifyResult struct {
	NowUnix       int64  `json:"nowUnix"`
	DeliveredText string `json:"deliveredText,omitempty"`
	TargetShard   string `json:"targetShard,omitempty"`
	FailedReason  string `json:"failedReason,omitempty"`
}

func (e *Engine) restateBuyerRun(ctx restate.WorkflowContext, in BuyerDealInput) (DealState, error) {
	dealID := restate.Key(ctx)
	want := in.Want
	if want.WantID == "" {
		want.WantID = dealID
	}

	// Step 1: Journal policy & kill-switch evaluation inside restate.Run (zero non-determinism on replay).
	eval, err := restate.Run(ctx, func(_ restate.RunContext) (buyerPolicyEval, error) {
		now := time.Now().Unix()
		if e.mem != nil && e.mem.DB() != nil {
			_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
		}
		kill, maxDeal, maxDay, _, spent, buyerOK, _ := e.policy.Snapshot()
		if kill {
			wantJSON, _ := json.Marshal(want)
			e.persistDealRecord(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "failed",
				WantJSON: wantJSON,
				Error:    "market: operator kill switch is engaged",
			})
			return buyerPolicyEval{NowUnix: now, KillSwitch: true}, nil
		}
		bypass := false
		if in.ApprovedOverrideID != "" {
			if e.mem != nil && e.mem.DB() != nil {
				if cErr := e.mem.DB().ConsumeApprovalOverride(in.ApprovedOverrideID); cErr != nil {
					return buyerPolicyEval{}, restate.ToTerminalError(fmt.Errorf("market: invalid or already consumed approval override %q: %w", in.ApprovedOverrideID, cErr))
				}
			}
			bypass = true
		}
		// Only require operator approval gate for live broker trades (when no pre-supplied offers bypass it OR when policy explicitly blocks).
		needsApproval := false
		reason := ""
		if !bypass {
			domainErr := e.policy.CanSpendForDomains("", want.Domains, 0)
			if len(in.Offers) == 0 && !buyerOK {
				needsApproval = true
				reason = "no active buyer policy approved (run `nektar policy approve --preset buyer` or approve in Dashboard)"
			} else if domainErr != nil {
				needsApproval = true
				reason = domainErr.Error()
			} else if maxDeal > 0 && want.MaxSats > maxDeal && len(in.Offers) == 0 {
				needsApproval = true
				reason = fmt.Sprintf("requested maxSats (%d) exceeds buyer policy maxPerDeal (%d)", want.MaxSats, maxDeal)
			} else if maxDay > 0 && spent+want.MaxSats > maxDay && len(in.Offers) == 0 {
				var rem uint64
				if maxDay > spent {
					rem = maxDay - spent
				}
				needsApproval = true
				reason = fmt.Sprintf("requested maxSats (%d) exceeds remaining daily cap (%d)", want.MaxSats, rem)
			}
		}
		return buyerPolicyEval{
			NowUnix:        now,
			NeedsApproval:  needsApproval,
			ApprovalReason: reason,
			BypassPolicy:   bypass,
		}, nil
	}, restate.WithName("buyer_policy_eval"))
	if err != nil {
		return DealState{}, err
	}

	st := DealState{DealID: dealID, Role: "buyer", Status: "open", Want: want, UpdatedAt: eval.NowUnix}
	if eval.KillSwitch {
		st.Status = "failed"
		st.Error = "market: operator kill switch is engaged"
		restate.Set(ctx, "state", st)
		return st, nil
	}
	restate.Set(ctx, "state", st)

	bypassPolicy := eval.BypassPolicy

	// Step 2: Real Restate Awakeable Approval Gate — resumes THIS exact wantID on operator approve/deny!
	if eval.NeedsApproval {
		apprTimeout := in.ApprovalTimeout
		if apprTimeout <= 0 {
			apprTimeout = 15 * time.Minute
		}
		apprAwk := restate.Awakeable[ApprovalDecision](ctx)
		apprID := "appr-" + strings.ReplaceAll(restate.UUID(ctx).String(), "-", "")[:16]
		awkID := apprAwk.Id()

		_, pErr := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			now := time.Now().Unix()
			wantJSON, _ := json.Marshal(want)
			if e.mem != nil && e.mem.DB() != nil {
				_ = e.mem.DB().SaveApproval(store.ApprovalRecord{
					ID:           apprID,
					DealID:       dealID,
					Reason:       eval.ApprovalReason,
					MaxExtraSats: want.MaxSats,
					Status:       "pending",
					AwakeableID:  awkID,
					ExpiresAt:    now + int64(apprTimeout.Seconds()),
				})
			}
			e.persistDealRecord(store.DealRecord{
				DealID:    dealID,
				Role:      "buyer",
				Status:    "approval_pending",
				WantJSON:  wantJSON,
				PriceSats: want.MaxSats,
				Error:     eval.ApprovalReason + " [approvalId=" + apprID + "]",
			})
			if br := e.getBroker(); br != nil {
				br.notify("approval_pending", map[string]any{"approvalId": apprID, "dealId": dealID, "reason": eval.ApprovalReason, "awakeableId": awkID})
			}
			return now, nil
		}, restate.WithName("buyer_persist_approval"))
		if pErr != nil {
			return st, pErr
		}

		st.Status = "approval_pending"
		st.ApprovalID = apprID
		st.AwakeableID = awkID
		st.Error = eval.ApprovalReason + " [approvalId=" + apprID + "]"
		restate.Set(ctx, "state", st)

		apprTimer := restate.After(ctx, apprTimeout)
		winner, waitErr := restate.WaitFirst(ctx, apprAwk, apprTimer)
		if waitErr != nil {
			return st, waitErr
		}
		if winner == apprTimer {
			expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "expired",
					Error:  "approval timed out",
				})
				return now, nil
			}, restate.WithName("buyer_approval_expired"))
			st.Status = "expired"
			st.Error = "approval timed out"
			st.UpdatedAt = expNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		dec, dErr := apprAwk.Result()
		if dErr != nil || !dec.Approved {
			reason := "denied by operator"
			if dErr != nil {
				reason = dErr.Error()
			} else if dec.Reason != "" {
				reason = dec.Reason
			}
			denyNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  reason,
				})
				return now, nil
			}, restate.WithName("buyer_approval_denied"))
			st.Status = "failed"
			st.Error = reason
			st.UpdatedAt = denyNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		bypassPolicy = true
	}

	// Step 3: Collect (if live broker) & rank offers deterministically inside restate.Run.
	rankedRes, err := restate.Run(ctx, func(rctx restate.RunContext) (buyerRankResult, error) {
		now := time.Now().Unix()
		offers := in.Offers
		var firstOfferMs int64
		wantJSON, _ := json.Marshal(want)
		if len(offers) == 0 {
			br := e.getBroker()
			if br == nil {
				e.persistDealRecord(store.DealRecord{
					DealID:   dealID,
					Role:     "buyer",
					Status:   "failed",
					WantJSON: wantJSON,
					Error:    "no eligible offers under policy",
				})
				return buyerRankResult{NowUnix: now, PolicyRejected: true, Error: "no eligible offers under policy"}, nil
			}
			e.persistDealRecord(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "collecting",
				WantJSON: wantJSON,
			})
			collected, fms, cErr := br.CollectOffersForWant(rctx, want, in.CollectTimeout)
			if cErr != nil {
				e.persistDealRecord(store.DealRecord{
					DealID:   dealID,
					Role:     "buyer",
					Status:   "failed",
					WantJSON: wantJSON,
					Error:    cErr.Error(),
				})
				return buyerRankResult{NowUnix: now, PolicyRejected: true, Error: cErr.Error()}, nil
			}
			offers = collected
			firstOfferMs = fms
			if len(offers) == 0 {
				dom := "ops"
				if len(want.Domains) > 0 {
					dom = want.Domains[0]
				}
				if e.mem != nil && e.mem.DB() != nil {
					_ = e.mem.DB().RecordUnmatchedWant(store.UnmatchedWantRecord{
						WantID:  dealID,
						Domain:  dom,
						Query:   want.Query,
						MaxSats: want.MaxSats,
					})
				}
				msg := fmt.Sprintf("no valid offers received for domain %q", dom)
				e.persistDealRecord(store.DealRecord{
					DealID:   dealID,
					Role:     "buyer",
					Status:   "expired",
					WantJSON: wantJSON,
					Error:    msg,
				})
				return buyerRankResult{NowUnix: now, NoOffers: true, Error: msg}, nil
			}
		}

		reps := make(map[string]store.SellerReputation)
		if e.mem != nil && e.mem.DB() != nil {
			if list, lErr := e.mem.DB().ListReputations(); lErr == nil {
				for _, r := range list {
					reps[r.SellerPub] = r
				}
			}
		}
		var rankPol *Policy
		if !bypassPolicy {
			rankPol = e.policy
		}
		ranked := RankOffersWithRep(want, offers, rankPol, reps)
		if len(ranked) == 0 {
			msg := "no eligible offers under policy"
			e.persistDealRecord(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "failed",
				WantJSON: wantJSON,
				Error:    msg,
			})
			return buyerRankResult{NowUnix: now, PolicyRejected: true, Error: msg}, nil
		}
		chosen := ranked[0].Offer
		chosenJSON, _ := json.Marshal(chosen)
		e.persistDealRecord(store.DealRecord{
			DealID:          dealID,
			Role:            "buyer",
			Status:          "offered",
			WantJSON:        wantJSON,
			ChosenOfferJSON: chosenJSON,
			CounterpartyPub: chosen.Pub,
			PriceSats:       chosen.PriceSats,
			ChirpURL:        chosen.ChirpURL,
			IsHubSeller:     chosen.IsHub,
			FirstOfferMs:    firstOfferMs,
		})
		return buyerRankResult{
			NowUnix:      now,
			Chosen:       &chosen,
			FirstOfferMs: firstOfferMs,
		}, nil
	}, restate.WithName("buyer_collect_and_rank"))
	if err != nil {
		return st, err
	}
	if rankedRes.NoOffers {
		st.Status = "expired"
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
		restate.Set(ctx, "state", st)
		return st, nil
	}
	if rankedRes.PolicyRejected || rankedRes.Chosen == nil {
		st.Status = "failed"
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
		restate.Set(ctx, "state", st)
		return st, nil
	}

	chosen := *rankedRes.Chosen
	st.ChosenOffer = &chosen
	st.ChirpURL = chosen.ChirpURL
	st.Status = "offered"
	st.Error = ""
	st.UpdatedAt = rankedRes.NowUnix
	restate.Set(ctx, "state", st)

	// Step 4: Reserve spend atomically through the private global Custody Virtual Object!
	if chosen.PriceSats > 0 {
		res, cErr := restate.Object[CustodyReserveResult](ctx, "Custody", "global", "reserve").
			Request(CustodyReserveInput{
				OperationID:  dealID,
				Attempt:      1,
				SellerPub:    chosen.Pub,
				Domains:      want.Domains,
				AmountSats:   chosen.PriceSats,
				BypassPolicy: bypassPolicy,
			})
		if cErr != nil {
			return st, cErr
		}
		if !res.Approved {
			failNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "custody reservation denied: " + res.Reason,
				})
				return now, nil
			}, restate.WithName("buyer_custody_denied"))
			st.Status = "failed"
			st.Error = "custody reservation denied: " + res.Reason
			st.UpdatedAt = failNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
	}

	// Step 5: Live on-chain payment + purchase delivery (when Seller advertised a pay target).
	var immediateClaim *ClaimSignal
	if chosen.PayAddress != "" || chosen.PayIdentityKey != "" {
		payRes, pErr := restate.Run(ctx, func(rctx restate.RunContext) (buyerPayResult, error) {
			now := time.Now().Unix()
			// Check if we already paid & persisted PaymentTxID before a crash/retry!
			var payTxID string
			var payBEEF []byte
			var outIdx uint32
			var derivPrefix, derivSuffix, senderKey string
			if existing, gErr := e.mem.DB().GetDeal(dealID); gErr == nil && existing.PaymentTxID != "" {
				payTxID = existing.PaymentTxID
				payBEEF = existing.PaymentBEEF
			} else {
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "payment_pending",
				})
				sendArgs := nwallet.SendArgs{
					Satoshis:    chosen.PriceSats,
					Description: "market purchase " + chosen.OfferID,
					Labels:      []string{"market", "purchase"},
				}
				if chosen.PayIdentityKey != "" {
					sendArgs.ToIdentityKey = chosen.PayIdentityKey
				} else {
					sendArgs.ToAddress = chosen.PayAddress
				}
				sendRes, sErr := e.mem.Chain().Send(rctx, sendArgs)
				if sErr != nil {
					if errors.Is(sErr, nwallet.ErrBroadcastUnknown) {
						// Never release Custody on unknown broadcast status; let Restate retry reconciliation!
						return buyerPayResult{}, sErr
					}
					return buyerPayResult{NowUnix: now, PreBroadcastErr: sErr.Error()}, nil
				}
				payTxID = sendRes.TxID
				payBEEF = sendRes.BEEF
				outIdx = sendRes.OutputIndex
				derivPrefix = sendRes.DerivationPrefix
				derivSuffix = sendRes.DerivationSuffix
				senderKey = sendRes.SenderIdentityKey
				if len(payBEEF) == 0 && sendRes.Tx != nil {
					var bErr error
					payBEEF, bErr = chain.SafeAtomicBEEF(sendRes.Tx)
					if bErr != nil {
						return buyerPayResult{}, bErr
					}
				}
				e.persistDealRecord(store.DealRecord{
					DealID:      dealID,
					Role:        "buyer",
					Status:      "paid_awaiting_reveal",
					PaymentTxID: payTxID,
					PaymentBEEF: payBEEF,
				})
			}

			res := buyerPayResult{NowUnix: now, TxID: payTxID}
			if br := e.getBroker(); br != nil {
				pReq := PurchaseRequest{
					OfferID:           chosen.OfferID,
					WantID:            dealID,
					BuyerPub:          br.PubHex(),
					TxID:              payTxID,
					PaymentBEEFHex:    hex.EncodeToString(payBEEF),
					OutputIndex:       outIdx,
					DerivationPrefix:  derivPrefix,
					DerivationSuffix:  derivSuffix,
					SenderIdentityKey: senderKey,
				}
				// Try direct HTTP first; if Seller is NAT-ed, fire bbox purchase_beef asynchronously
				// and return nil so BuyerDeal suspends durably on Restate Promise("claim")!
				pResp, dErr := br.DeliverPurchaseAsync(rctx, chosen, dealID, pReq)
				if dErr != nil {
					res.DeliveryErr = dErr.Error()
				} else if pResp != nil && pResp.OK {
					res.ImmediateClaim = &ClaimSignal{
						ClaimTxID:     payTxID,
						ContentKeyHex: pResp.ContentKeyHex,
						CiphertextHex: pResp.CiphertextHex,
						ChirpURL:      pResp.ChirpURL,
						ProofBundle:   pResp.ProofBundle,
					}
				}
			}
			return res, nil
		}, restate.WithName("buyer_pay_and_deliver"))
		if pErr != nil {
			return st, pErr
		}
		if payRes.PreBroadcastErr != "" {
			if chosen.PriceSats > 0 {
				_, _ = restate.Object[bool](ctx, "Custody", "global", "release").
					Request(CustodyReleaseInput{OperationID: dealID, Attempt: 1})
			}
			failNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "payment failed: " + payRes.PreBroadcastErr,
				})
				return now, nil
			}, restate.WithName("buyer_payment_failed"))
			st.Status = "failed"
			st.Error = "payment failed: " + payRes.PreBroadcastErr
			st.UpdatedAt = failNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		if payRes.TxID != "" {
			st.PaymentTxID = payRes.TxID
			st.Status = "paid_awaiting_reveal"
			st.UpdatedAt = payRes.NowUnix
			restate.Set(ctx, "state", st)
			restate.ObjectSend(ctx, "Tx", payRes.TxID, "emit").Send(
				TxEventInput{Kind: "broadcast", Detail: "buyer-deal:" + dealID, ObservedAt: payRes.NowUnix},
				restate.WithIdempotencyKey("tx:"+payRes.TxID+":broadcast:0"),
			)
		}
		if payRes.DeliveryErr != "" && payRes.ImmediateClaim == nil {
			failNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				if e.mem != nil && e.mem.DB() != nil {
					_ = e.mem.DB().RecordReputation(chosen.Pub, false, 0)
				}
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "purchase delivery failed: " + payRes.DeliveryErr,
				})
				return now, nil
			}, restate.WithName("buyer_delivery_failed"))
			st.Status = "failed"
			st.Error = "purchase delivery failed: " + payRes.DeliveryErr
			st.UpdatedAt = failNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		immediateClaim = payRes.ImmediateClaim
	}

	// Step 6: Obtain ClaimSignal (either synchronously returned or via Restate durable Promise("claim")).
	var sig ClaimSignal
	if immediateClaim != nil {
		sig = *immediateClaim
	} else {
		timeout := in.WaitTimeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		claimFut := restate.Promise[ClaimSignal](ctx, "claim")
		sleepFut := restate.After(ctx, timeout)
		winner, cancelErr := restate.WaitFirst(ctx, claimFut, sleepFut)
		if cancelErr != nil {
			return st, cancelErr
		}
		if winner == sleepFut {
			// R13: explicitly reject promise on expiry so late deliverKey calls fail honestly!
			_ = restate.Promise[ClaimSignal](ctx, "claim").Reject(restate.TerminalErrorf("deal %s expired waiting for seller key reveal", dealID))
			expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				if st.PaymentTxID != "" && e.mem != nil && e.mem.DB() != nil {
					_ = e.mem.DB().RecordReputation(chosen.Pub, false, 0)
				}
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "expired",
					Error:  "timed out waiting for seller claim reveal",
				})
				return now, nil
			}, restate.WithName("buyer_claim_expired"))
			st.Status = "expired"
			st.Error = "timed out waiting for seller claim reveal"
			st.UpdatedAt = expNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		var cErr error
		sig, cErr = claimFut.Result()
		if cErr != nil {
			return st, cErr
		}
	}

	// Step 7: Zero-trust BRC-369 + CHIRP + offline SPV verification & tainted memory commit inside restate.Run.
	verRes, err := restate.Run(ctx, func(rctx restate.RunContext) (buyerVerifyResult, error) {
		now := time.Now().Unix()
		var deliveredText string
		targetShard := strings.TrimSpace(in.TargetShard)
		if targetShard == "" {
			dom := "ops"
			if len(want.Domains) > 0 {
				dom = want.Domains[0]
			}
			targetShard = "market/" + dom
		}

		if sig.ContentKeyHex != "" && len(chosen.SaleOffer) > 0 {
			var saleOffer contracts.KeyedSaleOffer
			if err := json.Unmarshal(chosen.SaleOffer, &saleOffer); err != nil {
				return buyerVerifyResult{NowUnix: now, FailedReason: "decode saleOffer: " + err.Error()}, nil
			}
			if chosen.SellerURL != "" && chosen.ChirpURL != "" {
				if br := e.getBroker(); br != nil {
					ct, cErr := br.disc.FetchRemoteChirp(rctx, chosen.SellerURL, chosen.ChirpURL)
					if cErr != nil {
						return buyerVerifyResult{NowUnix: now, FailedReason: "remote CHIRP verification failed: " + cErr.Error()}, nil
					}
					ctHex := hex.EncodeToString(ct)
					if saleOffer.CiphertextHex != "" && ctHex != saleOffer.CiphertextHex {
						return buyerVerifyResult{NowUnix: now, FailedReason: "CHIRP ciphertext mismatch vs signed offer"}, nil
					}
					saleOffer.CiphertextHex = ctHex
				}
			} else if saleOffer.CiphertextHex == "" && sig.CiphertextHex != "" {
				saleOffer.CiphertextHex = sig.CiphertextHex
			}
			keyBytes, kErr := hex.DecodeString(strings.TrimSpace(sig.ContentKeyHex))
			if kErr != nil || len(keyBytes) != 32 {
				return buyerVerifyResult{NowUnix: now, FailedReason: "seller returned invalid 32-byte content key"}, nil
			}
			plaintext, pErr := contracts.ClaimKeyedSale(&saleOffer, keyBytes)
			if pErr != nil {
				return buyerVerifyResult{NowUnix: now, FailedReason: "BRC-369 key verification failed: " + pErr.Error()}, nil
			}
			if sig.ProofBundle != nil {
				if chosen.Leaf != "" && !strings.EqualFold(sig.ProofBundle.Leaf, chosen.Leaf) {
					return buyerVerifyResult{NowUnix: now, FailedReason: "seller SPV ProofBundle leaf mismatches signed offer leaf"}, nil
				}
				vr := contracts.VerifyFragmentCtx(rctx, *sig.ProofBundle, e.mem.Chain().Tracker())
				if !vr.OK && e.mem.Chain().Network() == "memory" && vr.Reason == "BEEFInvalid" && strings.Contains(vr.Detail, "BUMP root not valid in header chain") {
					vr = contracts.VerifyFragmentStructural(*sig.ProofBundle)
				}
				if !vr.OK {
					return buyerVerifyResult{NowUnix: now, FailedReason: fmt.Sprintf("seller SPV ProofBundle invalid (%s: %s)", vr.Reason, vr.Detail)}, nil
				}
			}
			deliveredText = string(plaintext)
			dom := "ops"
			if len(want.Domains) > 0 {
				dom = want.Domains[0]
			}
			if _, rErr := e.mem.Remember(rctx, memory.RememberInput{
				Shard:      targetShard,
				Text:       deliveredText,
				Source:     "market:" + shortPub(chosen.Pub),
				Class:      "market:" + dom,
				Tainted:    true, // F10: purchased external knowledge is ALWAYS marked tainted!
				AutoAnchor: e.mem.Chain().Network() == "memory" && e.mem.Chain().BalanceSats() > 1000,
			}); rErr != nil {
				return buyerVerifyResult{}, rErr
			}
		}

		if e.mem != nil && e.mem.DB() != nil && chosen.Pub != "" {
			_ = e.mem.DB().RecordReputation(chosen.Pub, true, chosen.PriceSats)
		}
		e.persistDealRecord(store.DealRecord{
			DealID:        dealID,
			Role:          "buyer",
			Status:        "settled",
			Shard:         targetShard,
			PaymentTxID:   st.PaymentTxID,
			ClaimKeyHex:   sig.ContentKeyHex,
			DeliveredText: deliveredText,
			Error:         "",
		})
		if br := e.getBroker(); br != nil {
			br.notify("deal_settled", map[string]any{"role": "buyer", "dealId": dealID, "txid": st.PaymentTxID, "sats": chosen.PriceSats, "shard": targetShard})
		}
		return buyerVerifyResult{NowUnix: now, DeliveredText: deliveredText, TargetShard: targetShard}, nil
	}, restate.WithName("buyer_verify_and_commit"))
	if err != nil {
		return st, err
	}
	if verRes.FailedReason != "" {
		_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
			if e.mem != nil && e.mem.DB() != nil && chosen.Pub != "" {
				_ = e.mem.DB().RecordReputation(chosen.Pub, false, 0)
			}
			e.persistDealRecord(store.DealRecord{
				DealID: dealID,
				Role:   "buyer",
				Status: "failed",
				Error:  verRes.FailedReason,
			})
			return true, nil
		}, restate.WithName("buyer_record_verify_failure"))
		st.Status = "failed"
		st.Error = verRes.FailedReason
		st.UpdatedAt = verRes.NowUnix
		restate.Set(ctx, "state", st)
		return st, nil
	}

	st.ClaimTxID = sig.ClaimTxID
	st.Shard = verRes.TargetShard
	st.DeliveredTxt = verRes.DeliveredText
	st.Status = "settled"
	st.Error = ""
	st.UpdatedAt = verRes.NowUnix
	restate.Set(ctx, "state", st)
	return st, nil
}

func (e *Engine) restateBuyerDeliverKey(ctx restate.WorkflowSharedContext, sig ClaimSignal) (bool, error) {
	if err := restate.Promise[ClaimSignal](ctx, "claim").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

type sellerPrepResult struct {
	Offer       contracts.KeyedSaleOffer `json:"offer"`
	ChirpURL    string                   `json:"chirpUrl"`
	ZeroConfMax uint64                   `json:"zeroConfMax"`
	NowUnix     int64                    `json:"nowUnix"`
	FailedErr   string                   `json:"failedErr,omitempty"`
}

func (e *Engine) restateSellerRun(ctx restate.WorkflowContext, in SellerDealInput) (DealState, error) {
	offerID := restate.Key(ctx)
	dealID := "sell-" + offerID

	// Step 1: Prepare BRC-369 keyed offer + persist pending_sales (with reveal key) + snapshot ZeroConfMaxSats inside restate.Run.
	prep, err := restate.Run(ctx, func(rctx restate.RunContext) (sellerPrepResult, error) {
		now := time.Now().Unix()
		if e.mem != nil && e.mem.DB() != nil {
			_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
		}
		_, _, _, zeroConfMax, _, _, _ := e.policy.Snapshot()

		var offer *contracts.KeyedSaleOffer
		var revealKeyHex, chirpURL string
		var pb *contracts.ProofBundle
		if in.PreCreatedOffer != nil && in.RevealKeyHex != "" {
			offer = in.PreCreatedOffer
			revealKeyHex = in.RevealKeyHex
			chirpURL = in.ChirpURL
			pb = in.ProofBundle
		} else {
			var rk [32]byte
			var pErr error
			offer, rk, chirpURL, pErr = e.PrepareDirectSale(rctx, in.Shard, in.Seq, in.PriceSats, in.BuyerPub)
			if pErr != nil {
				e.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "seller",
					Status: "failed",
					Shard:  in.Shard,
					Error:  pErr.Error(),
				})
				// Deterministic policy / not-found error -> TerminalError (no ~70x retry poison!).
				return sellerPrepResult{}, restate.ToTerminalError(pErr)
			}
			revealKeyHex = hex.EncodeToString(rk[:])
			if pBundle, _, _ := e.mem.Prove(rctx, in.Shard, in.Seq); pBundle != nil {
				pb = pBundle
			}
		}

		// Persist durable pending_sales + deals row so revealKey survives seller restarts (R11)!
		if e.mem != nil && e.mem.DB() != nil {
			saleJSON, _ := json.Marshal(offer)
			var pbJSON []byte
			if pb != nil {
				pbJSON, _ = json.Marshal(pb)
			}
			_ = e.mem.DB().SavePendingSale(store.PendingSaleRecord{
				OfferID:         offerID,
				WantID:          in.Want.WantID,
				BuyerPub:        in.BuyerPub,
				Shard:           in.Shard,
				Seq:             in.Seq,
				SaleOfferJSON:   saleJSON,
				RevealKeyHex:    revealKeyHex,
				ChirpURL:        chirpURL,
				ProofBundleJSON: pbJSON,
				ExpiresAt:       int64(offer.ExpiresAt),
			})
		}
		wantJSON, _ := json.Marshal(in.Want)
		e.persistDealRecord(store.DealRecord{
			DealID:          dealID,
			Role:            "seller",
			Status:          "offered",
			Shard:           in.Shard,
			WantJSON:        wantJSON,
			ClaimKeyHex:     revealKeyHex,
			ChirpURL:        chirpURL,
			CounterpartyPub: in.BuyerPub,
			PriceSats:       in.PriceSats,
		})
		return sellerPrepResult{
			Offer:       *offer,
			ChirpURL:    chirpURL,
			ZeroConfMax: zeroConfMax,
			NowUnix:     now,
		}, nil
	}, restate.WithName("seller_prepare_sale"))
	if err != nil {
		return DealState{DealID: offerID, Role: "seller", Status: "failed", Error: err.Error()}, err
	}

	st := DealState{
		DealID:    offerID,
		Role:      "seller",
		Status:    "offered",
		Want:      in.Want,
		SaleOffer: &prep.Offer,
		ChirpURL:  prep.ChirpURL,
		Shard:     in.Shard,
		UpdatedAt: prep.NowUnix,
	}
	restate.Set(ctx, "state", st)

	// Step 2: Bounded wait for payment promise (eliminates unbounded workflow leak — R13).
	ttl := in.OfferTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	payFut := restate.Promise[PaymentSignal](ctx, "payment")
	expFut := restate.After(ctx, ttl)
	winner, waitErr := restate.WaitFirst(ctx, payFut, expFut)
	if waitErr != nil {
		return st, waitErr
	}
	if winner == expFut {
		_ = restate.Promise[PaymentSignal](ctx, "payment").Reject(restate.TerminalErrorf("offer %s expired", offerID))
		expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			now := time.Now().Unix()
			e.persistDealRecord(store.DealRecord{
				DealID: dealID,
				Role:   "seller",
				Status: "expired",
				Error:  "offer expired waiting for buyer payment",
			})
			return now, nil
		}, restate.WithName("seller_offer_expired"))
		st.Status = "expired"
		st.Error = "offer expired waiting for buyer payment"
		st.UpdatedAt = expNow
		restate.Set(ctx, "state", st)
		return st, nil
	}
	paySig, pErr := payFut.Result()
	if pErr != nil {
		return st, pErr
	}

	// Step 3: If unconfirmed payment exceeds journaled ZeroConfMax, wait on confirmation promise (R13 — no dead end!).
	if !paySig.Confirmed && prep.ZeroConfMax > 0 && paySig.AmountSats > prep.ZeroConfMax {
		pendNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			now := time.Now().Unix()
			e.persistDealRecord(store.DealRecord{
				DealID:      dealID,
				Role:        "seller",
				Status:      "payment_pending",
				PaymentTxID: paySig.TxID,
			})
			return now, nil
		}, restate.WithName("seller_payment_pending"))
		st.Status = "payment_pending"
		st.PaymentTxID = paySig.TxID
		st.UpdatedAt = pendNow
		restate.Set(ctx, "state", st)

		confTimeout := in.ConfirmTimeout
		if confTimeout <= 0 {
			confTimeout = 2 * time.Hour
		}
		confFut := restate.Promise[PaymentSignal](ctx, "payment_confirmed")
		confTimer := restate.After(ctx, confTimeout)
		cWin, cwErr := restate.WaitFirst(ctx, confFut, confTimer)
		if cwErr != nil {
			return st, cwErr
		}
		if cWin == confTimer {
			_ = restate.Promise[PaymentSignal](ctx, "payment_confirmed").Reject(restate.TerminalErrorf("payment confirmation timed out for %s", offerID))
			expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
				now := time.Now().Unix()
				e.persistDealRecord(store.DealRecord{
					DealID:      dealID,
					Role:        "seller",
					Status:      "expired",
					PaymentTxID: paySig.TxID,
					Error:       "timed out waiting for payment confirmation",
				})
				return now, nil
			}, restate.WithName("seller_confirm_expired"))
			st.Status = "expired"
			st.Error = "timed out waiting for payment confirmation"
			st.UpdatedAt = expNow
			restate.Set(ctx, "state", st)
			return st, nil
		}
		confSig, cfErr := confFut.Result()
		if cfErr != nil {
			return st, cfErr
		}
		paySig = confSig
	}

	// Step 4: Settle seller deal durably in SQLite inside restate.Run.
	settleNow, sErr := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
		now := time.Now().Unix()
		if e.mem != nil && e.mem.DB() != nil {
			_ = e.mem.DB().MarkPendingSaleSettled(offerID)
		}
		e.persistDealRecord(store.DealRecord{
			DealID:      dealID,
			Role:        "seller",
			Status:      "settled",
			PaymentTxID: paySig.TxID,
			Error:       "",
		})
		return now, nil
	}, restate.WithName("seller_settle"))
	if sErr != nil {
		return st, sErr
	}
	st.PaymentTxID = paySig.TxID
	st.Status = "settled"
	st.UpdatedAt = settleNow
	restate.Set(ctx, "state", st)
	return st, nil
}

func (e *Engine) restateSellerDeliverPayment(ctx restate.WorkflowSharedContext, sig PaymentSignal) (bool, error) {
	if err := restate.Promise[PaymentSignal](ctx, "payment").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) restateSellerConfirmPayment(ctx restate.WorkflowSharedContext, sig PaymentSignal) (bool, error) {
	sig.Confirmed = true
	if err := restate.Promise[PaymentSignal](ctx, "payment_confirmed").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) restateDealStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (DealState, error) {
	return restate.Get[DealState](ctx, "state")
}

// restateCustodyReserve serializes ALL spend reservations on key "global" with a sub-ms SQLite transaction
// inside restate.Run (zero network I/O, idempotent on (OperationID, Attempt) — eliminates rt/key-collision & rt/double-effect).
func (e *Engine) restateCustodyReserve(ctx restate.ObjectContext, in CustodyReserveInput) (CustodyReserveResult, error) {
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	res, err := restate.Run(ctx, func(_ restate.RunContext) (CustodyReserveResult, error) {
		if e.mem != nil && e.mem.DB() != nil {
			_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
		}
		kill, maxDeal, maxDay, _, _, _, _ := e.policy.Snapshot()
		if kill {
			return CustodyReserveResult{Approved: false, Reason: "market: operator kill switch is engaged"}, nil
		}
		if !in.BypassPolicy {
			if err := e.policy.CanSpendForDomains(in.SellerPub, in.Domains, 0); err != nil {
				return CustodyReserveResult{Approved: false, Reason: err.Error()}, nil
			}
		} else {
			// Even on operator override, explicitly blocked sellers stay blocked!
			if err := e.policy.CanSpend(in.SellerPub, 0); err != nil && strings.Contains(err.Error(), "blocked") {
				return CustodyReserveResult{Approved: false, Reason: err.Error()}, nil
			}
			maxDeal = 0
			maxDay = 0
		}
		bal := e.mem.Chain().BalanceSats()
		if bal < in.AmountSats {
			return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: "insufficient balance"}, nil
		}
		already, rErr := e.mem.DB().ReserveSpendAtomic(in.OperationID, attempt, in.AmountSats, maxDeal, maxDay)
		if rErr != nil {
			return CustodyReserveResult{Approved: false, BalanceSats: bal, Reason: rErr.Error()}, nil
		}
		return CustodyReserveResult{
			Approved:        true,
			AlreadyReserved: already,
			BalanceSats:     bal - in.AmountSats,
		}, nil
	}, restate.WithName("custody_reserve_atomic"))
	if err != nil {
		return CustodyReserveResult{}, err
	}
	if res.Approved {
		restate.Set(ctx, "last_op", fmt.Sprintf("%s#%d", in.OperationID, attempt))
	}
	return res, nil
}

func (e *Engine) restateCustodyRelease(ctx restate.ObjectContext, in CustodyReleaseInput) (bool, error) {
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	return restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		if e.mem == nil || e.mem.DB() == nil {
			return true, nil
		}
		if err := e.mem.DB().ReleaseSpendReservation(in.OperationID, attempt); err != nil {
			return false, err
		}
		return true, nil
	}, restate.WithName("custody_release_atomic"))
}

func (e *Engine) restateCustodyBalance(ctx restate.ObjectSharedContext, _ restate.Void) (uint64, error) {
	return restate.Run(ctx, func(_ restate.RunContext) (uint64, error) {
		return e.mem.Chain().BalanceSats(), nil
	}, restate.WithName("custody_balance"))
}

func txStatusRank(status string) int {
	switch status {
	case "prepared":
		return 1
	case "submission_unknown":
		return 2
	case "broadcast":
		return 3
	case "mempool":
		return 4
	case "mined":
		return 5
	default:
		return 0
	}
}

func shouldAdvanceTxStatus(cur, next string) bool {
	if next == "" {
		return false
	}
	if cur == "" {
		return true
	}
	// Revival edge: a previously failed/aborted tx that actually mined on-chain ALWAYS advances to mined!
	if next == "mined" {
		return true
	}
	// Never regress a mined tx!
	if cur == "mined" {
		return false
	}
	// Terminal failure/abort can overwrite any pre-mined status.
	if next == "failed" || next == "aborted" {
		return cur != "failed" && cur != "aborted"
	}
	if cur == "failed" || cur == "aborted" {
		return false
	}
	return txStatusRank(next) > txStatusRank(cur)
}

func txEventExists(events []TxEventInput, ev TxEventInput) bool {
	for _, prev := range events {
		if prev.Kind == ev.Kind && prev.Shard == ev.Shard && prev.BlockHeight == ev.BlockHeight && prev.Detail == ev.Detail {
			return true
		}
	}
	return false
}

// restateTxEmit enforces content deduplication, monotonic lifecycle progression, revival-on-mine,
// and deterministic timestamping (never calls time.Now() outside restate.Run!).
func (e *Engine) restateTxEmit(ctx restate.ObjectContext, ev TxEventInput) (TxState, error) {
	txid := restate.Key(ctx)
	st, err := restate.Get[TxState](ctx, "tx")
	if err != nil {
		return TxState{}, err
	}
	st.TxID = txid
	if txEventExists(st.Events, ev) {
		return st, nil
	}
	obsAt := ev.ObservedAt
	if obsAt == 0 {
		obsAt, err = restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			return time.Now().Unix(), nil
		}, restate.WithName("tx_emit_timestamp"))
		if err != nil {
			return TxState{}, err
		}
		ev.ObservedAt = obsAt
	}
	if shouldAdvanceTxStatus(st.Status, ev.Kind) {
		st.Status = ev.Kind
	}
	if ev.BlockHeight > st.BlockHeight {
		st.BlockHeight = ev.BlockHeight
	}
	st.Events = append(st.Events, ev)
	// Bound event list length to prevent unbounded Restate KV growth on noisy retries.
	if len(st.Events) > 128 {
		st.Events = st.Events[len(st.Events)-128:]
	}
	st.UpdatedAt = obsAt
	restate.Set(ctx, "tx", st)
	return st, nil
}

func (e *Engine) restateTxState(ctx restate.ObjectSharedContext, _ restate.Void) (TxState, error) {
	return restate.Get[TxState](ctx, "tx")
}

type cadenceStepResult struct {
	NowUnix        int64  `json:"nowUnix"`
	TxID           string `json:"txid,omitempty"`
	HasMore        bool   `json:"hasMore"`
	NextSlot       int64  `json:"nextSlot,omitempty"`
	DelaySec       int64  `json:"delaySec,omitempty"`
	DeferredReason string `json:"deferredReason,omitempty"`
}

// restateAnchorCadenceTrigger checks SQLite unanchored fragments inside restate.Run, anchors at most
// one batch, emits a content-idempotent Tx/emit event, and self-arms ONLY when work remains using a
// quantized slot idempotency key (eliminates rt/arm-multiplication & rt/poll-as-mechanism!).
func (e *Engine) restateAnchorCadenceTrigger(ctx restate.ObjectContext, in AnchorCadenceInput) (AnchorCadenceState, error) {
	shard := restate.Key(ctx)
	st, err := restate.Get[AnchorCadenceState](ctx, "cadence")
	if err != nil {
		return AnchorCadenceState{}, err
	}
	st.Shard = shard

	step, runErr := restate.Run(ctx, func(rctx restate.RunContext) (cadenceStepResult, error) {
		now := time.Now().Unix()
		unanchored, uErr := e.mem.DB().ListUnanchored(shard)
		if uErr != nil {
			return cadenceStepResult{}, uErr
		}
		if len(unanchored) == 0 {
			return cadenceStepResult{NowUnix: now, HasMore: false}, nil
		}
		a, aErr := e.mem.AnchorShard(rctx, shard)
		if aErr != nil {
			msg := aErr.Error()
			// Classify deterministic / wallet-empty errors so we don't burn retries on a dry wallet;
			// transient broadcast/network errors bubble out to Restate's 5-attempt PauseOnMaxAttempts policy.
			if strings.Contains(msg, "insufficient") || strings.Contains(msg, "spendguard") || strings.Contains(msg, "no spendable") {
				out := cadenceStepResult{NowUnix: now, HasMore: true, DeferredReason: msg}
				if in.IntervalSeconds > 0 {
					out.NextSlot = (now/in.IntervalSeconds + 1) * in.IntervalSeconds
					out.DelaySec = out.NextSlot - now
					if out.DelaySec < 1 {
						out.DelaySec = in.IntervalSeconds
					}
				}
				return out, nil
			}
			return cadenceStepResult{}, aErr
		}
		txid := ""
		if a != nil {
			txid = a.TxID
		}
		rem, _ := e.mem.DB().ListUnanchored(shard)
		out := cadenceStepResult{
			NowUnix: now,
			TxID:    txid,
			HasMore: len(rem) > 0,
		}
		if in.IntervalSeconds > 0 && out.HasMore {
			out.NextSlot = (now/in.IntervalSeconds + 1) * in.IntervalSeconds
			out.DelaySec = out.NextSlot - now
			if out.DelaySec < 1 {
				out.DelaySec = in.IntervalSeconds
			}
		}
		return out, nil
	}, restate.WithName("anchor_cadence_step"))
	if runErr != nil {
		return st, runErr
	}

	if step.TxID != "" {
		st.LastTxID = step.TxID
		st.AnchorRuns++
		restate.ObjectSend(ctx, "Tx", step.TxID, "emit").Send(
			TxEventInput{Kind: "broadcast", Shard: shard, ObservedAt: step.NowUnix},
			restate.WithIdempotencyKey("tx:"+step.TxID+":broadcast:0"),
		)
	}
	st.LastRunAt = step.NowUnix
	st.NextSlot = step.NextSlot
	restate.Set(ctx, "cadence", st)

	if in.IntervalSeconds > 0 && step.HasMore && step.NextSlot > 0 {
		restate.ObjectSend(ctx, "AnchorCadence", shard, "trigger").Send(
			in,
			restate.WithDelay(time.Duration(step.DelaySec)*time.Second),
			restate.WithIdempotencyKey(fmt.Sprintf("anchor:%s:%d", shard, step.NextSlot)),
		)
	}
	return st, nil
}

func (e *Engine) restateAnchorCadenceStatus(ctx restate.ObjectSharedContext, _ restate.Void) (AnchorCadenceState, error) {
	return restate.Get[AnchorCadenceState](ctx, "cadence")
}

// --- NektarRuntime authenticated ingress gateway to private Tx & AnchorCadence objects ---

func txEventIdemKey(txid string, ev TxEventInput) string {
	h := sha256.Sum256([]byte(ev.Shard + "|" + ev.Detail))
	return fmt.Sprintf("tx:%s:%s:%d:%x", txid, ev.Kind, ev.BlockHeight, h[:6])
}

func (e *Engine) restateRuntimeEmitTx(ctx restate.Context, in RuntimeTxEmitInput) (bool, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return false, err
	}
	if strings.TrimSpace(in.TxID) == "" || strings.TrimSpace(in.Event.Kind) == "" {
		return false, restate.TerminalErrorf("txid and event.kind are required")
	}
	restate.ObjectSend(ctx, "Tx", in.TxID, "emit").Send(
		in.Event,
		restate.WithIdempotencyKey(txEventIdemKey(in.TxID, in.Event)),
	)
	return true, nil
}

func (e *Engine) restateRuntimeTxState(ctx restate.Context, txid string) (TxState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return TxState{}, err
	}
	return restate.Object[TxState](ctx, "Tx", txid, "state").Request(restate.Void{})
}

func (e *Engine) restateRuntimeTriggerAnchor(ctx restate.Context, in RuntimeAnchorTriggerInput) (bool, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return false, err
	}
	if strings.TrimSpace(in.Shard) == "" {
		return false, restate.TerminalErrorf("shard is required")
	}
	var opts []restate.SendOption
	if in.Slot > 0 {
		opts = append(opts, restate.WithIdempotencyKey(fmt.Sprintf("anchor:%s:%d", in.Shard, in.Slot)))
	}
	restate.ObjectSend(ctx, "AnchorCadence", in.Shard, "trigger").Send(
		AnchorCadenceInput{IntervalSeconds: in.IntervalSeconds},
		opts...,
	)
	return true, nil
}

func (e *Engine) restateRuntimeAnchorStatus(ctx restate.Context, shard string) (AnchorCadenceState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return AnchorCadenceState{}, err
	}
	return restate.Object[AnchorCadenceState](ctx, "AnchorCadence", shard, "status").Request(restate.Void{})
}

// PrepareDirectSale creates a signed BRC-369 keyed DisclosureSale offer for (shard, seq),
// stores the encrypted ciphertext in CHIRP (BRC-167), and returns the public offer + private reveal key k.
func (e *Engine) PrepareDirectSale(ctx context.Context, shard string, seq int64, priceSats uint64, buyerPub string) (*contracts.KeyedSaleOffer, [32]byte, string, error) {
	// Never disclose high-clearance fragments accidentally: market sales default to Level 0 clearance + Cedar disclose check.
	sellerPrincipal := policy.Principal{Type: "Agent", ID: "market-seller", Level: 0}
	recalled, err := e.mem.Recall(ctx, memory.RecallInput{Shard: shard, Principal: sellerPrincipal})
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	var target *memory.RecalledFragment
	for i := range recalled {
		if recalled[i].Seq == seq {
			cp := recalled[i]
			target = &cp
			break
		}
	}
	if target == nil {
		return nil, [32]byte{}, "", fmt.Errorf("market: fragment %s#%d not found or unauthorized", shard, seq)
	}
	if dec, cErr := e.mem.AuthorizeAction(sellerPrincipal, "disclose", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", target.Shard, target.Seq),
		Level: int64(target.Level),
		Shard: target.Shard,
		Class: target.Class,
	}); cErr != nil || !dec.Allow {
		return nil, [32]byte{}, "", fmt.Errorf("market: cedar policy denied disclose of %s#%d", shard, seq)
	}
	expiresAt := uint64(time.Now().Add(time.Hour).Unix())
	offer, err := contracts.CreateKeyedSaleOffer(
		e.saleKey,
		[]byte(target.Text),
		priceSats,
		expiresAt+3600,
		2000,
		expiresAt,
		buyerPub,
		target.Leaf,
		true,
	)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	ctBytes, _ := hex.DecodeString(offer.CiphertextHex)
	chirpURL, _, err := bstack.StoreChirp(e.mem.Objects(), ctBytes)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	return offer, offer.ContentKey(), chirpURL, nil
}

// ExecuteAtomicKnowledgeTrade runs a complete end-to-end buyer <-> seller BRC-369 paid disclosure
// trade on the chain: seller seals fragment with BRC-369 keyed commitment & stores ciphertext in CHIRP,
// buyer verifies seller's signed DisclosureSale offer & pays seller on-chain, seller reveals content
// scalar k, buyer verifies both hWrapped & BRC-369 commitment, decrypts CHIRP ciphertext, and commits
// the purchased fact into its own anchored memory shard!
func ExecuteAtomicKnowledgeTrade(
	ctx context.Context,
	buyer *Engine,
	seller *Engine,
	sellerShard string,
	sellerSeq int64,
	buyerShard string,
	priceSats uint64,
) (*DealState, error) {
	if err := buyer.policy.CanSpend(seller.mem.Chain().IdentityPubHex(), priceSats); err != nil {
		return nil, err
	}
	buyerPub := buyer.mem.Chain().IdentityPubHex()

	// 1. Seller prepares signed BRC-369 keyed sale offer + stores ciphertext in CHIRP.
	recalledSeller, _ := seller.mem.Recall(ctx, memory.RecallInput{Shard: sellerShard})
	var sellerLeaf string
	for _, rf := range recalledSeller {
		if rf.Seq == sellerSeq {
			sellerLeaf = rf.Leaf
			break
		}
	}
	saleOffer, revealKey, chirpURL, err := seller.PrepareDirectSale(ctx, sellerShard, sellerSeq, priceSats, buyerPub)
	if err != nil {
		return nil, err
	}

	// 2. Buyer cryptographically verifies the seller's signed DisclosureSale offer & covenant artifact hash!
	if err := contracts.VerifySaleOffer(saleOffer, buyerPub, sellerLeaf); err != nil {
		return nil, fmt.Errorf("market: buyer rejected invalid offer: %w", err)
	}

	// 3. Buyer pays seller on-chain (SpendGuard enforced!).
	sellerPayScript, err := seller.mem.Chain().Signer().FundScript()
	if err != nil {
		return nil, err
	}
	payTx, fee, spend, err := buyer.mem.Chain().BuildFundedTx(ctx, []*transaction.TransactionOutput{
		{Satoshis: priceSats, LockingScript: sellerPayScript},
	})
	if err != nil {
		return nil, fmt.Errorf("market: buyer fund payment: %w", err)
	}
	payTxID, err := buyer.mem.Chain().Broadcast(ctx, payTx, fee, spend)
	if err != nil {
		return nil, fmt.Errorf("market: buyer broadcast payment: %w", err)
	}
	if err := buyer.mem.Chain().AdoptChange(payTx); err != nil {
		return nil, err
	}
	buyer.policy.RecordSpend(priceSats)

	// 4. Buyer fetches ciphertext from seller's CHIRP store (verified by BRC-167 closure),
	//    verifies revealed key k against DisclosureSale hWrapped + BRC-369 KeyCommitment, and decrypts plaintext!
	verifiedCT, err := bstack.ResolveChirp(seller.mem.Objects(), chirpURL)
	if err != nil {
		return nil, fmt.Errorf("market: resolve CHIRP ciphertext: %w", err)
	}
	if hex.EncodeToString(verifiedCT) != saleOffer.CiphertextHex {
		return nil, errors.New("market: CHIRP ciphertext mismatch vs sale offer")
	}
	plaintext, err := contracts.ClaimKeyedSale(saleOffer, revealKey[:])
	if err != nil {
		return nil, fmt.Errorf("market: claim BRC-369 sale: %w", err)
	}

	// 5. Buyer commits the purchased, verified knowledge into its own memory shard (tainted: true)!
	if _, err := buyer.mem.Remember(ctx, memory.RememberInput{
		Shard:      buyerShard,
		Text:       string(plaintext),
		Source:     "market:" + seller.mem.Chain().IdentityPubHex()[:12],
		Tainted:    true,
		AutoAnchor: true,
	}); err != nil {
		return nil, fmt.Errorf("market: store purchased fact: %w", err)
	}

	dealID := "deal-" + payTxID[:12]
	deal := DealState{
		DealID:       dealID,
		Role:         "buyer",
		Status:       "settled",
		ChirpURL:     chirpURL,
		SaleOffer:    saleOffer,
		PaymentTxID:  payTxID,
		Shard:        buyerShard,
		DeliveredTxt: string(plaintext),
		UpdatedAt:    time.Now().Unix(),
	}
	buyer.persistDealRecord(store.DealRecord{
		DealID:        dealID,
		Role:          "buyer",
		Status:        "settled",
		Shard:         buyerShard,
		PaymentTxID:   payTxID,
		ChirpURL:      chirpURL,
		DeliveredText: string(plaintext),
		PriceSats:     priceSats,
	})
	seller.persistDealRecord(store.DealRecord{
		DealID:        "sell-" + dealID,
		Role:          "seller",
		Status:        "settled",
		Shard:         sellerShard,
		PaymentTxID:   payTxID,
		ChirpURL:      chirpURL,
		DeliveredText: string(plaintext),
		PriceSats:     priceSats,
	})
	return &deal, nil
}
