package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/kryptokrates/nektar/internal/bstack"
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
	var tx *transaction.Transaction
	_, parsedTx, _, pErr := guard.ParseBEEF(ev.BEEF, guard.DefaultBound)
	if pErr == nil && parsedTx != nil {
		tx = parsedTx
	} else if !isLive && pErr != nil && strings.Contains(pErr.Error(), "no inputs") && len(ev.BEEF) <= 64<<10 {
		if t0, tErr := transaction.NewTransactionFromBEEF(ev.BEEF); tErr == nil && t0 != nil && len(t0.Inputs) == 0 {
			tx = t0
		}
	}
	if tx == nil || (ev.TxID != "" && !strings.EqualFold(tx.TxID().String(), ev.TxID)) || int(ev.Vout) >= len(tx.Outputs) {
		return bstack.MarketPayload{}, false, false
	}
	if isLive {
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
		var sw bstack.SignedWant
		if err := json.Unmarshal(payload.Body, &sw); err != nil {
			return
		}
		if sw.Want.Net != myNet || sw.Want.Exp <= nowMs || sw.Want.Exp > maxExpMs {
			return
		}
		if err := bstack.VerifyArtifact(sw.Want.Pub, sw.Sig, sw.Want); err != nil {
			return
		}
		if sw.Want.Pub == b.PubHex() {
			return // ignore our own want
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
		var so bstack.SignedOffer
		if err := json.Unmarshal(payload.Body, &so); err != nil {
			return
		}
		if so.Offer.Net != myNet || so.Offer.Exp <= nowMs || so.Offer.Exp > maxExpMs {
			return
		}
		if leafRaw, lErr := hex.DecodeString(strings.TrimSpace(so.Offer.Leaf)); lErr != nil || len(leafRaw) != 32 {
			return
		}
		if err := bstack.VerifyArtifact(so.Offer.Pub, so.Sig, so.Offer); err != nil {
			return
		}
		if so.Offer.BuyerPub != "" && so.Offer.BuyerPub != b.PubHex() {
			return
		}
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
		var sa bstack.SignedAnnounce
		if err := json.Unmarshal(payload.Body, &sa); err != nil {
			return
		}
		if sa.Announce.Net != myNet || sa.Announce.Exp <= nowMs || sa.Announce.Exp > maxExpMs {
			return
		}
		if sa.Announce.Pub == "" || sa.Announce.Pub == b.PubHex() {
			return
		}
		if err := bstack.VerifyArtifact(sa.Announce.Pub, sa.Sig, sa.Announce); err != nil {
			return
		}
		b.dispatchAnnounceToStandingOrders(sa.Announce)
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
			_ = b.PublishWantForStandingOrder(ctx, entry.OrderID, entry.Domains, entry.Query, entry.MaxSats)
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
