package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/eventbus"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
)

// ExecuteBuyerDealDirect executes the zero-conf P2P BuyerDeal pipeline directly in-process
// without workflows.sqlite history serialization whenever no interactive operator approval,
// StandingOrder suspension, or async NAT claim wait is required. All crash-safety invariants
// (CustodyReserve, pre-broadcast SaveDealWithObligations, SpendGuard, and SettleDealAndFulfillObligations)
// execute identically against nektar.db and wallet.sqlite.
// Returns (state, handled=true, err) when completed directly, or (nil, false, nil) to fall back to durable go-workflows.
func (e *Engine) ExecuteBuyerDealDirect(ctx context.Context, dealID string, in BuyerDealInput) (*DealState, bool, error) {
	if e == nil || in.StandingTTL > 0 {
		return nil, false, nil
	}
	acts := &workflowActivities{eng: e}
	want := in.Want
	if want.WantID == "" {
		want.WantID = dealID
	}
	eval, err := acts.BuyerEvalPolicy(ctx, dealID, want, in.ApprovedOverrideID)
	if err != nil {
		return nil, true, err
	}
	if eval.NeedsApproval {
		return nil, false, nil
	}
	st := &DealState{DealID: dealID, Role: "buyer", Status: "open", Want: want, UpdatedAt: eval.NowUnix}
	if eval.KillSwitch {
		st.Status = "failed"
		st.Error = "market: operator kill switch is engaged"
		return st, true, nil
	}

	collectRes, err := acts.BuyerCollectOffers(ctx, dealID, want, in)
	if err != nil {
		return st, true, err
	}
	if collectRes.NoOffers && len(in.Offers) == 0 && len(collectRes.ActiveHosts) > 1 {
		var (
			mu   sync.Mutex
			seen = make(map[string]bool)
			out  []bstack.OfferAd
			wg   sync.WaitGroup
		)
		for _, h := range collectRes.ActiveHosts {
			wg.Add(1)
			go func(hostURL string) {
				defer wg.Done()
				batch, lErr := acts.LookupOffersSingleHost(ctx, hostURL, dealID, want.Pub)
				if lErr != nil {
					return
				}
				mu.Lock()
				for _, off := range batch {
					if off.OfferID != "" && seen[off.OfferID] {
						continue
					}
					if off.OfferID != "" {
						seen[off.OfferID] = true
					}
					out = append(out, off)
				}
				mu.Unlock()
			}(h)
		}
		wg.Wait()
		if len(out) > 0 {
			collectRes.Offers = out
			collectRes.NoOffers = false
			collectRes.Error = ""
		}
	}
	if collectRes.NoOffers {
		st.Status = "expired"
		st.Error = collectRes.Error
		st.UpdatedAt = collectRes.NowUnix
		return st, true, nil
	}
	if collectRes.PolicyRejected || len(collectRes.Offers) == 0 {
		st.Status = "failed"
		st.Error = collectRes.Error
		st.UpdatedAt = collectRes.NowUnix
		return st, true, nil
	}

	rankedRes, err := acts.BuyerRankAndMatch(ctx, dealID, want, collectRes, eval.BypassPolicy, eval.ApprovedCapSats)
	if err != nil {
		return st, true, err
	}
	if rankedRes.NeedsEscalation {
		return nil, false, nil
	}
	if rankedRes.NoOffers {
		st.Status = "expired"
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
		return st, true, nil
	}
	if rankedRes.PolicyRejected || rankedRes.Chosen == nil {
		st.Status = "failed"
		st.MatchAudit = rankedRes.MatchAudit
		st.Error = rankedRes.Error
		st.UpdatedAt = rankedRes.NowUnix
		return st, true, nil
	}

	candidates := rankedRes.Candidates
	if len(candidates) == 0 {
		candidates = []bstack.OfferAd{*rankedRes.Chosen}
	}
	st.MatchAudit = rankedRes.MatchAudit

	for idx, cand := range candidates {
		attempt := idx + 1
		hasNext := idx+1 < len(candidates)
		chosen := cand
		chosenCopy := cand
		st.ChosenOffer = &chosenCopy
		st.ChirpURL = chosen.ChirpURL
		if e.FastJournal() == nil || strings.TrimSpace(chosen.SellerURL) == "" {
			wantRaw, _ := json.Marshal(want)
			if chosenRaw, mErr := json.Marshal(chosenCopy); mErr == nil {
				_, _ = acts.PersistDealState(ctx, store.DealRecord{
					DealID:          dealID,
					Role:            "buyer",
					Status:          "offered",
					WantJSON:        wantRaw,
					ChosenOfferJSON: chosenRaw,
					CounterpartyPub: chosen.Pub,
					PriceSats:       chosen.PriceSats,
					ChirpURL:        chosen.ChirpURL,
				})
			}
		}
		if chosen.PriceSats > 0 {
			res, cErr := acts.CustodyReserve(ctx, CustodyReserveInput{
				OperationID:     dealID,
				Attempt:         attempt,
				SellerPub:       chosen.Pub,
				Domains:         want.Domains,
				AmountSats:      chosen.PriceSats,
				BypassPolicy:    eval.BypassPolicy,
				ApprovedCapSats: eval.ApprovedCapSats,
			})
			if cErr != nil {
				return st, true, cErr
			}
			if !res.Approved {
				if hasNext && !strings.Contains(strings.ToLower(res.Reason), "kill switch") {
					continue
				}
				failNow, _ := acts.PersistDealState(ctx, store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "custody reservation denied: " + res.Reason,
				})
				st.Status = "failed"
				st.Error = "custody reservation denied: " + res.Reason
				st.UpdatedAt = failNow
				return st, true, nil
			}
		}

		var immediateClaim *ClaimSignal
		if chosen.PayAddress != "" || chosen.PayIdentityKey != "" {
			payRes, pErr := acts.BuyerBroadcastPayment(ctx, dealID, chosen)
			if pErr != nil {
				return st, true, pErr
			}
			if payRes.PreBroadcastErr != "" {
				if chosen.PriceSats > 0 {
					_, _ = acts.CustodyRelease(ctx, CustodyReleaseInput{OperationID: dealID, Attempt: attempt})
				}
				if hasNext {
					continue
				}
				failNow, _ := acts.PersistDealState(ctx, store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "payment failed: " + payRes.PreBroadcastErr,
				})
				st.Status = "failed"
				st.Error = "payment failed: " + payRes.PreBroadcastErr
				st.UpdatedAt = failNow
				return st, true, nil
			}
			if payRes.TxID != "" {
				st.PaymentTxID = payRes.TxID
				st.Status = "paid_awaiting_reveal"
				st.UpdatedAt = payRes.NowUnix
				if payRes.EscrowTxID != "" {
					st.EscrowTxID = payRes.EscrowTxID
					_, _ = acts.ArmEscrowGuardian(ctx, EscrowGuardianInput{
						DealID:      dealID,
						EscrowTxID:  payRes.EscrowTxID,
						RefundAfter: payRes.EscrowRefundAfter,
					})
				}
			}
			delivRes, dErr := acts.BuyerDeliverPurchase(ctx, dealID, chosen, payRes)
			if dErr != nil || delivRes.DeliveryErr != "" {
				errMsg := delivRes.DeliveryErr
				if dErr != nil && errMsg == "" {
					errMsg = dErr.Error()
				}
				if st.EscrowTxID == "" {
					failNow, _ := acts.BuyerRecordDeliveryFailure(ctx, dealID, chosen.Pub, errMsg)
					st.Status = "failed"
					st.Error = "purchase delivery failed: " + errMsg
					st.UpdatedAt = failNow
					return st, true, nil
				}
			} else {
				immediateClaim = delivRes.ImmediateClaim
			}
		}

		if immediateClaim == nil {
			// NAT bbox / over-cap confirmation wait: hand off to DeliverBuyerKey late-claim recovery via EventBus.
			waitDur := in.WaitTimeout
			if waitDur <= 0 {
				waitDur = 30 * time.Second
			}
			var sub *eventbus.Subscription
			var evCh <-chan eventbus.Event
			if e.mem != nil && e.mem.EventBus() != nil {
				sub = e.mem.EventBus().Subscribe([]string{"deal:" + dealID}, 8)
				if sub != nil {
					evCh = sub.Events()
					defer sub.Close()
				}
			}
			deadline := time.NewTimer(waitDur)
			poll := time.NewTicker(25 * time.Millisecond)
			settled := false
			for !settled {
				if fj := e.FastJournal(); fj != nil {
					if rec, ok := fj.GetDealWAL(dealID); ok && rec.Status == "settled" {
						ds := dealStateFromRecord(rec)
						deadline.Stop()
						poll.Stop()
						return &ds, true, nil
					}
				}
				if e.mem != nil && e.mem.DB() != nil {
					if rec, gErr := e.mem.DB().GetDeal(dealID); gErr == nil && rec.Status == "settled" {
						ds := dealStateFromRecord(rec)
						deadline.Stop()
						poll.Stop()
						return &ds, true, nil
					}
				}
				select {
				case <-ctx.Done():
					deadline.Stop()
					poll.Stop()
					return st, true, ctx.Err()
				case <-deadline.C:
					poll.Stop()
					expNow, _ := acts.BuyerRecordClaimExpired(ctx, dealID, chosen.Pub, st.PaymentTxID != "")
					st.Status = "expired"
					st.Error = "timed out waiting for seller claim reveal"
					st.UpdatedAt = expNow
					return st, true, nil
				case <-evCh:
				case <-poll.C:
				}
			}
		}

		verRes, err := acts.BuyerVerifyAndCommit(ctx, dealID, want, in.TargetShard, chosen, st.PaymentTxID, st.MatchAudit, *immediateClaim)
		if err != nil {
			return st, true, err
		}
		if verRes.FailedReason != "" {
			if st.PaymentTxID == "" && hasNext {
				if chosen.PriceSats > 0 {
					_, _ = acts.CustodyRelease(ctx, CustodyReleaseInput{OperationID: dealID, Attempt: attempt})
				}
				continue
			}
			_, _ = acts.BuyerRecordVerifyFailure(ctx, dealID, chosen.Pub, verRes.FailedReason)
			st.Status = "failed"
			st.Error = verRes.FailedReason
			st.UpdatedAt = verRes.NowUnix
			return st, true, nil
		}
		if st.EscrowTxID != "" {
			_, _ = acts.DisarmEscrowGuardian(ctx, st.EscrowTxID)
		}
		st.ClaimTxID = immediateClaim.ClaimTxID
		st.Shard = verRes.TargetShard
		st.DeliveredTxt = verRes.DeliveredText
		st.DealReceipt = verRes.DealReceipt
		st.Status = "settled"
		st.Error = ""
		st.UpdatedAt = verRes.NowUnix
		return st, true, nil
	}
	return st, true, nil
}

// Want publishes a signed WantAd across active hubs, collects & ranks verified offers,
// executes AtomicBEEF payment (direct HTTP or NAT bbox fallback), verifies the revealed
// BRC-369 content key + remote CHIRP closure + SPV Merkle ProofBundle, and commits the knowledge!
// Execution runs durably through the embedded pure-Go go-workflows BuyerDealWorkflow (or zero-overhead
// in-process fast path when NEKTAR_FAST_DEALS=1 and no suspension gate is needed).
func (b *Broker) Want(ctx context.Context, req WantRequest) (*store.DealRecord, error) {
	if len(req.Domains) == 0 {
		req.Domains = []string{"ops"}
	}
	if strings.TrimSpace(req.Question) == "" {
		return nil, errors.New("market: question is required")
	}
	if err := nmu.ValidateWantFilters(req.MinGrade, req.FreshnessClass, req.RequiredRights, req.PricingMode); err != nil {
		return nil, err
	}
	if req.MaxSats == 0 {
		var maxPolicyDeal uint64 = 500
		if b.engine != nil && b.engine.Policy() != nil {
			_, mpd, _, _, _, _, _ := b.engine.Policy().Snapshot()
			if mpd > 0 {
				maxPolicyDeal = mpd
			}
		}
		estTok := req.EstTokensSaved
		if estTok == 0 {
			qTok := uint64(DefaultTokenCounter().CountTokens(req.Question))
			estTok = DefaultEstimatedReasoningTokens + qTok*10
		}
		bid := ComputeAutoBidSats(ArbitrageBidInput{
			EstTokensSaved:    estTok,
			MaxPolicyDealSats: maxPolicyDeal,
			SellerBonded:      true,
		})
		req.MaxSats = bid.MaxBidSats
	}
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil, errors.New("market: no active hub hosts configured (join a hub first)")
	}

	wantDomains := ExpandDomainsWithCorrelations(ctx, b.Matcher(), req.Domains, req.Question, b.ActiveNetworkTopics())
	if len(wantDomains) > bstack.MaxDomains {
		wantDomains = wantDomains[:bstack.MaxDomains]
	}
	wantID := fmt.Sprintf("want-%s-%s", shortPub(b.PubHex()), randIDHex())
	want := bstack.WantAd{
		WantID:            wantID,
		Pub:               b.PubHex(),
		Net:               b.mem.Chain().Network(),
		Domains:           wantDomains,
		Query:             req.Question,
		OntologyID:        strings.ToLower(strings.TrimSpace(req.OntologyID)),
		MaxSats:           req.MaxSats,
		MinGrade:          string(nmu.NormalizeGrade(req.MinGrade)),
		MaxSatsPer1kUnits: req.MaxSatsPer1kUnits,
		FreshnessClass:    string(nmu.NormalizeFreshness(req.FreshnessClass)),
		RequiredRights:    string(nmu.NormalizeRights(req.RequiredRights)),
		PricingMode:       string(nmu.NormalizePricingMode(req.PricingMode)),
		Revision:          1,
		Exp:               time.Now().Add(30 * time.Minute).UnixMilli(),
	}

	fj := b.engine.FastJournal()
	if b.engine != nil && b.engine.Policy() != nil {
		if fj != nil {
			fj.CachedPolicySync(b.mem.DB(), b.engine.Policy())
		} else if err := b.engine.Policy().Refresh(); err != nil {
			return nil, err
		}
		if kill, _, _, _, _, _, _ := b.engine.Policy().Snapshot(); kill {
			return nil, errors.New("market: operator kill switch is engaged")
		}
	}

	dealInput := BuyerDealInput{
		Want:               want,
		TargetShard:        req.TargetShard,
		CollectTimeout:     req.CollectTimeout,
		WaitTimeout:        5 * time.Minute,
		StandingTTL:        time.Duration(req.StandingTTLSeconds) * time.Second,
		ApprovedOverrideID: req.ApprovedOverrideID,
	}
	if (os.Getenv("NEKTAR_FAST_DEALS") == "1" || fj != nil) && b.engine != nil {
		st, handled, dErr := b.engine.ExecuteBuyerDealDirect(ctx, wantID, dealInput)
		if handled {
			if st != nil && (st.Status == "failed" || st.Status == "expired") {
				return &store.DealRecord{
					DealID:        wantID,
					Role:          "buyer",
					Status:        st.Status,
					Shard:         st.Shard,
					PaymentTxID:   st.PaymentTxID,
					DeliveredText: st.DeliveredTxt,
					Error:         st.Error,
				}, dErr
			}
			if fj != nil {
				if rec, ok := fj.GetDealWAL(wantID); ok && rec.Status == "settled" {
					return &rec, dErr
				}
			} else if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
				return &rec, dErr
			}
			if dErr != nil {
				return nil, dErr
			}
			return &store.DealRecord{
				DealID:        wantID,
				Role:          "buyer",
				Status:        st.Status,
				Shard:         st.Shard,
				PaymentTxID:   st.PaymentTxID,
				DeliveredText: st.DeliveredTxt,
				Error:         st.Error,
			}, nil
		}
	}

	wc, err := b.EnsureWorkflowClient()
	if err != nil {
		return nil, err
	}

	// Subscribe to EventBus "deal:<wantID>" BEFORE starting workflow so approval_pending wakes in 0ms with zero polling (PERF-12)!
	var dealSub *eventbus.Subscription
	var dealCh <-chan eventbus.Event
	if b.mem != nil && b.mem.EventBus() != nil {
		if sub := b.mem.EventBus().Subscribe([]string{"deal:" + wantID}, 16); sub != nil {
			dealSub = sub
			dealCh = sub.Events()
			defer dealSub.Close()
		}
	}

	if _, err := wc.StartBuyerDealAsync(ctx, wantID, dealInput); err != nil {
		return nil, err
	}

	attachCtx, attachCancel := context.WithCancel(ctx)
	defer attachCancel()
	type attachRes struct {
		st  *DealState
		err error
	}
	attachCh := make(chan attachRes, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		st, aErr := wc.AttachBuyerDeal(attachCtx, wantID)
		attachCh <- attachRes{st: st, err: aErr}
	}()

	// Fallback check only when EventBus is not attached on memory.Service.
	var fallbackCh <-chan time.Time
	if dealCh == nil {
		fb := time.NewTicker(100 * time.Millisecond)
		defer fb.Stop()
		fallbackCh = fb.C
	}

	for {
		select {
		case ar := <-attachCh:
			if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
				return &rec, nil
			}
			if ar.err != nil {
				return nil, ar.err
			}
			return &store.DealRecord{
				DealID:        wantID,
				Role:          "buyer",
				Status:        ar.st.Status,
				Shard:         ar.st.Shard,
				PaymentTxID:   ar.st.PaymentTxID,
				DeliveredText: ar.st.DeliveredTxt,
				Error:         ar.st.Error,
			}, nil
		case <-dealCh:
			if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil && rec.Status == "approval_pending" {
				return &rec, nil
			}
		case <-fallbackCh:
			if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil && rec.Status == "approval_pending" {
				return &rec, nil
			}
		case <-ctx.Done():
			if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil {
				return &rec, ctx.Err()
			}
			return nil, ctx.Err()
		}
	}
}
