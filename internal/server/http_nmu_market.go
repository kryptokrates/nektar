package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

type shardFeedFrame struct {
	Event string
	Data  []byte
}

// BroadcastShardFeedEvent pushes a real-time SSE event (`fragment_superseded` or `fragment_distilled`)
// to all active authenticated subscribers of shard without blocking writes.
func (a *App) BroadcastShardFeedEvent(shard, eventType string, payload any) {
	if a == nil || strings.TrimSpace(shard) == "" {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	frame := shardFeedFrame{Event: eventType, Data: raw}
	a.feedMu.Lock()
	subs := a.feedSubs[shard]
	for ch := range subs {
		select {
		case ch <- frame:
		default:
		}
	}
	a.feedMu.Unlock()
}

func (a *App) subscribeShardFeed(shard string) (chan shardFeedFrame, func()) {
	ch := make(chan shardFeedFrame, 32)
	a.feedMu.Lock()
	if a.feedSubs == nil {
		a.feedSubs = make(map[string]map[chan shardFeedFrame]struct{})
	}
	if a.feedSubs[shard] == nil {
		a.feedSubs[shard] = make(map[chan shardFeedFrame]struct{})
	}
	a.feedSubs[shard][ch] = struct{}{}
	a.feedMu.Unlock()
	unsub := func() {
		a.feedMu.Lock()
		if m := a.feedSubs[shard]; m != nil {
			delete(m, ch)
			if len(m) == 0 {
				delete(a.feedSubs, shard)
			}
		}
		a.feedMu.Unlock()
	}
	return ch, unsub
}

// MarketQueryItem is one verified FactPack item returned by POST /v1/market/query (per_query mode).
type MarketQueryItem struct {
	Shard       string                 `json:"shard"`
	Seq         int64                  `json:"seq"`
	Text        string                 `json:"text"`
	HDP         string                 `json:"hdp"`
	Leaf        string                 `json:"leaf"`
	OntologyID  string                 `json:"ontologyId,omitempty"`
	EdgesRoot   string                 `json:"edgesRoot,omitempty"`
	AnchorTxID  string                 `json:"anchorTxid,omitempty"`
	NMU         *nmu.Spec              `json:"nmu,omitempty"`
	ProofBundle *contracts.ProofBundle `json:"proofBundle,omitempty"`
	CreatedAt   int64                  `json:"createdAt"`
}

// handleMarketQuery serves standardized NMU-1 `per_query` metered recall over BRC-105 HTTP 402.
func (a *App) handleMarketQuery(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Shard      string `json:"shard"`
		Query      string `json:"query,omitempty"`
		OntologyID string `json:"ontologyId,omitempty"`
		MinGrade   string `json:"minGrade,omitempty"`
		BuyerPub   string `json:"buyerPub,omitempty"`
		Limit      int    `json:"limit,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	shard := strings.TrimSpace(in.Shard)
	if shard == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "shard is required"})
		return
	}
	if err := nmu.ValidateWantFilters(in.MinGrade, "", "", ""); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	listing, err := a.mem.DB().GetSellListing(shard)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if listing == nil || !listing.Active || !listing.SupportsMode(nmu.ModePerQuery) || listing.QueryPriceSats == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("shard %q does not have per_query metered recall enabled", shard)})
		return
	}

	txid, paid, senderPub, ok := a.verifyBRC105Payment(w, r, "market_query:"+shard, listing.QueryPriceSats, map[string]any{
		"shard":       shard,
		"pricingMode": string(nmu.ModePerQuery),
		"usageRights": string(nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights))),
	})
	if !ok {
		return
	}

	limit := in.Limit
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	now := time.Now().Unix()
	recalled, rErr := a.mem.Recall(r.Context(), memory.RecallInput{
		Shard:        shard,
		Query:        strings.TrimSpace(in.Query),
		Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
		Action:       "disclose",
		DisallowBorg: true,
		Limit:        limit * 3,
	})
	if rErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": rErr.Error()})
		return
	}

	hasActiveBond := false
	if bRec, _, bErr := market.LoadActiveBond(a.mem.DB()); bErr == nil && bRec != nil && !bRec.State.Seized {
		hasActiveBond = true
	}
	feedURL := ""
	if base := strings.TrimRight(strings.TrimSpace(a.cfg.PublicURL), "/"); base != "" && listing.SupportsMode(nmu.ModeSubscription) {
		feedURL = base + "/v1/market/feeds/" + shard
	}
	wantMinGrade := nmu.NormalizeGrade(in.MinGrade)
	wantOnt := strings.ToLower(strings.TrimSpace(in.OntologyID))

	items := make([]MarketQueryItem, 0, min(limit, len(recalled)))
	var totalUnits uint32
	var bestGrade nmu.Grade
	for _, f := range recalled {
		if len(items) >= limit {
			break
		}
		if dec, aErr := a.mem.AuthorizeSell(
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
		if sr := distill.Scrub(f.Text); sr.Verdict == distill.ScrubBlocked {
			continue
		}
		if f.Tainted && (f.GradeMismatch || f.NMU == nil || nmu.EffectiveRights(f.NMU.UsageRights) != nmu.RightsDistillResellable) {
			continue
		}
		if f.NMU != nil && nmu.IsExpired(f.NMU.FreshnessClass, f.NMU.ValidUntilUnix, now) {
			continue
		}
		cls, fOnt, fEdges := a.classifyLocalFragment(r.Context(), f, hasActiveBond, feedURL)
		if wantOnt != "" && !strings.EqualFold(fOnt, wantOnt) {
			continue
		}
		if wantMinGrade != "" && !nmu.MeetsMinGrade(cls.Grade, wantMinGrade) {
			continue
		}
		if nmu.GradeRank(cls.Grade) > nmu.GradeRank(bestGrade) {
			bestGrade = cls.Grade
		}
		totalUnits += cls.UnitCount
		effRights := nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights))
		effFresh := nmu.NormalizeFreshness(listing.FreshnessClass)
		var validUntil int64
		var subjVer string
		if f.NMU != nil {
			if f.NMU.UsageRights != "" {
				effRights = nmu.EffectiveRights(f.NMU.UsageRights)
			}
			if f.NMU.FreshnessClass != "" {
				effFresh = nmu.NormalizeFreshness(string(f.NMU.FreshnessClass))
			}
			validUntil = f.NMU.ValidUntilUnix
			subjVer = f.NMU.SubjectVersion
		}
		pb, _, _ := a.mem.Prove(r.Context(), f.Shard, f.Seq)
		items = append(items, MarketQueryItem{
			Shard:      f.Shard,
			Seq:        f.Seq,
			Text:       f.Text,
			HDP:        f.HDP,
			Leaf:       f.Leaf,
			OntologyID: fOnt,
			EdgesRoot:  fEdges,
			AnchorTxID: f.AnchorTxID,
			NMU: &nmu.Spec{
				Version:        nmu.SpecVersion,
				Grade:          cls.Grade,
				UnitCount:      cls.UnitCount,
				SatsPer1kUnits: nmu.ComputeSatsPer1kUnits(paid, maxU32(1, cls.UnitCount)),
				FreshnessClass: effFresh,
				ValidUntilUnix: validUntil,
				SubjectVersion: subjVer,
				UsageRights:    effRights,
				PricingMode:    nmu.ModePerQuery,
				FeedURL:        feedURL,
				AdvisoryNote:   nmu.AdvisoryNMUNote,
			},
			ProofBundle: pb,
			CreatedAt:   f.CreatedAt,
		})
	}

	buyerPub := strings.ToLower(strings.TrimSpace(in.BuyerPub))
	if buyerPub == "" {
		buyerPub = senderPub
	}
	var dr *market.DealReceipt
	var drTok string
	if a.mkt != nil {
		receipt := market.DealReceipt{
			Version:       1,
			DealID:        "qry-" + shortRandHex(6),
			OfferID:       "per-query:" + shard,
			BuyerPub:      buyerPub,
			SellerPub:     a.mkt.SalePubHex(),
			Domains:       listing.Domains,
			PriceSats:     paid,
			PaymentTxID:   txid,
			KeyCommitment: "per_query:" + txid,
			NMU: &nmu.Spec{
				Version:        nmu.SpecVersion,
				Grade:          bestGrade,
				UnitCount:      totalUnits,
				SatsPer1kUnits: nmu.ComputeSatsPer1kUnits(paid, maxU32(1, totalUnits)),
				UsageRights:    nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights)),
				PricingMode:    nmu.ModePerQuery,
				FeedURL:        feedURL,
				AdvisoryNote:   nmu.AdvisoryNMUNote,
			},
			SettledAt: now,
		}
		if sErr := a.mkt.SignSellerDealReceipt(r.Context(), &receipt); sErr == nil {
			dr = &receipt
			drTok, _ = market.EncodeDealReceiptToken(receipt)
		}
	}
	_ = a.mem.DB().RecordEconomicsEvent(store.EconomicsEventRow{
		Kind:       "deal_sell",
		SatsDelta:  int64(paid),
		OntologyID: reputation.NormalizeOntologyID(listing.OntologyID),
		Shard:      shard,
		DealID:     "qry:" + txid,
		CreatedAt:  now,
	})
	a.BroadcastDashboardEvent("deal_settled")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"shard":            shard,
		"pricingMode":      string(nmu.ModePerQuery),
		"paidSats":         paid,
		"paymentTxid":      txid,
		"count":            len(items),
		"totalUnits":       totalUnits,
		"items":            items,
		"dealReceipt":      dr,
		"dealReceiptToken": drTok,
		"advisoryNote":     nmu.AdvisoryNMUNote,
	})
}

// handleMarketSubscribe activates a time-windowed NMU-1 `subscription` Shard Feed (and Grade S update SLA) over BRC-105 HTTP 402.
func (a *App) handleMarketSubscribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Shard       string `json:"shard"`
		BuyerPub    string `json:"buyerPub"`
		DurationSec int64  `json:"durationSec,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	shard := strings.TrimSpace(in.Shard)
	if shard == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "shard is required"})
		return
	}
	buyerPub := strings.ToLower(strings.TrimSpace(in.BuyerPub))
	if _, _, err := core.ParseStrictCompressedPubKey(buyerPub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid 33-byte compressed hex buyerPub is required"})
		return
	}
	listing, err := a.mem.DB().GetSellListing(shard)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if listing == nil || !listing.Active || !listing.SupportsMode(nmu.ModeSubscription) || listing.SubscriptionPriceSats == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("shard %q does not have subscription feeds enabled", shard)})
		return
	}

	durSec := listing.EffectiveSubscriptionDurationSec()
	txid, paid, _, ok := a.verifyBRC105Payment(w, r, "market_subscription:"+shard, listing.SubscriptionPriceSats, map[string]any{
		"shard":           shard,
		"pricingMode":     string(nmu.ModeSubscription),
		"subscriptionSec": durSec,
		"usageRights":     string(nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights))),
	})
	if !ok {
		return
	}

	now := time.Now().Unix()
	subID := "sub-" + shortRandHex(8)
	sellerPub := a.mem.Chain().IdentityPubHex()
	if a.mkt != nil && a.mkt.SalePubHex() != "" {
		sellerPub = a.mkt.SalePubHex()
	}
	sub := store.ShardSubscription{
		SubID:       subID,
		Shard:       shard,
		BuyerPub:    buyerPub,
		SellerPub:   sellerPub,
		PricingMode: string(nmu.ModeSubscription),
		PriceSats:   paid,
		PaymentTxID: txid,
		UsageRights: string(nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights))),
		StartedAt:   now,
		ExpiresAt:   now + durSec,
		LastSeq:     0,
		Active:      true,
	}
	if err := a.mem.DB().CreateShardSubscription(sub); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "create subscription: " + err.Error()})
		return
	}

	feedURL := strings.TrimRight(requestBaseURL(r), "/") + "/v1/market/feeds/" + shard
	var dr *market.DealReceipt
	var drTok string
	if a.mkt != nil {
		receipt := market.DealReceipt{
			Version:       1,
			DealID:        subID,
			OfferID:       "subscription:" + shard,
			BuyerPub:      buyerPub,
			SellerPub:     sellerPub,
			Domains:       listing.Domains,
			PriceSats:     paid,
			PaymentTxID:   txid,
			KeyCommitment: "subscription:" + subID,
			NMU: &nmu.Spec{
				Version:         nmu.SpecVersion,
				Grade:           nmu.GradeS,
				FreshnessClass:  nmu.NormalizeFreshness(listing.FreshnessClass),
				UsageRights:     nmu.EffectiveRights(nmu.UsageRights(listing.UsageRights)),
				PricingMode:     nmu.ModeSubscription,
				SubscriptionSec: durSec,
				FeedURL:         feedURL,
				AdvisoryNote:    nmu.AdvisoryNMUNote,
			},
			SettledAt: now,
		}
		if sErr := a.mkt.SignSellerDealReceipt(r.Context(), &receipt); sErr == nil {
			dr = &receipt
			drTok, _ = market.EncodeDealReceiptToken(receipt)
		}
	}
	_ = a.mem.DB().RecordEconomicsEvent(store.EconomicsEventRow{
		Kind:       "deal_sell",
		SatsDelta:  int64(paid),
		OntologyID: reputation.NormalizeOntologyID(listing.OntologyID),
		Shard:      shard,
		DealID:     subID,
		CreatedAt:  now,
	})
	a.BroadcastDashboardEvent("subscription_created")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"subId":            subID,
		"shard":            shard,
		"feedUrl":          feedURL,
		"startedAt":        sub.StartedAt,
		"expiresAt":        sub.ExpiresAt,
		"usageRights":      sub.UsageRights,
		"paidSats":         paid,
		"paymentTxid":      txid,
		"dealReceipt":      dr,
		"dealReceiptToken": drTok,
		"advisoryNote":     nmu.AdvisoryNMUNote,
	})
}

// handleMarketFeedSSE serves authenticated live SSE Shard Feeds (`GET /v1/market/feeds/{shard}`),
// enforcing +/-60s replay-protected signatures over `GET|/v1/market/feeds/{shard}|{subId}|{ts}` and
// streaming both `fragment_distilled` updates and Grade S `fragment_superseded` invalidations.
func (a *App) handleMarketFeedSSE(w http.ResponseWriter, r *http.Request) {
	shard := strings.TrimSpace(r.PathValue("shard"))
	subID := strings.TrimSpace(r.Header.Get(nmu.HeaderSubID))
	pubHex := strings.ToLower(strings.TrimSpace(r.Header.Get(nmu.HeaderPub)))
	sigHex := strings.TrimSpace(r.Header.Get(nmu.HeaderSig))
	tsStr := strings.TrimSpace(r.Header.Get(nmu.HeaderTS))
	tsUnix, tsErr := strconv.ParseInt(tsStr, 10, 64)
	now := time.Now().Unix()
	if shard == "" || subID == "" || pubHex == "" || sigHex == "" || tsErr != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "missing subscriber auth headers (X-Nektar-Sub-ID, X-Nektar-Pub, X-Nektar-Sig, X-Nektar-TS)",
		})
		return
	}
	if err := nmu.VerifyFeedAuth(pubHex, sigHex, shard, subID, tsUnix, now); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	sub, err := a.mem.DB().GetShardSubscription(subID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sub == nil || !sub.Active || sub.Shard != shard || !strings.EqualFold(sub.BuyerPub, pubHex) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "subscription not found or unauthorized"})
		return
	}
	if now >= sub.ExpiresAt {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "subscription expired"})
		return
	}
	listing, _ := a.mem.DB().GetSellListing(shard)
	allowRaw := listing != nil && listing.AllowRaw

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sinceSeq := sub.LastSeq
	if qSeq := strings.TrimSpace(r.URL.Query().Get("sinceSeq")); qSeq != "" {
		if parsed, pErr := strconv.ParseInt(qSeq, 10, 64); pErr == nil && parsed >= 0 {
			sinceSeq = parsed
		}
	}

	ch, unsub := a.subscribeShardFeed(shard)
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeSSE := func(event string, payload any) {
		raw, mErr := json.Marshal(payload)
		if mErr != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		flusher.Flush()
	}

	writeSSE("connected", map[string]any{
		"subId":       sub.SubID,
		"shard":       shard,
		"expiresAt":   sub.ExpiresAt,
		"usageRights": sub.UsageRights,
	})

	// Helper to replay active distilled/sellable fragments with seq > sinceSeq.
	replayNew := func() {
		frags, rErr := a.mem.Recall(r.Context(), memory.RecallInput{
			Shard:        shard,
			Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
			Action:       "disclose",
			DisallowBorg: true,
		})
		if rErr != nil {
			return
		}
		for _, f := range frags {
			if f.Seq <= sinceSeq {
				continue
			}
			if dec, aErr := a.mem.AuthorizeSell(
				policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
				policy.Resource{
					Type:      "Fragment",
					ID:        fmt.Sprintf("%s#%d", f.Shard, f.Seq),
					Level:     int64(f.Level),
					Shard:     f.Shard,
					Class:     f.Class,
					Distilled: f.Distilled,
				},
				allowRaw,
			); aErr != nil || !dec.Allow {
				continue
			}
			if sr := distill.Scrub(f.Text); sr.Verdict == distill.ScrubBlocked {
				continue
			}
			ontID, edgesRoot, _ := a.mem.DB().GetFragmentGraphMeta(f.Shard, f.Seq)
			if ontID == "" {
				ontID = f.OntologyID
			}
			writeSSE("fragment_distilled", map[string]any{
				"shard":       f.Shard,
				"seq":         f.Seq,
				"text":        f.Text,
				"hdp":         f.HDP,
				"leaf":        f.Leaf,
				"ontologyId":  ontID,
				"edgesRoot":   edgesRoot,
				"nmu":         f.NMU,
				"usageRights": sub.UsageRights,
				"createdAt":   f.CreatedAt,
			})
			sinceSeq = f.Seq
			_ = a.mem.DB().UpdateSubscriptionLastSeq(sub.SubID, sinceSeq)
		}
	}
	replayNew()

	remaining := time.Until(time.Unix(sub.ExpiresAt, 0))
	if remaining <= 0 {
		writeSSE("subscription_expired", map[string]any{"subId": sub.SubID, "shard": shard})
		return
	}
	expiryTimer := time.NewTimer(remaining)
	defer expiryTimer.Stop()
	pollTicker := time.NewTicker(2 * time.Second)
	defer pollTicker.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stopCh:
			return
		case <-expiryTimer.C:
			writeSSE("subscription_expired", map[string]any{"subId": sub.SubID, "shard": shard})
			return
		case frame := <-ch:
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.Event, frame.Data)
			flusher.Flush()
		case <-pollTicker.C:
			replayNew()
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// TickerFilter filters GET /v1/market/ticker commodity price board queries.
type TickerFilter struct {
	OntologyID  string `json:"ontologyId,omitempty"`
	Grade       string `json:"grade,omitempty"`
	PricingMode string `json:"pricingMode,omitempty"`
	Domain      string `json:"domain,omitempty"`
}

// CommodityTickerRow represents one standardized (ontologyId, grade, pricingMode) commodity market row.
type CommodityTickerRow struct {
	OntologyID          string   `json:"ontologyId"`
	Grade               string   `json:"grade"`
	PricingMode         string   `json:"pricingMode"`
	MinSatsPer1kUnits   uint64   `json:"minSatsPer1kUnits"`
	MedianSatsPer1k     uint64   `json:"medianSatsPer1kUnits"`
	MinSatsPerTriple    string   `json:"minSatsPerTriple"`
	MedianSatsPerTriple string   `json:"medianSatsPerTriple"`
	MinAskSats          uint64   `json:"minAskSats"`
	ActiveOffers        int      `json:"activeOffers"`
	BondedSellers       int      `json:"bondedSellers"`
	LiveFeeds           int      `json:"liveFeeds"`
	TotalUnitsAvailable uint64   `json:"totalUnitsAvailable"`
	Domains             []string `json:"domains,omitempty"`
	SampleShards        []string `json:"sampleShards,omitempty"`
}

// CommodityTickerResponse is the Ciyuan-style standardized commodity price board response.
type CommodityTickerResponse struct {
	Rows         []CommodityTickerRow `json:"rows"`
	UpdatedAt    int64                `json:"updatedAt"`
	AdvisoryNote string               `json:"advisoryNote"`
}

func (a *App) handleMarketTicker(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	resp := a.BuildCommodityTicker(r.Context(), TickerFilter{
		OntologyID:  strings.TrimSpace(q.Get("ontologyId")),
		Grade:       strings.TrimSpace(q.Get("grade")),
		PricingMode: strings.TrimSpace(q.Get("pricingMode")),
		Domain:      strings.TrimSpace(q.Get("domain")),
	})
	writeJSON(w, http.StatusOK, resp)
}

// BuildCommodityTicker aggregates live standardized NMU-1 quotes across local listings, discovered peers,
// and embedded Hub ls_nektar lookup records grouped by (ontologyId, grade, pricingMode). Zero floats.
func (a *App) BuildCommodityTicker(ctx context.Context, filter TickerFilter) CommodityTickerResponse {
	return BuildCommodityTickerFrom(ctx, a.mem, a.disc, a.cfg.Hub, a.cfg.PublicURL, filter)
}

// BuildCommodityTickerFrom builds a standardized NMU-1 Commodity Ticker snapshot from any runtime bundle
// (shared by HTTP GET /v1/market/ticker, Dashboard /state, MCP marketContract, and offline `nektar ticker`).
func BuildCommodityTickerFrom(
	ctx context.Context,
	mem *memory.Service,
	disc *discovery.Service,
	hubNode *hub.Node,
	publicURL string,
	filter TickerFilter,
) CommodityTickerResponse {
	type tickerSample struct {
		ontologyID  string
		grade       string
		pricingMode string
		ratePer1k   uint64
		askSats     uint64
		units       uint64
		sellerPub   string
		bonded      bool
		hasLiveFeed bool
		domains     []string
		shard       string
	}
	var samples []tickerSample

	selfPub := ""
	if mem != nil && mem.Chain() != nil {
		selfPub = strings.ToLower(strings.TrimSpace(mem.Chain().IdentityPubHex()))
	}
	selfBonded := false
	if mem != nil && mem.DB() != nil {
		if bRec, _, bErr := market.LoadActiveBond(mem.DB()); bErr == nil && bRec != nil && !bRec.State.Seized {
			selfBonded = true
		}
		if listings, err := mem.DB().ListSellListings(); err == nil {
			baseURL := strings.TrimRight(strings.TrimSpace(publicURL), "/")
			for _, l := range listings {
				if !l.Active {
					continue
				}
				feedEP := ""
				if baseURL != "" && l.SupportsMode(nmu.ModeSubscription) {
					feedEP = baseURL + "/v1/market/feeds/" + l.Shard
				}
				bestGrade, totalUnits, avgUnits, fragOnt := summarizeLocalListingFrom(ctx, mem, l, selfBonded, feedEP)
				ontID := strings.ToLower(strings.TrimSpace(l.OntologyID))
				if ontID == "" {
					ontID = fragOnt
				}
				if ontID == "" {
					ontID = "unpinned"
				}
				gradeStr := string(bestGrade)
				if gradeStr == "" {
					gradeStr = string(nmu.GradeC)
				}
				for _, mode := range l.EnabledModes() {
					var ask, rate uint64
					switch mode {
					case nmu.ModeSpot:
						dp := market.ParseDynamicPriceState(l.DynamicPricingJSON, l.PriceSats)
						dp.PerUnitMode = l.PricePerUnit || dp.PerUnitMode
						ask, rate = dp.QuoteForFactPack(l.PriceSats, avgUnits, false, 0)
					case nmu.ModePerQuery:
						ask = l.QueryPriceSats
						rate = nmu.ComputeSatsPer1kUnits(ask, avgUnits)
					case nmu.ModeSubscription:
						ask = l.SubscriptionPriceSats
						rate = nmu.ComputeSatsPer1kUnits(ask, maxU32(1, uint32(minU64(totalUnits, uint64(nmu.MaxUnitCount)))))
					}
					samples = append(samples, tickerSample{
						ontologyID:  ontID,
						grade:       gradeStr,
						pricingMode: string(mode),
						ratePer1k:   rate,
						askSats:     ask,
						units:       maxU64(1, totalUnits),
						sellerPub:   selfPub,
						bonded:      selfBonded,
						hasLiveFeed: feedEP != "" || l.SupportsMode(nmu.ModeSubscription),
						domains:     l.Domains,
						shard:       l.Shard,
					})
				}
			}
		}
	}

	// Discovered peers' PackRefs.
	if disc != nil {
		for _, peer := range disc.Peers() {
			if strings.EqualFold(peer.Pub, selfPub) {
				continue
			}
			for _, pk := range peer.Packs {
				ont := strings.ToLower(strings.TrimSpace(pk.OntologyID))
				if ont == "" {
					ont = "unpinned"
				}
				g := string(nmu.NormalizeGrade(pk.Grade))
				if g == "" {
					g = string(nmu.GradeC)
				}
				modes := pk.EnabledModes
				if len(modes) == 0 {
					if pk.PricingMode != "" {
						modes = []string{pk.PricingMode}
					} else {
						modes = []string{string(nmu.ModeSpot)}
					}
				}
				units := uint64(pk.UnitCount)
				if units == 0 {
					units = uint64(maxInt(1, pk.Count))
				}
				for _, m := range modes {
					pm := nmu.EffectivePricingMode(nmu.PricingMode(m))
					var ask, rate uint64
					switch pm {
					case nmu.ModeSpot:
						ask = pk.PriceSats
						rate = pk.SatsPer1kUnits
						if rate == 0 && ask > 0 {
							rate = nmu.ComputeSatsPer1kUnits(ask, pk.UnitCount)
						}
					case nmu.ModePerQuery:
						ask = pk.QueryPriceSats
						rate = nmu.ComputeSatsPer1kUnits(ask, pk.UnitCount)
					case nmu.ModeSubscription:
						ask = pk.SubscriptionPriceSats
						rate = nmu.ComputeSatsPer1kUnits(ask, maxU32(1, uint32(minU64(units, uint64(nmu.MaxUnitCount)))))
					}
					if ask == 0 && rate == 0 {
						continue
					}
					samples = append(samples, tickerSample{
						ontologyID:  ont,
						grade:       g,
						pricingMode: string(pm),
						ratePer1k:   rate,
						askSats:     ask,
						units:       units,
						sellerPub:   strings.ToLower(strings.TrimSpace(peer.Pub)),
						bonded:      g == string(nmu.GradeA) || g == string(nmu.GradeS),
						hasLiveFeed: pk.FeedEndpoint != "",
						domains:     pk.Domains,
						shard:       pk.Shard,
					})
				}
			}
		}
	}

	// Embedded Hub ls_nektar live KindOffer records.
	if hubNode != nil {
		if ls := hubNode.MarketLookup(); ls != nil {
			qBytes, _ := json.Marshal(bstack.MarketQuery{Kind: bstack.KindOffer, Limit: 250})
			if ans, lErr := ls.Lookup(ctx, &lookup.LookupQuestion{Service: bstack.LookupService, Query: qBytes}); lErr == nil && ans != nil {
				if evs, ok := ans.Result.([]bstack.MarketEvent); ok {
					for _, ev := range evs {
						var so bstack.SignedOffer
						if json.Unmarshal(ev.Payload.Body, &so) != nil {
							continue
						}
						off := so.Offer
						ont := strings.ToLower(strings.TrimSpace(off.OntologyID))
						if ont == "" {
							ont = "unpinned"
						}
						g := string(nmu.NormalizeGrade(off.Grade))
						if g == "" {
							g = string(nmu.GradeC)
						}
						pm := string(nmu.EffectivePricingMode(nmu.PricingMode(off.PricingMode)))
						rate := off.SatsPer1kUnits
						if rate == 0 && off.PriceSats > 0 {
							rate = nmu.ComputeSatsPer1kUnits(off.PriceSats, off.UnitCount)
						}
						u := uint64(off.UnitCount)
						if u == 0 {
							u = 1
						}
						samples = append(samples, tickerSample{
							ontologyID:  ont,
							grade:       g,
							pricingMode: pm,
							ratePer1k:   rate,
							askSats:     off.PriceSats,
							units:       u,
							sellerPub:   strings.ToLower(strings.TrimSpace(off.Pub)),
							bonded:      off.BondOutpoint != "",
							hasLiveFeed: off.FeedURL != "",
							domains:     off.Domains,
							shard:       off.Shard,
						})
					}
				}
			}
		}
	}

	wantOnt := strings.ToLower(strings.TrimSpace(filter.OntologyID))
	wantGrade := string(nmu.NormalizeGrade(filter.Grade))
	wantMode := string(nmu.NormalizePricingMode(filter.PricingMode))
	wantDom := strings.ToLower(strings.TrimSpace(filter.Domain))

	type bucket struct {
		ontologyID string
		grade      string
		mode       string
		rates      []uint64
		minAsk     uint64
		offers     int
		bondedSet  map[string]bool
		liveFeeds  int
		units      uint64
		domSet     map[string]bool
		shardSet   map[string]bool
	}
	buckets := make(map[string]*bucket)

	for _, s := range samples {
		if wantOnt != "" && s.ontologyID != wantOnt {
			continue
		}
		if wantGrade != "" && s.grade != wantGrade {
			continue
		}
		if wantMode != "" && s.pricingMode != wantMode {
			continue
		}
		if wantDom != "" {
			domMatch := false
			for _, d := range s.domains {
				if strings.EqualFold(strings.TrimSpace(d), wantDom) {
					domMatch = true
					break
				}
			}
			if !domMatch {
				continue
			}
		}
		key := s.ontologyID + "|" + s.grade + "|" + s.pricingMode
		b := buckets[key]
		if b == nil {
			b = &bucket{
				ontologyID: s.ontologyID,
				grade:      s.grade,
				mode:       s.pricingMode,
				bondedSet:  make(map[string]bool),
				domSet:     make(map[string]bool),
				shardSet:   make(map[string]bool),
			}
			buckets[key] = b
		}
		b.offers++
		if s.ratePer1k > 0 {
			b.rates = append(b.rates, s.ratePer1k)
		}
		if s.askSats > 0 && (b.minAsk == 0 || s.askSats < b.minAsk) {
			b.minAsk = s.askSats
		}
		if s.bonded && s.sellerPub != "" {
			b.bondedSet[s.sellerPub] = true
		}
		if s.hasLiveFeed {
			b.liveFeeds++
		}
		b.units += s.units
		for _, d := range s.domains {
			if dl := strings.ToLower(strings.TrimSpace(d)); dl != "" {
				b.domSet[dl] = true
			}
		}
		if s.shard != "" {
			b.shardSet[s.shard] = true
		}
	}

	rows := make([]CommodityTickerRow, 0, len(buckets))
	for _, b := range buckets {
		sort.Slice(b.rates, func(i, j int) bool { return b.rates[i] < b.rates[j] })
		var minRate, medRate uint64
		if len(b.rates) > 0 {
			minRate = b.rates[0]
			mid := len(b.rates) / 2
			if len(b.rates)%2 == 1 {
				medRate = b.rates[mid]
			} else {
				medRate = (b.rates[mid-1] + b.rates[mid]) / 2
			}
		}
		doms := make([]string, 0, len(b.domSet))
		for d := range b.domSet {
			doms = append(doms, d)
		}
		sort.Strings(doms)
		shards := make([]string, 0, len(b.shardSet))
		for sh := range b.shardSet {
			shards = append(shards, sh)
		}
		sort.Strings(shards)
		if len(shards) > 5 {
			shards = shards[:5]
		}
		rows = append(rows, CommodityTickerRow{
			OntologyID:          b.ontologyID,
			Grade:               b.grade,
			PricingMode:         b.mode,
			MinSatsPer1kUnits:   minRate,
			MedianSatsPer1k:     medRate,
			MinSatsPerTriple:    nmu.FormatSatsPerTriple(minRate),
			MedianSatsPerTriple: nmu.FormatSatsPerTriple(medRate),
			MinAskSats:          b.minAsk,
			ActiveOffers:        b.offers,
			BondedSellers:       len(b.bondedSet),
			LiveFeeds:           b.liveFeeds,
			TotalUnitsAvailable: b.units,
			Domains:             doms,
			SampleShards:        shards,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].OntologyID != rows[j].OntologyID {
			return rows[i].OntologyID < rows[j].OntologyID
		}
		ri := nmu.GradeRank(nmu.Grade(rows[i].Grade))
		rj := nmu.GradeRank(nmu.Grade(rows[j].Grade))
		if ri != rj {
			return ri > rj
		}
		return rows[i].PricingMode < rows[j].PricingMode
	})

	return CommodityTickerResponse{
		Rows:         rows,
		UpdatedAt:    time.Now().Unix(),
		AdvisoryNote: nmu.AdvisoryNMUNote,
	}
}

func (a *App) classifyLocalFragment(ctx context.Context, f memory.RecalledFragment, hasActiveBond bool, feedURL string) (nmu.GradeClassification, string, string) {
	ontID, edgesRoot, _ := a.mem.DB().GetFragmentGraphMeta(f.Shard, f.Seq)
	if ontID == "" {
		ontID = strings.ToLower(strings.TrimSpace(f.OntologyID))
	}
	var triples []reasoning.Triple
	if rawRec, err := a.mem.DB().GetFragment(f.Shard, f.Seq); err == nil && len(rawRec.TriplesJSON) > 0 {
		_ = json.Unmarshal(rawRec.TriplesJSON, &triples)
	}
	if edges, err := a.mem.DB().ListKnowledgeEdges(store.KnowledgeEdgeFilter{Shard: f.Shard, Seq: f.Seq, Status: "accepted", Limit: 512}); err == nil {
		for _, ke := range edges {
			triples = append(triples, reasoning.Triple{Subject: ke.Subject, Predicate: ke.Predicate, Object: ke.Object, Hash: ke.TripleHash})
		}
	}
	var allowed map[string]struct{}
	if ontID != "" {
		allowed = a.mem.ResolveOntologyPredicateSet(ctx, ontID)
	}
	cls := nmu.ClassifyFragmentGrade(nmu.FragmentGradeInput{
		HasCleanText:           strings.TrimSpace(f.Text) != "",
		Distilled:              f.Distilled || len(edgesRoot) == 64,
		OntologyID:             ontID,
		Triples:                triples,
		AllowedPredicates:      allowed,
		MangleConsistent:       true,
		EdgesRoot:              edgesRoot,
		MPEHasOntologyAndEdges: len(ontID) == 64 && len(edgesRoot) == 64,
		ActiveBond:             hasActiveBond,
		LiveFeedURL:            feedURL,
	})
	return cls, ontID, edgesRoot
}

func (a *App) summarizeLocalListing(ctx context.Context, l store.SellListing, hasActiveBond bool, feedEP string) (bestGrade nmu.Grade, totalUnits uint64, avgUnits uint32, ontID string) {
	return summarizeLocalListingFrom(ctx, a.mem, l, hasActiveBond, feedEP)
}

func summarizeLocalListingFrom(ctx context.Context, mem *memory.Service, l store.SellListing, hasActiveBond bool, feedEP string) (bestGrade nmu.Grade, totalUnits uint64, avgUnits uint32, ontID string) {
	if mem == nil || mem.DB() == nil {
		return nmu.GradeC, 0, 1, ""
	}
	frags, err := mem.DB().ListFragments(l.Shard)
	if err != nil {
		return nmu.GradeC, 0, 1, ""
	}
	count := 0
	for _, f := range frags {
		if f.Forgotten {
			continue
		}
		count++
		fOnt, edgesRoot, _ := mem.DB().GetFragmentGraphMeta(f.Shard, f.Seq)
		if fOnt == "" {
			fOnt = strings.ToLower(strings.TrimSpace(f.OntologyID))
		}
		if ontID == "" && len(fOnt) == 64 {
			ontID = fOnt
		}
		var triples []reasoning.Triple
		if len(f.TriplesJSON) > 0 {
			_ = json.Unmarshal(f.TriplesJSON, &triples)
		}
		if edges, eErr := mem.DB().ListKnowledgeEdges(store.KnowledgeEdgeFilter{Shard: f.Shard, Seq: f.Seq, Status: "accepted", Limit: 512}); eErr == nil {
			for _, ke := range edges {
				triples = append(triples, reasoning.Triple{Subject: ke.Subject, Predicate: ke.Predicate, Object: ke.Object, Hash: ke.TripleHash})
			}
		}
		var allowed map[string]struct{}
		if fOnt != "" {
			allowed = mem.ResolveOntologyPredicateSet(ctx, fOnt)
		}
		cls := nmu.ClassifyFragmentGrade(nmu.FragmentGradeInput{
			HasCleanText:           true,
			Distilled:              f.Distilled || len(edgesRoot) == 64,
			OntologyID:             fOnt,
			Triples:                triples,
			AllowedPredicates:      allowed,
			MangleConsistent:       true,
			EdgesRoot:              edgesRoot,
			MPEHasOntologyAndEdges: len(fOnt) == 64 && len(edgesRoot) == 64,
			ActiveBond:             hasActiveBond,
			LiveFeedURL:            feedEP,
		})
		if nmu.GradeRank(cls.Grade) > nmu.GradeRank(bestGrade) {
			bestGrade = cls.Grade
		}
		u := cls.UnitCount
		if u == 0 {
			u = 1
		}
		totalUnits += uint64(u)
	}
	if count == 0 {
		return nmu.GradeC, 0, 1, ontID
	}
	avg := totalUnits / uint64(count)
	if avg == 0 {
		avg = 1
	}
	if avg > uint64(nmu.MaxUnitCount) {
		avg = uint64(nmu.MaxUnitCount)
	}
	return bestGrade, totalUnits, uint32(avg), ontID
}

func shortRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func maxU32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
