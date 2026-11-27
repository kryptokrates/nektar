package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
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

	rcps, _ := a.mem.DB().ListReceipts(50)
	deals, _ := a.mem.DB().ListDeals(100)
	listings, _ := a.mem.DB().ListSellListings()
	policies, _ := a.mem.DB().ListOperatorPolicies()
	pendingApprs, _ := a.mem.DB().ListPendingApprovals()

	cutoff24h := time.Now().Add(-24 * time.Hour).Unix()
	rcpSec := a.buildDashboardReceipts(ctx, rcps, deals, cutoff24h)
	dailySpent, _ := a.mem.DB().DailySpend("market_daily_spend")
	spent := rcpSec.spent
	if dailySpent > spent {
		spent = dailySpent
	}

	polSec := a.buildDashboardPolicies(policies)
	var activeVault map[string]any
	if vRec, _, vErr := market.LoadActiveVault(a.mem.DB()); vErr == nil && vRec != nil && vRec.Satoshis > 0 {
		activeVault = map[string]any{
			"txid":            vRec.TxID,
			"sats":            vRec.Satoshis,
			"maxPerSpendSats": vRec.State.PerTxCap,
			"paused":          vRec.State.Paused,
		}
	}

	netSec := a.buildDashboardMarketAndNet(ctx, listings)
	autoSec := a.buildDashboardAutomation(ctx, deals, listings, pendingApprs)
	econSec := a.buildDashboardAutonomous(ctx, cutoff24h)

	steps := []map[string]any{
		{"name": "memory seeded", "done": totalRecs > 0},
		{"name": "receipt issued", "done": len(rcps) > 0},
		{"name": "wallet funded", "done": bal > 0},
		{"name": "policy approved", "done": polSec.hasActivePolicy},
		{"name": "network joined", "done": len(netSec.activeHostURLs) > 0},
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
	if u := a.restateAdminURL(); u != "" {
		adminURL = u
	}
	var pushURL any = nil
	if a.broker != nil && a.broker.PublicURL() != "" {
		pushURL = a.broker.PublicURL()
	}

	feedRows := a.buildDashboardFeed()
	repos, _ := a.mem.ListRepos(ctx)
	if repos == nil {
		repos = []memory.RepoSummary{}
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

	custody := "embedded"
	if a.mem.Chain().WalletMode() == "client" {
		custody = "bsv-desktop"
	}
	assetBaskets := a.mem.Chain().ListAssetBaskets(ctx)
	var basketLockedSats uint64
	var basketItems int
	for _, b := range assetBaskets {
		basketLockedSats += b.TotalSats
		basketItems += b.Count
	}

	return map[string]any{
		"network":   a.mem.Chain().Network(),
		"model":     "nektar/v0.1-go",
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
			"address":          addr,
			"identityPub":      a.mem.Chain().IdentityPubHex(),
			"sats":             bal,
			"confirmedSats":    balBreakdown.ConfirmedSats,
			"unprovenSats":     balBreakdown.UnprovenSats,
			"reservedSats":     balBreakdown.ReservedSats,
			"spent":            spent,
			"spent24h":         rcpSec.spent24h,
			"dailySpent":       dailySpent,
			"dailyCapSats":     polSec.dailyCapSats,
			"perDealCapSats":   polSec.perDealCapSats,
			"vault":            activeVault,
			"utxos":            utxos,
			"fuelUtxos":        balBreakdown.FuelCount,
			"reserveUtxos":     balBreakdown.ReserveCount,
			"defaultUtxos":     balBreakdown.DefaultCount,
			"walletMode":       a.mem.Chain().WalletMode(),
			"custody":          custody,
			"baskets":          assetBaskets,
			"basketItems":      basketItems,
			"basketLockedSats": basketLockedSats,
			"feeRateSatPerKB":  feeRate,
			"funded":           bal > 0 || custody == "bsv-desktop",
			"payments":         "BRC-100 + BRC-29 + AtomicBEEF",
		},
		"receipts": map[string]any{
			"count":     len(rcps),
			"earned":    rcpSec.earned,
			"earned24h": rcpSec.earned24h,
			"amounts":   rcpSec.amounts,
			"list":      rcpSec.list,
		},
		"memory": map[string]any{
			"shards":   len(shards),
			"repos":    len(repos),
			"records":  totalRecs,
			"anchored": anchoredRecs,
			"unproven": unproven,
			"coverage": coverage,
			"prover":   "SPV + Rúnar + Mangle",
		},
		"repos":       repos,
		"feed":        feedRows,
		"ai":          aiStatus,
		"economics":   econSec.economics,
		"reputation":  econSec.reputation,
		"distill":     econSec.distill,
		"microEarner": econSec.microEarner,
		"market":      netSec.marketRows,
		"marketTape": autoSec.marketTape,
		"net": map[string]any{
			"hosts":          netSec.hostRows,
			"seeds":          discovery.DefaultSeedHosts(a.mem.Chain().Network()),
			"slots":          map[string]int{"peers": len(netSec.peers), "bounties": len(netSec.bounties), "listings": len(listings)},
			"boot":           "dir-v1",
			"push":           pushURL,
			"peers":          netSec.peers,
			"bounties":       netSec.bounties,
			"liquidity":      netSec.liquidity,
			"unmatchedWants": netSec.unmatchedWants,
			"withholdings":   netSec.withholdings,
		},
		"automation": map[string]any{
			"pending": autoSec.pendingRows,
			"ops":     autoSec.ops,
			"deals":   autoSec.dealFeed,
			"rt": map[string]any{
				"executor": autoSec.rtExecutor,
				"adminUrl": adminURL,
				"pending":  autoSec.rtPending,
				"nextAt":   autoSec.rtNextAt,
				"journal":  autoSec.rtJournal,
			},
		},
		"share": map[string]any{
			"enabled": true,
			"url":     baseURL + "/card",
		},
		"settings": map[string]any{
			"policies": polSec.policyRows,
			"hosts": map[string]any{
				"env":     netSec.envHosts,
				"added":   netSec.addedHosts,
				"blocked": netSec.blockedHosts,
			},
		},
	}
}

// buildDashboardFeed builds the recent memory stream for the live Home timeline with automatic SPV settlement state.
func (a *App) buildDashboardFeed() []map[string]any {
	unsettledList, uErr := a.mem.DB().ListUnsettledAnchors()
	unsettledKeys := make(map[string]bool, len(unsettledList))
	for _, ua := range unsettledList {
		unsettledKeys[fmt.Sprintf("%s#%d", ua.Shard, ua.N)] = true
	}
	recentFrags, _ := a.mem.DB().ListRecentFragments(30)
	feedRows := make([]map[string]any, 0, len(recentFrags))
	for _, f := range recentFrags {
		encrypted := f.BorgGroup != "" || f.LatticeEpoch > 0
		txt := ""
		if f.BorgGroup != "" {
			txt = fmt.Sprintf("[encrypted · borg:%s · epoch %d]", f.BorgGroup, f.BorgEpoch)
		} else if f.LatticeEpoch > 0 {
			txt = fmt.Sprintf("[encrypted · clka · L%d · epoch %d]", f.Level, f.LatticeEpoch)
		} else {
			txt = shortStr(string(f.Data), 500)
		}
		// Fail closed if unsettled anchor lookup errored or fragment has no recorded anchor txid.
		settled := uErr == nil && f.BatchN >= 0 && f.AnchorTxID != "" && !unsettledKeys[fmt.Sprintf("%s#%d", f.Shard, f.BatchN)]
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
			"path":       f.Path,
			"mediaType":  f.MediaType,
			"chirpUrl":   f.ChirpURL,
			"mpeRoot":    f.MPERoot,
			"commitHash": f.CommitHash,
		})
	}
	return feedRows
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
