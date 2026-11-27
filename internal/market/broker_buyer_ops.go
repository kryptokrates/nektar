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

	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// verifySaleDelivery is the single zero-trust buyer verification path shared by the Restate
// BuyerDeal workflow and local Broker.Want: decodes SaleOffer, extracts `k` from on-chain
// DisclosureSale.Buy(k) when ContentKeyHex is omitted, fetches offloaded CHIRP ciphertext,
// decrypts & verifies BRC-369 key commitment, and enforces live-network SPV ProofBundle checks
// (including 0-conf carried-ancestry policy + durable proof_upgrade_owed obligation).
func verifySaleDelivery(
	ctx context.Context,
	mem *memory.Service,
	disc *discovery.Service,
	pol *Policy,
	dealID string,
	chosen bstack.OfferAd,
	contentKeyHex, claimTxBEEFHex, ciphertextHex string,
	proofBundle *contracts.ProofBundle,
) ([]byte, string, string) {
	contentKeyHex = strings.TrimSpace(contentKeyHex)
	claimTxBEEFHex = strings.TrimSpace(claimTxBEEFHex)
	if contentKeyHex == "" && claimTxBEEFHex == "" {
		return nil, "", "missing contentKeyHex and claimTxBeefHex in seller claim signal"
	}
	if len(chosen.SaleOffer) == 0 {
		return nil, "", "missing signed BRC-369 SaleOffer in chosen offer"
	}
	var saleOffer contracts.KeyedSaleOffer
	if err := json.Unmarshal(chosen.SaleOffer, &saleOffer); err != nil {
		return nil, "", "decode saleOffer: " + err.Error()
	}
	// Trustless L1 fallback: if ContentKeyHex was omitted/dropped off HTTP, extract `k` directly
	// from the on-chain DisclosureSale.Buy(k) transaction's unlocking script!
	if contentKeyHex == "" && claimTxBEEFHex != "" {
		if rawClaim, dErr := hex.DecodeString(claimTxBEEFHex); dErr == nil {
			if _, claimTx, _, pErr := guard.ParseBEEF(rawClaim, guard.DefaultBound); pErr == nil && claimTx != nil {
				if extracted, eErr := contracts.ExtractRevealedKeyFromBuyTx(claimTx, 0, saleOffer.Config.HWrapped); eErr == nil {
					contentKeyHex = hex.EncodeToString(extracted)
				}
			}
		}
	}
	if contentKeyHex == "" {
		return nil, "", "missing contentKeyHex in seller claim signal (and on-chain extraction failed)"
	}
	// Prefer inline CiphertextHex from signed SaleOffer or seller ClaimSignal before remote CHIRP HTTP fetch.
	if saleOffer.CiphertextHex == "" {
		if ciphertextHex != "" {
			saleOffer.CiphertextHex = ciphertextHex
		} else if chosen.SellerURL != "" && chosen.ChirpURL != "" && disc != nil {
			ct, cErr := disc.FetchRemoteChirp(ctx, chosen.SellerURL, chosen.ChirpURL)
			if cErr != nil {
				return nil, "", "remote CHIRP verification failed: " + cErr.Error()
			}
			saleOffer.CiphertextHex = hex.EncodeToString(ct)
		}
	}
	keyBytes, kErr := hex.DecodeString(contentKeyHex)
	if kErr != nil || len(keyBytes) != 32 {
		core.ZeroBytes(keyBytes)
		return nil, "", "seller returned invalid 32-byte content key"
	}
	defer core.ZeroBytes(keyBytes)
	plaintext, pErr := contracts.ClaimKeyedSale(&saleOffer, keyBytes)
	if pErr != nil {
		return nil, "", "BRC-369 key verification failed: " + pErr.Error()
	}
	// Unconditional SPV ProofBundle verification on live networks (zero Leaf=="" bypass — Finding #3!).
	// Accepts mined BUMP at SettleDepth OR verified 0-conf carried ancestry within buyer zeroconf policy
	// (recording a durable proof_upgrade_owed obligation until mined BUMP lands).
	isLive := mem != nil && mem.Chain() != nil && mem.Chain().Network() != "memory"
	if proofBundle == nil && isLive {
		return nil, "", "missing SPV ProofBundle from seller"
	}
	if proofBundle != nil {
		if !strings.EqualFold(proofBundle.Leaf, chosen.Leaf) {
			return nil, "", "seller SPV ProofBundle leaf mismatches signed offer leaf"
		}
		var vr contracts.VerifyResult
		if isLive {
			vr = contracts.VerifyFragmentWithDepth(ctx, *proofBundle, mem.Chain().Tracker(), mem.Chain().SettleDepth())
		} else {
			vr = contracts.VerifyFragmentCtx(ctx, *proofBundle, mem.Chain().Tracker())
			if !vr.OK && vr.Reason == "BEEFInvalid" && strings.Contains(vr.Detail, "BUMP root not valid in header chain") {
				vr = contracts.VerifyFragmentStructural(*proofBundle)
			}
		}
		zeroConfAnchorOK := false
		if isLive && !vr.OK && vr.AncestryVerified && (vr.Reason == "Unsettled" || vr.Reason == "InsufficientDepth") && pol != nil {
			zeroConfMax, _, _ := pol.ZeroConfLimits()
			if zeroConfMax > 0 && chosen.PriceSats <= zeroConfMax {
				zeroConfAnchorOK = true
			}
		}
		if !vr.OK && !zeroConfAnchorOK {
			return nil, "", fmt.Sprintf("seller SPV ProofBundle invalid (%s: %s)", vr.Reason, vr.Detail)
		}
		if isLive && !vr.Settled {
			if !zeroConfAnchorOK {
				return nil, "", "seller SPV ProofBundle is not settled in a mined block"
			}
			if mem != nil && mem.DB() != nil {
				_ = RecordProofUpgradeOwed(mem.DB(), dealID, "buyer", chosen.Pub, proofBundle.Anchor.TxID, chosen.PriceSats)
			}
		}
	}
	return plaintext, contentKeyHex, ""
}

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
	// Memory-only local test path when Want() suspended without a live Restate server.
	// Live networks (testnet/mainnet) strictly require Restate durable execution above.
	if b.mem.Chain().Network() == "memory" {
		appr, dErr := b.mem.DB().DecideApproval(approvalID, approve)
		if dErr != nil {
			return store.ApprovalRecord{}, nil, dErr
		}
		if !approve {
			return appr, nil, nil
		}
		origDeal, gErr := b.mem.DB().GetDeal(appr.DealID)
		if gErr != nil {
			return appr, nil, fmt.Errorf("market: load approval deal %s: %w", appr.DealID, gErr)
		}
		var want bstack.WantAd
		if err := json.Unmarshal(origDeal.WantJSON, &want); err != nil {
			return appr, nil, fmt.Errorf("market: decode approval want payload: %w", err)
		}
		deal, wErr := b.Want(ctx, WantRequest{
			Domains:            want.Domains,
			Question:           want.Query,
			MaxSats:            want.MaxSats,
			ApprovedOverrideID: appr.ID,
		})
		return appr, deal, wErr
	}
	return store.ApprovalRecord{}, nil, errors.New("market: Restate durable executor is required for approval resolution")
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
			payload, ok, _ := b.verifyMarketEventBEEF(ctx, ev)
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

// LookupOffersSingleHost queries a single hub's /lookup endpoint for wantID and cryptographically verifies
// each returned OfferAd and its embedded BRC-369 SaleOffer. Invoked concurrently via restate.RunAsync.
func (b *Broker) LookupOffersSingleHost(ctx context.Context, host, wantID, buyerPub string) []bstack.OfferAd {
	raw := b.lookupOffers(ctx, []string{host}, wantID)
	if len(raw) == 0 {
		return nil
	}
	if buyerPub == "" {
		buyerPub = b.PubHex()
	}
	out := make([]bstack.OfferAd, 0, len(raw))
	for _, o := range raw {
		if o.WantID != wantID {
			continue
		}
		var so contracts.KeyedSaleOffer
		if err := json.Unmarshal(o.SaleOffer, &so); err != nil {
			continue
		}
		if err := contracts.VerifySaleOffer(&so, buyerPub, o.Leaf); err != nil {
			continue
		}
		if !strings.EqualFold(so.SellerPub, o.Pub) || so.Config.Price != o.PriceSats || !strings.EqualFold(so.KeyCommitment, o.KeyCommitHex) {
			continue
		}
		out = append(out, o)
	}
	return out
}
