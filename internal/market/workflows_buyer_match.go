package market

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
)

func (e *Engine) buyerEvalPolicy(dealID string, want bstack.WantAd, approvedOverrideID string) (buyerPolicyEval, error) {
	now := time.Now().Unix()
	if e.mem != nil && e.mem.DB() != nil {
		_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
	}
	kill, maxDeal, maxDay, _, spent, buyerOK, _ := e.policy.Snapshot()
	if kill {
		wantJSON, _ := json.Marshal(want)
		e.persistDealRecord(store.DealRecord{
			DealID:   dealID,
			Role:     "buyer",
			Status:   "failed",
			WantJSON: wantJSON,
			Error:    "market: operator kill switch is engaged",
		})
		return buyerPolicyEval{NowUnix: now, KillSwitch: true}, nil
	}
	bypass := false
	if approvedOverrideID != "" {
		if e.mem != nil && e.mem.DB() != nil {
			if cErr := e.mem.DB().ConsumeApprovalOverride(approvedOverrideID); cErr != nil {
				return buyerPolicyEval{}, restate.ToTerminalError(fmt.Errorf("market: invalid or already consumed approval override %q: %w", approvedOverrideID, cErr))
			}
		}
		bypass = true
	}
	// Always enforce operator buyer approval & caps regardless of whether in.Offers were pre-supplied.
	needsApproval := false
	reason := ""
	if !bypass {
		domainErr := e.policy.CanSpendForDomains("", want.Domains, 0)
		if !buyerOK {
			needsApproval = true
			reason = "no active buyer policy approved (run `nektar policy approve --preset buyer` or approve in Dashboard)"
		} else if domainErr != nil {
			needsApproval = true
			reason = domainErr.Error()
		} else if maxDeal == 0 || want.MaxSats > maxDeal {
			needsApproval = true
			reason = fmt.Sprintf("requested maxSats (%d) exceeds buyer policy maxPerDeal (%d)", want.MaxSats, maxDeal)
		} else if maxDay == 0 || spent+want.MaxSats > maxDay {
			var rem uint64
			if maxDay > spent {
				rem = maxDay - spent
			}
			needsApproval = true
			reason = fmt.Sprintf("requested maxSats (%d) exceeds remaining daily cap (%d)", want.MaxSats, rem)
		}
	}
	return buyerPolicyEval{
		NowUnix:        now,
		NeedsApproval:  needsApproval,
		ApprovalReason: reason,
		BypassPolicy:   bypass,
	}, nil
}

func (e *Engine) buyerCollectOffers(rctx restate.RunContext, dealID string, want bstack.WantAd, in BuyerDealInput) (buyerCollectResult, error) {
	now := time.Now().Unix()
	offers := in.Offers
	var firstOfferMs int64
	wantJSON, _ := json.Marshal(want)
	if len(offers) > 0 {
		// Strictly require and verify signed SaleOffer bound to want.Pub, sellerPub, price, and keyCommitment.
		verified := make([]bstack.OfferAd, 0, len(offers))
		for _, off := range offers {
			if len(off.SaleOffer) == 0 {
				continue
			}
			var so contracts.KeyedSaleOffer
			if json.Unmarshal(off.SaleOffer, &so) != nil {
				continue
			}
			buyerPub := want.Pub
			if buyerPub == "" {
				buyerPub = off.BuyerPub
			}
			if contracts.VerifySaleOffer(&so, buyerPub, off.Leaf) != nil {
				continue
			}
			if off.Pub != "" && !strings.EqualFold(so.SellerPub, off.Pub) {
				continue
			}
			if so.Config.Price != off.PriceSats {
				continue
			}
			if off.KeyCommitHex != "" && !strings.EqualFold(so.KeyCommitment, off.KeyCommitHex) {
				continue
			}
			verified = append(verified, off)
		}
		offers = verified
	}
	if len(offers) == 0 {
		br := e.getBroker()
		if br == nil {
			e.persistDealRecord(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "failed",
				WantJSON: wantJSON,
				Error:    "no eligible offers under policy",
			})
			return buyerCollectResult{NowUnix: now, PolicyRejected: true, Error: "no eligible offers under policy"}, nil
		}
		e.persistDealRecord(store.DealRecord{
			DealID:   dealID,
			Role:     "buyer",
			Status:   "collecting",
			WantJSON: wantJSON,
		})
		collected, fms, cErr := br.CollectOffersForWant(rctx, want, in.CollectTimeout)
		if cErr != nil {
			e.persistDealRecord(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "failed",
				WantJSON: wantJSON,
				Error:    cErr.Error(),
			})
			return buyerCollectResult{NowUnix: now, PolicyRejected: true, Error: cErr.Error()}, nil
		}
		offers = collected
		firstOfferMs = fms
		if len(offers) == 0 {
			dom := "ops"
			if len(want.Domains) > 0 {
				dom = want.Domains[0]
			}
			if e.mem != nil && e.mem.DB() != nil {
				_ = e.mem.DB().RecordUnmatchedWant(store.UnmatchedWantRecord{
					WantID:  dealID,
					Domain:  dom,
					Query:   want.Query,
					MaxSats: want.MaxSats,
				})
			}
			msg := fmt.Sprintf("no valid offers received for domain %q", dom)
			e.persistDealRecord(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "expired",
				WantJSON: wantJSON,
				Error:    msg,
			})
			return buyerCollectResult{NowUnix: now, NoOffers: true, Error: msg}, nil
		}
	}
	return buyerCollectResult{
		NowUnix:      now,
		Offers:       offers,
		FirstOfferMs: firstOfferMs,
	}, nil
}

func (e *Engine) buyerRankAndMatch(rctx restate.RunContext, dealID string, want bstack.WantAd, collectRes buyerCollectResult, bypassPolicy bool) (buyerRankResult, error) {
	now := time.Now().Unix()
	wantJSON, _ := json.Marshal(want)
	if e.mem != nil && e.mem.DB() != nil {
		_ = SyncPolicyFromDB(e.mem.DB(), e.policy)
	}
	reps := make(map[string]store.SellerReputation)
	if e.mem != nil && e.mem.DB() != nil {
		if list, lErr := e.mem.DB().ListReputations(); lErr == nil {
			for _, r := range list {
				reps[r.SellerPub] = r
			}
		}
	}
	var rankPol *Policy
	if !bypassPolicy {
		rankPol = e.policy
	}
	verifiedBonds := make(map[string]uint64)
	var port *chain.Port
	if e.mem != nil {
		port = e.mem.Chain()
	}
	for _, off := range collectRes.Offers {
		if off.BondOutpoint != "" && off.BondBEEFHex != "" {
			if sats := VerifyOfferBondSats(rctx, off, port); sats > 0 {
				verifiedBonds[off.OfferID] = sats
			}
		}
	}
	ranked := RankOffersWithRepAndBonds(want, collectRes.Offers, rankPol, reps, verifiedBonds)
	if len(ranked) == 0 {
		msg := "no eligible offers under policy"
		e.persistDealRecord(store.DealRecord{
			DealID:   dealID,
			Role:     "buyer",
			Status:   "failed",
			WantJSON: wantJSON,
			Error:    msg,
		})
		return buyerRankResult{NowUnix: now, PolicyRejected: true, Error: msg}, nil
	}

	chosen := ranked[0].Offer
	var matchAudit *MatchAudit
	var matchAuditJSON []byte
	if m := e.Matcher(); m != nil {
		mDec := m.MatchOffersForBuyer(rctx, want, ranked, reps, e.policy)
		maCopy := mDec.Audit
		matchAudit = &maCopy
		matchAuditJSON, _ = json.Marshal(maCopy)
		if mDec.Verdict == "reject" || mDec.Chosen == nil {
			msg := mDec.Reason
			if msg == "" {
				msg = "no offers passed semantic matching policy"
			}
			e.persistDealRecord(store.DealRecord{
				DealID:         dealID,
				Role:           "buyer",
				Status:         "failed",
				WantJSON:       wantJSON,
				MatchAuditJSON: matchAuditJSON,
				Error:          msg,
			})
			return buyerRankResult{NowUnix: now, PolicyRejected: true, Error: msg, MatchAudit: matchAudit}, nil
		}
		chosen = *mDec.Chosen
		if mDec.Verdict == "escalate" {
			chosenJSON, _ := json.Marshal(chosen)
			e.persistDealRecord(store.DealRecord{
				DealID:          dealID,
				Role:            "buyer",
				Status:          "approval_pending",
				WantJSON:        wantJSON,
				ChosenOfferJSON: chosenJSON,
				MatchAuditJSON:  matchAuditJSON,
				CounterpartyPub: chosen.Pub,
				PriceSats:       chosen.PriceSats,
				ChirpURL:        chosen.ChirpURL,
				IsHubSeller:     chosen.IsHub,
				FirstOfferMs:    collectRes.FirstOfferMs,
				Error:           mDec.Reason,
			})
			return buyerRankResult{
				NowUnix:          now,
				NeedsEscalation:  true,
				EscalationReason: mDec.Reason,
				Chosen:           &chosen,
				MatchAudit:       matchAudit,
				FirstOfferMs:     collectRes.FirstOfferMs,
			}, nil
		}
	}

	chosenJSON, _ := json.Marshal(chosen)
	e.persistDealRecord(store.DealRecord{
		DealID:          dealID,
		Role:            "buyer",
		Status:          "offered",
		WantJSON:        wantJSON,
		ChosenOfferJSON: chosenJSON,
		MatchAuditJSON:  matchAuditJSON,
		CounterpartyPub: chosen.Pub,
		PriceSats:       chosen.PriceSats,
		ChirpURL:        chosen.ChirpURL,
		IsHubSeller:     chosen.IsHub,
		FirstOfferMs:    collectRes.FirstOfferMs,
	})
	return buyerRankResult{
		NowUnix:      now,
		Chosen:       &chosen,
		MatchAudit:   matchAudit,
		FirstOfferMs: collectRes.FirstOfferMs,
	}, nil
}

func (e *Engine) buyerAwaitMatchEscalation(ctx restate.WorkflowContext, dealID string, chosen bstack.OfferAd, escalationReason string, approvalTimeout time.Duration, st *DealState) (bool, error) {
	escTimeout := approvalTimeout
	if escTimeout <= 0 {
		escTimeout = 15 * time.Minute
	}
	escAwk := restate.Awakeable[ApprovalDecision](ctx)
	escID := "appr-" + strings.ReplaceAll(restate.UUID(ctx).String(), "-", "")[:16]
	escAwkID := escAwk.Id()

	escNow, eErr := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
		now := time.Now().Unix()
		if e.mem != nil && e.mem.DB() != nil {
			_ = e.mem.DB().SaveApproval(store.ApprovalRecord{
				ID:           escID,
				DealID:       dealID,
				Reason:       escalationReason,
				MaxExtraSats: chosen.PriceSats,
				Status:       "pending",
				AwakeableID:  escAwkID,
				ExpiresAt:    now + int64(escTimeout.Seconds()),
			})
		}
		e.persistDealRecord(store.DealRecord{
			DealID:    dealID,
			Role:      "buyer",
			Status:    "approval_pending",
			PriceSats: chosen.PriceSats,
			Error:     escalationReason + " [approvalId=" + escID + "]",
		})
		if br := e.getBroker(); br != nil {
			br.notify("approval_pending", map[string]any{"approvalId": escID, "dealId": dealID, "reason": escalationReason, "awakeableId": escAwkID})
		}
		return now, nil
	}, restate.WithName("buyer_persist_match_escalation"))
	if eErr != nil {
		return false, eErr
	}

	st.Status = "approval_pending"
	st.ApprovalID = escID
	st.AwakeableID = escAwkID
	st.Error = escalationReason + " [approvalId=" + escID + "]"
	st.UpdatedAt = escNow
	restate.Set(ctx, "state", *st)

	escTimer := restate.After(ctx, escTimeout)
	winner, waitErr := restate.WaitFirst(ctx, escAwk, escTimer)
	if waitErr != nil {
		return false, waitErr
	}
	if winner == escTimer {
		expNow, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			now := time.Now().Unix()
			e.persistDealRecord(store.DealRecord{
				DealID: dealID,
				Role:   "buyer",
				Status: "expired",
				Error:  "semantic match escalation timed out",
			})
			return now, nil
		}, restate.WithName("buyer_match_escalation_expired"))
		st.Status = "expired"
		st.Error = "semantic match escalation timed out"
		st.UpdatedAt = expNow
		restate.Set(ctx, "state", *st)
		return false, nil
	}
	dec, dErr := escAwk.Result()
	if dErr != nil || !dec.Approved {
		reason := "semantic match denied by operator"
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
		}, restate.WithName("buyer_match_escalation_denied"))
		st.Status = "failed"
		st.Error = reason
		st.UpdatedAt = denyNow
		restate.Set(ctx, "state", *st)
		return false, nil
	}
	_, _ = restate.Run(ctx, func(_ restate.RunContext) (bool, error) {
		if e.mem != nil && e.mem.DB() != nil && st.ApprovalID != "" {
			_, _ = e.mem.DB().MarkApprovalResolvedAwakeable(st.ApprovalID, true)
		}
		e.persistDealRecord(store.DealRecord{
			DealID: dealID,
			Role:   "buyer",
			Status: "offered",
			Error:  "",
		})
		return true, nil
	}, restate.WithName("buyer_match_escalation_consume"))
	return true, nil
}
