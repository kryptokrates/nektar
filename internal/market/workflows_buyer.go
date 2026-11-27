package market

import (
	"fmt"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

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
	// and multiple active hubs were journaled inside buyer_collect_offers, query every hub in parallel deterministically!
	if collectRes.NoOffers && len(in.Offers) == 0 && len(collectRes.ActiveHosts) > 1 {
		if br := e.getBroker(); br != nil {
			if extra := e.buyerLookupMultiHubParallel(ctx, br, collectRes.ActiveHosts, dealID, want); len(extra) > 0 {
				collectRes.Offers = extra
				collectRes.NoOffers = false
				collectRes.Error = ""
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
				OrderID:           orderID,
				Domains:           want.Domains,
				Query:             want.Query,
				OntologyID:        want.OntologyID,
				MaxSats:           want.MaxSats,
				MinGrade:          want.MinGrade,
				MaxSatsPer1kUnits: want.MaxSatsPer1kUnits,
				FreshnessClass:    want.FreshnessClass,
				RequiredRights:    want.RequiredRights,
				PricingMode:       want.PricingMode,
				TargetShard:       in.TargetShard,
				TTL:               in.StandingTTL,
				WaitTimeout:       in.WaitTimeout,
				ApprovalTimeout:   in.ApprovalTimeout,
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
		st.ClaimPromise = claimPromiseName(attempt)
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
				if hasNext && !strings.Contains(strings.ToLower(res.Reason), "kill switch") {
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
		var awaitingConfirm bool
		var requiredDepth int
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
			awaitingConfirm = payRes.AwaitingConfirmation
			requiredDepth = payRes.RequiredDepth
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
			if awaitingConfirm {
				minConfirmWait := 2 * time.Hour
				if requiredDepth > 0 {
					if depthWait := time.Duration(requiredDepth) * 30 * time.Minute; depthWait > minConfirmWait {
						minConfirmWait = depthWait
					}
				}
				if timeout < minConfirmWait {
					timeout = minConfirmWait
				}
			}
			claimName := claimPromiseName(attempt)
			if st.ClaimPromise != claimName {
				st.ClaimPromise = claimName
				restate.Set(ctx, "state", st)
			}
			claimFut := restate.Promise[ClaimSignal](ctx, claimName)
			sleepFut := restate.After(ctx, timeout)
			winner, cancelErr := restate.WaitFirst(ctx, claimFut, sleepFut)
			if cancelErr != nil {
				return st, cancelErr
			}
			if winner == sleepFut {
				// R13: explicitly reject promise on expiry so late deliverKey calls fail honestly!
				_ = restate.Promise[ClaimSignal](ctx, claimName).Reject(restate.TerminalErrorf("deal %s expired waiting for seller key reveal", dealID))
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

func claimPromiseName(attempt int) string {
	if attempt <= 1 {
		return "claim"
	}
	return fmt.Sprintf("claim_%d", attempt)
}

func (e *Engine) restateBuyerDeliverKey(ctx restate.WorkflowSharedContext, sig ClaimSignal) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	claimName := "claim"
	if st, err := restate.Get[DealState](ctx, "state"); err == nil && st.ClaimPromise != "" {
		claimName = st.ClaimPromise
	}
	if err := restate.Promise[ClaimSignal](ctx, claimName).Resolve(sig); err != nil {
		return false, err
	}
	return true, nil
}
