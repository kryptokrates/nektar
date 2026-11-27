package market

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

type buyerPolicyEval struct {
	NowUnix        int64  `json:"nowUnix"`
	KillSwitch     bool   `json:"killSwitch"`
	NeedsApproval  bool   `json:"needsApproval"`
	ApprovalReason string `json:"approvalReason,omitempty"`
	BypassPolicy   bool   `json:"bypassPolicy"`
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
	NowUnix              int64        `json:"nowUnix"`
	TxID                 string       `json:"txid,omitempty"`
	EscrowTxID           string       `json:"escrowTxid,omitempty"`
	EscrowRefundAfter    uint64       `json:"escrowRefundAfter,omitempty"`
	PreBroadcastErr      string       `json:"preBroadcastErr,omitempty"`
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

// buyerLookupMultiHubParallel fans out per-hub offer lookups concurrently via restate.RunAsync
// and collects results in deterministic completion order via restate.WaitIter.
func (e *Engine) buyerLookupMultiHubParallel(ctx restate.WorkflowContext, br *Broker, hosts []string, dealID string, want bstack.WantAd) []bstack.OfferAd {
	futs := make([]restate.Future, 0, len(hosts))
	typed := make([]restate.RunAsyncFuture[[]bstack.OfferAd], 0, len(hosts))
	for i, h := range hosts {
		host := h
		fut := restate.RunAsync(ctx, func(rctx restate.RunContext) ([]bstack.OfferAd, error) {
			return br.LookupOffersSingleHost(rctx, host, dealID, want.Pub), nil
		}, restate.WithName(fmt.Sprintf("buyer_lookup_hub_%d", i)))
		futs = append(futs, fut)
		typed = append(typed, fut)
	}
	seen := make(map[string]bool)
	var out []bstack.OfferAd
	for fut, err := range restate.Wait(ctx, futs...) {
		if err != nil {
			continue
		}
		for _, tf := range typed {
			if tf == fut {
				if batch, rErr := tf.Result(); rErr == nil {
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
				break
			}
		}
	}
	return out
}

func (e *Engine) buyerAwaitPolicyApproval(ctx restate.WorkflowContext, dealID string, want bstack.WantAd, approvalReason string, approvalTimeout time.Duration, st *DealState) (bool, error) {
	apprTimeout := approvalTimeout
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
				Reason:       approvalReason,
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
			Error:     approvalReason + " [approvalId=" + apprID + "]",
		})
		if br := e.getBroker(); br != nil {
			br.notify("approval_pending", map[string]any{"approvalId": apprID, "dealId": dealID, "reason": approvalReason, "awakeableId": awkID})
		}
		return now, nil
	}, restate.WithName("buyer_persist_approval"))
	if pErr != nil {
		return false, pErr
	}

	st.Status = "approval_pending"
	st.ApprovalID = apprID
	st.AwakeableID = awkID
	st.Error = approvalReason + " [approvalId=" + apprID + "]"
	restate.Set(ctx, "state", *st)

	apprTimer := restate.After(ctx, apprTimeout)
	winner, waitErr := restate.WaitFirst(ctx, apprAwk, apprTimer)
	if waitErr != nil {
		return false, waitErr
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
		restate.Set(ctx, "state", *st)
		return false, nil
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
		restate.Set(ctx, "state", *st)
		return false, nil
	}
	// Mark the awakeable approval 'consumed' in SQLite so it can NEVER be replayed via ApprovedOverrideID.
	_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		if e.mem != nil && e.mem.DB() != nil && st.ApprovalID != "" {
			_, _ = e.mem.DB().MarkApprovalResolvedAwakeable(st.ApprovalID, true)
		}
		return true, nil
	}, restate.WithName("buyer_approval_consume"))
	return true, nil
}
