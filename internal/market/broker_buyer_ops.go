package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/store"
)

// CollectOffersForWant publishes a signed WantAd across active hubs and collects cryptographically verified OfferAds.
// Called by both the Restate BuyerDeal workflow (inside restate.Run) and direct local unit tests.
func (b *Broker) CollectOffersForWant(ctx context.Context, want bstack.WantAd, collectTimeout time.Duration) ([]bstack.OfferAd, int64, error) {
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil, 0, errors.New("market: no active hub hosts configured (join a hub first)")
	}
	wantID := want.WantID
	inbox := make(chan bstack.OfferAd, 32)
	b.mu.Lock()
	b.offerInbox[wantID] = inbox
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.offerInbox, wantID)
		b.mu.Unlock()
	}()

	sig, err := bstack.SignArtifact(b.engine.saleKey, want)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	if err != nil {
		return nil, 0, err
	}
	startTime := time.Now()
	topics := lanesForDomains(want.Net, want.Domains)
	submitted, subErr := b.disc.BuildAndSubmitMarketAd(ctx, b.engine.saleKey, bstack.MarketPayload{Kind: bstack.KindWant, Body: body}, hosts, topics)
	if submitted == 0 {
		if subErr != nil {
			return nil, 0, fmt.Errorf("failed to submit WantAd to any active hub: %w", subErr)
		}
		return nil, 0, errors.New("failed to submit WantAd to any active hub")
	}

	timeout := collectTimeout
	if timeout <= 0 {
		timeout = b.cfg.CollectTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	lookupTick := time.NewTimer(300 * time.Millisecond)
	defer lookupTick.Stop()

	seenOffers := make(map[string]bstack.OfferAd)
	var firstOfferMs int64
	collectOffers := func(o bstack.OfferAd) {
		if o.WantID != wantID {
			return
		}
		if _, dup := seenOffers[o.OfferID]; dup {
			return
		}
		var so contracts.KeyedSaleOffer
		if err := json.Unmarshal(o.SaleOffer, &so); err != nil {
			return
		}
		if err := contracts.VerifySaleOffer(&so, b.PubHex(), o.Leaf); err != nil {
			return
		}
		if !strings.EqualFold(so.SellerPub, o.Pub) || so.Config.Price != o.PriceSats || !strings.EqualFold(so.KeyCommitment, o.KeyCommitHex) {
			return
		}
		if firstOfferMs == 0 {
			firstOfferMs = time.Since(startTime).Milliseconds()
			if firstOfferMs == 0 {
				firstOfferMs = 1
			}
		}
		seenOffers[o.OfferID] = o
	}

	var graceTimer *time.Timer
	var graceCh <-chan time.Time
collectLoop:
	for {
		select {
		case <-ctx.Done():
			break collectLoop
		case o := <-inbox:
			collectOffers(o)
			if len(seenOffers) > 0 && graceTimer == nil {
				graceTimer = time.NewTimer(b.cfg.GraceTimeout)
				graceCh = graceTimer.C
			}
		case <-lookupTick.C:
			for _, o := range b.lookupOffers(ctx, hosts, wantID) {
				collectOffers(o)
			}
			if len(seenOffers) > 0 && graceTimer == nil {
				graceTimer = time.NewTimer(150 * time.Millisecond)
				graceCh = graceTimer.C
			}
		case <-graceCh:
			break collectLoop
		case <-timer.C:
			for _, o := range b.lookupOffers(ctx, hosts, wantID) {
				collectOffers(o)
			}
			break collectLoop
		}
	}
	if graceTimer != nil {
		graceTimer.Stop()
	}

	offers := make([]bstack.OfferAd, 0, len(seenOffers))
	for _, o := range seenOffers {
		offers = append(offers, o)
	}
	return offers, firstOfferMs, nil
}

// ApproveAndExecute settles a pending operator approval with atomic SQLite CAS.
// When the paused BuyerDeal is suspended on a Restate Awakeable (active Restate mode),
// it resolves that EXACT awakeable first, then atomically marks the approval consumed/denied
// so transient Restate errors remain retryable and approved awakeable IDs cannot be replayed.
func (b *Broker) ApproveAndExecute(ctx context.Context, approvalID string, approve bool) (store.ApprovalRecord, *store.DealRecord, error) {
	existing, err := b.mem.DB().GetApproval(approvalID)
	if err != nil {
		return store.ApprovalRecord{}, nil, err
	}
	// Active Restate Awakeable resume path (resumes the exact suspended BuyerDeal workflow!).
	if existing.AwakeableID != "" {
		if existing.Status != "pending" && existing.Status != "approved" {
			return existing, nil, store.ErrApprovalNotPending
		}
		if existing.ExpiresAt <= time.Now().Unix() {
			return existing, nil, store.ErrApprovalNotPending
		}
		rc := b.RestateClient()
		if rc == nil {
			return existing, nil, errors.New("market: Restate runtime unavailable to resolve awakeable approval")
		}
		dec := ApprovalDecision{Approved: approve, DecidedBy: "operator"}
		if !approve {
			dec.Reason = "denied by operator"
		}
		if rErr := rc.ResolveAwakeable(ctx, existing.AwakeableID, dec); rErr != nil {
			return existing, nil, rErr
		}
		appr, mErr := b.mem.DB().MarkApprovalResolvedAwakeable(approvalID, approve)
		if mErr != nil {
			// Awakeable was already delivered to Restate; tolerate concurrent state update.
			appr = existing
			if approve {
				appr.Status = "consumed"
			} else {
				appr.Status = "denied"
			}
		}
		aCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if st, aErr := rc.AttachBuyerDeal(aCtx, appr.DealID); aErr == nil && st != nil {
			if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
				return appr, &deal, nil
			}
			return appr, &store.DealRecord{
				DealID:        appr.DealID,
				Role:          "buyer",
				Status:        st.Status,
				Shard:         st.Shard,
				PaymentTxID:   st.PaymentTxID,
				DeliveredText: st.DeliveredTxt,
				Error:         st.Error,
			}, nil
		}
		if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
			return appr, &deal, nil
		}
		return appr, nil, nil
	}

	appr, err := b.mem.DB().DecideApproval(approvalID, approve)
	if err != nil {
		return store.ApprovalRecord{}, nil, err
	}
	if !approve {
		if deal, dErr := b.mem.DB().GetDeal(appr.DealID); dErr == nil {
			deal.Status = "failed"
			deal.Error = "denied by operator"
			_ = b.mem.DB().SaveDeal(deal)
			return appr, &deal, nil
		}
		return appr, nil, nil
	}
	if b.mem.Chain().Network() != "memory" {
		return appr, nil, errors.New("market: legacy non-Restate approval execution is disabled on live networks")
	}
	deal, err := b.mem.DB().GetDeal(appr.DealID)
	if err != nil {
		return appr, nil, nil
	}
	var want bstack.WantAd
	if json.Unmarshal(deal.WantJSON, &want) != nil {
		return appr, &deal, nil
	}
	settled, err := b.Want(ctx, WantRequest{
		Domains:            want.Domains,
		Question:           want.Query,
		MaxSats:            want.MaxSats,
		ApprovedOverrideID: approvalID,
	})
	return appr, settled, err
}

func (b *Broker) lookupOffers(ctx context.Context, hosts []string, wantID string) []bstack.OfferAd {
	qBytes, _ := json.Marshal(map[string]any{
		"service": bstack.LookupService,
		"query":   bstack.MarketQuery{Kind: bstack.KindOffer, WantID: wantID, Limit: 50},
	})
	var out []bstack.OfferAd
	for _, h := range hosts {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h, "/")+"/lookup", bytes.NewReader(qBytes))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := b.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		var ans struct {
			Result []bstack.MarketEvent `json:"result"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&ans)
		resp.Body.Close()
		nowMs := time.Now().UnixMilli()
		maxExpMs := nowMs + bstack.MaxLifetime.Milliseconds() + 60_000
		myNet := b.mem.Chain().Network()
		for _, ev := range ans.Result {
			payload, ok := b.verifyMarketEventBEEF(ctx, ev)
			if !ok || payload.Kind != bstack.KindOffer {
				continue
			}
			var so bstack.SignedOffer
			if json.Unmarshal(payload.Body, &so) != nil {
				continue
			}
			if so.Offer.Net != myNet || so.Offer.Exp <= nowMs || so.Offer.Exp > maxExpMs {
				continue
			}
			if leafRaw, lErr := hex.DecodeString(strings.TrimSpace(so.Offer.Leaf)); lErr != nil || len(leafRaw) != 32 {
				continue
			}
			if so.Offer.BuyerPub != "" && so.Offer.BuyerPub != b.PubHex() {
				continue
			}
			if bstack.VerifyArtifact(so.Offer.Pub, so.Sig, so.Offer) == nil {
				out = append(out, so.Offer)
			}
		}
	}
	return out
}
