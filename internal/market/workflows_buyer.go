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
	NowUnix           int64        `json:"nowUnix"`
	TxID              string       `json:"txid,omitempty"`
	EscrowTxID        string       `json:"escrowTxid,omitempty"`
	EscrowRefundAfter uint64       `json:"escrowRefundAfter,omitempty"`
	PreBroadcastErr   string       `json:"preBroadcastErr,omitempty"`
	ImmediateClaim    *ClaimSignal `json:"immediateClaim,omitempty"`
	DeliveryErr       string       `json:"deliveryErr,omitempty"`
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
	// Parallel Multi-Hub Fan-Out (restate.RunAsync + restate.WaitIter): if initial collect found no offers
	// and multiple active hubs are configured, query every hub in parallel and collect deterministically!
	if collectRes.NoOffers && len(in.Offers) == 0 {
		if br := e.getBroker(); br != nil && br.Discovery() != nil {
			if hosts := br.Discovery().ActiveHosts(); len(hosts) > 1 {
				if extra := e.buyerLookupMultiHubParallel(ctx, br, hosts, dealID, want); len(extra) > 0 {
					collectRes.Offers = extra
					collectRes.NoOffers = false
					collectRes.Error = ""
				}
			}
		}
	}
	if collectRes.NoOffers {
		// Optional durable StandingOrder fallback: if caller requested StandingTTL > 0, hand off to a
		// 0%-CPU StandingOrder workflow that wakes via restate.Signal whenever a matching OfferAd arrives!
		if in.StandingTTL > 0 {
			orderID := "so-" + dealID
			var sendOpts []restate.SendOption
			if hdrs := e.internalCallHeaders(); hdrs != nil {
				sendOpts = append(sendOpts, restate.WithHeaders(hdrs))
			}
			restate.WorkflowSend(ctx, "StandingOrder", orderID, "run").Send(StandingOrderInput{
				OrderID:         orderID,
				Domains:         want.Domains,
				Query:           want.Query,
				MaxSats:         want.MaxSats,
				TargetShard:     in.TargetShard,
				TTL:             in.StandingTTL,
				WaitTimeout:     in.WaitTimeout,
				ApprovalTimeout: in.ApprovalTimeout,
			}, sendOpts...)
			st.Status = "watching"
			st.StandingOrderID = orderID
			st.Error = ""
			st.UpdatedAt = collectRes.NowUnix
			restate.Set(ctx, "state", st)
			return st, nil
		}
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

	candidates := rankedRes.Candidates
	if len(candidates) == 0 {
		candidates = []bstack.OfferAd{*rankedRes.Chosen}
	}

	chosen := candidates[0]
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

	// Steps 4-7 with deterministic Top-K Multi-Seller Failover:
	// If Seller #1 fails pre-broadcast (e.g., invalid offer, unreachable, blocked seller, or unpaid verify),
	// Custody is released cleanly and BuyerDeal automatically fails over to Seller #2/#3 without re-calling
	// buyer_collect_offers or buyer_jev_match. Once an on-chain tx is broadcast, NEVER double-pay a second seller!
	for idx, cand := range candidates {
		attempt := idx + 1
		hasNext := idx+1 < len(candidates)
		chosen = cand
		chosenCopy := cand
		st.ChosenOffer = &chosenCopy
		st.ChirpURL = chosen.ChirpURL
		if attempt > 1 {
			st.Status = "offered"
			st.Error = ""
			restate.Set(ctx, "state", st)
		}

		// Step 4: Reserve spend atomically through the private global Custody Virtual Object!
		if chosen.PriceSats > 0 {
			res, cErr := restate.Object[CustodyReserveResult](ctx, "Custody", "global", "reserve").
				Request(CustodyReserveInput{
					OperationID:  dealID,
					Attempt:      attempt,
					SellerPub:    chosen.Pub,
					Domains:      want.Domains,
					AmountSats:   chosen.PriceSats,
					BypassPolicy: bypassPolicy,
				})
			if cErr != nil {
				return st, cErr
			}
			if !res.Approved {
				if hasNext && strings.Contains(strings.ToLower(res.Reason), "blocked") {
					continue
				}
				stepName := "buyer_custody_denied"
				if attempt > 1 {
					stepName = fmt.Sprintf("buyer_custody_denied_%d", attempt)
				}
				failNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
					now := time.Now().Unix()
					e.persistDealRecord(store.DealRecord{
						DealID: dealID,
						Role:   "buyer",
						Status: "failed",
						Error:  "custody reservation denied: " + res.Reason,
					})
					return now, nil
				}, restate.WithName(stepName))
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
			payStep := "buyer_pay_and_deliver"
			if attempt > 1 {
				payStep = fmt.Sprintf("buyer_pay_and_deliver_%d", attempt)
			}
			payRes, pErr := restate.Run(ctx, func(rctx restate.RunContext) (buyerPayResult, error) {
				return e.buyerPayAndDeliver(rctx, dealID, chosen)
			}, restate.WithName(payStep))
			if pErr != nil {
				return st, pErr
			}
			if payRes.PreBroadcastErr != "" {
				if chosen.PriceSats > 0 {
					_, _ = restate.Object[bool](ctx, "Custody", "global", "release").
						Request(CustodyReleaseInput{OperationID: dealID, Attempt: attempt})
				}
				if hasNext {
					continue
				}
				failStep := "buyer_payment_failed"
				if attempt > 1 {
					failStep = fmt.Sprintf("buyer_payment_failed_%d", attempt)
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
				}, restate.WithName(failStep))
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
				if payRes.EscrowTxID != "" {
					st.EscrowTxID = payRes.EscrowTxID
				}
				restate.Set(ctx, "state", st)
				restate.ObjectSend(ctx, "Tx", payRes.TxID, "emit").Send(
					TxEventInput{Kind: "broadcast", Detail: "buyer-deal:" + dealID, ObservedAt: payRes.NowUnix},
					restate.WithIdempotencyKey("tx:"+payRes.TxID+":broadcast:0"),
				)
				// Automatically arm durable EscrowGuardian so funds are guaranteed to auto-refund after RefundAfter!
				if payRes.EscrowTxID != "" {
					var gOpts []restate.SendOption
					if hdrs := e.internalCallHeaders(); hdrs != nil {
						gOpts = append(gOpts, restate.WithHeaders(hdrs))
					}
					restate.WorkflowSend(ctx, "EscrowGuardian", payRes.EscrowTxID, "run").Send(
						EscrowGuardianInput{
							DealID:      dealID,
							EscrowTxID:  payRes.EscrowTxID,
							RefundAfter: payRes.EscrowRefundAfter,
						},
						gOpts...,
					)
				}
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
		verStep := "buyer_verify_and_commit"
		if attempt > 1 {
			verStep = fmt.Sprintf("buyer_verify_and_commit_%d", attempt)
		}
		verRes, err := restate.Run(ctx, func(rctx restate.RunContext) (buyerVerifyResult, error) {
			return e.buyerVerifyAndCommit(rctx, dealID, want, in.TargetShard, chosen, st.PaymentTxID, st.MatchAudit, sig)
		}, restate.WithName(verStep))
		if err != nil {
			return st, err
		}
		if verRes.FailedReason != "" {
			if st.PaymentTxID == "" && hasNext {
				if chosen.PriceSats > 0 {
					_, _ = restate.Object[bool](ctx, "Custody", "global", "release").
						Request(CustodyReleaseInput{OperationID: dealID, Attempt: attempt})
				}
				continue
			}
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

		// Disarm EscrowGuardian cleanly once the deal settles!
		if st.EscrowTxID != "" {
			var gOpts []restate.SendOption
			if hdrs := e.internalCallHeaders(); hdrs != nil {
				gOpts = append(gOpts, restate.WithHeaders(hdrs))
			}
			restate.WorkflowSend(ctx, "EscrowGuardian", st.EscrowTxID, "markClaimed").Send(restate.Void{}, gOpts...)
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
	return st, nil
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

func (e *Engine) restateBuyerDeliverKey(ctx restate.WorkflowSharedContext, sig ClaimSignal) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	if err := restate.Promise[ClaimSignal](ctx, "claim").Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}
