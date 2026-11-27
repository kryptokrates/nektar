package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

type dashboardReceiptsSection struct {
	earned    uint64
	earned24h uint64
	spent     uint64
	spent24h  uint64
	amounts   []uint64
	list      []map[string]any
}

func (a *App) buildDashboardReceipts(
	ctx context.Context,
	rcps []store.ReceiptRecord,
	deals []store.DealRecord,
	cutoff24h int64,
) dashboardReceiptsSection {
	sec := dashboardReceiptsSection{
		amounts: make([]uint64, 0, len(rcps)+len(deals)),
		list:    make([]map[string]any, 0, len(rcps)),
	}
	for idx, r := range rcps {
		tok, _ := reasoning.ReceiptToken(r.ReceiptHash)
		var parsed reasoning.Receipt
		_ = json.Unmarshal(r.BodyJSON, &parsed)
		shSeen := make(map[string]bool)
		var shList []string
		for _, ref := range parsed.FragmentRefs {
			if !shSeen[ref.Shard] {
				shSeen[ref.Shard] = true
				shList = append(shList, ref.Shard)
			}
		}
		var satsVal any = nil
		if parsed.Payment != nil && parsed.Payment.AmountSats > 0 {
			satsVal = parsed.Payment.AmountSats
			sec.earned += parsed.Payment.AmountSats
			if r.CreatedAt >= cutoff24h {
				sec.earned24h += parsed.Payment.AmountSats
			}
			sec.amounts = append(sec.amounts, parsed.Payment.AmountSats)
		}
		verdict := "amber"
		if idx < 20 && tok != "" {
			if vr, vErr := a.mem.VerifyReceipt(ctx, tok); vErr == nil && vr != nil && vr.Verdict != "" {
				verdict = vr.Verdict
			}
		}
		sec.list = append(sec.list, map[string]any{
			"seq":      r.ShardSeq,
			"tool":     "groundedAnswer",
			"sats":     satsVal,
			"shards":   shList,
			"at":       r.CreatedAt * 1000,
			"url":      "/v1/verify/" + tok,
			"token":    tok,
			"answer":   shortStr(parsed.Answer, 300),
			"anchored": r.AnchorTxID != "",
			"verdict":  verdict,
		})
	}
	for _, d := range deals {
		if d.Status == "settled" && d.PriceSats > 0 {
			if d.Role == "seller" && d.PaymentTxID != "" {
				sec.earned += d.PriceSats
				if d.UpdatedAt >= cutoff24h {
					sec.earned24h += d.PriceSats
				}
				sec.amounts = append(sec.amounts, d.PriceSats)
			} else if d.Role == "buyer" {
				sec.spent += d.PriceSats
				if d.UpdatedAt >= cutoff24h {
					sec.spent24h += d.PriceSats
				}
			}
		}
	}
	return sec
}

type dashboardPoliciesSection struct {
	policyRows      []map[string]any
	hasActivePolicy bool
	dailyCapSats    uint64
	perDealCapSats  uint64
}

func (a *App) buildDashboardPolicies(policies []store.OperatorPolicyRecord) dashboardPoliciesSection {
	sec := dashboardPoliciesSection{
		policyRows: make([]map[string]any, 0, len(policies)),
	}
	for _, p := range policies {
		if p.Active {
			sec.hasActivePolicy = true
			if p.MaxPerDay > sec.dailyCapSats {
				sec.dailyCapSats = p.MaxPerDay
			}
			if p.MaxPerDeal > sec.perDealCapSats {
				sec.perDealCapSats = p.MaxPerDeal
			}
		}
		policyBody := map[string]any{
			"roles":   p.Roles,
			"domains": p.Domains,
			"limits": map[string]any{
				"maxPerDeal":  p.MaxPerDeal,
				"maxPerDay":   p.MaxPerDay,
				"zeroConfMax": p.ZeroConfMax,
			},
		}
		if strings.TrimSpace(p.AIPolicyJSON) != "" {
			var aiPol market.AIMatchingPolicy
			if json.Unmarshal([]byte(p.AIPolicyJSON), &aiPol) == nil {
				policyBody["aiMatching"] = aiPol
			}
		}
		row := map[string]any{
			"policyHash": p.PolicyHash,
			"policyId":   p.PolicyID,
			"approvedAt": maxI64(0, time.Now().Unix()-p.ApprovedAt),
			"approvedBy": p.ApprovedBy,
			"active":     p.Active,
			"policy":     policyBody,
		}
		if p.RevokedAt > 0 {
			row["revokedAt"] = maxI64(0, time.Now().Unix()-p.RevokedAt)
		}
		sec.policyRows = append(sec.policyRows, row)
	}
	return sec
}

type dashboardNetSection struct {
	marketRows     []map[string]any
	unmatchedWants []store.UnmatchedWantRecord
	withholdings   []discovery.WithholdingWarning
	envHosts       []string
	addedHosts     []string
	blockedHosts   []string
	activeHostURLs []string
	hostRows       []map[string]any
	peers          []discovery.PeerInfo
	bounties       []hub.Bounty
	liquidity      map[string]any
}

func (a *App) buildDashboardMarketAndNet(ctx context.Context, listings []store.SellListing) dashboardNetSection {
	marketRows := make([]map[string]any, 0, len(listings))
	for _, l := range listings {
		frags, _ := a.mem.DB().ListFragments(l.Shard)
		works, shardEarned, _ := a.mem.DB().SellerShardStats(l.Shard)
		marketRows = append(marketRows, map[string]any{
			"shard":   l.Shard,
			"domains": l.Domains,
			"records": len(frags),
			"works":   works,
			"earned":  shardEarned,
			"active":  l.Active,
			"offers":  fmt.Sprintf("%d sats", l.PriceSats),
		})
	}
	unmatchedWants, _ := a.mem.DB().ListUnmatchedWants(20)
	if unmatchedWants == nil {
		unmatchedWants = []store.UnmatchedWantRecord{}
	}
	withholdings := a.disc.Withholdings()
	if withholdings == nil {
		withholdings = []discovery.WithholdingWarning{}
	}

	envHosts, addedHosts, blockedHosts := a.disc.HostLists()
	if envHosts == nil {
		envHosts = []string{}
	}
	if addedHosts == nil {
		addedHosts = []string{}
	}
	if blockedHosts == nil {
		blockedHosts = []string{}
	}
	addedSet := make(map[string]bool, len(addedHosts))
	for _, u := range addedHosts {
		addedSet[u] = true
	}
	persistedHosts, _ := a.mem.DB().ListHosts()
	hostState := make(map[string]string, len(persistedHosts))
	for _, h := range persistedHosts {
		hostState[h.URL] = h.State
	}
	activeHostURLs := a.disc.ActiveHosts()
	hostRows := make([]map[string]any, 0, len(activeHostURLs))
	for _, u := range activeHostURLs {
		st := hostState[u]
		if st == "" {
			st = "up"
		}
		hostRows = append(hostRows, map[string]any{
			"url":   u,
			"state": st,
			"saved": addedSet[u],
		})
	}
	peers := a.disc.Peers()
	if peers == nil {
		peers = []discovery.PeerInfo{}
	}
	bounties := a.toolbox.fetchHubBounties(ctx, activeHostURLs)
	if bounties == nil {
		bounties = []hub.Bounty{}
	}
	var liquidity map[string]any
	if a.broker != nil {
		liquidity = a.broker.LiquidityMetrics()
	}
	return dashboardNetSection{
		marketRows:     marketRows,
		unmatchedWants: unmatchedWants,
		withholdings:   withholdings,
		envHosts:       envHosts,
		addedHosts:     addedHosts,
		blockedHosts:   blockedHosts,
		activeHostURLs: activeHostURLs,
		hostRows:       hostRows,
		peers:          peers,
		bounties:       bounties,
		liquidity:      liquidity,
	}
}

type dashboardAutomationSection struct {
	pendingRows []map[string]any
	dealFeed    []map[string]any
	marketTape  []market.MarketTapeEvent
	ops         []map[string]any
	rtExecutor  string
	rtPending   []map[string]any
	rtJournal   []map[string]any
	rtNextAt    any
}

func (a *App) buildDashboardAutomation(
	ctx context.Context,
	deals []store.DealRecord,
	listings []store.SellListing,
	pendingApprs []store.ApprovalRecord,
) dashboardAutomationSection {
	// Automation panel (Approvals + Broker ops + REAL Restate durable execution telemetry — F26).
	pendingRows := make([]map[string]any, 0, len(pendingApprs))
	for _, p := range pendingApprs {
		pendingRows = append(pendingRows, map[string]any{
			"id":           p.ID,
			"reason":       p.Reason,
			"maxExtraSats": p.MaxExtraSats,
			"operationId":  p.DealID,
			"awakeableId":  p.AwakeableID,
			"expiresAt":    time.Unix(p.ExpiresAt, 0).UTC().Format(time.RFC3339),
		})
	}
	dealFeed := make([]map[string]any, 0, len(deals))
	for i, d := range deals {
		if i >= 24 {
			break
		}
		cause := d.Error
		if cause == "" {
			cause = fmt.Sprintf("%s · %d sats", d.Role, d.PriceSats)
		}
		entry := map[string]any{
			"op":            d.DealID,
			"at":            d.UpdatedAt * 1000,
			"to":            d.Status,
			"cause":         cause,
			"role":          d.Role,
			"priceSats":     d.PriceSats,
			"shard":         d.Shard,
			"paymentTxid":   d.PaymentTxID,
			"deliveredText": shortStr(d.DeliveredText, 180),
		}
		if len(d.MatchAuditJSON) > 0 {
			var ma market.MatchAudit
			if json.Unmarshal(d.MatchAuditJSON, &ma) == nil {
				entry["matchAudit"] = ma
			}
		}
		dealFeed = append(dealFeed, entry)
	}

	// Unified real-time market tape (live verified hub wants/offers + persisted local deals, newest first).
	tapeSeen := make(map[string]bool)
	marketTape := make([]market.MarketTapeEvent, 0, 60)
	if a.broker != nil {
		for _, ev := range a.broker.RecentMarketEvents(48) {
			if ev.ID != "" && tapeSeen[ev.ID] {
				continue
			}
			if ev.ID != "" {
				tapeSeen[ev.ID] = true
			}
			marketTape = append(marketTape, ev)
		}
	}
	for i, d := range deals {
		if i >= 32 {
			break
		}
		if d.DealID != "" && tapeSeen[d.DealID] {
			continue
		}
		if d.DealID != "" {
			tapeSeen[d.DealID] = true
		}
		side := "buy"
		if d.Role == "seller" {
			side = "sell"
		}
		var doms []string
		if d.Shard != "" {
			doms = []string{d.Shard}
		}
		summary := shortStr(strings.TrimSpace(d.DeliveredText), 140)
		if summary == "" {
			summary = d.Error
		}
		if summary == "" && d.Shard != "" {
			summary = "#" + d.Shard
		}
		marketTape = append(marketTape, market.MarketTapeEvent{
			ID:      d.DealID,
			Kind:    "deal",
			Side:    side,
			Domains: doms,
			Summary: summary,
			Sats:    d.PriceSats,
			Shard:   d.Shard,
			Status:  d.Status,
			IsSelf:  true,
			At:      d.UpdatedAt * 1000,
		})
	}
	sort.SliceStable(marketTape, func(i, j int) bool {
		return marketTape[i].At > marketTape[j].At
	})
	if len(marketTape) > 60 {
		marketTape = marketTape[:60]
	}

	rtExecutor := "unattached"
	rtPending := make([]map[string]any, 0)
	rtJournal := make([]map[string]any, 0)
	var rtNextAt any = nil
	if rc := a.restateClient(); rc != nil {
		if invs, qErr := rc.QueryInvocations(ctx, 25); qErr == nil {
			rtExecutor = "restate"
			var earliestNext int64
			for _, inv := range invs {
				stLower := strings.ToLower(inv.Status)
				isActive := stLower == "pending" || stLower == "scheduled" || stLower == "running" || stLower == "suspended" || stLower == "backing-off" || stLower == "paused" || stLower == "ready"
				if isActive {
					nextAt := inv.NextRetryAt
					if nextAt == 0 {
						nextAt = inv.ModifiedAt
					}
					if nextAt > 0 && (earliestNext == 0 || nextAt < earliestNext) {
						earliestNext = nextAt
					}
					rtPending = append(rtPending, map[string]any{
						"kind":        inv.Target,
						"operationId": inv.ID,
						"status":      inv.Status,
						"attempts":    inv.RetryCount,
						"nextAt":      nextAt,
						"lastError":   inv.LastFailureErr,
					})
				}
				cause := inv.LastFailureErr
				if cause == "" {
					cause = inv.ID
				}
				rtJournal = append(rtJournal, map[string]any{
					"op":    inv.Target,
					"at":    inv.ModifiedAt,
					"to":    inv.Status,
					"cause": cause,
				})
			}
			if earliestNext > 0 {
				rtNextAt = earliestNext
			}
		} else {
			rtExecutor = "restate-unavailable"
		}
	}

	ops := []map[string]any{}
	if a.broker != nil {
		ops = append(ops,
			map[string]any{"role": "broker", "kind": "sse-market-feed", "state": "running"},
			map[string]any{"role": "broker", "kind": "nat-bbox-relay", "state": "running"},
		)
		if len(listings) > 0 {
			ops = append(ops, map[string]any{"role": "seller", "kind": fmt.Sprintf("auto-quote (%d shard)", len(listings)), "state": "running"})
		}
	}

	return dashboardAutomationSection{
		pendingRows: pendingRows,
		dealFeed:    dealFeed,
		marketTape:  marketTape,
		ops:         ops,
		rtExecutor:  rtExecutor,
		rtPending:   rtPending,
		rtJournal:   rtJournal,
		rtNextAt:    rtNextAt,
	}
}
