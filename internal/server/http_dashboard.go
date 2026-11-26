package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

// BuildDashboardState returns the exact typed `State` JSON expected by `ui/App.svelte`.
func (a *App) BuildDashboardState(ctx context.Context, baseURL string) map[string]any {
	if baseURL == "" {
		baseURL = strings.TrimRight(a.cfg.PublicURL, "/")
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18420"
	}
	addr, _ := a.mem.Chain().FundAddress()
	balBreakdown, _ := a.mem.Chain().Balance(ctx)
	bal := balBreakdown.SpendableSats
	utxos := balBreakdown.SpendableCount
	feeRate := a.mem.Chain().FeeRateSatPerKB(ctx)
	shards, _ := a.mem.ListUserShards()
	totalRecs, anchoredRecs, _ := a.mem.DB().CountFragments()
	unproven := totalRecs - anchoredRecs
	coverage := 0.0
	if totalRecs > 0 {
		coverage = float64(anchoredRecs) / float64(totalRecs)
	}

	// Receipts + Seller Earnings feed
	rcps, _ := a.mem.DB().ListReceipts(50)
	deals, _ := a.mem.DB().ListDeals(100)
	listings, _ := a.mem.DB().ListSellListings()
	policies, _ := a.mem.DB().ListOperatorPolicies()
	pendingApprs, _ := a.mem.DB().ListPendingApprovals()

	var earned, earned24h uint64
	cutoff24h := time.Now().Add(-24 * time.Hour).Unix()
	amounts := make([]uint64, 0, len(rcps)+len(deals))
	rcpList := make([]map[string]any, 0, len(rcps))
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
			earned += parsed.Payment.AmountSats
			if r.CreatedAt >= cutoff24h {
				earned24h += parsed.Payment.AmountSats
			}
			amounts = append(amounts, parsed.Payment.AmountSats)
		}
		verdict := "amber"
		if idx < 20 && tok != "" {
			if vr, vErr := a.mem.VerifyReceipt(ctx, tok); vErr == nil && vr != nil && vr.Verdict != "" {
				verdict = vr.Verdict
			}
		}
		rcpList = append(rcpList, map[string]any{
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
		if d.Role == "seller" && d.Status == "settled" && d.PriceSats > 0 && d.PaymentTxID != "" {
			earned += d.PriceSats
			if d.UpdatedAt >= cutoff24h {
				earned24h += d.PriceSats
			}
			amounts = append(amounts, d.PriceSats)
		}
	}

	// Market shards summary (real settled works & sats per shard)
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

	// Hosts + Peers + Bounties + Liquidity
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

	// Policies formatted for Settings -> Policy tab
	policyRows := make([]map[string]any, 0, len(policies))
	hasActivePolicy := false
	for _, p := range policies {
		if p.Active {
			hasActivePolicy = true
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
		policyRows = append(policyRows, row)
	}

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
	if a.cfg.RestateClient != nil {
		if invs, qErr := a.cfg.RestateClient.QueryInvocations(ctx, 25); qErr == nil {
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

	steps := []map[string]any{
		{"name": "memory seeded", "done": totalRecs > 0},
		{"name": "receipt issued", "done": len(rcps) > 0},
		{"name": "wallet funded", "done": bal > 0},
		{"name": "policy approved", "done": hasActivePolicy},
		{"name": "network joined", "done": len(activeHostURLs) > 0},
	}
	doneCount := 0
	nextStep := "trade & earn"
	for _, st := range steps {
		if st["done"].(bool) {
			doneCount++
		} else if nextStep == "trade & earn" {
			nextStep = st["name"].(string)
		}
	}

	var adminURL any = nil
	if a.cfg.RestateAdminURL != "" {
		adminURL = a.cfg.RestateAdminURL
	}
	var pushURL any = nil
	if a.broker != nil && a.broker.PublicURL() != "" {
		pushURL = a.broker.PublicURL()
	}

	// Recent memory stream for the live Home timeline with automatic SPV settlement state.
	unsettledList, _ := a.mem.DB().ListUnsettledAnchors()
	unsettledKeys := make(map[string]bool, len(unsettledList))
	for _, ua := range unsettledList {
		unsettledKeys[fmt.Sprintf("%s#%d", ua.Shard, ua.N)] = true
	}
	recentFrags, _ := a.mem.DB().ListRecentFragments(30)
	feedRows := make([]map[string]any, 0, len(recentFrags))
	for _, f := range recentFrags {
		encrypted := f.BorgGroup != ""
		txt := ""
		if encrypted {
			txt = fmt.Sprintf("[encrypted · borg:%s · epoch %d]", f.BorgGroup, f.BorgEpoch)
		} else {
			txt = shortStr(string(f.Data), 500)
		}
		settled := f.BatchN >= 0 && !unsettledKeys[fmt.Sprintf("%s#%d", f.Shard, f.BatchN)]
		feedRows = append(feedRows, map[string]any{
			"shard":      f.Shard,
			"seq":        f.Seq,
			"hdp":        f.HDP,
			"text":       txt,
			"encrypted":  encrypted,
			"borgGroup":  f.BorgGroup,
			"level":      f.Level,
			"class":      f.Class,
			"batchN":     f.BatchN,
			"anchorTxid": f.AnchorTxID,
			"settled":    settled,
			"tainted":    f.Tainted,
			"createdAt":  f.CreatedAt,
		})
	}

	// Jev open-weight semantic matcher status (if attached).
	var aiStatus map[string]any
	if a.broker != nil {
		mm := a.broker.MatcherMetrics()
		aiStatus = map[string]any{
			"enabled":      mm.Enabled,
			"mode":         mm.DefaultMode,
			"requests":     mm.Requests,
			"rejections":   mm.Rejections,
			"escalations":  mm.Escalations,
			"fallbacks":    mm.Fallbacks,
			"errors":       mm.Errors,
			"avgLatencyMs": mm.AvgLatencyMs,
		}
		if mm.ClientHealth != nil {
			aiStatus["endpoint"] = mm.ClientHealth.Endpoint
			aiStatus["model"] = mm.ClientHealth.Model
			aiStatus["circuitOpen"] = mm.ClientHealth.CircuitOpen
		}
	}

	var hubState map[string]any
	if a.cfg.Hub != nil && a.cfg.Hub.Store() != nil {
		fs := a.cfg.Hub.FeeSchedule()
		earn, _ := a.cfg.Hub.Store().FeeEarningsSummary(cutoff24h)
		hubState = map[string]any{
			"role":             fs.Role,
			"preset":           fs.Preset,
			"payeeIdentityKey": a.cfg.Hub.PayeeIdentityKey(),
			"fees":             fs,
			"earnings":         earn,
		}
	}

	return map[string]any{
		"network":   a.mem.Chain().Network(),
		"model":     "nektar/v0.4-go",
		"pub":       shortStr(a.mem.Chain().IdentityPubHex(), 20),
		"uptimeSec": int64(time.Since(a.startedAt).Seconds()),
		"hub":       hubState,
		"loop": map[string]any{
			"steps":    steps,
			"done":     doneCount,
			"frontier": fmt.Sprintf("%d/%d", doneCount, len(steps)),
			"next":     nextStep,
		},
		"treasury": map[string]any{
			"address":         addr,
			"identityPub":     a.mem.Chain().IdentityPubHex(),
			"sats":            bal,
			"confirmedSats":   balBreakdown.ConfirmedSats,
			"unprovenSats":    balBreakdown.UnprovenSats,
			"reservedSats":    balBreakdown.ReservedSats,
			"utxos":           utxos,
			"fuelUtxos":       balBreakdown.FuelCount,
			"reserveUtxos":    balBreakdown.ReserveCount,
			"defaultUtxos":    balBreakdown.DefaultCount,
			"walletMode":      a.mem.Chain().WalletMode(),
			"feeRateSatPerKB": feeRate,
			"funded":          bal > 0,
			"payments":        "BRC-100 + BRC-29 + AtomicBEEF",
		},
		"receipts": map[string]any{
			"count":     len(rcps),
			"earned":    earned,
			"earned24h": earned24h,
			"amounts":   amounts,
			"list":      rcpList,
		},
		"memory": map[string]any{
			"shards":   len(shards),
			"records":  totalRecs,
			"anchored": anchoredRecs,
			"unproven": unproven,
			"coverage": coverage,
			"prover":   "SPV + Rúnar + Mangle",
		},
		"feed":       feedRows,
		"ai":         aiStatus,
		"market":     marketRows,
		"marketTape": marketTape,
		"net": map[string]any{
			"hosts":          hostRows,
			"seeds":          discovery.DefaultSeedHosts(a.mem.Chain().Network()),
			"slots":          map[string]int{"peers": len(peers), "bounties": len(bounties), "listings": len(listings)},
			"boot":           "dir-v1",
			"push":           pushURL,
			"peers":          peers,
			"bounties":       bounties,
			"liquidity":      liquidity,
			"unmatchedWants": unmatchedWants,
			"withholdings":   withholdings,
		},
		"automation": map[string]any{
			"pending": pendingRows,
			"ops":     ops,
			"deals":   dealFeed,
			"rt": map[string]any{
				"executor": rtExecutor,
				"adminUrl": adminURL,
				"pending":  rtPending,
				"nextAt":   rtNextAt,
				"journal":  rtJournal,
			},
		},
		"share": map[string]any{
			"enabled": true,
			"url":     baseURL + "/card",
		},
		"settings": map[string]any{
			"policies": policyRows,
			"hosts": map[string]any{
				"env":     envHosts,
				"added":   addedHosts,
				"blocked": blockedHosts,
			},
		},
	}
}

func (a *App) handleDashboardSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 32)
	a.evMu.Lock()
	a.evSubs[ch] = struct{}{}
	a.evMu.Unlock()
	defer func() {
		a.evMu.Lock()
		delete(a.evSubs, ch)
		close(ch)
		a.evMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "data: {\"event\":\"connected\"}\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stopCh:
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
