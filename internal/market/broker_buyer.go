package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

// Want publishes a signed WantAd across active hubs, collects & ranks verified offers,
// executes AtomicBEEF payment (direct HTTP or NAT bbox fallback), verifies the revealed
// BRC-369 content key + remote CHIRP closure + SPV Merkle ProofBundle, and commits the knowledge!
// When an active RestateClient is attached (default in `nektar serve`), execution runs durably
// through the Restate BuyerDeal workflow + Custody VO + Tx VO (zero local-broker bypass on live networks!).
func (b *Broker) Want(ctx context.Context, req WantRequest) (*store.DealRecord, error) {
	if len(req.Domains) == 0 {
		req.Domains = []string{"ops"}
	}
	if strings.TrimSpace(req.Question) == "" {
		return nil, errors.New("market: question is required")
	}
	if req.MaxSats == 0 {
		req.MaxSats = 500
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
		WantID:   wantID,
		Pub:      b.PubHex(),
		Net:      b.mem.Chain().Network(),
		Domains:  wantDomains,
		Query:    req.Question,
		MaxSats:  req.MaxSats,
		Revision: 1,
		Exp:      time.Now().Add(30 * time.Minute).UnixMilli(),
	}

	// Active Restate Durable Executor path (SOLE production path on live networks — NO local-broker bypass!).
	rc := b.RestateClient()
	if rc == nil && b.mem.Chain().Network() != "memory" {
		return nil, errors.New("market: Restate durable executor is required for live trades (run `nektar restate-up` or configure NEKTAR_RESTATE_ADMIN)")
	}
	if rc != nil {
		if _, err := rc.StartBuyerDealAsync(ctx, wantID, BuyerDealInput{
			Want:               want,
			TargetShard:        req.TargetShard,
			CollectTimeout:     req.CollectTimeout,
			WaitTimeout:        5 * time.Minute,
			StandingTTL:        time.Duration(req.StandingTTLSeconds) * time.Second,
			ApprovedOverrideID: req.ApprovedOverrideID,
		}); err != nil {
			return nil, err
		}
		// Race streaming AttachBuyerDeal (Timeout: 0) against approval_pending wake (eliminates 80ms spin-poll!).
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
			st, aErr := rc.AttachBuyerDeal(attachCtx, wantID)
			attachCh <- attachRes{st: st, err: aErr}
		}()
		checkTicker := time.NewTicker(250 * time.Millisecond)
		defer checkTicker.Stop()
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
			case <-checkTicker.C:
				if rec, gErr := b.mem.DB().GetDeal(wantID); gErr == nil && rec.Status == "approval_pending" {
					// Workflow is durably suspended on its Restate Awakeable; return immediately to caller!
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

	// Memory-only local test path (shares CollectOffersForWant + verifySaleDelivery with Restate BuyerDeal).
	_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
	kill, maxDeal, maxDay, _, spent, buyerOK, _ := b.engine.Policy().Snapshot()
	if kill {
		return nil, errors.New("market: operator kill switch is engaged")
	}
	if req.ApprovedOverrideID != "" {
		if err := b.mem.DB().ConsumeApprovalOverride(req.ApprovedOverrideID); err != nil {
			return nil, fmt.Errorf("market: invalid or consumed approval override %q: %w", req.ApprovedOverrideID, err)
		}
	}
	wantJSON, _ := json.Marshal(want)

	// Load-bearing operator consent gate: require active buyer policy, allowed domain, & caps unless explicitly approved!
	domainErr := b.engine.Policy().CanSpendForDomains("", req.Domains, 0)
	if req.ApprovedOverrideID == "" && (!buyerOK || domainErr != nil || (maxDeal > 0 && req.MaxSats > maxDeal) || (maxDay > 0 && spent+req.MaxSats > maxDay)) {
		reason := "no active buyer policy approved (run `nektar policy approve --preset buyer` or approve in Dashboard)"
		if buyerOK && domainErr != nil {
			reason = domainErr.Error()
		} else if buyerOK && maxDeal > 0 && req.MaxSats > maxDeal {
			reason = fmt.Sprintf("requested maxSats (%d) exceeds buyer policy maxPerDeal (%d)", req.MaxSats, maxDeal)
		} else if buyerOK && maxDay > 0 && spent+req.MaxSats > maxDay {
			var rem uint64
			if maxDay > spent {
				rem = maxDay - spent
			}
			reason = fmt.Sprintf("requested maxSats (%d) exceeds remaining daily cap (%d)", req.MaxSats, rem)
		}
		appID := "appr-" + randIDHex()
		_ = b.mem.DB().SaveApproval(store.ApprovalRecord{
			ID:           appID,
			DealID:       wantID,
			Reason:       reason,
			MaxExtraSats: req.MaxSats,
			Status:       "pending",
			ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
		})
		rec := store.DealRecord{
			DealID:    wantID,
			Role:      "buyer",
			Status:    "approval_pending",
			WantJSON:  wantJSON,
			PriceSats: req.MaxSats,
			Error:     reason + " [approvalId=" + appID + "]",
		}
		_ = b.mem.DB().SaveDeal(rec)
		b.notify("approval_pending", map[string]any{"approvalId": appID, "dealId": wantID, "reason": reason})
		return &rec, nil
	}

	rec := store.DealRecord{
		DealID:   wantID,
		Role:     "buyer",
		Status:   "collecting",
		WantJSON: wantJSON,
	}
	_ = b.mem.DB().SaveDeal(rec)

	offers, firstOfferMs, err := b.CollectOffersForWant(ctx, want, req.CollectTimeout)
	if err != nil {
		rec.Status = "failed"
		rec.Error = err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}
	if len(offers) == 0 {
		_ = b.mem.DB().RecordUnmatchedWant(store.UnmatchedWantRecord{
			WantID:  wantID,
			Domain:  req.Domains[0],
			Query:   req.Question,
			MaxSats: req.MaxSats,
		})
		rec.Status = "expired"
		rec.Error = fmt.Sprintf("no valid offers received within %s for domain %q — try `nektar peers` to see active sellers/bounties or widen domains/maxSats", b.cfg.CollectTimeout.Round(time.Millisecond), req.Domains[0])
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, nil
	}

	// Load verified local seller reputations & rank deterministically in integer bps!
	repList, _ := b.mem.DB().ListReputations()
	reps := make(map[string]store.SellerReputation, len(repList))
	for _, r := range repList {
		reps[r.SellerPub] = r
	}
	rankPol := b.engine.Policy()
	if req.ApprovedOverrideID != "" {
		rankPol = nil // explicitly approved override bypasses default cap check
	}
	ranked := RankOffersWithRep(want, offers, rankPol, reps)
	if len(ranked) == 0 {
		rec.Status = "failed"
		rec.Error = "all received offers exceeded policy caps"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, nil
	}

	chosen := ranked[0].Offer
	if matcher := b.Matcher(); matcher != nil {
		mDec := matcher.MatchOffersForBuyer(ctx, want, ranked, reps, b.engine.Policy())
		auditJSON, _ := json.Marshal(mDec.Audit)
		rec.MatchAuditJSON = auditJSON
		if mDec.Verdict == "reject" || mDec.Chosen == nil {
			rec.Status = "failed"
			rec.Error = mDec.Reason
			if rec.Error == "" {
				rec.Error = "offers rejected by semantic matching policy"
			}
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, nil
		}
		if mDec.Verdict == "escalate" && req.ApprovedOverrideID == "" {
			appID := "appr-" + randIDHex()
			_ = b.mem.DB().SaveApproval(store.ApprovalRecord{
				ID:           appID,
				DealID:       wantID,
				Reason:       mDec.Reason,
				MaxExtraSats: mDec.Chosen.PriceSats,
				Status:       "pending",
				ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
			})
			chosenJSON, _ := json.Marshal(mDec.Chosen)
			rec.ChosenOfferJSON = chosenJSON
			rec.CounterpartyPub = mDec.Chosen.Pub
			rec.PriceSats = mDec.Chosen.PriceSats
			rec.Status = "approval_pending"
			rec.Error = mDec.Reason + " [approvalId=" + appID + "]"
			_ = b.mem.DB().SaveDeal(rec)
			b.notify("approval_pending", map[string]any{"approvalId": appID, "dealId": wantID, "reason": mDec.Reason})
			return &rec, nil
		}
		chosen = *mDec.Chosen
	}
	chosenJSON, _ := json.Marshal(chosen)
	rec.ChosenOfferJSON = chosenJSON
	rec.CounterpartyPub = chosen.Pub
	rec.PriceSats = chosen.PriceSats
	rec.ChirpURL = chosen.ChirpURL
	rec.IsHubSeller = chosen.IsHub
	rec.FirstOfferMs = firstOfferMs
	rec.Status = "offered"
	_ = b.mem.DB().SaveDeal(rec)

	return b.wantSettleChosen(ctx, wantID, req, hosts, chosen, rec)
}
