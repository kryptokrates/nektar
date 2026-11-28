package market

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/reputation"
)

// verifyMarketEventBEEF validates carried BEEF bytes on an incoming Hub MarketEvent before trusting its payload.
// Returns (payload, ok, retryable): retryable is true ONLY when the header tracker suffered a transport outage,
// signaling callers not to advance the Hub cursor past an unverified event.
func (b *Broker) verifyMarketEventBEEF(ctx context.Context, ev bstack.MarketEvent) (bstack.MarketPayload, bool, bool) {
	myNet := b.mem.Chain().Network()
	isLive := myNet == "main" || myNet == "testnet"
	if isLive && len(ev.BEEF) == 0 {
		return bstack.MarketPayload{}, false, false
	}
	if len(ev.BEEF) == 0 {
		return ev.Payload, true, false
	}
	offchainAds := os.Getenv("NEKTAR_OFFCHAIN_ADS") == "1"
	var tx *transaction.Transaction
	_, parsedTx, _, pErr := guard.ParseBEEF(ev.BEEF, guard.DefaultBound)
	if pErr == nil && parsedTx != nil {
		tx = parsedTx
	} else if (!isLive || offchainAds) && pErr != nil && strings.Contains(pErr.Error(), "no inputs") && len(ev.BEEF) <= 64<<10 {
		if t0, tErr := transaction.NewTransactionFromBEEF(ev.BEEF); tErr == nil && t0 != nil && len(t0.Inputs) == 0 {
			tx = t0
		}
	}
	if tx == nil || (ev.TxID != "" && !strings.EqualFold(tx.TxID().String(), ev.TxID)) || int(ev.Vout) >= len(tx.Outputs) {
		return bstack.MarketPayload{}, false, false
	}
	if isLive && (len(tx.Inputs) > 0 || !offchainAds) {
		vout, p, cErr := bstack.VerifyMarketCarrier(tx, myNet, ev.Topic, time.Now())
		if cErr != nil || vout != ev.Vout {
			return bstack.MarketPayload{}, false, false
		}
		tracker := b.mem.Chain().Tracker()
		if tracker == nil {
			return bstack.MarketPayload{}, false, true
		}
		verdict, _ := verify.Check(ctx, tx, tracker)
		if verdict == verify.Transport {
			return bstack.MarketPayload{}, false, true
		}
		if verdict != verify.Passed {
			return bstack.MarketPayload{}, false, false
		}
		return p, true, false
	}
	p, ok, _ := bstack.ExtractNkt1Payload(tx.Outputs[ev.Vout].LockingScript)
	if !ok {
		return bstack.MarketPayload{}, false, false
	}
	if isLive {
		if vErr := bstack.ValidateMarketPayload(p, myNet, ev.Topic, time.Now()); vErr != nil {
			return bstack.MarketPayload{}, false, false
		}
	}
	return p, true, false
}

// handleMarketEvent processes one Hub event and returns true iff verification was deferred due to a
// transient header-tracker outage (caller must NOT advance the Hub cursor past it).
func (b *Broker) handleMarketEvent(ctx context.Context, ev bstack.MarketEvent) bool {
	payload, ok, retryable := b.verifyMarketEventBEEF(ctx, ev)
	if retryable {
		return true
	}
	if !ok {
		return false
	}
	b.dispatchVerifiedMarketEvent(ctx, payload)
	return false
}

func (b *Broker) dispatchVerifiedMarketEvent(ctx context.Context, payload bstack.MarketPayload) {
	nowMs := time.Now().UnixMilli()
	maxExpMs := nowMs + bstack.MaxLifetime.Milliseconds() + 60_000
	myNet := b.mem.Chain().Network()
	switch payload.Kind {
	case bstack.KindWant:
		sw, err := bstack.DecodeWantPayload(payload.Body)
		if err != nil {
			return
		}
		if sw.Want.Net != myNet || sw.Want.Exp <= nowMs || sw.Want.Exp > maxExpMs {
			return
		}
		if sw.Want.Pub == b.PubHex() {
			return // ignore our own want before paying ECDSA verify cost
		}
		// Fast relevance pre-filter BEFORE ECDSA verification: skip WantAds if this node has no
		// active sell listing matching any requested domain (cuts Buyer & non-matching Seller CPU by ~10x!).
		if fj := b.engine.FastJournal(); fj != nil {
			listings, _ := fj.CachedListings(b.mem.DB())
			if len(listings) == 0 {
				return
			}
			domMatch := false
			for i := range listings {
				if !listings[i].Active {
					continue
				}
				for _, ld := range listings[i].Domains {
					if ld == "*" {
						domMatch = true
						break
					}
					for _, wd := range sw.Want.Domains {
						if strings.EqualFold(ld, wd) {
							domMatch = true
							break
						}
					}
					if domMatch {
						break
					}
				}
				if domMatch {
					break
				}
			}
			if !domMatch {
				return
			}
		}
		// Fast duplicate check BEFORE ECDSA verification to avoid re-verifying already-seen WantAds.
		b.mu.RLock()
		_, dupPre := b.seenWants[sw.Want.WantID]
		b.mu.RUnlock()
		if dupPre {
			return
		}
		if err := bstack.VerifyArtifactRaw(sw.Want.Pub, sw.Sig, sw.Want.DomainTag(), sw.RawCBOR); err != nil {
			return
		}
		b.mu.Lock()
		if _, dup := b.seenWants[sw.Want.WantID]; dup {
			b.mu.Unlock()
			return
		}
		nowUnix := time.Now().Unix()
		if len(b.seenWants) >= 4096 {
			cutoff := nowUnix - 3600
			for k, ts := range b.seenWants {
				if ts < cutoff || len(b.seenWants) >= 3072 {
					delete(b.seenWants, k)
				}
			}
		}
		b.seenWants[sw.Want.WantID] = nowUnix
		b.mu.Unlock()
		if b.engine.FastJournal() == nil {
			b.RecordMarketTape("market_want", MarketTapeEvent{
				ID:      sw.Want.WantID,
				Kind:    "want",
				Side:    "buy",
				Domains: append([]string(nil), sw.Want.Domains...),
				Summary: sw.Want.Query,
				Sats:    sw.Want.MaxSats,
				Pub:     shortPub(sw.Want.Pub),
				Status:  "open",
				At:      nowMs,
			})
		}
		b.wg.Add(1)
		go func(w bstack.WantAd) {
			defer b.wg.Done()
			if b.quoteSem != nil {
				select {
				case b.quoteSem <- struct{}{}:
					defer func() { <-b.quoteSem }()
				case <-ctx.Done():
					return
				}
			}
			b.autoQuoteWant(ctx, w)
		}(sw.Want)

	case bstack.KindOffer:
		so, err := bstack.DecodeOfferPayload(payload.Body)
		if err != nil {
			return
		}
		if so.Offer.Net != myNet || so.Offer.Exp <= nowMs || so.Offer.Exp > maxExpMs {
			return
		}
		// Fast relevance pre-filter BEFORE ECDSA verification: in fast-journal mode, only verify OfferAds
		// addressed to this Buyer's active offerInbox OR watching StandingOrders.
		if fj := b.engine.FastJournal(); fj != nil {
			if so.Offer.BuyerPub != "" && !strings.EqualFold(so.Offer.BuyerPub, b.PubHex()) {
				return
			}
			b.mu.RLock()
			_, hasInbox := b.offerInbox[so.Offer.WantID]
			b.mu.RUnlock()
			if !hasInbox && !fj.HasStandingOrders(b.mem.DB()) {
				return
			}
		}
		if leafRaw, lErr := hex.DecodeString(strings.TrimSpace(so.Offer.Leaf)); lErr != nil || len(leafRaw) != 32 {
			return
		}
		if err := bstack.VerifyArtifactRaw(so.Offer.Pub, so.Sig, so.Offer.DomainTag(), so.RawCBOR); err != nil {
			return
		}
		if so.Offer.BuyerPub != "" && so.Offer.BuyerPub != b.PubHex() {
			return
		}
		if b.engine.FastJournal() == nil {
			summary := strings.TrimSpace(so.Offer.Description)
			if summary == "" {
				summary = so.Offer.Shard
			}
			b.RecordMarketTape("market_offer", MarketTapeEvent{
				ID:      so.Offer.OfferID,
				Kind:    "offer",
				Side:    "sell",
				Domains: append([]string(nil), so.Offer.Domains...),
				Summary: summary,
				Sats:    so.Offer.PriceSats,
				Pub:     shortPub(so.Offer.Pub),
				Shard:   so.Offer.Shard,
				Status:  "quoted",
				At:      nowMs,
			})
		}
		b.mu.RLock()
		ch := b.offerInbox[so.Offer.WantID]
		b.mu.RUnlock()
		if ch != nil {
			select {
			case ch <- so.Offer:
			default:
			}
		}
		b.dispatchOfferToStandingOrders(so.Offer)

	case bstack.KindAnnounce:
		sa, err := bstack.DecodeAnnouncePayload(payload.Body)
		if err != nil {
			return
		}
		if sa.Announce.Net != myNet || sa.Announce.Exp <= nowMs || sa.Announce.Exp > maxExpMs {
			return
		}
		if sa.Announce.Pub == "" || sa.Announce.Pub == b.PubHex() {
			return
		}
		if err := bstack.VerifyArtifactRaw(sa.Announce.Pub, sa.Sig, sa.Announce.DomainTag(), sa.RawCBOR); err != nil {
			return
		}
		b.dispatchAnnounceToStandingOrders(sa.Announce)

	case bstack.KindAttestation:
		sat, err := bstack.DecodeAttestationPayload(payload.Body)
		if err != nil {
			return
		}
		ad := sat.Attestation
		if ad.Net != myNet || ad.Exp <= nowMs || ad.Exp > maxExpMs {
			return
		}
		if err := bstack.VerifyArtifactRaw(ad.Pub, sat.Sig, ad.DomainTag(), sat.RawCBOR); err != nil {
			return
		}
		att, decErr := reputation.DecodeAttestationToken(ad.Token)
		if decErr != nil {
			return
		}
		if !strings.EqualFold(att.IssuerPub, ad.Pub) || !strings.EqualFold(att.SubjectPub, ad.SubjectPub) {
			return
		}
		if b.mem != nil && b.mem.DB() != nil {
			if ont, rErr := SaveVerifiedAttestation(b.mem.DB(), att, nil); rErr == nil {
				b.scheduleReputationRecompute(ont)
				b.notify("reputation_attestation", map[string]any{
					"attestHash":  att.AttestHash,
					"role":        att.Role,
					"issuerPub":   shortPub(att.IssuerPub),
					"subjectPub":  shortPub(att.SubjectPub),
					"ontologyId":  att.OntologyID,
					"compositeBp": att.Scores.CompositeBP,
				})
			}
		}
	}
}

// dispatchAnnounceToStandingOrders triggers a rate-limited WantAd broadcast (max 1x per 5m per order+seller)
// whenever a newly announced seller advertises SellDomains matching an active watching StandingOrder.
func (b *Broker) dispatchAnnounceToStandingOrders(ann bstack.AnnounceAd) {
	if b.mem == nil || b.mem.DB() == nil || len(ann.SellDomains) == 0 {
		return
	}
	entries := LoadStandingOrderIndex(b.mem.DB())
	if len(entries) == 0 {
		return
	}
	now := time.Now().Unix()
	var matched []StandingOrderIndexEntry
	b.mu.Lock()
	if b.announceWantSeen == nil {
		b.announceWantSeen = make(map[string]int64)
	}
	if len(b.announceWantSeen) >= 2048 {
		for k, ts := range b.announceWantSeen {
			if now-ts > 600 {
				delete(b.announceWantSeen, k)
			}
		}
		for len(b.announceWantSeen) >= 2048 {
			var oldestKey string
			var oldestTS int64
			for k, ts := range b.announceWantSeen {
				if oldestKey == "" || ts < oldestTS {
					oldestKey = k
					oldestTS = ts
				}
			}
			if oldestKey == "" {
				break
			}
			delete(b.announceWantSeen, oldestKey)
		}
	}
	for _, entry := range entries {
		if entry.Status != "watching" || entry.OrderID == "" {
			continue
		}
		if entry.ExpiresAtUnix > 0 && entry.ExpiresAtUnix <= now {
			continue
		}
		if !standingOrderDomainMatches(entry.Domains, ann.SellDomains) {
			continue
		}
		rateKey := entry.OrderID + ":" + ann.Pub
		if last, ok := b.announceWantSeen[rateKey]; ok && now-last < 300 {
			continue
		}
		b.announceWantSeen[rateKey] = now
		matched = append(matched, entry)
	}
	b.mu.Unlock()
	if len(matched) == 0 {
		return
	}
	b.spawnTracked(15*time.Second, func(ctx context.Context) {
		for _, entry := range matched {
			_ = b.PublishWantForStandingOrderNMU(ctx, StandingOrderState{
				OrderID:           entry.OrderID,
				Domains:           entry.Domains,
				Query:             entry.Query,
				OntologyID:        entry.OntologyID,
				MaxSats:           entry.MaxSats,
				MinGrade:          entry.MinGrade,
				MaxSatsPer1kUnits: entry.MaxSatsPer1kUnits,
				FreshnessClass:    entry.FreshnessClass,
				RequiredRights:    entry.RequiredRights,
				PricingMode:       entry.PricingMode,
			})
		}
	})
}

// dispatchOfferToStandingOrders wakes any active durable Restate StandingOrder workflows whose
// domain & maxSats match an incoming verified OfferAd from the Hub SSE stream.
func (b *Broker) dispatchOfferToStandingOrders(off bstack.OfferAd) {
	rc := b.RestateClient()
	if rc == nil || b.mem == nil || b.mem.DB() == nil {
		return
	}
	entries := LoadStandingOrderIndex(b.mem.DB())
	if len(entries) == 0 {
		return
	}
	now := time.Now().Unix()
	var targets []string
	for _, entry := range entries {
		if entry.Status != "watching" || entry.OrderID == "" {
			continue
		}
		if entry.ExpiresAtUnix > 0 && entry.ExpiresAtUnix <= now {
			continue
		}
		if entry.MaxSats > 0 && off.PriceSats > entry.MaxSats {
			continue
		}
		if !standingOrderDomainMatches(entry.Domains, off.Domains) {
			continue
		}
		if !standingOrderOntologyMatches(context.Background(), b.mem, entry.OntologyID, entry.AllowBridge, off.OntologyID) {
			continue
		}
		if !standingOrderNMUMatches(entry.MinGrade, entry.MaxSatsPer1kUnits, entry.FreshnessClass, entry.RequiredRights, entry.PricingMode, off, now) {
			continue
		}
		targets = append(targets, entry.OrderID)
	}
	if len(targets) == 0 {
		return
	}
	b.spawnTracked(10*time.Second, func(ctx context.Context) {
		for _, orderID := range targets {
			_, _ = rc.SignalStandingOrderOfferAsync(ctx, orderID, off)
		}
	})
}
