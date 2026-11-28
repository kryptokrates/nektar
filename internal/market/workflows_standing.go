package market

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/workflow"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reputation"
)

// StandingOrderWorkflow implements a 0%-CPU limit-order for knowledge on pure-Go go-workflows.
// Uses native workflow.NewSignalChannel[standingOrderEvent](ctx, "events") so offers, steers, and cancels
// arriving even 0ms after creation are durably queued in workflows.sqlite (eliminating RT-2 & RT-4).
func StandingOrderWorkflow(ctx workflow.Context, in StandingOrderInput) (StandingOrderState, error) {
	var acts *workflowActivities
	inst := workflow.WorkflowInstance(ctx)
	orderID := inst.InstanceID

	eventsCh := workflow.NewSignalChannel[standingOrderEvent](ctx, "events")

	ttl := in.TTL
	if ttl <= 0 && in.TTLSeconds > 0 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if ttl > 30*24*time.Hour {
		ttl = 30 * 24 * time.Hour
	}
	maxFills := in.MaxFills
	if maxFills <= 0 {
		maxFills = 1
	}
	if maxFills > 50 {
		maxFills = 50
	}
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 16
	}
	if maxAttempts < maxFills {
		maxAttempts = maxFills * 4
	}

	initNow := workflow.Now(ctx).Unix()
	doms := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		if s := strings.ToLower(strings.TrimSpace(d)); s != "" {
			doms = append(doms, s)
		}
	}
	if len(doms) == 0 {
		doms = []string{"ops"}
	}

	initCfg, err := workflow.ExecuteActivity[standingInitConfig](ctx, dealActivityOpts(), acts.StandingOrderInit, in).Get(ctx)
	if err != nil {
		return StandingOrderState{}, err
	}

	st := StandingOrderState{
		OrderID:                orderID,
		InvocationID:           inst.ExecutionID,
		WaitingSeq:             1,
		Status:                 "watching",
		Domains:                doms,
		Query:                  strings.TrimSpace(in.Query),
		OntologyID:             strings.ToLower(strings.TrimSpace(in.OntologyID)),
		AllowBridge:            in.AllowBridge,
		RequireTrustedOrBonded: initCfg.RequireTrust,
		MaxSats:                initCfg.MaxSats,
		MinGrade:               string(nmu.NormalizeGrade(in.MinGrade)),
		MaxSatsPer1kUnits:      in.MaxSatsPer1kUnits,
		FreshnessClass:         string(nmu.NormalizeFreshness(in.FreshnessClass)),
		RequiredRights:         string(nmu.NormalizeRights(in.RequiredRights)),
		PricingMode:            string(nmu.NormalizePricingMode(in.PricingMode)),
		TargetShard:            strings.TrimSpace(in.TargetShard),
		AgentID:                strings.TrimSpace(in.AgentID),
		ExpiresAtUnix:          initNow + int64(ttl.Seconds()),
		MaxFills:               maxFills,
		CreatedAt:              initNow,
		UpdatedAt:              initNow,
	}
	_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderRegisterAndAnnounce, st).Get(ctx)

	seenOffers := make(map[string]bool)

	for st.Status == "watching" || st.Status == "paused" {
		nowUnix := workflow.Now(ctx).Unix()
		if nowUnix >= st.ExpiresAtUnix {
			st.Status = "expired"
			st.UpdatedAt = nowUnix
			break
		}

		// Drain any immediately ready signal without scheduling a timer; otherwise sleep until signal or TTL.
		ev, got := eventsCh.ReceiveNonBlocking()
		if !got {
			remaining := time.Duration(st.ExpiresAtUnix-nowUnix) * time.Second
			if remaining <= 0 {
				remaining = time.Second
			}
			tCtx, cancelTimer := workflow.WithCancel(ctx)
			expTimer := workflow.ScheduleTimer(tCtx, remaining)
			workflow.Select(ctx,
				workflow.Receive(eventsCh, func(_ workflow.Context, v standingOrderEvent, ok bool) {
					cancelTimer()
					if ok {
						ev = v
						got = true
					}
				}),
				workflow.Await(expTimer, func(_ workflow.Context, _ workflow.Future[any]) {
				}),
			)
			cancelTimer()
			if !got {
				st.Status = "expired"
				st.UpdatedAt = workflow.Now(ctx).Unix()
				break
			}
		}

		nowUnix = workflow.Now(ctx).Unix()
		st.LastProcessedSeq++
		st.WaitingSeq = int(st.LastProcessedSeq + 1)
		switch ev.Kind {
		case "cancel":
			st.Status = "cancelled"
			st.Error = strings.TrimSpace(ev.Reason)
			st.UpdatedAt = nowUnix
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, true).Get(ctx)
			return st, nil

		case "steer":
			if ev.Steer != nil {
				if ev.Steer.MaxSats != nil && *ev.Steer.MaxSats > 0 {
					st.MaxSats = *ev.Steer.MaxSats
				}
				if ev.Steer.Paused != nil {
					if *ev.Steer.Paused {
						st.Status = "paused"
					} else if st.Status == "paused" {
						st.Status = "watching"
					}
				}
				if ev.Steer.ExtendSeconds > 0 {
					st.ExpiresAtUnix += ev.Steer.ExtendSeconds
				}
			}
			st.UpdatedAt = nowUnix
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)

		case "offer":
			if ev.Offer == nil || st.Status != "watching" {
				st.UpdatedAt = nowUnix
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
				continue
			}
			off := *ev.Offer
			if off.OfferID != "" && seenOffers[off.OfferID] {
				continue
			}
			if off.OfferID != "" {
				seenOffers[off.OfferID] = true
			}
			st.OffersSeen++
			if st.MaxSats > 0 && off.PriceSats > st.MaxSats {
				st.UpdatedAt = nowUnix
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
				continue
			}
			if !standingOrderDomainMatches(st.Domains, off.Domains) {
				st.UpdatedAt = nowUnix
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
				continue
			}
			if !standingOrderNMUMatches(st.MinGrade, st.MaxSatsPer1kUnits, st.FreshnessClass, st.RequiredRights, st.PricingMode, off, nowUnix) {
				st.UpdatedAt = nowUnix
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
				continue
			}
			evalOK, eErr := workflow.ExecuteActivity[standingOfferEvalResult](ctx, dealActivityOpts(), acts.StandingOrderEvalOffer, st, off).Get(ctx)
			if eErr != nil || !evalOK.Accepted {
				st.UpdatedAt = nowUnix
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
				continue
			}
			if st.Attempts >= maxAttempts {
				st.Status = "expired"
				st.Error = fmt.Sprintf("standing order reached max attempts (%d)", maxAttempts)
				st.UpdatedAt = nowUnix
				break
			}

			st.Attempts++
			dealID := fmt.Sprintf("so-%s-%d", orderID, st.Attempts)
			st.Status = "matching"
			st.LastDealID = dealID
			st.Error = ""
			st.UpdatedAt = nowUnix
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)

			wantAd := bstack.WantAd{
				WantID:            dealID,
				Domains:           st.Domains,
				Query:             st.Query,
				OntologyID:        st.OntologyID,
				MaxSats:           st.MaxSats,
				MinGrade:          st.MinGrade,
				MaxSatsPer1kUnits: st.MaxSatsPer1kUnits,
				FreshnessClass:    st.FreshnessClass,
				RequiredRights:    st.RequiredRights,
				PricingMode:       st.PricingMode,
				Pub:               evalOK.BuyerPub,
			}

			dealRes, dErr := workflow.CreateSubWorkflowInstance[DealState](ctx, workflow.SubWorkflowOptions{
				InstanceID: dealID,
			}, BuyerDealWorkflow, BuyerDealInput{
				Want:            wantAd,
				Offers:          []bstack.OfferAd{off},
				TargetShard:     st.TargetShard,
				WaitTimeout:     in.WaitTimeout,
				ApprovalTimeout: in.ApprovalTimeout,
			}).Get(ctx)

			st.UpdatedAt = workflow.Now(ctx).Unix()
			if dErr == nil && dealRes.Status == "settled" {
				st.Fills++
				st.PaymentTxID = dealRes.PaymentTxID
				st.DeliveredText = dealRes.DeliveredTxt
				st.TargetShard = dealRes.Shard
				st.Error = ""
				if st.Fills >= st.MaxFills {
					st.Status = "filled"
					break
				}
				st.Status = "watching"
				_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
				continue
			}
			st.Status = "watching"
			if dErr != nil {
				st.Error = dErr.Error()
			} else if dealRes.Error != "" {
				st.Error = dealRes.Error
			}
			_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, false).Get(ctx)
		}
	}

	_, _ = workflow.ExecuteActivity[bool](ctx, dealActivityOpts(), acts.StandingOrderSaveState, st, true).Get(ctx)
	return st, nil
}

type standingInitConfig struct {
	RequireTrust bool   `json:"requireTrust"`
	MaxSats      uint64 `json:"maxSats"`
}

type standingOfferEvalResult struct {
	Accepted bool   `json:"accepted"`
	BuyerPub string `json:"buyerPub,omitempty"`
}

func (a *workflowActivities) StandingOrderInit(_ context.Context, in StandingOrderInput) (standingInitConfig, error) {
	e := a.eng
	reqTrust := in.RequireTrustedOrBonded
	if !in.AllowUntrusted && e.mem != nil && e.mem.Chain() != nil {
		net := e.mem.Chain().Network()
		if net == "main" || net == "testnet" {
			reqTrust = true
		}
	}
	maxSats := in.MaxSats
	if maxSats == 0 {
		var maxPolicyDeal uint64 = 500
		if e.policy != nil {
			_, mpd, _, _, _, _, _ := e.policy.Snapshot()
			if mpd > 0 {
				maxPolicyDeal = mpd
			}
		}
		qTok := uint64(DefaultTokenCounter().CountTokens(in.Query))
		bid := ComputeAutoBidSats(ArbitrageBidInput{
			EstTokensSaved:    DefaultEstimatedReasoningTokens + qTok*10,
			MaxPolicyDealSats: maxPolicyDeal,
			SellerBonded:      true,
		})
		maxSats = bid.MaxBidSats
	}
	return standingInitConfig{RequireTrust: reqTrust, MaxSats: maxSats}, nil
}

func (a *workflowActivities) StandingOrderRegisterAndAnnounce(ctx context.Context, st StandingOrderState) (bool, error) {
	a.eng.saveStandingOrderState(st)
	a.eng.upsertStandingOrderIndex(st)
	if br := a.eng.getBroker(); br != nil {
		_ = br.PublishWantForStandingOrderNMU(ctx, st)
	}
	return true, nil
}

func (a *workflowActivities) StandingOrderSaveState(_ context.Context, st StandingOrderState, clearInbox bool) (bool, error) {
	a.eng.saveStandingOrderState(st)
	a.eng.upsertStandingOrderIndex(st)
	if clearInbox {
		a.eng.clearStandingOrderInbox(st.OrderID)
	}
	return true, nil
}

func (a *workflowActivities) StandingOrderEvalOffer(ctx context.Context, st StandingOrderState, off bstack.OfferAd) (standingOfferEvalResult, error) {
	e := a.eng
	buyerPub := off.BuyerPub
	if buyerPub == "" && e.mem != nil && e.mem.Chain() != nil {
		buyerPub = e.mem.Chain().IdentityPubHex()
	}
	if st.OntologyID != "" && !standingOrderOntologyMatches(ctx, e.mem, st.OntologyID, st.AllowBridge, off.OntologyID) {
		return standingOfferEvalResult{Accepted: false, BuyerPub: buyerPub}, nil
	}
	if off.IsHub || e.mem == nil || e.mem.DB() == nil || off.Pub == "" {
		return standingOfferEvalResult{Accepted: true, BuyerPub: buyerPub}, nil
	}
	var bonds []reputation.PeerBondInfo
	if off.BondOutpoint != "" && off.BondBEEFHex != "" {
		if bSats := VerifyOfferBondSats(ctx, off, e.mem.Chain()); bSats > 0 {
			bonds = append(bonds, reputation.PeerBondInfo{
				Pub: off.Pub, BondSats: bSats, Status: "active",
			})
		}
	}
	evalOnt := st.OntologyID
	if evalOnt == "" {
		evalOnt = off.OntologyID
	}
	tEval, _, err := EvaluateTrustForOntology(e.mem.DB(), e.salePubHex(), evalOnt, e.policy, bonds, nil)
	if err != nil || tEval == nil {
		return standingOfferEvalResult{Accepted: !st.RequireTrustedOrBonded, BuyerPub: buyerPub}, nil
	}
	pubLower := strings.ToLower(strings.TrimSpace(off.Pub))
	if tEval.SybilOrUntrusted[pubLower] {
		return standingOfferEvalResult{Accepted: false, BuyerPub: buyerPub}, nil
	}
	if st.RequireTrustedOrBonded && !tEval.TrustedSellers[pubLower] {
		return standingOfferEvalResult{Accepted: false, BuyerPub: buyerPub}, nil
	}
	return standingOfferEvalResult{Accepted: true, BuyerPub: buyerPub}, nil
}
