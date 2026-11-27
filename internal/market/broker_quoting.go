package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

// autoQuoteWant inspects an incoming verified WantAd and automatically quotes our best matching anchored fragment.
func (b *Broker) autoQuoteWant(ctx context.Context, want bstack.WantAd) {
	_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
	kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
	if kill {
		return
	}
	// Hub makers or explicitly approved sellers quote automatically.
	if !b.cfg.IsHub && !sellerOK {
		return
	}
	listings, err := b.mem.DB().ListSellListings()
	if err != nil || len(listings) == 0 {
		return
	}
	wantDoms := make(map[string]bool, len(want.Domains))
	for _, d := range want.Domains {
		wantDoms[strings.ToLower(strings.TrimSpace(d))] = true
	}

	// Collect ALL active SellListings eligible under domain & maxSats (no first-listing blindness).
	var eligibleListings []store.SellListing
	for i := range listings {
		l := listings[i]
		if !l.Active {
			continue
		}
		if want.MaxSats > 0 && l.PriceSats > want.MaxSats {
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
		return
	}

	// Cap active pendingSales (total + per-buyer) to prevent WantAd flood RAM/CHIRP/workflow exhaustion.
	nowCheck := time.Now().Unix()
	b.mu.Lock()
	buyerActive := 0
	for id, old := range b.pendingSales {
		if old.ExpiresAt > 0 && old.ExpiresAt < nowCheck {
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
		return
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
		frags, rErr := b.mem.Recall(ctx, memory.RecallInput{
			Shard:        listing.Shard,
			Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
			Action:       "disclose",
			DisallowBorg: true,
		})
		if rErr != nil || len(frags) == 0 {
			continue
		}
		for _, f := range frags {
			// Fail-closed Cedar Action::"sell" distillation gate (resource.distilled == true unless listing.AllowRaw).
			if dec, aErr := b.mem.AuthorizeSell(
				policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
				policy.Resource{
					Type:      "Fragment",
					ID:        fmt.Sprintf("%s#%d", f.Shard, f.Seq),
					Level:     int64(f.Level),
					Shard:     f.Shard,
					Class:     f.Class,
					Distilled: f.Distilled,
				},
				listing.AllowRaw,
			); aErr != nil || !dec.Allow {
				continue
			}
			// Fail-closed PII & secret scrubber before ANY fragment can be quoted externally.
			if sr := distill.Scrub(f.Text); sr.Verdict == distill.ScrubBlocked {
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
				fragOntID, _, _ := b.mem.DB().GetFragmentGraphMeta(f.Shard, f.Seq)
				if fragOntID == "" {
					fragOntID, _ = b.mem.DB().FragmentOntologyID(f.Shard, f.Seq)
				}
				if !standingOrderOntologyMatches(ctx, b.mem, want.OntologyID, true, fragOntID) {
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
		return
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
			return
		}
		lCopy := dec.Chosen.Listing
		fCopy := dec.Chosen.Fragment
		matched = &lCopy
		best = &fCopy
	} else {
		dec := deterministicSellerDecision(want.Query, sellerCands, "deterministic")
		if !dec.Approved || dec.Chosen == nil {
			return
		}
		lCopy := dec.Chosen.Listing
		fCopy := dec.Chosen.Fragment
		matched = &lCopy
		best = &fCopy
	}
	if matched == nil || best == nil {
		return
	}
	// Ensure fragment has a verified SPV ProofBundle before quoting (on live networks require mined & settled)!
	if best.AnchorTxID == "" && b.mem.Chain().Network() == "memory" && b.mem.Chain().BalanceSats() > 1000 {
		_, _ = b.mem.AnchorShard(ctx, matched.Shard)
	}
	pb, vr, pErr := b.mem.Prove(ctx, matched.Shard, best.Seq)
	if b.mem.Chain().Network() != "memory" {
		if pErr != nil || pb == nil || !vr.OK || !vr.Settled {
			var reason, detail string
			if vr != nil {
				reason, detail = vr.Reason, vr.Detail
			}
			log.Printf("market: skip quote want=%s shard=%s#%d: prove err=%v ok=%v settled=%v reason=%q detail=%q", want.WantID, matched.Shard, best.Seq, pErr, vr != nil && vr.OK, vr != nil && vr.Settled, reason, detail)
			return // never quote an unmined/unsettled fragment on live networks
		}
	}

	quotePrice := matched.PriceSats
	if len(matched.DynamicPricingJSON) > 0 || (b.mem != nil && b.mem.DB() != nil && want.Pub != "") {
		dp := ParseDynamicPriceState(matched.DynamicPricingJSON, matched.PriceSats)
		buyerPriority := false
		var buyerCollabBP int64
		if b.mem != nil && b.mem.DB() != nil && want.Pub != "" {
			evalOnt := reputation.NormalizeOntologyID(want.OntologyID)
			if tEval, vecs, tErr := EvaluateTrustForOntology(b.mem.DB(), b.PubHex(), evalOnt, b.engine.Policy(), nil, nil); tErr == nil && tEval != nil {
				bPub := strings.ToLower(strings.TrimSpace(want.Pub))
				buyerPriority = tEval.PriorityBuyers[bPub]
				if v, ok := vecs[bPub]; ok {
					buyerCollabBP = v.CollabBP
				}
			}
		}
		quotePrice = dp.QuotePriceForBuyer(matched.PriceSats, buyerPriority, buyerCollabBP)
		if want.MaxSats > 0 && quotePrice > want.MaxSats {
			return
		}
	}

	saleOffer, revealKey, chirpURL, err := b.engine.PrepareDirectSale(ctx, matched.Shard, best.Seq, quotePrice, want.Pub)
	if err != nil {
		log.Printf("market: PrepareDirectSale want=%s shard=%s#%d failed: %v", want.WantID, matched.Shard, best.Seq, err)
		return
	}
	// Keep OfferAd wire payload compact under MaxPayload (16KB): strip ciphertext if > 4KB (buyer fetches via CHIRP).
	wireSale := *saleOffer
	if len(wireSale.CiphertextHex) > 8192 {
		wireSale.CiphertextHex = ""
	}
	saleJSON, _ := json.Marshal(wireSale)
	payAddr, _ := b.mem.Chain().FundAddress()
	offerID := fmt.Sprintf("off-%s-%s", shortPub(b.PubHex()), randIDHex())

	// Build and verify OfferAd fits within post-CBOR bstack.MaxPayload BEFORE persisting pendingSale!
	b.mu.RLock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	isHub := b.cfg.IsHub
	b.mu.RUnlock()

	var offerTags []string
	if cls := sanitizeUntrusted(best.Class, 32); cls != "" {
		offerTags = append(offerTags, cls)
	}
	if best.AnchorTxID != "" {
		offerTags = append(offerTags, "anchored")
	}
	var offerBondOutpoint, offerBondBEEF string
	if bRec, _, bErr := LoadActiveBond(b.mem.DB()); bErr == nil && bRec != nil && !bRec.State.Seized {
		offerBondOutpoint = bRec.Outpoint()
		if len(bRec.BEEFHex) <= 8192 {
			offerBondBEEF = bRec.BEEFHex
		}
	}
	// Include both matched listing domains and want domains (capped at bstack.MaxDomains) so
	// OfferAd topics ALWAYS satisfy Hub topicMatchesDomains() and reach the buyer's want lanes!
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
	bestOntID, bestEdgesRoot, _ := b.mem.DB().GetFragmentGraphMeta(matched.Shard, best.Seq)
	if bestOntID == "" {
		bestOntID, _ = b.mem.DB().FragmentOntologyID(matched.Shard, best.Seq)
	}
	offer := bstack.OfferAd{
		OfferID:        offerID,
		WantID:         want.WantID,
		Pub:            b.PubHex(),
		BuyerPub:       want.Pub,
		Net:            b.mem.Chain().Network(),
		Domains:        offerDomains,
		PriceSats:      quotePrice,
		Shard:          matched.Shard,
		Leaf:           best.Leaf,
		OntologyID:     bestOntID,
		EdgesRoot:      bestEdgesRoot,
		ChirpURL:       chirpURL,
		KeyCommitHex:   saleOffer.KeyCommitment,
		PayAddress:     payAddr,
		PayIdentityKey: b.mem.Chain().IdentityPubHex(),
		SellerURL:      pubURL,
		BboxOffice:     office,
		SaleOffer:      saleJSON,
		Description:    sanitizeUntrusted(matched.Description, maxOfferDescChars),
		Tags:           offerTags,
		BondOutpoint:   offerBondOutpoint,
		BondBEEFHex:    offerBondBEEF,
		IsHub:          isHub,
		Direct:         pubURL != "",
		Revision:       1,
		Exp:            time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	var offerPayload bstack.MarketPayload
	payloadReady := false
	for attempt := 0; attempt < 3; attempt++ {
		sig, sErr := bstack.SignArtifact(b.engine.saleKey, offer)
		if sErr != nil {
			return
		}
		body, mErr := json.Marshal(bstack.SignedOffer{Offer: offer, Sig: sig})
		if mErr != nil {
			return
		}
		payload := bstack.MarketPayload{Kind: bstack.KindOffer, Body: body}
		if encoded, eErr := bstack.EncodePayload(payload); eErr == nil && len(encoded) <= bstack.MaxPayload {
			offerPayload = payload
			payloadReady = true
			break
		}
		// Strip inline ciphertext first, then BondBEEFHex if still > MaxPayload.
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
		return
	}
	if !payloadReady {
		return
	}

	ps := &pendingSale{
		OfferID:     offerID,
		WantID:      want.WantID,
		BuyerPub:    want.Pub,
		Shard:       matched.Shard,
		Seq:         best.Seq,
		SaleOffer:   saleOffer,
		RevealKey:   revealKey,
		ChirpURL:    chirpURL,
		ProofBundle: pb,
		ExpiresAt:   int64(saleOffer.ExpiresAt),
	}
	b.mu.Lock()
	b.pendingSales[offerID] = ps
	nowUnix := time.Now().Unix()
	for id, old := range b.pendingSales {
		if old.ExpiresAt > 0 && old.ExpiresAt < nowUnix {
			core.Zero32Array(&old.RevealKey)
			if old.SaleOffer != nil {
				old.SaleOffer.ClearContentKey()
			}
			delete(b.pendingSales, id)
		}
	}
	b.mu.Unlock()
	fullSaleJSON, _ := json.Marshal(saleOffer)
	var pbJSON []byte
	if pb != nil {
		pbJSON, _ = json.Marshal(pb)
	}
	sealedRevealKey, sealErr := b.sealKeyAtRest(ctx, hex.EncodeToString(revealKey[:]))
	if sealErr != nil {
		log.Printf("market: seal pending sale key for offer %s failed: %v", offerID, sealErr)
		return
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

	topics := lanesForDomains(offer.Net, offer.Domains)
	if subN, subErr := b.disc.BuildAndSubmitMarketAd(ctx, b.engine.saleKey, offerPayload, b.disc.ActiveHosts(), topics); subN == 0 && subErr != nil {
		log.Printf("market: submit OfferAd %s (want=%s) failed: %v", offerID, want.WantID, subErr)
	}
	// Start durable Restate SellerDeal workflow asynchronously (/send — never blocks SSE consumer!).
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
