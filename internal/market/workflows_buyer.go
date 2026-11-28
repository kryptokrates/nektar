package market

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/workflow"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

// BuyerDealWorkflow orchestrates a complete buyer knowledge acquisition saga on pure-Go go-workflows:
//  1. DB-authoritative operator policy & bounded approval override check (RCA-3 / STATE-7)
//  2. Durable signal-based operator approval gate (zero HTTP awakeables)
//  3. Offer collection + deterministic multi-hub fan-out + optional StandingOrder handoff
//  4. 6D reputation ranking + optional Jev semantic match & escalation gate
//  5. Atomic SQLite Custody spend reservation with Top-K pre-broadcast seller failover
//  6. Separated on-chain payment broadcast (STATE-2 pre-broadcast obligation) & idempotent purchase delivery (RT-3 / STATE-1)
//  7. Bounded claim signal wait -> zero-trust BRC-369 + SPV verification -> atomic settlement (RCA-2 / ARCH-F12)
func BuyerDealWorkflow(ctx workflow.Context, in BuyerDealInput) (DealState, error) {
	var acts *workflowActivities
	dealID := workflow.WorkflowInstance(ctx).InstanceID
	want := in.Want
	if want.WantID == "" {
		want.WantID = dealID
	}

	// Step 1: Evaluate DB-authoritative policy & bounded override.
	eval, err := workflow.ExecuteActivity[buyerPolicyEval](ctx, dealActivityOpts(), acts.BuyerEvalPolicy, dealID, want, in.ApprovedOverrideID).Get(ctx)
	if err != nil {
		return DealState{}, err
	}

	st := DealState{DealID: dealID, Role: "buyer", Status: "open", Want: want, UpdatedAt: eval.NowUnix}
	if eval.KillSwitch {
		st.Status = "failed"
		st.Error = "market: operator kill switch is engaged"
		return st, nil
	}

	approvedCapSats := eval.ApprovedCapSats
	bypassDomainRole := eval.BypassPolicy

	// Step 2: Durable SignalChannel Policy Approval Gate.
	if eval.NeedsApproval {
		approved, aErr := buyerAwaitApprovalSignal(ctx, dealID, "pol", want, nil, eval.ApprovalReason, want.MaxSats, in.ApprovalTimeout, &st)
		if aErr != nil {
			return st, aErr
		}
		if !approved {
			return st, nil
		}
		bypassDomainRole = true
		approvedCapSats = want.MaxSats
	}

	// Step 3a: Collect & cryptographically verify offers.
	collectRes, err := workflow.ExecuteActivity[buyerCollectResult](ctx, dealActivityOpts(), acts.BuyerCollectOffers, dealID, want, in).Get(ctx)
	if err != nil {
		return st, err
	}
	if collectRes.NoOffers && len(in.Offers) == 0 && len(collectRes.ActiveHosts) > 1 {
		if extra := buyerLookupMultiHubParallelWF(ctx, collectRes.ActiveHosts, dealID, want); len(extra) > 0 {
			collectRes.Offers = extra
			collectRes.NoOffers = false
			collectRes.Error = ""
		}
	}
	if collectRes.NoOffers {
		if in.StandingTTL > 0 {
			orderID := "so-" + dealID
			soIn := StandingOrderInput{
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
			}
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.BuyerSpawnStandingOrder, orderID, soIn).Get(ctx)
			st.Status = "watching"
			st.StandingOrderID = orderID
			st.Error = ""
			st.UpdatedAt = workflow.Now(ctx).Unix()
			return st, nil
		}
		st.Status = "expired"
		st.Error = collectRes.Error
		st.UpdatedAt = collectRes.NowUnix
		return st, nil
	}
	if collectRes.PolicyRejected || len(collectRes.Offers) == 0 {
		st.Status = "failed"
		st.Error = collectRes.Error
		st.UpdatedAt = collectRes.NowUnix
		return st, nil
	}

	// Step 3b: Deterministic 6D ranking + optional Jev semantic match.
	rankedRes, err := workflow.ExecuteActivity[buyerRankResult](ctx, dealActivityOpts(), acts.BuyerRankAndMatch, dealID, want, collectRes, bypassDomainRole, approvedCapSats).Get(ctx)
	if err != nil {
		return st, err
	}
	if rankedRes.NoOffers {
		st.Status = "expired"
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
		return st, nil
	}
	if rankedRes.PolicyRejected || rankedRes.Chosen == nil {
		st.Status = "failed"
		st.MatchAudit = rankedRes.MatchAudit
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
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

	// Step 3c: Post-match semantic escalation gate (does NOT grant cap bypass!).
	if rankedRes.NeedsEscalation {
		approved, eErr := buyerAwaitApprovalSignal(ctx, dealID, "esc", want, &chosen, rankedRes.EscalationReason, chosen.PriceSats, in.ApprovalTimeout, &st)
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

	// Open durable "claim" signal channel BEFORE broadcasting payment so fast/in-process seller reveals
	// arriving immediately after delivery are buffered cleanly with zero race window.
	claimCh := workflow.NewSignalChannel[ClaimSignal](ctx, "claim")

	// Steps 4-7 with deterministic Top-K Multi-Seller Failover (pre-broadcast only).
	for idx, cand := range candidates {
		attempt := idx + 1
		hasNext := idx+1 < len(candidates)
		chosen = cand
		chosenCopy := cand
		st.ChosenOffer = &chosenCopy
		st.ChirpURL = chosen.ChirpURL
		st.ClaimPromise = "claim"
		if attempt > 1 {
			st.Status = "offered"
			st.Error = ""
		}
		if chosenRaw, mErr := json.Marshal(chosenCopy); mErr == nil {
			_, _ = workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
				DealID:          dealID,
				Role:            "buyer",
				Status:          "offered",
				ChosenOfferJSON: chosenRaw,
				CounterpartyPub: chosen.Pub,
				PriceSats:       chosen.PriceSats,
				ChirpURL:        chosen.ChirpURL,
			}).Get(ctx)
		}

		// Step 4: Atomic SQLite Custody reservation (bounded by approvedCapSats per STATE-7).
		if chosen.PriceSats > 0 {
			res, cErr := workflow.ExecuteActivity[CustodyReserveResult](ctx, dealActivityOpts(), acts.CustodyReserve, CustodyReserveInput{
				OperationID:     dealID,
				Attempt:         attempt,
				SellerPub:       chosen.Pub,
				Domains:         want.Domains,
				AmountSats:      chosen.PriceSats,
				BypassPolicy:    bypassDomainRole,
				ApprovedCapSats: approvedCapSats,
			}).Get(ctx)
			if cErr != nil {
				return st, cErr
			}
			if !res.Approved {
				if hasNext && !strings.Contains(strings.ToLower(res.Reason), "kill switch") {
					continue
				}
				failNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "custody reservation denied: " + res.Reason,
				}).Get(ctx)
				st.Status = "failed"
				st.Error = "custody reservation denied: " + res.Reason
				st.UpdatedAt = failNow
				return st, nil
			}
		}

		// Step 5a & 5b: Separated Payment Broadcast (STATE-2) & Idempotent Purchase Delivery (RT-3 / STATE-1).
		var immediateClaim *ClaimSignal
		var awaitingConfirm bool
		var requiredDepth int
		if chosen.PayAddress != "" || chosen.PayIdentityKey != "" {
			payRes, pErr := workflow.ExecuteActivity[buyerPayResult](ctx, dealActivityOpts(), acts.BuyerBroadcastPayment, dealID, chosen).Get(ctx)
			if pErr != nil {
				return st, pErr
			}
			if payRes.PreBroadcastErr != "" {
				if chosen.PriceSats > 0 {
					_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.CustodyRelease, CustodyReleaseInput{
						OperationID: dealID,
						Attempt:     attempt,
					}).Get(ctx)
				}
				if hasNext {
					continue
				}
				failNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "payment failed: " + payRes.PreBroadcastErr,
				}).Get(ctx)
				st.Status = "failed"
				st.Error = "payment failed: " + payRes.PreBroadcastErr
				st.UpdatedAt = failNow
				return st, nil
			}

			if payRes.TxID != "" {
				st.PaymentTxID = payRes.TxID
				st.Status = "paid_awaiting_reveal"
				st.UpdatedAt = payRes.NowUnix
				if payRes.EscrowTxID != "" {
					st.EscrowTxID = payRes.EscrowTxID
					_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.ArmEscrowGuardian, EscrowGuardianInput{
						DealID:      dealID,
						EscrowTxID:  payRes.EscrowTxID,
						RefundAfter: payRes.EscrowRefundAfter,
					}).Get(ctx)
				}
			}

			// Step 5b: Deliver signed PurchaseRequest in its own retried activity (RT-3: never double-pays,
			// and transient seller HTTP glitches retry cleanly without killing a broadcast payment!).
			delivRes, dErr := workflow.ExecuteActivity[buyerDeliverResult](ctx, dealActivityOpts(), acts.BuyerDeliverPurchase, dealID, chosen, payRes).Get(ctx)
			if dErr != nil || delivRes.DeliveryErr != "" {
				errMsg := delivRes.DeliveryErr
				if dErr != nil && errMsg == "" {
					errMsg = dErr.Error()
				}
				// Even if direct HTTP delivery failed after retries, when an on-chain escrow was funded,
				// EscrowGuardian is ALREADY armed to refund after RefundAfter — wait for claim or refund!
				if st.EscrowTxID == "" {
					failNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.BuyerRecordDeliveryFailure, dealID, chosen.Pub, errMsg).Get(ctx)
					st.Status = "failed"
					st.Error = "purchase delivery failed: " + errMsg
					st.UpdatedAt = failNow
					return st, nil
				}
			} else {
				immediateClaim = delivRes.ImmediateClaim
				awaitingConfirm = delivRes.AwaitingConfirmation
				requiredDepth = delivRes.RequiredDepth
			}
		}

		// Step 6: Obtain ClaimSignal (either synchronously returned or via durable SignalChannel("claim")).
		var sig ClaimSignal
		if immediateClaim != nil {
			sig = *immediateClaim
		} else {
			if st.PaymentTxID == "" {
				wantJSON, _ := workflow.ExecuteActivity[[]byte](ctx, singleAttemptActivityOpts(), acts.MarshalJSONValue, want).Get(ctx)
				chosenJSON, _ := workflow.ExecuteActivity[[]byte](ctx, singleAttemptActivityOpts(), acts.MarshalJSONValue, chosen).Get(ctx)
				offNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.PersistDealState, store.DealRecord{
					DealID:          dealID,
					Role:            "buyer",
					Status:          "offered",
					WantJSON:        wantJSON,
					ChosenOfferJSON: chosenJSON,
					CounterpartyPub: chosen.Pub,
					ChirpURL:        chosen.ChirpURL,
					PriceSats:       chosen.PriceSats,
					Error:           "",
				}).Get(ctx)
				st.Status = "offered"
				st.UpdatedAt = offNow
			}
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
			tCtx, cancelTimer := workflow.WithCancel(ctx)
			timerFut := workflow.ScheduleTimer(tCtx, timeout)
			gotClaim := false
			workflow.Select(ctx,
				workflow.Receive(claimCh, func(_ workflow.Context, v ClaimSignal, ok bool) {
					cancelTimer()
					if ok {
						sig = v
						gotClaim = true
					}
				}),
				workflow.Await(timerFut, func(_ workflow.Context, _ workflow.Future[any]) {
				}),
			)
			cancelTimer()
			if !gotClaim {
				expNow, _ := workflow.ExecuteActivity[int64](ctx, dealActivityOpts(), acts.BuyerRecordClaimExpired, dealID, chosen.Pub, st.PaymentTxID != "").Get(ctx)
				st.Status = "expired"
				st.Error = "timed out waiting for seller claim reveal"
				st.UpdatedAt = expNow
				return st, nil
			}
		}

		// Step 7: Zero-trust BRC-369 + CHIRP + offline SPV verification & tainted memory commit.
		verRes, err := workflow.ExecuteActivity[buyerVerifyResult](ctx, dealActivityOpts(), acts.BuyerVerifyAndCommit, dealID, want, in.TargetShard, chosen, st.PaymentTxID, st.MatchAudit, sig).Get(ctx)
		if err != nil {
			return st, err
		}
		if verRes.FailedReason != "" {
			if st.PaymentTxID == "" && hasNext {
				if chosen.PriceSats > 0 {
					_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.CustodyRelease, CustodyReleaseInput{
						OperationID: dealID,
						Attempt:     attempt,
					}).Get(ctx)
				}
				continue
			}
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.BuyerRecordVerifyFailure, dealID, chosen.Pub, verRes.FailedReason).Get(ctx)
			st.Status = "failed"
			st.Error = verRes.FailedReason
			st.UpdatedAt = verRes.NowUnix
			return st, nil
		}

		if st.EscrowTxID != "" {
			_, _ = workflow.ExecuteActivity[bool](ctx, singleAttemptActivityOpts(), acts.DisarmEscrowGuardian, st.EscrowTxID).Get(ctx)
		}

		st.ClaimTxID = sig.ClaimTxID
		st.Shard = verRes.TargetShard
		st.DeliveredTxt = verRes.DeliveredText
		st.DealReceipt = verRes.DealReceipt
		st.Status = "settled"
		st.Error = ""
		st.UpdatedAt = verRes.NowUnix
		return st, nil
	}
	return st, nil
}
