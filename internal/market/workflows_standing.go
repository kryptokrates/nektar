package market

import (
	"fmt"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/reputation"
)

func (e *Engine) restateStandingOrderRun(ctx restate.WorkflowContext, in StandingOrderInput) (StandingOrderState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return StandingOrderState{}, err
	}
	orderID := restate.Key(ctx)
	invID := ""
	if req := ctx.Request(); req != nil {
		invID = req.ID
	}

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

	initNow, err := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
		return time.Now().Unix(), nil
	}, restate.WithName("standing_order_init_time"))
	if err != nil {
		return StandingOrderState{}, err
	}

	doms := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		if s := strings.ToLower(strings.TrimSpace(d)); s != "" {
			doms = append(doms, s)
		}
	}
	if len(doms) == 0 {
		doms = []string{"ops"}
	}

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

	st := StandingOrderState{
		OrderID:                orderID,
		InvocationID:           invID,
		WaitingSeq:             1,
		Status:                 "watching",
		Domains:                doms,
		Query:                  strings.TrimSpace(in.Query),
		OntologyID:             strings.ToLower(strings.TrimSpace(in.OntologyID)),
		AllowBridge:            in.AllowBridge,
		RequireTrustedOrBonded: reqTrust,
		MaxSats:                maxSats,
		TargetShard:            strings.TrimSpace(in.TargetShard),
		AgentID:                strings.TrimSpace(in.AgentID),
		ExpiresAtUnix:          initNow + int64(ttl.Seconds()),
		MaxFills:               maxFills,
		CreatedAt:              initNow,
		UpdatedAt:              initNow,
	}
	restate.Set(ctx, "state", st)
	_, _ = restate.Run(ctx, func(rctx restate.RunContext) (bool, error) {
		e.upsertStandingOrderIndex(st)
		// Publish an initial WantAd across active hubs so currently online sellers quote immediately.
		if br := e.getBroker(); br != nil {
			_ = br.PublishWantForStandingOrder(rctx, orderID, doms, st.Query, st.OntologyID, st.MaxSats)
		}
		return true, nil
	}, restate.WithName("standing_order_register_and_announce"))

	seenOffers := make(map[string]bool)
	drainStep := 0

	for st.Status == "watching" || st.Status == "paused" {
		nowUnix, nErr := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			return time.Now().Unix(), nil
		}, restate.WithName(fmt.Sprintf("standing_clock_%d", drainStep)))
		if nErr != nil {
			return st, nErr
		}
		if nowUnix >= st.ExpiresAtUnix {
			st.Status = "expired"
			st.UpdatedAt = nowUnix
			break
		}

		// Peek queued events non-destructively BEFORE sleeping so a crash before Restate journals
		// the batch NEVER drops events! Events are acknowledged only after durable state update.
		drainStep++
		afterSeq := st.LastProcessedSeq
		batch, dErr := restate.Run(ctx, func(_ restate.RunContext) ([]standingOrderEvent, error) {
			return e.peekStandingOrderEvents(orderID, afterSeq), nil
		}, restate.WithName(fmt.Sprintf("standing_drain_inbox_%d", drainStep)))
		if dErr != nil {
			return st, dErr
		}

		if len(batch) == 0 {
			remaining := time.Duration(st.ExpiresAtUnix-nowUnix) * time.Second
			if remaining <= 0 {
				remaining = time.Second
			}
			// Park at 0% CPU / 0 MB RAM waiting for the next wake signal OR TTL expiry.
			wakeName := fmt.Sprintf("wake_%d", st.WaitingSeq)
			wakeFut := restate.Signal[bool](ctx, wakeName)
			expFut := restate.After(ctx, remaining)
			winner, wErr := restate.WaitFirst(ctx, wakeFut, expFut)
			if wErr != nil {
				return st, wErr
			}
			if winner == expFut {
				expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
					return time.Now().Unix(), nil
				}, restate.WithName(fmt.Sprintf("standing_expire_%d", st.WaitingSeq)))
				st.Status = "expired"
				st.UpdatedAt = expNow
				break
			}
			st.WaitingSeq++
			restate.Set(ctx, "state", st)
			// Loop back to peek ALL events that arrived before or with this wake signal!
			continue
		}

		// Record monotonic cursor BEFORE acknowledging SQLite inbox so replay never re-processes or drops events.
		var batchMaxSeq int64 = st.LastProcessedSeq
		for _, ev := range batch {
			if ev.Seq > batchMaxSeq {
				batchMaxSeq = ev.Seq
			}
		}
		if batchMaxSeq > st.LastProcessedSeq {
			st.LastProcessedSeq = batchMaxSeq
			restate.Set(ctx, "state", st)
			_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
				e.ackStandingOrderEvents(orderID, batchMaxSeq)
				return true, nil
			}, restate.WithName(fmt.Sprintf("standing_ack_inbox_%d", drainStep)))
		}

		// Process every drained event in deterministic FIFO order.
		for _, ev := range batch {
			switch ev.Kind {
			case "cancel":
				cNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
					return time.Now().Unix(), nil
				}, restate.WithName(fmt.Sprintf("standing_cancel_%d", drainStep)))
				st.Status = "cancelled"
				st.Error = strings.TrimSpace(ev.Reason)
				st.UpdatedAt = cNow
				restate.Set(ctx, "state", st)
				_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
					e.upsertStandingOrderIndex(st)
					e.clearStandingOrderInbox(orderID)
					return true, nil
				}, restate.WithName("standing_cancel_index"))
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
				restate.Set(ctx, "state", st)
				_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
					e.upsertStandingOrderIndex(st)
					return true, nil
				}, restate.WithName(fmt.Sprintf("standing_steer_index_%d", drainStep)))

			case "offer":
				if ev.Offer == nil || st.Status != "watching" {
					restate.Set(ctx, "state", st)
					continue
				}
				off := *ev.Offer
				if off.OfferID != "" && seenOffers[off.OfferID] {
					restate.Set(ctx, "state", st)
					continue
				}
				if off.OfferID != "" {
					seenOffers[off.OfferID] = true
				}
				st.OffersSeen++
				if st.MaxSats > 0 && off.PriceSats > st.MaxSats {
					st.UpdatedAt = nowUnix
					restate.Set(ctx, "state", st)
					continue
				}
				if !standingOrderDomainMatches(st.Domains, off.Domains) {
					st.UpdatedAt = nowUnix
					restate.Set(ctx, "state", st)
					continue
				}
				if st.OntologyID != "" {
					ontOK, oErr := restate.Run(ctx, func(rctx restate.RunContext) (bool, error) {
						return standingOrderOntologyMatches(rctx, e.mem, st.OntologyID, st.AllowBridge, off.OntologyID), nil
					}, restate.WithName(fmt.Sprintf("standing_ont_match_%d_%d", drainStep, st.OffersSeen)))
					if oErr != nil || !ontOK {
						st.UpdatedAt = nowUnix
						restate.Set(ctx, "state", st)
						continue
					}
				}
				// Automated Sybil & reputation guard via Google Mangle stratified Datalog:
				// always block sybil_or_untrusted sellers; when RequireTrustedOrBonded is enabled,
				// auto-spend ONLY with Mangle trusted_seller/2 peers (bonded or above 6D+EigenTrust threshold) or Hub makers.
				trustOK, tErr := restate.Run(ctx, func(rctx restate.RunContext) (bool, error) {
					if off.IsHub || e.mem == nil || e.mem.DB() == nil || off.Pub == "" {
						return true, nil
					}
					var bonds []reputation.PeerBondInfo
					if off.BondOutpoint != "" && off.BondBEEFHex != "" {
						if bSats := VerifyOfferBondSats(rctx, off, e.mem.Chain()); bSats > 0 {
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
						return !st.RequireTrustedOrBonded, nil
					}
					pubLower := strings.ToLower(strings.TrimSpace(off.Pub))
					if tEval.SybilOrUntrusted[pubLower] {
						return false, nil
					}
					if st.RequireTrustedOrBonded && !tEval.TrustedSellers[pubLower] {
						return false, nil
					}
					return true, nil
				}, restate.WithName(fmt.Sprintf("standing_trust_gate_%d_%d", drainStep, st.OffersSeen)))
				if tErr != nil || !trustOK {
					st.UpdatedAt = nowUnix
					restate.Set(ctx, "state", st)
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
				restate.Set(ctx, "state", st)

				wantAd := bstack.WantAd{
					WantID:     dealID,
					Domains:    st.Domains,
					Query:      st.Query,
					OntologyID: st.OntologyID,
					MaxSats:    st.MaxSats,
					Pub:        off.BuyerPub,
				}
				if wantAd.Pub == "" && e.mem != nil && e.mem.Chain() != nil {
					wantAd.Pub = e.mem.Chain().IdentityPubHex()
				}

				// Invoke BuyerDeal durably with deterministic internal auth headers.
				var reqOpts []restate.RequestOption
				if hdrs := e.internalCallHeaders(); hdrs != nil {
					reqOpts = append(reqOpts, restate.WithHeaders(hdrs))
				}
				dealRes, dErr := restate.Workflow[DealState](ctx, "BuyerDeal", dealID, "run").
					Request(BuyerDealInput{
						Want:            wantAd,
						Offers:          []bstack.OfferAd{off},
						TargetShard:     st.TargetShard,
						WaitTimeout:     in.WaitTimeout,
						ApprovalTimeout: in.ApprovalTimeout,
					}, reqOpts...)

				doneNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
					return time.Now().Unix(), nil
				}, restate.WithName(fmt.Sprintf("standing_attempt_done_%d", st.Attempts)))
				st.UpdatedAt = doneNow

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
					// Recurring StandingOrder (maxFills > 1): keep watching for additional matching offers!
					st.Status = "watching"
					restate.Set(ctx, "state", st)
					continue
				}
				// Seller failed or rejected — keep the StandingOrder alive for the next matching offer!
				st.Status = "watching"
				if dErr != nil {
					st.Error = dErr.Error()
				} else if dealRes.Error != "" {
					st.Error = dealRes.Error
				}
				restate.Set(ctx, "state", st)
			}
			if st.Status != "watching" && st.Status != "paused" {
				break
			}
		}
	}

	restate.Set(ctx, "state", st)
	_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		e.upsertStandingOrderIndex(st)
		e.clearStandingOrderInbox(orderID)
		return true, nil
	}, restate.WithName("standing_order_finalize_index"))
	return st, nil
}

func (e *Engine) restateStandingOrderOnOffer(ctx restate.WorkflowSharedContext, off bstack.OfferAd) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	orderID := restate.Key(ctx)
	st, err := restate.Get[StandingOrderState](ctx, "state")
	if err != nil || st.InvocationID == "" {
		// Workflow is still initializing state: enqueue durably AND return retryable error so
		// idempotency keys never cache a permanent false no-op before initial restate.Set commits!
		_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
			e.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "offer", Offer: &off})
			return true, nil
		}, restate.WithName("standing_enqueue_offer_early"))
		return false, fmt.Errorf("standing order %s initializing, retry", orderID)
	}
	if st.Status != "watching" && st.Status != "paused" && st.Status != "matching" {
		return false, nil
	}
	if _, err := restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		e.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "offer", Offer: &off})
		return true, nil
	}, restate.WithName("standing_enqueue_offer")); err != nil {
		return false, err
	}
	// Resolve BOTH wake_<WaitingSeq> and wake_<WaitingSeq+1> so a concurrent wake transition
	// between drain and WaitFirst can NEVER strand an already-enqueued event until TTL!
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("wake_%d", st.WaitingSeq), true)
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("wake_%d", st.WaitingSeq+1), true)
	return true, nil
}

func (e *Engine) restateStandingOrderSteer(ctx restate.WorkflowSharedContext, steer SteerStandingOrderInput) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	orderID := restate.Key(ctx)
	st, err := restate.Get[StandingOrderState](ctx, "state")
	if err != nil || st.InvocationID == "" {
		_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
			e.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "steer", Steer: &steer})
			return true, nil
		}, restate.WithName("standing_enqueue_steer_early"))
		return false, fmt.Errorf("standing order %s initializing, retry", orderID)
	}
	if st.Status != "watching" && st.Status != "paused" && st.Status != "matching" {
		return false, nil
	}
	if _, err := restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		e.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "steer", Steer: &steer})
		return true, nil
	}, restate.WithName("standing_enqueue_steer")); err != nil {
		return false, err
	}
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("wake_%d", st.WaitingSeq), true)
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("wake_%d", st.WaitingSeq+1), true)
	return true, nil
}

func (e *Engine) restateStandingOrderCancel(ctx restate.WorkflowSharedContext, reason string) (bool, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return false, err
	}
	orderID := restate.Key(ctx)
	st, err := restate.Get[StandingOrderState](ctx, "state")
	if err != nil || st.InvocationID == "" {
		_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
			e.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "cancel", Reason: reason})
			return true, nil
		}, restate.WithName("standing_enqueue_cancel_early"))
		return false, fmt.Errorf("standing order %s initializing, retry", orderID)
	}
	if st.Status != "watching" && st.Status != "paused" && st.Status != "matching" {
		return false, nil
	}
	if _, err := restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		e.enqueueStandingOrderEvent(orderID, standingOrderEvent{Kind: "cancel", Reason: reason})
		return true, nil
	}, restate.WithName("standing_enqueue_cancel")); err != nil {
		return false, err
	}
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("wake_%d", st.WaitingSeq), true)
	restate.ResolveSignal(ctx, st.InvocationID, fmt.Sprintf("wake_%d", st.WaitingSeq+1), true)
	return true, nil
}

func (e *Engine) restateStandingOrderStatus(ctx restate.WorkflowSharedContext, _ restate.Void) (StandingOrderState, error) {
	if err := e.checkInternalAuthReq(ctx.Request()); err != nil {
		return StandingOrderState{}, err
	}
	return restate.Get[StandingOrderState](ctx, "state")
}
