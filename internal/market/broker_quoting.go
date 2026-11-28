package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

// HandleDirectQuote verifies a Buyer's signed DirectQuoteRequest and returns a signed OfferAd
// directly over HTTP, skipping Hub WantAd/OfferAd round-trips while enforcing identical Cedar/NMU/SPV rules.
func (b *Broker) HandleDirectQuote(ctx context.Context, req DirectQuoteRequest) (*bstack.OfferAd, error) {
	if strings.TrimSpace(req.BuyerPub) == "" || strings.TrimSpace(req.BuyerSig) == "" {
		return nil, errors.New("market: signed direct quote request (buyerPub + buyerSig) is required")
	}
	nowUnix := time.Now().Unix()
	if req.TS < nowUnix-300 || req.TS > nowUnix+300 {
		return nil, errors.New("market: expired or missing direct quote timestamp")
	}
	if err := bstack.VerifyArtifact(req.BuyerPub, req.BuyerSig, req); err != nil {
		return nil, fmt.Errorf("market: invalid direct quote signature: %w", err)
	}
	wantID := strings.TrimSpace(req.WantID)
	if wantID == "" {
		wantID = "dq-" + randIDHex()
	}
	want := bstack.WantAd{
		WantID:  wantID,
		Pub:     req.BuyerPub,
		Net:     b.mem.Chain().Network(),
		Domains: req.Domains,
		Query:   req.Query,
		MaxSats: req.MaxSats,
		Exp:     time.Now().Add(5 * time.Minute).UnixMilli(),
	}
	offer, _, ok := b.buildOfferForWant(ctx, want, true)
	if !ok || offer == nil {
		return nil, errors.New("market: no matching sellable fragment for direct quote")
	}
	return offer, nil
}

// autoQuoteWant inspects an incoming verified WantAd and automatically quotes our best matching anchored fragment.
func (b *Broker) autoQuoteWant(ctx context.Context, want bstack.WantAd) {
	offer, offerPayload, ok := b.buildOfferForWant(ctx, want, false)
	if !ok || offer == nil {
		return
	}
	topics := topicsForMarketAd(offer.Net, offer.Domains, offer.OntologyID)
	if subN, subErr := b.disc.BuildAndSubmitMarketAd(ctx, b.engine.signer, offerPayload, b.disc.ActiveHosts(), topics); subN == 0 && subErr != nil {
		log.Printf("market: submit OfferAd %s (want=%s) failed: %v", offer.OfferID, want.WantID, subErr)
	}
}

func (b *Broker) buildOfferForWant(ctx context.Context, want bstack.WantAd, directOnly bool) (*bstack.OfferAd, bstack.MarketPayload, bool) {
	fj := b.engine.FastJournal()
	if fj != nil {
		fj.CachedPolicySync(b.mem.DB(), b.engine.Policy())
	} else {
		_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
	}
	kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
	if kill {
		return nil, bstack.MarketPayload{}, false
	}
	// Hub makers or explicitly approved sellers quote automatically.
	if !b.cfg.IsHub && !sellerOK {
		return nil, bstack.MarketPayload{}, false
	}
	var listings []store.SellListing
	var err error
	if fj != nil {
		listings, err = fj.CachedListings(b.mem.DB())
	} else {
		listings, err = b.mem.DB().ListSellListings()
	}
	if err != nil || len(listings) == 0 {
		return nil, bstack.MarketPayload{}, false
	}
	wantDoms := make(map[string]bool, len(want.Domains))
	for _, d := range want.Domains {
		wantDoms[strings.ToLower(strings.TrimSpace(d))] = true
	}

	// Collect ALL active SellListings eligible under domain, pricing mode, & base budget (no first-listing blindness).
	wantMode := nmu.EffectivePricingMode(nmu.PricingMode(want.PricingMode))
	wantMinGrade := nmu.NormalizeGrade(want.MinGrade)
	wantReqRights := nmu.NormalizeRights(want.RequiredRights)
	wantFresh := nmu.NormalizeFreshness(want.FreshnessClass)

	var eligibleListings []store.SellListing
	for i := range listings {
		l := listings[i]
		if !l.Active {
			continue
		}
		if !l.SupportsMode(string(wantMode)) {
			continue
		}
		if !l.PricePerUnit && wantMode == nmu.ModeSpot && want.MaxSats > 0 && l.PriceSats > want.MaxSats {
			continue
		}
		for _, ld := range l.Domains {
			if ld == "*" || wantDoms[strings.ToLower(ld)] {
				eligibleListings = append(eligibleListings, l)
				break
			}
		}
	}
	if len(eligibleListings) == 0 {
		return nil, bstack.MarketPayload{}, false
	}

	b.mu.RLock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	isHub := b.cfg.IsHub
	b.mu.RUnlock()

	var offerBondOutpoint, offerBondBEEF string
	hasActiveBond := false
	if fj != nil {
		hasActiveBond, offerBondOutpoint, offerBondBEEF = fj.CachedActiveBond(b.mem.DB())
	} else if bRec, _, bErr := LoadActiveBond(b.mem.DB()); bErr == nil && bRec != nil && !bRec.State.Seized {
		hasActiveBond = true
		offerBondOutpoint = bRec.Outpoint()
		if len(bRec.BEEFHex) <= 8192 {
			offerBondBEEF = bRec.BEEFHex
		}
	}

	// Cap active unsettled pendingSales (total + per-buyer) without O(N) map scans on fast paths.
	nowCheck := time.Now().Unix()
	if fj != nil {
		b.mu.RLock()
		overCap := len(b.pendingSales) >= maxPendingSalesTotal
		b.mu.RUnlock()
		if overCap {
			return nil, bstack.MarketPayload{}, false
		}
	} else {
		b.mu.Lock()
		buyerActive := 0
		for id, old := range b.pendingSales {
			if old.Settled || (old.ExpiresAt > 0 && old.ExpiresAt < nowCheck) {
				core.Zero32Array(&old.RevealKey)
				if old.SaleOffer != nil {
					old.SaleOffer.ClearContentKey()
				}
				delete(b.pendingSales, id)
				continue
			}
			if old.BuyerPub == want.Pub {
				buyerActive++
			}
		}
		overCap := len(b.pendingSales) >= maxPendingSalesTotal || buyerActive >= maxPendingSalesPerBuyer
		b.mu.Unlock()
		if overCap {
			return nil, bstack.MarketPayload{}, false
		}
	}

	// Stage 1 (Deterministic Cedar Pre-Gate): ONLY Level 0, non-Borg, Cedar-"disclose" fragments
	// from explicitly armed sell_listings may enter candidate selection.
	tokens := extractQueryTokens(want.Query)
	matcher := b.Matcher()
	useAI := false
	if matcher != nil {
		aiPol := AIMatchingPolicy{}
		if p := b.engine.Policy().AIMatchingPolicy(); p != nil {
			aiPol = *p
		}
		useAI = aiPol.Normalized(matcher.DefaultMode()).Mode != AIModeOff
	}

	var sellerCands []SellerCandidate
	for _, listing := range eligibleListings {
		var frags []memory.RecalledFragment
		var rErr error
		if fj != nil {
			frags, rErr = fj.CachedSellableFragments(ctx, b.mem, listing.Shard)
		} else {
			// PERF-2: skip per-candidate SPV verification during shard candidate scan; Prove() runs strictly ONCE on the winner!
			frags, rErr = b.mem.Recall(ctx, memory.RecallInput{
				Shard:        listing.Shard,
				Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
				Action:       "disclose",
				DisallowBorg: true,
				VerifySPV:    false,
			})
		}
		if rErr != nil || len(frags) == 0 {
			continue
		}
		candFeedURL := ""
		if strings.TrimSpace(pubURL) != "" && listing.SupportsMode(string(nmu.ModeSubscription)) {
			candFeedURL = strings.TrimRight(strings.TrimSpace(pubURL), "/") + "/v1/market/feeds/" + listing.Shard
		}
		for _, f := range frags {
			// RCA-1: single authoritative fail-closed sell egress gate (Cedar Action::"sell", PII Scrub, Tainted resale, NMU TTL, MinGrade).
			eval := b.mem.EvaluateSellableFragment(ctx, f, &listing, hasActiveBond, candFeedURL, nowCheck)
			if !eval.Allowed {
				continue
			}
			if wantMinGrade != "" && !nmu.MeetsMinGrade(eval.Classification.Grade, wantMinGrade) {
				continue
			}
			if wantReqRights != "" && !nmu.MeetsRequiredRights(eval.UsageRights, wantReqRights) {
				continue
			}
			if wantFresh != "" && eval.FreshnessClass != wantFresh {
				continue
			}
			// On live networks, require an anchor tx (mined or 0-conf carried first-party BEEF);
			// Prove() below enforces full SPV verification of either mined BUMP or carried ancestry.
			if b.mem.Chain().Network() != "memory" {
				if f.AnchorTxID == "" {
					continue
				}
			}
			// Ontology pin filtering: when the buyer pinned want.OntologyID, quote ONLY fragments conforming
			// to that exact ontology (or bridged to it via a locally committed Bridge Shard).
			if want.OntologyID != "" {
				if !standingOrderOntologyMatches(ctx, b.mem, want.OntologyID, true, eval.OntologyID) {
					continue
				}
			}
			if useAI {
				// Dedicated Cedar pre-egress check: allows operators to forbid Agent::"jev-matcher"
				// on specific shards/classes without disabling local selling.
				if dec, aErr := b.mem.AuthorizeAction(policy.JevMatcherPrincipal, "disclose", policy.Resource{
					Type:  "Fragment",
					ID:    fmt.Sprintf("%s#%d", f.Shard, f.Seq),
					Level: int64(f.Level),
					Shard: f.Shard,
					Class: f.Class,
				}); aErr != nil || !dec.Allow {
					continue
				}
			}
			lexScore := lexicalFragmentScore(f.Text, tokens)
			// When Jev is offline/disabled, require lexical overlap for cluster-only matches
			// so broad cluster tags (e.g. cluster:sports-fitness) never blindly quote unrelated data.
			if !useAI && len(tokens) > 0 && lexScore == 0 && !HasExactUserDomainOverlap(want.Domains, listing.Domains) {
				continue
			}
			sellerCands = append(sellerCands, SellerCandidate{
				Listing:      listing,
				Fragment:     f,
				LexicalScore: lexScore,
			})
		}
	}
	if len(sellerCands) == 0 {
		return nil, bstack.MarketPayload{}, false
	}

	var matched *store.SellListing
	var best *memory.RecalledFragment
	if useAI {
		// Pre-sort candidates by lexical score desc, then newest seq desc, and cap at maxSellerJevCandidates.
		sort.SliceStable(sellerCands, func(i, j int) bool {
			if sellerCands[i].LexicalScore != sellerCands[j].LexicalScore {
				return sellerCands[i].LexicalScore > sellerCands[j].LexicalScore
			}
			return sellerCands[i].Fragment.Seq > sellerCands[j].Fragment.Seq
		})
		dec := matcher.MatchWantForSeller(ctx, want, sellerCands, b.engine.Policy())
		if !dec.Approved || dec.Chosen == nil {
			return nil, bstack.MarketPayload{}, false
		}
		lCopy := dec.Chosen.Listing
		fCopy := dec.Chosen.Fragment
		matched = &lCopy
		best = &fCopy
	} else {
		dec := deterministicSellerDecision(want.Query, sellerCands, "deterministic")
		if !dec.Approved || dec.Chosen == nil {
			return nil, bstack.MarketPayload{}, false
		}
		lCopy := dec.Chosen.Listing
		fCopy := dec.Chosen.Fragment
		matched = &lCopy
		best = &fCopy
	}
	if matched == nil || best == nil {
		return nil, bstack.MarketPayload{}, false
	}
	// Ensure fragment has a verified SPV ProofBundle before quoting (cached for immutable anchored fragments!).
	pbKey := fmt.Sprintf("pb:%s:%d:%s:%s", matched.Shard, best.Seq, best.Leaf, best.AnchorTxID)
	var pb *contracts.ProofBundle
	if fj != nil {
		if _, _, cachedPB, ok := fj.GetCachedCapsule(pbKey); ok && cachedPB != nil {
			pb = cachedPB
		}
	}
	if pb == nil {
		if best.AnchorTxID == "" && b.mem.Chain().Network() == "memory" && b.mem.Chain().BalanceSats() > 1000 {
			_, _ = b.mem.AnchorShard(ctx, matched.Shard)
		}
		var vr *contracts.VerifyResult
		var pErr error
		pb, vr, pErr = b.mem.Prove(ctx, matched.Shard, best.Seq)
		if b.mem.Chain().Network() != "memory" {
			if pErr != nil || pb == nil || !vr.OK || !vr.Settled {
				var reason, detail string
				if vr != nil {
					reason, detail = vr.Reason, vr.Detail
				}
				log.Printf("market: skip quote want=%s shard=%s#%d: prove err=%v ok=%v settled=%v reason=%q detail=%q", want.WantID, matched.Shard, best.Seq, pErr, vr != nil && vr.OK, vr != nil && vr.Settled, reason, detail)
				return nil, bstack.MarketPayload{}, false
			}
		}
		if fj != nil && pb != nil {
			fj.PutCachedCapsule(pbKey, []byte{1}, "pb", pb)
		}
	}

	bestFeedURL := ""
	if strings.TrimSpace(pubURL) != "" && matched.SupportsMode(string(nmu.ModeSubscription)) {
		bestFeedURL = strings.TrimRight(strings.TrimSpace(pubURL), "/") + "/v1/market/feeds/" + matched.Shard
	}
	clsKey := fmt.Sprintf("cls:%s:%d:%s:%v:%s", matched.Shard, best.Seq, best.Leaf, hasActiveBond, bestFeedURL)
	var bestCls nmu.GradeClassification
	var bestOntID, bestEdgesRoot string
	if fj != nil {
		if c, oid, er, ok := fj.GetCachedClassification(clsKey); ok {
			bestCls, bestOntID, bestEdgesRoot = c, oid, er
		} else {
			bestCls, bestOntID, bestEdgesRoot = b.classifyFragmentForMarket(ctx, matched.Shard, best.Seq, best.Distilled, best.OntologyID, hasActiveBond, bestFeedURL)
			fj.PutCachedClassification(clsKey, bestCls, bestOntID, bestEdgesRoot)
		}
	} else {
		bestCls, bestOntID, bestEdgesRoot = b.classifyFragmentForMarket(ctx, matched.Shard, best.Seq, best.Distilled, best.OntologyID, hasActiveBond, bestFeedURL)
	}
	if len(bestOntID) != 64 {
		bestOntID = ""
	}
	if len(bestEdgesRoot) != 64 {
		bestEdgesRoot = ""
	}
	bestUnits := bestCls.UnitCount
	if bestUnits == 0 {
		bestUnits = 1
	}

	// Compute quote price & normalized satsPer1kUnits (honoring multi-mode & PricePerUnit).
	baseQuoteAsk := matched.PriceSats
	var subSec int64
	switch wantMode {
	case nmu.ModePerQuery:
		baseQuoteAsk = matched.QueryPriceSats
	case nmu.ModeSubscription:
		baseQuoteAsk = matched.SubscriptionPriceSats
		subSec = matched.EffectiveSubscriptionDurationSec()
	}
	dp := ParseDynamicPriceState(matched.DynamicPricingJSON, baseQuoteAsk)
	if wantMode == nmu.ModeSpot {
		dp.PerUnitMode = matched.PricePerUnit || dp.PerUnitMode
	} else {
		dp.PerUnitMode = false
		dp.Enabled = false
	}
	buyerPriority := false
	var buyerCollabBP int64
	if fj == nil && b.mem != nil && b.mem.DB() != nil && want.Pub != "" {
		evalOnt := reputation.NormalizeOntologyID(want.OntologyID)
		if tEval, vecs, tErr := EvaluateTrustForOntology(b.mem.DB(), b.PubHex(), evalOnt, b.engine.Policy(), nil, nil); tErr == nil && tEval != nil {
			bPub := strings.ToLower(strings.TrimSpace(want.Pub))
			buyerPriority = tEval.PriorityBuyers[bPub]
			if v, ok := vecs[bPub]; ok {
				buyerCollabBP = v.CollabBP
			}
		}
	}
	quotePrice, quoteSatsPer1k := dp.QuoteForFactPack(baseQuoteAsk, bestUnits, buyerPriority, buyerCollabBP)
	if want.MaxSats > 0 && quotePrice > want.MaxSats {
		return nil, bstack.MarketPayload{}, false
	}
	if want.MaxSatsPer1kUnits > 0 && quoteSatsPer1k > want.MaxSatsPer1kUnits {
		return nil, bstack.MarketPayload{}, false
	}

	effFresh := nmu.NormalizeFreshness(matched.FreshnessClass)
	effRights := nmu.EffectiveRights(nmu.UsageRights(matched.UsageRights))
	var effValidUntil int64
	var effSubjVer string
	if best.NMU != nil {
		if best.NMU.FreshnessClass != "" {
			effFresh = nmu.NormalizeFreshness(string(best.NMU.FreshnessClass))
		}
		if best.NMU.UsageRights != "" {
			effRights = nmu.EffectiveRights(best.NMU.UsageRights)
		}
		effValidUntil = best.NMU.ValidUntilUnix
		effSubjVer = best.NMU.SubjectVersion
	}

	saleOffer, revealKey, chirpURL, err := b.engine.PrepareDirectSaleWithFragment(ctx, *best, matched, quotePrice, want.Pub)
	if err != nil {
		log.Printf("market: PrepareDirectSale want=%s shard=%s#%d failed: %v", want.WantID, matched.Shard, best.Seq, err)
		return nil, bstack.MarketPayload{}, false
	}
	// Two-tier payload delivery: inline <= 12 KiB ciphertexts directly inside OfferAd (zero CHIRP fetch!);
	// offload > 12 KiB capsules/blobs to bstore + CHIRP.
	wireSale := *saleOffer
	if len(wireSale.CiphertextHex) > bstack.InlineOfferMaxHexLen {
		wireSale.CiphertextHex = ""
	}
	saleJSON, _ := json.Marshal(wireSale)
	payAddr, _ := b.mem.Chain().FundAddress()
	offerID := fmt.Sprintf("off-%s-%s", shortPub(b.PubHex()), randIDHex())

	var offerTags []string
	if cls := sanitizeUntrusted(best.Class, 32); cls != "" {
		offerTags = append(offerTags, cls)
	}
	if best.AnchorTxID != "" {
		offerTags = append(offerTags, "anchored")
	}
	if bestCls.Grade != "" {
		offerTags = append(offerTags, "grade:"+string(bestCls.Grade))
	}
	domSeen := make(map[string]bool)
	offerDomains := make([]string, 0, len(want.Domains)+len(matched.Domains))
	for _, d := range append(append([]string(nil), want.Domains...), matched.Domains...) {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" && !domSeen[d] {
			domSeen[d] = true
			offerDomains = append(offerDomains, d)
			if len(offerDomains) >= bstack.MaxDomains {
				break
			}
		}
	}
	offerNMU := &nmu.Spec{
		Version:         nmu.SpecVersion,
		Grade:           bestCls.Grade,
		UnitCount:       bestUnits,
		SatsPer1kUnits:  quoteSatsPer1k,
		FreshnessClass:  effFresh,
		ValidUntilUnix:  effValidUntil,
		SubjectVersion:  effSubjVer,
		UsageRights:     effRights,
		PricingMode:     wantMode,
		SubscriptionSec: subSec,
		FeedURL:         bestFeedURL,
		AdvisoryNote:    nmu.AdvisoryNMUNote,
	}
	offer := bstack.OfferAd{
		OfferID:         offerID,
		WantID:          want.WantID,
		Pub:             b.PubHex(),
		BuyerPub:        want.Pub,
		Net:             b.mem.Chain().Network(),
		Domains:         offerDomains,
		PriceSats:       quotePrice,
		Grade:           string(bestCls.Grade),
		UnitCount:       bestUnits,
		SatsPer1kUnits:  quoteSatsPer1k,
		FreshnessClass:  string(effFresh),
		ValidUntilUnix:  effValidUntil,
		SubjectVersion:  effSubjVer,
		UsageRights:     string(effRights),
		PricingMode:     string(wantMode),
		SubscriptionSec: subSec,
		FeedURL:         bestFeedURL,
		Shard:           matched.Shard,
		PayloadFormat:   saleOffer.PayloadFormat,
		Leaf:            best.Leaf,
		OntologyID:      bestOntID,
		EdgesRoot:       bestEdgesRoot,
		ChirpURL:        chirpURL,
		KeyCommitHex:    saleOffer.KeyCommitment,
		PayAddress:      payAddr,
		PayIdentityKey:  b.mem.Chain().IdentityPubHex(),
		SellerURL:       pubURL,
		BboxOffice:      office,
		SaleOffer:       saleJSON,
		Description:     sanitizeUntrusted(matched.Description, maxOfferDescChars),
		Tags:            offerTags,
		BondOutpoint:    offerBondOutpoint,
		BondBEEFHex:     offerBondBEEF,
		IsHub:           isHub,
		Direct:          pubURL != "",
		Revision:        1,
		Exp:             time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	var offerPayload bstack.MarketPayload
	if !directOnly {
		payloadReady := false
		for attempt := 0; attempt < 3; attempt++ {
			sig, sErr := bstack.SignArtifactWithSigner(b.engine.signer, offer)
			if sErr != nil {
				return nil, bstack.MarketPayload{}, false
			}
			payload, mErr := bstack.NewOfferPayload(bstack.SignedOffer{Offer: offer, Sig: sig})
			if mErr != nil {
				return nil, bstack.MarketPayload{}, false
			}
			if encoded, eErr := bstack.EncodePayload(payload); eErr == nil && len(encoded) <= bstack.MaxPayload {
				offerPayload = payload
				payloadReady = true
				break
			}
			if attempt == 0 && wireSale.CiphertextHex != "" {
				wireSale.CiphertextHex = ""
				saleJSON, _ = json.Marshal(wireSale)
				offer.SaleOffer = saleJSON
				continue
			}
			if offer.BondBEEFHex != "" {
				offer.BondBEEFHex = ""
				continue
			}
			return nil, bstack.MarketPayload{}, false
		}
		if !payloadReady {
			return nil, bstack.MarketPayload{}, false
		}
	}

	ps := &pendingSale{
		OfferID:     offerID,
		WantID:      want.WantID,
		BuyerPub:    want.Pub,
		Shard:       matched.Shard,
		Seq:         best.Seq,
		SaleOffer:   saleOffer,
		OfferNMU:    offerNMU,
		RevealKey:   revealKey,
		ChirpURL:    chirpURL,
		ProofBundle: pb,
		ExpiresAt:   int64(saleOffer.ExpiresAt),
	}
	b.mu.Lock()
	b.pendingSales[offerID] = ps
	if fj == nil || len(b.pendingSales) > 8192 {
		nowUnix := time.Now().Unix()
		for id, old := range b.pendingSales {
			if id != offerID && (old.Settled || (old.ExpiresAt > 0 && old.ExpiresAt < nowUnix)) {
				core.Zero32Array(&old.RevealKey)
				if old.SaleOffer != nil {
					old.SaleOffer.ClearContentKey()
				}
				delete(b.pendingSales, id)
			}
		}
	}
	b.mu.Unlock()

	if fj == nil {
		fullSaleJSON, _ := json.Marshal(saleOffer)
		var pbJSON []byte
		if pb != nil {
			pbJSON, _ = json.Marshal(pb)
		}
		sealedRevealKey, sealErr := b.sealKeyAtRest(ctx, hex.EncodeToString(revealKey[:]))
		if sealErr != nil {
			log.Printf("market: seal pending sale key for offer %s failed: %v", offerID, sealErr)
			return nil, bstack.MarketPayload{}, false
		}
		_ = b.mem.DB().SavePendingSale(store.PendingSaleRecord{
			OfferID:         offerID,
			WantID:          want.WantID,
			BuyerPub:        want.Pub,
			Shard:           matched.Shard,
			Seq:             best.Seq,
			SaleOfferJSON:   fullSaleJSON,
			RevealKeyHex:    sealedRevealKey,
			ChirpURL:        chirpURL,
			ProofBundleJSON: pbJSON,
			ExpiresAt:       int64(saleOffer.ExpiresAt),
		})
		if rc := b.RestateClient(); rc != nil {
			oid, w, sh, sq, price, bpub, so, rk, cu, bundle := offerID, want, matched.Shard, best.Seq, quotePrice, want.Pub, saleOffer, revealKey, chirpURL, pb
			b.spawnTracked(10*time.Second, func(sCtx context.Context) {
				_, _ = rc.StartSellerDealAsync(sCtx, oid, SellerDealInput{
					Want:            w,
					Shard:           sh,
					Seq:             sq,
					PriceSats:       price,
					BuyerPub:        bpub,
					PreCreatedOffer: so,
					RevealKeyHex:    hex.EncodeToString(rk[:]),
					ChirpURL:        cu,
					ProofBundle:     bundle,
				})
			})
		}
	}
	return &offer, offerPayload, true
}

func pickBestFragment(frags []memory.RecalledFragment, query string) *memory.RecalledFragment {
	if len(frags) == 0 {
		return nil
	}
	var tokens []string
	for _, raw := range strings.Fields(strings.ToLower(query)) {
		tok := strings.Trim(raw, ".,?!:;\"'()[]{}")
		if len(tok) >= 3 {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) == 0 {
		cp := frags[len(frags)-1]
		return &cp
	}
	bestIdx := -1
	bestScore := 0
	for i, f := range frags {
		lower := strings.ToLower(f.Text)
		score := 0
		for _, tok := range tokens {
			if strings.Contains(lower, tok) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil
	}
	cp := frags[bestIdx]
	return &cp
}

// classifyFragmentForMarket computes the exact provable NMU-1 Grade and ontology-scoped UnitCount
// for one local fragment using canonical triples, L1 MPE graph metadata, active bond, and live feed availability.
func (b *Broker) classifyFragmentForMarket(
	ctx context.Context,
	shard string,
	seq int64,
	distilled bool,
	fragOnt string,
	hasActiveBond bool,
	liveFeedURL string,
) (nmu.GradeClassification, string, string) {
	if b == nil || b.mem == nil || b.mem.DB() == nil {
		return nmu.GradeClassification{Grade: nmu.GradeC, UnitCount: 1}, fragOnt, ""
	}
	ontID, edgesRoot, _ := b.mem.DB().GetFragmentGraphMeta(shard, seq)
	if ontID == "" {
		ontID, _ = b.mem.DB().FragmentOntologyID(shard, seq)
	}
	if ontID == "" {
		ontID = strings.ToLower(strings.TrimSpace(fragOnt))
	}
	var triples []reasoning.Triple
	if rawRec, gErr := b.mem.DB().GetFragment(shard, seq); gErr == nil && len(rawRec.TriplesJSON) > 0 {
		_ = json.Unmarshal(rawRec.TriplesJSON, &triples)
	}
	if edges, eErr := b.mem.DB().ListKnowledgeEdges(store.KnowledgeEdgeFilter{
		Shard: shard, Seq: seq, Status: "accepted", Limit: 512,
	}); eErr == nil {
		for _, ke := range edges {
			triples = append(triples, reasoning.Triple{
				Subject:   ke.Subject,
				Predicate: ke.Predicate,
				Object:    ke.Object,
				Hash:      ke.TripleHash,
			})
		}
	}
	var allowedPreds map[string]struct{}
	if ontID != "" {
		allowedPreds = b.mem.ResolveOntologyPredicateSet(ctx, ontID)
	}
	cls := nmu.ClassifyFragmentGrade(nmu.FragmentGradeInput{
		HasCleanText:           true,
		Distilled:              distilled || len(edgesRoot) == 64,
		OntologyID:             ontID,
		Triples:                triples,
		AllowedPredicates:      allowedPreds,
		MangleConsistent:       true,
		EdgesRoot:              edgesRoot,
		MPEHasOntologyAndEdges: len(ontID) == 64 && len(edgesRoot) == 64,
		ActiveBond:             hasActiveBond,
		LiveFeedURL:            liveFeedURL,
	})
	return cls, ontID, edgesRoot
}
