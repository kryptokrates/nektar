package market

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// ArmSellResult embeds store.SellListing with additive readiness diagnostics so callers and UIs
// immediately see whether autoQuoteWant will quote incoming WantAds or why quoting is blocked.
type ArmSellResult struct {
	store.SellListing
	QuotingReady   bool     `json:"quotingReady"`
	BlockedReasons []string `json:"blockedReasons,omitempty"`
}

// ArmSell persists a SellListing, runs fail-closed secret/PII scrubbing across all fragments in the shard,
// anchors the shard if spendable coins exist, and re-announces on `dir`.
func (b *Broker) ArmSell(ctx context.Context, in SellInput) (ArmSellResult, error) {
	if strings.TrimSpace(in.Shard) == "" {
		return ArmSellResult{}, errors.New("market: shard is required")
	}
	shard := strings.TrimSpace(in.Shard)
	// Fail-closed secret/PII scrub BEFORE arming any shard for sale!
	hasUndistilled := false
	hasAnyFrag := false
	if b.mem != nil {
		if frags, rErr := b.mem.Recall(ctx, memory.RecallInput{
			Shard:        shard,
			Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
			Action:       "disclose",
			DisallowBorg: true,
		}); rErr == nil {
			for _, f := range frags {
				hasAnyFrag = true
				sr := distill.Scrub(f.Text)
				if sr.Verdict == distill.ScrubBlocked {
					ruleID := "secret-detected"
					if len(sr.BlockedRules) > 0 {
						ruleID = sr.BlockedRules[0]
					}
					if b.mem.DB() != nil {
						_ = b.mem.DB().RecordDistillQuarantine(store.DistillQuarantineRecord{
							ID:          fmt.Sprintf("sell-%s-%d-%s", shard, f.Seq, ruleID),
							Shard:       shard,
							Seq:         f.Seq,
							RuleID:      ruleID,
							ContentHash: sr.ContentHashHex,
						})
					}
					return ArmSellResult{}, fmt.Errorf("market: cannot arm shard %q for sale — secret/PII blocked by rule %s (run `nektar distill` to sanitize)", shard, ruleID)
				}
				if !f.Distilled {
					hasUndistilled = true
				}
			}
		}
	}
	if len(in.Domains) == 0 {
		in.Domains = []string{"general"}
	}
	if err := nmu.ValidateWantFilters(in.MinAdvertisedGrade, in.FreshnessClass, in.UsageRights, ""); err != nil {
		return ArmSellResult{}, err
	}
	if in.PriceSats == 0 && in.QueryPriceSats == 0 && in.SubscriptionPriceSats == 0 {
		in.PriceSats = 100
	}
	norm := make([]string, 0, len(in.Domains))
	for _, d := range in.Domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			norm = append(norm, d)
		}
	}
	norm = ExpandDomainsWithCorrelations(ctx, b.Matcher(), norm, in.Description, b.ActiveNetworkTopics())
	var dynJSON []byte
	if in.DynamicPricing {
		dp := DynamicPriceState{
			Enabled:          true,
			PerUnitMode:      in.PricePerUnit,
			FloorSats:        in.FloorSats,
			CeilingSats:      in.CeilingSats,
			CurrentPriceSats: in.PriceSats,
			DecayIntervalSec: DefaultDecayIntervalSec,
		}
		dp.normalize(in.PriceSats)
		dynJSON, _ = json.Marshal(dp)
	}
	subDur := in.SubscriptionDurationSec
	if in.SubscriptionPriceSats > 0 && subDur <= 0 {
		subDur = store.DefaultSubscriptionDurationSec
	}
	listing := store.SellListing{
		Shard:                   shard,
		Domains:                 norm,
		PriceSats:               in.PriceSats,
		PricePerUnit:            in.PricePerUnit,
		QueryPriceSats:          in.QueryPriceSats,
		SubscriptionPriceSats:   in.SubscriptionPriceSats,
		SubscriptionDurationSec: subDur,
		FreshnessClass:          string(nmu.NormalizeFreshness(in.FreshnessClass)),
		UsageRights:             string(nmu.EffectiveRights(nmu.UsageRights(in.UsageRights))),
		MinAdvertisedGrade:      string(nmu.NormalizeGrade(in.MinAdvertisedGrade)),
		Description:             in.Description,
		OntologyID:              strings.ToLower(strings.TrimSpace(in.OntologyID)),
		AllowRaw:                in.AllowRaw,
		DynamicPricingJSON:      dynJSON,
		Active:                  true,
		CreatedAt:               time.Now().Unix(),
	}
	if err := b.mem.DB().SaveSellListing(listing); err != nil {
		return ArmSellResult{}, err
	}
	if b.engine != nil && b.engine.FastJournal() != nil {
		b.engine.FastJournal().InvalidateListings()
	}
	b.ReconnectStreams()
	if b.mem.Chain().BalanceSats() >= 250 {
		_, _ = b.mem.AnchorShard(ctx, listing.Shard)
	}
	_, _ = b.AnnounceSelf(ctx)

	var blocked []string
	if hasAnyFrag && hasUndistilled && !in.AllowRaw {
		blocked = append(blocked, "raw_shard_requires_distill_or_allow_raw")
	}
	var zcMax uint64
	if b.engine != nil && b.engine.Policy() != nil {
		_ = SyncPolicyFromDB(b.mem.DB(), b.engine.Policy())
		kill, _, _, _, _, _, sellerOK := b.engine.Policy().Snapshot()
		zcMax, _, _ = b.engine.Policy().ZeroConfLimits()
		if kill {
			blocked = append(blocked, "kill_switch_engaged")
		}
		if !b.cfg.IsHub && !sellerOK {
			blocked = append(blocked, "seller_policy_needed")
		}
	}
	if b.mem != nil && b.mem.Chain() != nil && b.mem.Chain().Network() != "memory" {
		if b.mem.Chain().BalanceSats() == 0 {
			blocked = append(blocked, "wallet_unfunded")
		}
		hasUsableAnchor := false
		if frags, rErr := b.mem.Recall(ctx, memory.RecallInput{
			Shard:        listing.Shard,
			Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
			Action:       "disclose",
			DisallowBorg: true,
		}); rErr == nil {
			for _, f := range frags {
				if f.AnchorState == "mined" || (f.AnchorTxID != "" && zcMax > 0 && listing.PriceSats <= zcMax) {
					hasUsableAnchor = true
					break
				}
			}
		}
		if !hasUsableAnchor {
			blocked = append(blocked, "anchor_pending_mine")
		}
	}

	res := ArmSellResult{
		SellListing:    listing,
		QuotingReady:   len(blocked) == 0,
		BlockedReasons: blocked,
	}
	b.notify("sell_armed", res)
	if res.QuotingReady && b.disc != nil {
		lCopy := listing
		b.spawnTracked(15*time.Second, func(sCtx context.Context) {
			b.sweepOpenWantsForListing(sCtx, lCopy)
		})
	}
	return res, nil
}

// sweepOpenWantsForListing queries active Hubs' ls_nektar index for live WantAds matching listing.Domains
// so StandingOrder bounties posted BEFORE Seller came online are quoted and settled immediately!
func (b *Broker) sweepOpenWantsForListing(ctx context.Context, listing store.SellListing) {
	if b == nil || b.disc == nil || b.mem == nil || b.mem.Chain() == nil {
		return
	}
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return
	}
	myNet := b.mem.Chain().Network()
	myPub := b.PubHex()
	nowMs := time.Now().UnixMilli()
	maxExpMs := nowMs + bstack.MaxLifetime.Milliseconds() + 60_000
	doms := listing.Domains
	if len(doms) == 0 {
		doms = []string{""}
	}
	for _, dom := range doms {
		q := bstack.MarketQuery{Kind: bstack.KindWant, Domain: strings.ToLower(strings.TrimSpace(dom)), Limit: 50}
		if q.Domain == "*" {
			q.Domain = ""
		}
		qBytes, _ := json.Marshal(map[string]any{
			"service": bstack.LookupService,
			"query":   q,
		})
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
			for _, ev := range ans.Result {
				payload, ok, _ := b.verifyMarketEventBEEF(ctx, ev)
				if !ok || payload.Kind != bstack.KindWant {
					continue
				}
				sw, decErr := bstack.DecodeWantPayload(payload.Body)
				if decErr != nil || sw.Want.Net != myNet || sw.Want.Exp <= nowMs || sw.Want.Exp > maxExpMs {
					continue
				}
				if sw.Want.Pub == "" || strings.EqualFold(sw.Want.Pub, myPub) {
					continue
				}
				if bstack.VerifyArtifactRaw(sw.Want.Pub, sw.Sig, sw.Want.DomainTag(), sw.RawCBOR) != nil {
					continue
				}
				b.mu.Lock()
				_, seen := b.seenWants[sw.Want.WantID]
				if !seen {
					b.seenWants[sw.Want.WantID] = time.Now().Unix()
				}
				b.mu.Unlock()
				b.autoQuoteWant(ctx, sw.Want)
			}
		}
	}
}

// AnnounceSelf publishes this node's signed AnnounceAd to the enumerable `dir` lane across active hubs.
func (b *Broker) AnnounceSelf(ctx context.Context) (int, error) {
	listings, _ := b.mem.DB().ListSellListings()
	domSeen := make(map[string]bool)
	var sellDoms []string
	for _, l := range listings {
		if !l.Active {
			continue
		}
		for _, d := range l.Domains {
			if !domSeen[d] {
				domSeen[d] = true
				sellDoms = append(sellDoms, d)
			}
		}
	}
	b.mu.RLock()
	pubURL := b.cfg.PublicURL
	office := b.cfg.BboxOffice
	handle := b.cfg.Handle
	isHub := b.cfg.IsHub
	packs := append([]bstack.PackRef(nil), b.packs...)
	bountyDoms := append([]string(nil), b.bountyDoms...)
	b.mu.RUnlock()

	var bondOutpoint, bondBEEF string
	hasActiveBond := false
	if bRec, _, bErr := LoadActiveBond(b.mem.DB()); bErr == nil && bRec != nil && !bRec.State.Seized {
		hasActiveBond = true
		bondOutpoint = bRec.Outpoint()
		if len(bRec.BEEFHex) <= 8192 {
			bondBEEF = bRec.BEEFHex
		}
	}

	// Enrich AnnounceAd.Packs with multi-mode NMU-1 summaries for active listings so ls_nektar &
	// GET /v1/market/ticker discover spot, per_query, and subscription markets cleanly.
	packIdx := make(map[string]int, len(packs))
	for i, p := range packs {
		packIdx[p.Shard] = i
	}
	baseURL := strings.TrimRight(strings.TrimSpace(pubURL), "/")
	for _, l := range listings {
		if !l.Active {
			continue
		}
		modeStrs := l.EnabledModes()
		var queryEP, feedEP string
		if baseURL != "" {
			if l.SupportsMode(string(nmu.ModePerQuery)) {
				queryEP = baseURL + "/v1/market/query"
			}
			if l.SupportsMode(string(nmu.ModeSubscription)) {
				feedEP = baseURL + "/v1/market/feeds/" + l.Shard
			}
		}
		bestGrade, avgUnits, fragCount, fragOnt := b.summarizeListingNMU(ctx, l, hasActiveBond, feedEP)
		ontID := strings.ToLower(strings.TrimSpace(l.OntologyID))
		if ontID == "" {
			ontID = fragOnt
		}
		if len(ontID) != 64 {
			ontID = ""
		}
		var spotPrice, unitRate uint64
		if l.PriceSats > 0 {
			dp := ParseDynamicPriceState(l.DynamicPricingJSON, l.PriceSats)
			dp.PerUnitMode = l.PricePerUnit || dp.PerUnitMode
			spotPrice, unitRate = dp.QuoteForFactPack(l.PriceSats, avgUnits, false, 0)
		}
		ref := bstack.PackRef{
			Shard:                 l.Shard,
			Domains:               append([]string(nil), l.Domains...),
			OntologyID:            ontID,
			Count:                 fragCount,
			Grade:                 string(bestGrade),
			UnitCount:             avgUnits,
			PricingMode:           modeStrs[0],
			EnabledModes:          modeStrs,
			PriceSats:             spotPrice,
			SatsPer1kUnits:        unitRate,
			QueryPriceSats:        l.QueryPriceSats,
			SubscriptionPriceSats: l.SubscriptionPriceSats,
			SubscriptionSec:       l.EffectiveSubscriptionDurationSec(),
			FreshnessClass:        l.FreshnessClass,
			UsageRights:           string(nmu.EffectiveRights(nmu.UsageRights(l.UsageRights))),
			QueryEndpoint:         queryEP,
			FeedEndpoint:          feedEP,
		}
		if idx, exists := packIdx[l.Shard]; exists {
			if ref.ChirpURL == "" {
				ref.ChirpURL = packs[idx].ChirpURL
			}
			if ref.RootHex == "" {
				ref.RootHex = packs[idx].RootHex
			}
			if ref.OntologyID == "" {
				ref.OntologyID = packs[idx].OntologyID
			}
			ref.TargetOntologyID = packs[idx].TargetOntologyID
			packs[idx] = ref
		} else if len(packs) < 16 {
			packIdx[l.Shard] = len(packs)
			packs = append(packs, ref)
		}
	}

	var hubRole, hubPayKey, hubTermsURL string
	var hubFees *bstack.HubFeeAdvert
	if b.cfg.Hub != nil {
		isHub = true
		fs := b.cfg.Hub.FeeSchedule()
		hubRole = string(fs.Role)
		hubPayKey = b.cfg.Hub.PayeeIdentityKey()
		hubFees = fs.AdvertSummary()
		if pubURL != "" {
			base := strings.TrimRight(pubURL, "/")
			if strings.HasSuffix(base, "/hub") {
				hubTermsURL = base + "/v1/terms"
			} else {
				hubTermsURL = base + "/hub/v1/terms"
			}
		}
	}

	return b.disc.PublishAnnounce(ctx, bstack.AnnounceAd{
		Handle:            handle,
		PublicURL:         pubURL,
		BboxOffice:        office,
		Domains:           sellDoms,
		SellDomains:       sellDoms,
		BountyDomains:     bountyDoms,
		Packs:             packs,
		BondOutpoint:      bondOutpoint,
		BondBEEFHex:       bondBEEF,
		IsHub:             isHub,
		HubRole:           hubRole,
		HubPayIdentityKey: hubPayKey,
		HubTermsURL:       hubTermsURL,
		HubFees:           hubFees,
	})
}

func (b *Broker) summarizeListingNMU(ctx context.Context, l store.SellListing, hasActiveBond bool, feedEP string) (bestGrade nmu.Grade, avgUnits uint32, count int, ontID string) {
	if b == nil || b.mem == nil || b.mem.DB() == nil {
		return "", 1, 0, ""
	}
	recalled, err := b.mem.Recall(ctx, memory.RecallInput{
		Shard:        l.Shard,
		Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
		Action:       "disclose",
		DisallowBorg: true,
		VerifySPV:    false,
	})
	if err != nil {
		return "", 1, 0, ""
	}
	nowUnix := time.Now().Unix()
	var totalUnits uint64
	for _, f := range recalled {
		eval := b.mem.EvaluateSellableFragment(ctx, f, &l, hasActiveBond, feedEP, nowUnix)
		if !eval.Allowed {
			continue
		}
		count++
		if ontID == "" && len(eval.OntologyID) == 64 {
			ontID = eval.OntologyID
		}
		if nmu.GradeRank(eval.Classification.Grade) > nmu.GradeRank(bestGrade) {
			bestGrade = eval.Classification.Grade
		}
		u := eval.Classification.UnitCount
		if u == 0 {
			u = 1
		}
		totalUnits += uint64(u)
	}
	if count == 0 {
		return "", 1, 0, ontID
	}
	avg := totalUnits / uint64(count)
	if avg == 0 {
		avg = 1
	}
	if avg > uint64(nmu.MaxUnitCount) {
		avg = uint64(nmu.MaxUnitCount)
	}
	return bestGrade, uint32(avg), count, ontID
}
