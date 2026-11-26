package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// Want publishes a signed WantAd across active hubs, collects & ranks verified offers,
// executes AtomicBEEF payment (direct HTTP or NAT bbox fallback), verifies the revealed
// BRC-369 content key + remote CHIRP closure + SPV Merkle ProofBundle, and commits the knowledge!
// When an active RestateClient is attached (default in `nektar serve`), execution runs durably
// through the Restate BuyerDeal workflow + Custody VO + Tx VO (zero local-broker bypass!).
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
		go func() {
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

	// Register offer inbox BEFORE publishing WantAd so zero fast quotes are missed!
	inbox := make(chan bstack.OfferAd, 32)
	b.mu.Lock()
	b.offerInbox[wantID] = inbox
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.offerInbox, wantID)
		b.mu.Unlock()
	}()

	rec := store.DealRecord{
		DealID:   wantID,
		Role:     "buyer",
		Status:   "collecting",
		WantJSON: wantJSON,
	}
	_ = b.mem.DB().SaveDeal(rec)

	sig, err := bstack.SignArtifact(b.engine.saleKey, want)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	if err != nil {
		return nil, err
	}
	startTime := time.Now()
	topics := lanesForDomains(want.Net, want.Domains)
	submitted, subErr := b.disc.BuildAndSubmitMarketAd(ctx, b.engine.saleKey, bstack.MarketPayload{Kind: bstack.KindWant, Body: body}, hosts, topics)
	if submitted == 0 {
		rec.Status = "failed"
		if subErr != nil {
			rec.Error = fmt.Sprintf("failed to submit WantAd to any active hub: %v", subErr)
		} else {
			rec.Error = "failed to submit WantAd to any active hub"
		}
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}

	// Collect live SSE offers + one fallback /lookup poll after 300ms.
	timeout := req.CollectTimeout
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
		// Cryptographically verify embedded DisclosureSale offer BEFORE ranking!
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

	// Wait up to timeout; if we receive an organic offer AND wait at least 200ms for competition, proceed swiftly!
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

	if len(seenOffers) == 0 {
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

	offers := make([]bstack.OfferAd, 0, len(seenOffers))
	for _, o := range seenOffers {
		offers = append(offers, o)
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

	// Build, SpendGuard-check, and broadcast BSV payment (preferring BRC-29 derived key when Seller advertises PayIdentityKey).
	sendArgs := nwallet.SendArgs{
		Satoshis:    chosen.PriceSats,
		Description: "market purchase " + chosen.OfferID,
		Labels:      []string{"market", "purchase"},
	}
	if chosen.PayIdentityKey != "" {
		sendArgs.ToIdentityKey = chosen.PayIdentityKey
	} else {
		sendArgs.ToAddress = chosen.PayAddress
	}
	sendRes, err := b.mem.Chain().Send(ctx, sendArgs)
	if err != nil {
		rec.Status = "failed"
		rec.Error = "payment failed: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}
	payTxID := sendRes.TxID
	payBEEF := sendRes.BEEF
	if len(payBEEF) == 0 && sendRes.Tx != nil {
		payBEEF, err = chain.SafeAtomicBEEF(sendRes.Tx)
		if err != nil {
			rec.Status = "failed"
			rec.Error = "encode payment AtomicBEEF: " + err.Error()
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, err
		}
	}
	b.engine.Policy().RecordSpend(chosen.PriceSats)
	rec.PaymentTxID = payTxID
	rec.PaymentBEEF = payBEEF
	rec.Status = "paid"
	_ = b.mem.DB().SaveDeal(rec)

	// Deliver AtomicBEEF (+ BRC-29 remittance when used) to Seller via Direct HTTP OR NAT bbox fallback!
	pReq := PurchaseRequest{
		OfferID:           chosen.OfferID,
		WantID:            wantID,
		BuyerPub:          b.PubHex(),
		TxID:              payTxID,
		PaymentBEEFHex:    hex.EncodeToString(payBEEF),
		OutputIndex:       sendRes.OutputIndex,
		DerivationPrefix:  sendRes.DerivationPrefix,
		DerivationSuffix:  sendRes.DerivationSuffix,
		SenderIdentityKey: sendRes.SenderIdentityKey,
	}
	b.signPurchaseRequest(&pReq)
	var pResp *PurchaseResponse
	if chosen.SellerURL != "" {
		pResp, err = b.sendDirectPurchase(ctx, chosen.SellerURL, pReq)
	}
	if pResp == nil && chosen.BboxOffice != "" {
		for _, h := range hosts {
			pResp, err = b.sendBboxPurchase(ctx, h, chosen.Pub, chosen.BboxOffice, wantID, pReq)
			if err == nil && pResp != nil && pResp.OK {
				break
			}
		}
	}
	if err != nil || pResp == nil || !pResp.OK {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		if err != nil {
			rec.Error = "purchase delivery failed: " + err.Error()
		} else if pResp != nil {
			rec.Error = "seller rejected purchase: " + pResp.Error
		}
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}

	// Zero-trust Buyer Verification:
	// 1. Decode signed SaleOffer
	var saleOffer contracts.KeyedSaleOffer
	if err := json.Unmarshal(chosen.SaleOffer, &saleOffer); err != nil {
		return nil, err
	}
	// 2. Skip remote CHIRP HTTP refetch when verified CiphertextHex is already inline in signed SaleOffer (Finding #33);
	//    fetch remote BRC-167 CHIRP closure only when ciphertext was offloaded (>4 KiB).
	if saleOffer.CiphertextHex == "" {
		if chosen.SellerURL != "" && chosen.ChirpURL != "" {
			ct, cErr := b.disc.FetchRemoteChirp(ctx, chosen.SellerURL, chosen.ChirpURL)
			if cErr != nil {
				_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
				rec.Status = "failed"
				rec.Error = "remote CHIRP verification failed: " + cErr.Error()
				_ = b.mem.DB().SaveDeal(rec)
				return &rec, cErr
			}
			saleOffer.CiphertextHex = hex.EncodeToString(ct)
		} else if pResp.CiphertextHex != "" {
			saleOffer.CiphertextHex = pResp.CiphertextHex
		}
	}

	// 3. Verify revealed BRC-369 content key k against DisclosureSale hWrapped AND KeyCommitment, then decrypt!
	keyBytes, err := hex.DecodeString(strings.TrimSpace(pResp.ContentKeyHex))
	if err != nil || len(keyBytes) != 32 {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		rec.Error = "seller returned invalid 32-byte content key"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}
	plaintext, err := contracts.ClaimKeyedSale(&saleOffer, keyBytes)
	if err != nil {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		rec.Error = "BRC-369 key verification failed: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}

	// 4. Full offline SPV verification of seller's ProofBundle (UNCONDITIONAL on live networks — zero Leaf=="" bypass!).
	if pResp.ProofBundle == nil && b.mem.Chain().Network() != "memory" {
		_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
		rec.Status = "failed"
		rec.Error = "missing SPV ProofBundle from seller"
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, errors.New(rec.Error)
	}
	if pResp.ProofBundle != nil {
		if !strings.EqualFold(pResp.ProofBundle.Leaf, chosen.Leaf) {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "seller SPV ProofBundle leaf mismatches signed offer leaf"
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
		var vr contracts.VerifyResult
		if b.mem.Chain().Network() != "memory" {
			vr = contracts.VerifyFragmentWithDepth(ctx, *pResp.ProofBundle, b.mem.Chain().Tracker(), b.mem.Chain().SettleDepth())
		} else {
			vr = contracts.VerifyFragmentCtx(ctx, *pResp.ProofBundle, b.mem.Chain().Tracker())
			if !vr.OK && vr.Reason == "BEEFInvalid" && strings.Contains(vr.Detail, "BUMP root not valid in header chain") {
				vr = contracts.VerifyFragmentStructural(*pResp.ProofBundle)
			}
		}
		if !vr.OK {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = fmt.Sprintf("seller SPV ProofBundle invalid (%s: %s)", vr.Reason, vr.Detail)
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
		if b.mem.Chain().Network() != "memory" && !vr.Settled {
			_ = b.mem.DB().RecordReputation(chosen.Pub, false, 0)
			rec.Status = "failed"
			rec.Error = "seller SPV ProofBundle is not settled in a mined block"
			_ = b.mem.DB().SaveDeal(rec)
			return &rec, errors.New(rec.Error)
		}
	}

	// 5. Commit verified knowledge into Buyer's own memory shard & record verified Seller reputation!
	// Anchor synchronously in memory mode; on live testnet/main commit locally immediately so Want returns fast.
	targetShard := strings.TrimSpace(req.TargetShard)
	if targetShard == "" {
		targetShard = "market/" + req.Domains[0]
	}
	if _, err := b.mem.Remember(ctx, memory.RememberInput{
		Shard:      targetShard,
		Text:       string(plaintext),
		Source:     "market:" + shortPub(chosen.Pub),
		Class:      "market:" + req.Domains[0],
		Tainted:    true,
		AutoAnchor: b.mem.Chain().Network() == "memory" && b.mem.Chain().BalanceSats() > 1000,
	}); err != nil {
		rec.Status = "failed"
		rec.Error = "commit purchased fact: " + err.Error()
		_ = b.mem.DB().SaveDeal(rec)
		return &rec, err
	}

	_ = b.mem.DB().RecordReputation(chosen.Pub, true, chosen.PriceSats)
	rec.Shard = targetShard
	rec.ClaimKeyHex = pResp.ContentKeyHex
	rec.DeliveredText = string(plaintext)
	rec.Status = "settled"
	rec.Error = ""
	_ = b.mem.DB().SaveDeal(rec)
	b.notify("deal_settled", map[string]any{"role": "buyer", "dealId": rec.DealID, "txid": payTxID, "sats": chosen.PriceSats, "shard": targetShard})
	return &rec, nil
}
