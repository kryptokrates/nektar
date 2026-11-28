package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/core"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/deals"
)

// WorkflowInvocation summarizes a live workflow execution persisted in workflows.sqlite.
type WorkflowInvocation = deals.InvocationInfo

// RestateInvocation aliases WorkflowInvocation for JSON/UI backward compatibility.
type RestateInvocation = WorkflowInvocation

// WorkflowClient drives Nektar's embedded pure-Go go-workflows durable execution runtime.
type WorkflowClient struct {
	eng *Engine
	rt  *deals.Runtime
}

// RestateClient aliases WorkflowClient during clean cutover so callers transition with zero churn.
type RestateClient = WorkflowClient

// NewWorkflowClient constructs a WorkflowClient bound to Engine and an embedded deals.Runtime.
func NewWorkflowClient(eng *Engine, rt *deals.Runtime) *WorkflowClient {
	return &WorkflowClient{
		eng: eng,
		rt:  rt,
	}
}

// AttachTestWorkflows starts an embedded in-memory go-workflows SQLite runtime on broker's Engine
// and returns a cleanup function that drains workers and closes the in-memory SQLite DB.
func AttachTestWorkflows(broker *Broker) func() {
	if broker == nil || broker.Engine() == nil {
		return func() {}
	}
	wc, err := broker.Engine().EnsureWorkflows()
	if err != nil {
		return func() {}
	}
	broker.SetWorkflowClient(wc)
	return func() {
		_ = broker.Engine().CloseWorkflows()
	}
}

// AttachTestRestate aliases AttachTestWorkflows for existing integration tests.
func AttachTestRestate(broker *Broker) func() {
	return AttachTestWorkflows(broker)
}

func (c *WorkflowClient) IsRunning() bool {
	return c != nil && c.rt != nil && c.rt.IsRunning()
}

func (c *WorkflowClient) HealthCheck(_ context.Context) error {
	if !c.IsRunning() {
		return errors.New("workflows: embedded go-workflows runtime is not running")
	}
	return nil
}

// StartBuyerDealAsync submits a durable BuyerDealWorkflow keyed by wantID.
func (c *WorkflowClient) StartBuyerDealAsync(ctx context.Context, wantID string, in BuyerDealInput) (string, error) {
	if !c.IsRunning() {
		return "", errors.New("workflows: runtime not running")
	}
	if in.Want.WantID == "" {
		in.Want.WantID = wantID
	}
	inst, err := c.rt.CreateOrAttach(ctx, wantID, BuyerDealWorkflow, in)
	if err != nil {
		return "", fmt.Errorf("workflows: start BuyerDeal %s: %w", wantID, err)
	}
	return inst.ExecutionID, nil
}

// AttachBuyerDeal waits for BuyerDealWorkflow (wantID) to complete and returns its final DealState.
func (c *WorkflowClient) AttachBuyerDeal(ctx context.Context, wantID string) (*DealState, error) {
	if !c.IsRunning() {
		return nil, errors.New("workflows: runtime not running")
	}
	inst, err := c.rt.CreateOrAttach(ctx, wantID, BuyerDealWorkflow, BuyerDealInput{})
	if err != nil {
		return nil, err
	}
	timeout := 10 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			timeout = rem
		}
	}
	st, err := client.GetWorkflowResult[DealState](ctx, c.rt.Client(), inst, timeout)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// BuyerDealStatus queries the authoritative SQLite DealState for wantID.
func (c *WorkflowClient) BuyerDealStatus(_ context.Context, wantID string) (*DealState, error) {
	if c == nil || c.eng == nil || c.eng.mem == nil || c.eng.mem.DB() == nil {
		return nil, errors.New("workflows: database unavailable")
	}
	rec, err := c.eng.mem.DB().GetDeal(wantID)
	if err != nil {
		return nil, err
	}
	st := dealStateFromRecord(rec)
	return &st, nil
}

// DeliverBuyerKey delivers the seller's BRC-369 content scalar k to BuyerDealWorkflow.
// STATE-3 fix: if the BuyerDeal workflow already timed out ("expired") while a live on-chain payment
// exists in SQLite (`PaymentTxID != ""`), it NEVER rejects a valid late reveal or EscrowGuardian extraction —
// it executes buyerVerifyAndCommit idempotently so paid knowledge is always recovered!
func (c *WorkflowClient) DeliverBuyerKey(ctx context.Context, wantID string, sig ClaimSignal) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	if c.eng != nil && c.eng.mem != nil && c.eng.mem.DB() != nil {
		if rec, gErr := c.eng.mem.DB().GetDeal(wantID); gErr == nil {
			if rec.Status == "settled" {
				return true, nil
			}
			if rec.Status == "expired" || rec.Status == "failed" || rec.Status == "refunded" {
				if rec.PaymentTxID != "" && rec.Status != "refunded" {
					return c.eng.recoverLateBuyerClaim(ctx, rec, sig)
				}
				return false, fmt.Errorf("workflows: buyer deal %s already %s", wantID, rec.Status)
			}
		}
	}
	sigErr := c.rt.Signal(ctx, wantID, "claim", sig)
	if sigErr == nil {
		if c.eng != nil && c.eng.mem != nil && c.eng.mem.DB() != nil {
			if rec, gErr := c.eng.mem.DB().GetDeal(wantID); gErr == nil && rec.PaymentTxID != "" && rec.Status == "expired" {
				return c.eng.recoverLateBuyerClaim(ctx, rec, sig)
			}
		}
		return true, nil
	}
	if c.eng != nil && c.eng.mem != nil && c.eng.mem.DB() != nil {
		if rec, gErr := c.eng.mem.DB().GetDeal(wantID); gErr == nil {
			if rec.Status == "settled" {
				return true, nil
			}
			if rec.PaymentTxID != "" {
				return c.eng.recoverLateBuyerClaim(ctx, rec, sig)
			}
		}
	}
	return false, sigErr
}

const (
	sellerWorkflowPrefix = "seller_wf:"
	escrowWorkflowPrefix = "escrow_wf:"
)

func sellerWorkflowInstanceID(offerID string) string {
	return sellerWorkflowPrefix + strings.TrimPrefix(offerID, sellerWorkflowPrefix)
}

func sellerDealID(offerID string) string {
	clean := strings.TrimPrefix(offerID, sellerWorkflowPrefix)
	if strings.HasPrefix(clean, "sell-") {
		return clean
	}
	return "sell-" + clean
}

// StartSellerDealAsync submits a durable SellerDealWorkflow keyed by "seller_wf:"+offerID.
func (c *WorkflowClient) StartSellerDealAsync(ctx context.Context, offerID string, in SellerDealInput) (string, error) {
	if !c.IsRunning() {
		return "", errors.New("workflows: runtime not running")
	}
	instID := sellerWorkflowInstanceID(offerID)
	inst, err := c.rt.CreateOrAttach(ctx, instID, SellerDealWorkflow, in)
	if err != nil {
		return "", fmt.Errorf("workflows: start SellerDeal %s: %w", offerID, err)
	}
	return inst.ExecutionID, nil
}

// AttachSellerDeal waits for SellerDealWorkflow to finish and returns its DealState.
func (c *WorkflowClient) AttachSellerDeal(ctx context.Context, offerID string) (*DealState, error) {
	if !c.IsRunning() {
		return nil, errors.New("workflows: runtime not running")
	}
	instID := sellerWorkflowInstanceID(offerID)
	inst, err := c.rt.CreateOrAttach(ctx, instID, SellerDealWorkflow, SellerDealInput{})
	if err != nil {
		return nil, err
	}
	timeout := 10 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 {
			timeout = rem
		}
	}
	st, err := client.GetWorkflowResult[DealState](ctx, c.rt.Client(), inst, timeout)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// SellerDealStatus queries the authoritative SQLite DealState for offerID.
func (c *WorkflowClient) SellerDealStatus(_ context.Context, offerID string) (*DealState, error) {
	if c == nil || c.eng == nil || c.eng.mem == nil || c.eng.mem.DB() == nil {
		return nil, errors.New("workflows: database unavailable")
	}
	rec, err := c.eng.mem.DB().GetDeal(sellerDealID(offerID))
	if err != nil {
		return nil, err
	}
	st := dealStateFromRecord(rec)
	st.DealID = strings.TrimPrefix(offerID, sellerWorkflowPrefix)
	return &st, nil
}

// DeliverSellerPayment signals SellerDealWorkflow with a verified PaymentSignal.
func (c *WorkflowClient) DeliverSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	if sig.Confirmed && c.eng != nil && c.eng.mem != nil && c.eng.mem.Chain() != nil && c.eng.mem.Chain().Network() != "memory" {
		if sig.MerklePathHex == "" {
			sig.Confirmed = false
		} else if bh, err := c.eng.verifyMerklePathProof(ctx, sig.TxID, sig.MerklePathHex, true); err != nil {
			sig.Confirmed = false
		} else {
			sig.BlockHeight = bh
		}
	}
	if err := c.rt.Signal(ctx, sellerWorkflowInstanceID(offerID), "payment", sig); err != nil {
		return false, err
	}
	return true, nil
}

func (c *WorkflowClient) DeliverSellerPaymentAsync(ctx context.Context, offerID string, sig PaymentSignal) (string, error) {
	ok, err := c.DeliverSellerPayment(ctx, offerID, sig)
	if err != nil || !ok {
		return "", err
	}
	return sellerWorkflowInstanceID(offerID), nil
}

// ConfirmSellerPayment signals a waiting SellerDealWorkflow that the buyer payment reached settleDepth.
func (c *WorkflowClient) ConfirmSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	if c.eng != nil && c.eng.mem != nil && c.eng.mem.Chain() != nil && c.eng.mem.Chain().Network() != "memory" {
		bh, err := c.eng.verifyMerklePathProof(ctx, sig.TxID, sig.MerklePathHex, true)
		if err != nil {
			return false, err
		}
		sig.BlockHeight = bh
	}
	sig.Confirmed = true
	if err := c.rt.Signal(ctx, sellerWorkflowInstanceID(offerID), "payment_confirmed", sig); err != nil {
		return false, err
	}
	return true, nil
}

func (c *WorkflowClient) ConfirmSellerPaymentAsync(ctx context.Context, offerID string, sig PaymentSignal) (string, error) {
	ok, err := c.ConfirmSellerPayment(ctx, offerID, sig)
	if err != nil || !ok {
		return "", err
	}
	return sellerWorkflowInstanceID(offerID), nil
}

// StartStandingOrderAsync launches a durable 0%-CPU StandingOrderWorkflow keyed by orderID.
func (c *WorkflowClient) StartStandingOrderAsync(ctx context.Context, orderID string, in StandingOrderInput) (string, error) {
	if !c.IsRunning() {
		return "", errors.New("workflows: runtime not running")
	}
	in.OrderID = orderID
	inst, err := c.rt.CreateOrAttach(ctx, orderID, StandingOrderWorkflow, in)
	if err != nil {
		return "", fmt.Errorf("workflows: start StandingOrder %s: %w", orderID, err)
	}
	return inst.ExecutionID, nil
}

// StandingOrderStatus reads the latest persisted StandingOrderState for orderID.
func (c *WorkflowClient) StandingOrderStatus(_ context.Context, orderID string) (*StandingOrderState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.GetStandingOrderState(orderID)
}

// SignalStandingOrderOffer delivers an OfferAd directly to StandingOrderWorkflow's native signal channel (RT-2 fix).
func (c *WorkflowClient) SignalStandingOrderOffer(ctx context.Context, orderID string, off bstack.OfferAd) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	err := c.rt.Signal(ctx, orderID, "events", standingOrderEvent{
		Kind:  "offer",
		Offer: &off,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *WorkflowClient) SignalStandingOrderOfferAsync(ctx context.Context, orderID string, off bstack.OfferAd) (string, error) {
	ok, err := c.SignalStandingOrderOffer(ctx, orderID, off)
	if err != nil || !ok {
		return "", err
	}
	return orderID, nil
}

// SteerStandingOrder live-updates maxSats, pause state, or TTL on a running StandingOrderWorkflow.
func (c *WorkflowClient) SteerStandingOrder(ctx context.Context, orderID string, steer SteerStandingOrderInput) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	err := c.rt.Signal(ctx, orderID, "events", standingOrderEvent{
		Kind:  "steer",
		Steer: &steer,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// CancelStandingOrder terminates a watching/paused StandingOrderWorkflow cleanly.
func (c *WorkflowClient) CancelStandingOrder(ctx context.Context, orderID, reason string) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	err := c.rt.Signal(ctx, orderID, "events", standingOrderEvent{
		Kind:   "cancel",
		Reason: reason,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func escrowWorkflowInstanceID(escrowTxID string) string {
	return escrowWorkflowPrefix + strings.TrimPrefix(escrowTxID, escrowWorkflowPrefix)
}

// StartEscrowGuardianAsync arms a durable EscrowGuardianWorkflow keyed by "escrow_wf:"+escrowTxID.
func (c *WorkflowClient) StartEscrowGuardianAsync(ctx context.Context, escrowTxID string, in EscrowGuardianInput) (string, error) {
	if !c.IsRunning() {
		return "", errors.New("workflows: runtime not running")
	}
	cleanTxID := strings.TrimPrefix(escrowTxID, escrowWorkflowPrefix)
	in.EscrowTxID = cleanTxID
	instID := escrowWorkflowInstanceID(cleanTxID)
	inst, err := c.rt.CreateOrAttach(ctx, instID, EscrowGuardianWorkflow, in)
	if err != nil {
		return "", fmt.Errorf("workflows: start EscrowGuardian %s: %w", escrowTxID, err)
	}
	return inst.ExecutionID, nil
}

// EscrowGuardianStatus reads the latest persisted EscrowGuardianState for escrowTxID.
func (c *WorkflowClient) EscrowGuardianStatus(_ context.Context, escrowTxID string) (*EscrowGuardianState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.GetEscrowGuardianState(strings.TrimPrefix(escrowTxID, escrowWorkflowPrefix))
}

// MarkEscrowGuardianClaimed disarms an armed EscrowGuardianWorkflow after successful settlement.
func (c *WorkflowClient) MarkEscrowGuardianClaimed(ctx context.Context, escrowTxID string) (bool, error) {
	if !c.IsRunning() {
		return false, errors.New("workflows: runtime not running")
	}
	if err := c.rt.Signal(ctx, escrowWorkflowInstanceID(escrowTxID), "claimed", true); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveApproval signals a suspended BuyerDealWorkflow approval channel encoded in signalToken ("wf:<instanceID>:<signalName>").
func (c *WorkflowClient) ResolveApproval(ctx context.Context, signalToken string, dec ApprovalDecision) error {
	if !c.IsRunning() {
		return errors.New("workflows: runtime not running")
	}
	trimmed := strings.TrimPrefix(strings.TrimSpace(signalToken), "wf:")
	idx := strings.IndexByte(trimmed, ':')
	if idx <= 0 || idx+1 >= len(trimmed) {
		return fmt.Errorf("workflows: invalid approval signal token %q", signalToken)
	}
	instanceID := trimmed[:idx]
	sigName := trimmed[idx+1:]
	return c.rt.Signal(ctx, instanceID, sigName, dec)
}

// ResolveAwakeable preserves signature compatibility for ResolveApproval.
func (c *WorkflowClient) ResolveAwakeable(ctx context.Context, awakeableID string, payload any) error {
	dec, ok := payload.(ApprovalDecision)
	if !ok {
		return fmt.Errorf("workflows: unexpected approval payload type %T", payload)
	}
	return c.ResolveApproval(ctx, awakeableID, dec)
}

// QueryInvocations returns real workflow executions persisted in workflows.sqlite (`instances` + `history`).
func (c *WorkflowClient) QueryInvocations(ctx context.Context, limit int) ([]WorkflowInvocation, error) {
	if !c.IsRunning() {
		return nil, errors.New("workflows: runtime not running")
	}
	return c.rt.QueryInvocations(ctx, limit)
}

// KillInvocation cancels a running workflow instance by instanceID or executionID.
func (c *WorkflowClient) KillInvocation(ctx context.Context, id string) error {
	if !c.IsRunning() {
		return errors.New("workflows: runtime not running")
	}
	return c.rt.Cancel(ctx, strings.TrimSpace(id))
}

// ResumeInvocation is a no-op compatibility helper (go-workflows SQLite sagas resume automatically).
func (c *WorkflowClient) ResumeInvocation(ctx context.Context, id string) error {
	if !c.IsRunning() {
		return errors.New("workflows: runtime not running")
	}
	_ = core.WorkflowInstanceStateActive
	return nil
}

// RestartInvocationAsNew returns an explicit unsupported error for one-shot workflow instances.
func (c *WorkflowClient) RestartInvocationAsNew(_ context.Context, _ string) (string, error) {
	return "", errors.New("restarting workflows is not supported; start a new workflow instance instead")
}

// --- In-Process Coordinators (replacing external Restate Virtual Objects with zero HTTP overhead) ---

func (c *WorkflowClient) EmitTxEvent(ctx context.Context, txid string, ev TxEventInput) (string, error) {
	if c == nil || c.eng == nil {
		return "", errors.New("workflows: engine unavailable")
	}
	st, err := c.eng.EmitTxEventLocal(ctx, txid, ev)
	if err != nil {
		return "", err
	}
	return st.TxID, nil
}

func (c *WorkflowClient) GetTxState(_ context.Context, txid string) (*TxState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.GetTxStateLocal(txid)
}

func (c *WorkflowClient) TriggerAnchorCadence(ctx context.Context, shard string, intervalSec int64) (string, error) {
	if c == nil || c.eng == nil {
		return "", errors.New("workflows: engine unavailable")
	}
	st, err := c.eng.TriggerAnchorCadenceLocal(ctx, shard, intervalSec)
	if err != nil {
		return "", err
	}
	return st.Shard, nil
}

func (c *WorkflowClient) GetAnchorCadenceStatus(_ context.Context, shard string) (*AnchorCadenceState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.GetAnchorCadenceStatusLocal(shard)
}

func (c *WorkflowClient) EpistemicTransition(ctx context.Context, shard string, in EpistemicTransitionInput) (*EpistemicTransitionResult, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.ExecuteEpistemicTransition(ctx, shard, in)
}

func (c *WorkflowClient) EpistemicAnchor(ctx context.Context, shard string) (*EpistemicShardState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.ExecuteEpistemicAnchor(ctx, shard)
}

func (c *WorkflowClient) EpistemicState(_ context.Context, shard string) (*EpistemicShardState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	st := c.eng.loadEpistemicStateFromDB(shard)
	return &st, nil
}

func (c *WorkflowClient) TriggerSleepCycle(ctx context.Context, in SleepCycleInput) (*SleepCycleState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.TriggerSleepCycleLocal(ctx, in)
}

func (c *WorkflowClient) TriggerSleepCycleAsync(ctx context.Context, in SleepCycleInput) (string, error) {
	st, err := c.TriggerSleepCycle(ctx, in)
	if err != nil {
		return "", err
	}
	return st.Status, nil
}

func (c *WorkflowClient) SleepCycleStatus(_ context.Context) (*SleepCycleState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	st := LoadSleepCycleState(c.eng.mem)
	return &st, nil
}

func (c *WorkflowClient) RunMicroEarner(ctx context.Context, in MicroEarnerInput) (*MicroEarnerState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	return c.eng.RunMicroEarnerLocal(ctx, in)
}

func (c *WorkflowClient) ToggleMicroEarner(_ context.Context, in MicroEarnerToggleInput) (*MicroEarnerState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	st := SetMicroEarnerEnabled(c.eng.mem, in.Enabled, in.TargetFloatSats)
	return &st, nil
}

func (c *WorkflowClient) MicroEarnerStatus(_ context.Context) (*MicroEarnerState, error) {
	if c == nil || c.eng == nil {
		return nil, errors.New("workflows: engine unavailable")
	}
	st := LoadMicroEarnerState(c.eng.mem)
	return &st, nil
}
