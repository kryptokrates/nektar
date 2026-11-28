package market

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/workflow"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

func (a *workflowActivities) BuyerEvalPolicy(_ context.Context, dealID string, want bstack.WantAd, approvedOverrideID string) (buyerPolicyEval, error) {
	return a.eng.buyerEvalPolicy(dealID, want, approvedOverrideID)
}

func (a *workflowActivities) BuyerCollectOffers(ctx context.Context, dealID string, want bstack.WantAd, in BuyerDealInput) (buyerCollectResult, error) {
	return a.eng.buyerCollectOffers(ctx, dealID, want, in)
}

func (a *workflowActivities) BuyerRankAndMatch(ctx context.Context, dealID string, want bstack.WantAd, collectRes buyerCollectResult, bypassPolicy bool, approvedCapSats uint64) (buyerRankResult, error) {
	return a.eng.buyerRankAndMatch(ctx, dealID, want, collectRes, bypassPolicy, approvedCapSats)
}

func (e *Engine) buyerEvalPolicy(dealID string, want bstack.WantAd, approvedOverrideID string) (buyerPolicyEval, error) {
	now := time.Now().Unix()
	fj := e.FastJournal()
	if e.policy != nil {
		if fj != nil {
			fj.CachedPolicySync(e.mem.DB(), e.policy)
		} else if err := e.policy.Refresh(); err != nil {
			return buyerPolicyEval{}, err
		}
	}
	kill, maxDeal, maxDay, _, spent, buyerOK, _ := e.policy.Snapshot()
	if kill {
		wantJSON, _ := json.Marshal(want)
		if err := e.persistDealRecordChecked(store.DealRecord{
			DealID:   dealID,
			Role:     "buyer",
			Status:   "failed",
			WantJSON: wantJSON,
			Error:    "market: operator kill switch is engaged",
		}); err != nil {
			return buyerPolicyEval{}, err
		}
		return buyerPolicyEval{NowUnix: now, KillSwitch: true}, nil
	}
	bypass := false
	var approvedCap uint64
	if approvedOverrideID != "" {
		if e.mem != nil && e.mem.DB() != nil {
			// STATE-7 fix: bind override consumption strictly to (dealID, MaxExtraSats) instead of infinite cap!
			appr, err := e.mem.DB().GetApproval(approvedOverrideID)
			if err != nil {
				return buyerPolicyEval{}, workflow.NewPermanentError(fmt.Errorf("market: invalid approval override %q: %w", approvedOverrideID, err))
			}
			boundDealID := appr.DealID
			if boundDealID == "" {
				boundDealID = dealID
			}
			if cErr := e.mem.DB().ConsumeApprovalOverrideBounded(approvedOverrideID, boundDealID, want.MaxSats, 0); cErr != nil {
				return buyerPolicyEval{}, workflow.NewPermanentError(fmt.Errorf("market: invalid or already consumed approval override %q: %w", approvedOverrideID, cErr))
			}
			approvedCap = appr.MaxExtraSats
			if approvedCap == 0 {
				approvedCap = want.MaxSats
			}
		} else {
			approvedCap = want.MaxSats
		}
		bypass = true
	}

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
		NowUnix:         now,
		NeedsApproval:   needsApproval,
		ApprovalReason:  reason,
		BypassPolicy:    bypass,
		ApprovedCapSats: approvedCap,
	}, nil
}

func (e *Engine) buyerCollectOffers(rctx context.Context, dealID string, want bstack.WantAd, in BuyerDealInput) (buyerCollectResult, error) {
	now := time.Now().Unix()
	offers := in.Offers
	var firstOfferMs int64
	wantJSON, _ := json.Marshal(want)
	if len(offers) > 0 {
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
			if want.OntologyID != "" && !standingOrderOntologyMatches(rctx, e.mem, want.OntologyID, true, off.OntologyID) {
				continue
			}
			verified = append(verified, off)
		}
		offers = verified
	}
	var activeHosts []string
	if len(offers) == 0 {
		br := e.getBroker()
		if br == nil {
			if err := e.persistDealRecordChecked(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "failed",
				WantJSON: wantJSON,
				Error:    "no eligible offers under policy",
			}); err != nil {
				return buyerCollectResult{}, err
			}
			return buyerCollectResult{NowUnix: now, PolicyRejected: true, Error: "no eligible offers under policy"}, nil
		}
		if br.Discovery() != nil {
			activeHosts = append([]string(nil), br.Discovery().ActiveHosts()...)
			sort.Strings(activeHosts)
		}
		if e.FastJournal() == nil {
			if err := e.persistDealRecordChecked(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "collecting",
				WantJSON: wantJSON,
			}); err != nil {
				return buyerCollectResult{}, err
			}
		}
		collected, fms, cErr := br.CollectOffersForWant(rctx, want, in.CollectTimeout)
		if cErr != nil {
			_ = e.persistDealRecordChecked(store.DealRecord{
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
			if err := e.persistDealRecordChecked(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "expired",
				WantJSON: wantJSON,
				Error:    msg,
			}); err != nil {
				return buyerCollectResult{}, err
			}
			return buyerCollectResult{NowUnix: now, NoOffers: true, ActiveHosts: activeHosts, Error: msg}, nil
		}
	}
	return buyerCollectResult{
		NowUnix:      now,
		Offers:       offers,
		ActiveHosts:  activeHosts,
		FirstOfferMs: firstOfferMs,
	}, nil
}

func (e *Engine) buyerRankAndMatch(rctx context.Context, dealID string, want bstack.WantAd, collectRes buyerCollectResult, bypassPolicy bool, approvedCapSats uint64) (buyerRankResult, error) {
	now := time.Now().Unix()
	wantJSON, _ := json.Marshal(want)
	fj := e.FastJournal()
	if e.policy != nil {
		if fj != nil {
			fj.CachedPolicySync(e.mem.DB(), e.policy)
		} else if err := e.policy.Refresh(); err != nil {
			return buyerRankResult{}, err
		}
	}
	reps := make(map[string]store.SellerReputation)
	if fj == nil && e.mem != nil && e.mem.DB() != nil {
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
	// Enforce bounded approved cap even when domain/role policy was bypassed via operator approval (STATE-7).
	eligibleOffers := collectRes.Offers
	if bypassPolicy && approvedCapSats > 0 {
		filtered := make([]bstack.OfferAd, 0, len(eligibleOffers))
		for _, off := range eligibleOffers {
			if off.PriceSats <= approvedCapSats {
				filtered = append(filtered, off)
			}
		}
		eligibleOffers = filtered
	}
	verifiedBonds := make(map[string]uint64)
	var port *chain.Port
	if e.mem != nil {
		port = e.mem.Chain()
	}
	for _, off := range eligibleOffers {
		if off.BondOutpoint != "" && off.BondBEEFHex != "" {
			if sats := VerifyOfferBondSats(rctx, off, port); sats > 0 {
				verifiedBonds[off.OfferID] = sats
			}
		}
	}
	var rep6D map[string]reputation.Vector6D
	if fj == nil && e.mem != nil && e.mem.DB() != nil {
		_, vecs, _ := EvaluateTrustForOntology(e.mem.DB(), e.salePubHex(), want.OntologyID, e.policy, nil, nil)
		rep6D = vecs
	}
	ranked := RankOffersWith6DReputation(want, eligibleOffers, rankPol, reps, rep6D, verifiedBonds)
	if len(ranked) == 0 {
		msg := "no eligible offers under policy"
		if fj == nil {
			if err := e.persistDealRecordChecked(store.DealRecord{
				DealID:   dealID,
				Role:     "buyer",
				Status:   "failed",
				WantJSON: wantJSON,
				Error:    msg,
			}); err != nil {
				return buyerRankResult{}, err
			}
		}
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
			if fj == nil {
				if err := e.persistDealRecordChecked(store.DealRecord{
					DealID:         dealID,
					Role:           "buyer",
					Status:         "failed",
					WantJSON:       wantJSON,
					MatchAuditJSON: matchAuditJSON,
					Error:          msg,
				}); err != nil {
					return buyerRankResult{}, err
				}
			}
			return buyerRankResult{NowUnix: now, PolicyRejected: true, Error: msg, MatchAudit: matchAudit}, nil
		}
		chosen = *mDec.Chosen
		if mDec.Verdict == "escalate" {
			chosenJSON, _ := json.Marshal(chosen)
			if err := e.persistDealRecordChecked(store.DealRecord{
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
			}); err != nil {
				return buyerRankResult{}, err
			}
			return buyerRankResult{
				NowUnix:          now,
				NeedsEscalation:  true,
				EscalationReason: mDec.Reason,
				Chosen:           &chosen,
				Candidates:       buildTopKCandidates(chosen, ranked, 3),
				MatchAudit:       matchAudit,
				FirstOfferMs:     collectRes.FirstOfferMs,
			}, nil
		}
	}

	candidates := buildTopKCandidates(chosen, ranked, 3)
	if fj == nil {
		chosenJSON, _ := json.Marshal(chosen)
		if err := e.persistDealRecordChecked(store.DealRecord{
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
		}); err != nil {
			return buyerRankResult{}, err
		}
	}
	return buyerRankResult{
		NowUnix:      now,
		Chosen:       &chosen,
		Candidates:   candidates,
		MatchAudit:   matchAudit,
		FirstOfferMs: collectRes.FirstOfferMs,
	}, nil
}

func buildTopKCandidates(chosen bstack.OfferAd, ranked []ScoredOffer, k int) []bstack.OfferAd {
	if k <= 0 {
		k = 3
	}
	out := make([]bstack.OfferAd, 0, k)
	seen := make(map[string]bool, k)
	out = append(out, chosen)
	if chosen.OfferID != "" {
		seen[chosen.OfferID] = true
	}
	for _, ro := range ranked {
		if len(out) >= k {
			break
		}
		if ro.Offer.OfferID != "" && seen[ro.Offer.OfferID] {
			continue
		}
		if ro.Offer.OfferID == chosen.OfferID && ro.Offer.Pub == chosen.Pub {
			continue
		}
		if ro.Offer.OfferID != "" {
			seen[ro.Offer.OfferID] = true
		}
		out = append(out, ro.Offer)
	}
	return out
}
