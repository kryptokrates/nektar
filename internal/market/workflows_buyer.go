package market

import (
	"encoding/json"
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
	FirstOfferMs   int64            `json:"firstOfferMs"`
}

type buyerRankResult struct {
	NowUnix          int64           `json:"nowUnix"`
	NoOffers         bool            `json:"noOffers"`
	PolicyRejected   bool            `json:"policyRejected"`
	NeedsEscalation  bool            `json:"needsEscalation,omitempty"`
	EscalationReason string          `json:"escalationReason,omitempty"`
	Error            string          `json:"error,omitempty"`
	Chosen           *bstack.OfferAd `json:"chosen,omitempty"`
	MatchAudit       *MatchAudit     `json:"matchAudit,omitempty"`
	FirstOfferMs     int64           `json:"firstOfferMs"`
}

type buyerPayResult struct {
	NowUnix         int64        `json:"nowUnix"`
	TxID            string       `json:"txid,omitempty"`
	PreBroadcastErr string       `json:"preBroadcastErr,omitempty"`
	ImmediateClaim  *ClaimSignal `json:"immediateClaim,omitempty"`
	DeliveryErr     string       `json:"deliveryErr,omitempty"`
}

type buyerVerifyResult struct {
	NowUnix       int64        `json:"nowUnix"`
	DeliveredText string       `json:"deliveredText,omitempty"`
	TargetShard   string       `json:"targetShard,omitempty"`
	DealReceipt   *DealReceipt `json:"dealReceipt,omitempty"`
	FailedReason  string       `json:"failedReason,omitempty"`
}

func (e *Engine) restateBuyerRun(ctx restate.WorkflowContext, in BuyerDealInput) (DealState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return DealState{}, err
	}
	dealID := restate.Key(ctx)
	want := in.Want
	if want.WantID == "" {
		want.WantID = dealID
	}

	// Step 1: Journal policy & kill-switch evaluation inside restate.Run (zero non-determinism on replay).
	eval, err := restate.Run(ctx, func(_ restate.RunContext) (buyerPolicyEval, error) {
		return e.buyerEvalPolicy(dealID, want, in.ApprovedOverrideID)
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
		approved, aErr := e.buyerAwaitPolicyApproval(ctx, dealID, want, eval.ApprovalReason, in.ApprovalTimeout, &st)
		if aErr != nil {
			return st, aErr
		}
		if !approved {
			return st, nil
		}
		bypassPolicy = true
	}

	// Step 3a: Collect & cryptographically verify offers inside restate.Run (isolated from AI retries).
	collectRes, err := restate.Run(ctx, func(rctx restate.RunContext) (buyerCollectResult, error) {
		return e.buyerCollectOffers(rctx, dealID, want, in)
	}, restate.WithName("buyer_collect_offers"))
	if err != nil {
		return st, err
	}
	if collectRes.NoOffers {
		st.Status = "expired"
		st.Error = collectRes.Error
		st.UpdatedAt = collectRes.NowUnix
		restate.Set(ctx, "state", st)
		return st, nil
	}
	if collectRes.PolicyRejected || len(collectRes.Offers) == 0 {
		st.Status = "failed"
		st.Error = collectRes.Error
		st.UpdatedAt = collectRes.NowUnix
		restate.Set(ctx, "state", st)
		return st, nil
	}

	// Step 3b: Deterministic base ranking + optional Jev System One semantic match inside its own
	// restate.Run step so journal replays never re-call Jev (0x RT0016) and retries never re-broadcast WantAd.
	rankedRes, err := restate.Run(ctx, func(rctx restate.RunContext) (buyerRankResult, error) {
		return e.buyerRankAndMatch(rctx, dealID, want, collectRes, bypassPolicy)
	}, restate.WithName("buyer_jev_match"))
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
		st.MatchAudit = rankedRes.MatchAudit
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
		restate.Set(ctx, "state", st)
		return st, nil
	}

	chosen := *rankedRes.Chosen
	st.ChosenOffer = &chosen
	st.MatchAudit = rankedRes.MatchAudit
	st.ChirpURL = chosen.ChirpURL

	// Step 3c: Post-match Restate Awakeable Escalation when Jev flags borderline confidence or semantic risk.
	// Note: Approving this semantic escalation does NOT set bypassPolicy=true; Custody/global still enforces hard caps!
	if rankedRes.NeedsEscalation {
		approved, eErr := e.buyerAwaitMatchEscalation(ctx, dealID, chosen, rankedRes.EscalationReason, in.ApprovalTimeout, &st)
		if eErr != nil {
			return st, eErr
		}
		if !approved {
			return st, nil
		}
	}

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
			return e.buyerPayAndDeliver(rctx, dealID, chosen)
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
		return e.buyerVerifyAndCommit(rctx, dealID, want, in.TargetShard, chosen, st.PaymentTxID, st.MatchAudit, sig)
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
	st.DealReceipt = verRes.DealReceipt
	st.Status = "settled"
	st.Error = ""
	st.UpdatedAt = verRes.NowUnix
	restate.Set(ctx, "state", st)
	return st, nil
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

func (e *Engine) restateBuyerDeliverKey(ctx restate.WorkflowSharedContext, sig ClaimSignal) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	if err := restate.Promise[ClaimSignal](ctx, "claim").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}
