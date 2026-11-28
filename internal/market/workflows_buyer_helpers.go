package market

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cschleiden/go-workflows/workflow"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

type buyerPolicyEval struct {
	NowUnix         int64  `json:"nowUnix"`
	KillSwitch      bool   `json:"killSwitch"`
	NeedsApproval   bool   `json:"needsApproval"`
	ApprovalReason  string `json:"approvalReason,omitempty"`
	BypassPolicy    bool   `json:"bypassPolicy"`
	ApprovedCapSats uint64 `json:"approvedCapSats,omitempty"`
}

type buyerCollectResult struct {
	NowUnix        int64            `json:"nowUnix"`
	NoOffers       bool             `json:"noOffers"`
	PolicyRejected bool             `json:"policyRejected"`
	Error          string           `json:"error,omitempty"`
	Offers         []bstack.OfferAd `json:"offers,omitempty"`
	ActiveHosts    []string         `json:"activeHosts,omitempty"`
	FirstOfferMs   int64            `json:"firstOfferMs"`
}

type buyerRankResult struct {
	NowUnix          int64            `json:"nowUnix"`
	NoOffers         bool             `json:"noOffers"`
	PolicyRejected   bool             `json:"policyRejected"`
	NeedsEscalation  bool             `json:"needsEscalation,omitempty"`
	EscalationReason string           `json:"escalationReason,omitempty"`
	Error            string           `json:"error,omitempty"`
	Chosen           *bstack.OfferAd  `json:"chosen,omitempty"`
	Candidates       []bstack.OfferAd `json:"candidates,omitempty"`
	MatchAudit       *MatchAudit      `json:"matchAudit,omitempty"`
	FirstOfferMs     int64            `json:"firstOfferMs"`
}

type buyerPayResult struct {
	NowUnix           int64                    `json:"nowUnix"`
	TxID              string                   `json:"txid,omitempty"`
	PayBEEFHex        string                   `json:"payBeefHex,omitempty"`
	PayBEEFRaw        []byte                   `json:"-"`
	OutputIndex       uint32                   `json:"outputIndex,omitempty"`
	DerivPrefix       string                   `json:"derivPrefix,omitempty"`
	DerivSuffix       string                   `json:"derivSuffix,omitempty"`
	SenderKey         string                   `json:"senderKey,omitempty"`
	EscrowTxID        string                   `json:"escrowTxid,omitempty"`
	EscrowRefundAfter uint64                   `json:"escrowRefundAfter,omitempty"`
	EscrowPayload     *EscrowObligationPayload `json:"escrowPayload,omitempty"`
	PreBroadcastErr   string                   `json:"preBroadcastErr,omitempty"`
}

type buyerDeliverResult struct {
	NowUnix              int64        `json:"nowUnix"`
	ImmediateClaim       *ClaimSignal `json:"immediateClaim,omitempty"`
	AwaitingConfirmation bool         `json:"awaitingConfirmation,omitempty"`
	RequiredDepth        int          `json:"requiredDepth,omitempty"`
	DeliveryErr          string       `json:"deliveryErr,omitempty"`
}

type buyerVerifyResult struct {
	NowUnix       int64        `json:"nowUnix"`
	DeliveredText string       `json:"deliveredText,omitempty"`
	TargetShard   string       `json:"targetShard,omitempty"`
	DealReceipt   *DealReceipt `json:"dealReceipt,omitempty"`
	FailedReason  string       `json:"failedReason,omitempty"`
}

// buyerLookupMultiHubParallelWF fans out per-hub offer lookups concurrently via ExecuteActivity.
func buyerLookupMultiHubParallelWF(ctx workflow.Context, hosts []string, dealID string, want bstack.WantAd) []bstack.OfferAd {
	var acts *workflowActivities
	futs := make([]workflow.Future[[]bstack.OfferAd], 0, len(hosts))
	for _, h := range hosts {
		futs = append(futs, workflow.ExecuteActivity[[]bstack.OfferAd](ctx, singleAttemptActivityOpts(), acts.LookupOffersSingleHost, h, dealID, want.Pub))
	}
	seen := make(map[string]bool)
	var out []bstack.OfferAd
	for _, fut := range futs {
		batch, err := fut.Get(ctx)
		if err != nil {
			continue
		}
		for _, off := range batch {
			if off.OfferID != "" && seen[off.OfferID] {
				continue
			}
			if off.OfferID != "" {
				seen[off.OfferID] = true
			}
			out = append(out, off)
		}
	}
	return out
}

func buyerAwaitApprovalSignal(
	ctx workflow.Context,
	dealID, kind string,
	want bstack.WantAd,
	chosen *bstack.OfferAd,
	reason string,
	maxExtraSats uint64,
	approvalTimeout time.Duration,
	st *DealState,
) (bool, error) {
	var acts *workflowActivities
	apprTimeout := approvalTimeout
	if apprTimeout <= 0 {
		apprTimeout = 15 * time.Minute
	}
	apprID := fmt.Sprintf("appr-%s-%s", dealID, kind)
	sigName := "approval:" + apprID
	sigToken := fmt.Sprintf("wf:%s:%s", dealID, sigName)
	sigCh := workflow.NewSignalChannel[ApprovalDecision](ctx, sigName)

	pNow, err := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.BuyerPersistApproval, dealID, apprID, sigToken, want, chosen, reason, maxExtraSats, int64(apprTimeout.Seconds())).Get(ctx)
	if err != nil {
		return false, err
	}

	st.Status = "approval_pending"
	st.ApprovalID = apprID
	st.AwakeableID = sigToken
	st.Error = reason + " [approvalId=" + apprID + "]"
	st.UpdatedAt = pNow

	tCtx, cancelTimer := workflow.WithCancel(ctx)
	timerFut := workflow.ScheduleTimer(tCtx, apprTimeout)
	var dec ApprovalDecision
	gotSignal := false
	workflow.Select(ctx,
		workflow.Receive(sigCh, func(_ workflow.Context, v ApprovalDecision, ok bool) {
			cancelTimer()
			if ok {
				dec = v
				gotSignal = true
			}
		}),
		workflow.Await(timerFut, func(_ workflow.Context, _ workflow.Future[any]) {
		}),
	)
	cancelTimer()

	if !gotSignal {
		expMsg := "approval timed out"
		if kind == "esc" {
			expMsg = "semantic match escalation timed out"
		}
		expNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
			DealID: dealID,
			Role:   "buyer",
			Status: "expired",
			Error:  expMsg,
		}).Get(ctx)
		st.Status = "expired"
		st.Error = expMsg
		st.UpdatedAt = expNow
		return false, nil
	}
	if !dec.Approved {
		denyMsg := "denied by operator"
		if kind == "esc" {
			denyMsg = "semantic match denied by operator"
		}
		if dec.Reason != "" {
			denyMsg = dec.Reason
		}
		denyNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
			DealID: dealID,
			Role:   "buyer",
			Status: "failed",
			Error:  denyMsg,
		}).Get(ctx)
		st.Status = "failed"
		st.Error = denyMsg
		st.UpdatedAt = denyNow
		return false, nil
	}
	_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.BuyerConsumeResolvedApproval, dealID, apprID, kind == "esc").Get(ctx)
	return true, nil
}

// --- Buyer Workflow Activity Methods ---

func (a *workflowActivities) PersistDealState(_ context.Context, rec store.DealRecord) (int64, error) {
	now := time.Now().Unix()
	if err := a.eng.persistDealRecordChecked(rec); err != nil {
		return now, err
	}
	return now, nil
}

func (a *workflowActivities) MarshalJSONValue(_ context.Context, val any) ([]byte, error) {
	return json.Marshal(val)
}

func (a *workflowActivities) LookupOffersSingleHost(ctx context.Context, host, dealID, pub string) ([]bstack.OfferAd, error) {
	br := a.eng.getBroker()
	if br == nil {
		return nil, nil
	}
	return br.LookupOffersSingleHost(ctx, host, dealID, pub), nil
}

func (a *workflowActivities) BuyerSpawnStandingOrder(ctx context.Context, orderID string, in StandingOrderInput) (bool, error) {
	wc := a.eng.WorkflowClient()
	if wc == nil {
		return false, nil
	}
	_, err := wc.StartStandingOrderAsync(ctx, orderID, in)
	return err == nil, err
}

func (a *workflowActivities) ArmEscrowGuardian(ctx context.Context, in EscrowGuardianInput) (bool, error) {
	wc := a.eng.WorkflowClient()
	if wc == nil {
		return false, nil
	}
	_, err := wc.StartEscrowGuardianAsync(ctx, in.EscrowTxID, in)
	return err == nil, err
}

func (a *workflowActivities) DisarmEscrowGuardian(ctx context.Context, escrowTxID string) (bool, error) {
	wc := a.eng.WorkflowClient()
	if wc == nil {
		return false, nil
	}
	return wc.MarkEscrowGuardianClaimed(ctx, escrowTxID)
}

func (a *workflowActivities) BuyerPersistApproval(_ context.Context, dealID, apprID, sigToken string, want bstack.WantAd, chosen *bstack.OfferAd, reason string, maxExtraSats uint64, timeoutSec int64) (int64, error) {
	now := time.Now().Unix()
	wantJSON, _ := json.Marshal(want)
	if a.eng.mem != nil && a.eng.mem.DB() != nil {
		if err := a.eng.mem.DB().SaveApproval(store.ApprovalRecord{
			ID:           apprID,
			DealID:       dealID,
			Reason:       reason,
			MaxExtraSats: maxExtraSats,
			Status:       "pending",
			AwakeableID:  sigToken,
			ExpiresAt:    now + timeoutSec,
		}); err != nil {
			return 0, err
		}
	}
	rec := store.DealRecord{
		DealID:    dealID,
		Role:      "buyer",
		Status:    "approval_pending",
		WantJSON:  wantJSON,
		PriceSats: maxExtraSats,
		Error:     reason + " [approvalId=" + apprID + "]",
	}
	if chosen != nil {
		rec.ChosenOfferJSON, _ = json.Marshal(*chosen)
		rec.CounterpartyPub = chosen.Pub
		rec.ChirpURL = chosen.ChirpURL
	}
	if err := a.eng.persistDealRecordChecked(rec); err != nil {
		return 0, err
	}
	if br := a.eng.getBroker(); br != nil {
		br.notify("approval_pending", map[string]any{
			"approvalId":  apprID,
			"dealId":      dealID,
			"reason":      reason,
			"awakeableId": sigToken,
		})
	}
	return now, nil
}

func (a *workflowActivities) BuyerConsumeResolvedApproval(_ context.Context, dealID, apprID string, markOffered bool) (bool, error) {
	if a.eng.mem != nil && a.eng.mem.DB() != nil && apprID != "" {
		_, _ = a.eng.mem.DB().MarkApprovalResolvedAwakeable(apprID, true)
	}
	if markOffered {
		if err := a.eng.persistDealRecordChecked(store.DealRecord{
			DealID: dealID,
			Role:   "buyer",
			Status: "offered",
			Error:  "",
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (a *workflowActivities) BuyerRecordDeliveryFailure(_ context.Context, dealID, sellerPub, errMsg string) (int64, error) {
	now := time.Now().Unix()
	if a.eng.mem != nil && a.eng.mem.DB() != nil && sellerPub != "" {
		_ = a.eng.mem.DB().RecordReputation(sellerPub, false, 0)
	}
	if err := a.eng.persistDealRecordChecked(store.DealRecord{
		DealID: dealID,
		Role:   "buyer",
		Status: "failed",
		Error:  "purchase delivery failed: " + errMsg,
	}); err != nil {
		return 0, err
	}
	return now, nil
}

func (a *workflowActivities) BuyerRecordClaimExpired(_ context.Context, dealID, sellerPub string, hadPayment bool) (int64, error) {
	now := time.Now().Unix()
	if hadPayment && a.eng.mem != nil && a.eng.mem.DB() != nil && sellerPub != "" {
		_ = a.eng.mem.DB().RecordReputation(sellerPub, false, 0)
	}
	if err := a.eng.persistDealRecordChecked(store.DealRecord{
		DealID: dealID,
		Role:   "buyer",
		Status: "expired",
		Error:  "timed out waiting for seller claim reveal",
	}); err != nil {
		return 0, err
	}
	return now, nil
}

func (a *workflowActivities) BuyerRecordVerifyFailure(_ context.Context, dealID, sellerPub, failReason string) (bool, error) {
	if a.eng.mem != nil && a.eng.mem.DB() != nil && sellerPub != "" {
		_ = a.eng.mem.DB().RecordReputation(sellerPub, false, 0)
	}
	if err := a.eng.persistDealRecordChecked(store.DealRecord{
		DealID: dealID,
		Role:   "buyer",
		Status: "failed",
		Error:  failReason,
	}); err != nil {
		return false, err
	}
	return true, nil
}
