package market

import (
	"context"
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
		var maxPolicyDeal uint64 = 500
		if b.engine != nil && b.engine.Policy() != nil {
			_, mpd, _, _, _, _, _ := b.engine.Policy().Snapshot()
			if mpd > 0 {
				maxPolicyDeal = mpd
			}
		}
		estTok := req.EstTokensSaved
		if estTok == 0 {
			// Estimate saved reasoning tokens conservatively from question complexity + baseline.
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
		WantID:     wantID,
		Pub:        b.PubHex(),
		Net:        b.mem.Chain().Network(),
		Domains:    wantDomains,
		Query:      req.Question,
		OntologyID: strings.ToLower(strings.TrimSpace(req.OntologyID)),
		MaxSats:    req.MaxSats,
		Revision:   1,
		Exp:        time.Now().Add(30 * time.Minute).UnixMilli(),
	}

	// Fail closed immediately before launching a Restate workflow when operator kill switch is engaged.
	if b.engine != nil && b.engine.Policy() != nil {
		if kill, _, _, _, _, _, _ := b.engine.Policy().Snapshot(); kill {
			return nil, errors.New("market: operator kill switch is engaged")
		}
	}
	// Active Restate Durable Executor path (SOLE production & test path — NO local-broker bypass!).
	rc := b.RestateClient()
	if rc == nil {
		return nil, errors.New("market: Restate durable executor is required for trades (run `nektar restate-up` or configure NEKTAR_RESTATE_ADMIN)")
	}
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
