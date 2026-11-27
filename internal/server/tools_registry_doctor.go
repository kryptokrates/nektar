package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/reasoning"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // green | amber | red
	Detail string `json:"detail"`
}

func (tb *Toolbox) ExecDoctor(ctx context.Context) map[string]any {
	checks := []DoctorCheck{}
	nextSteps := []string{}

	// 1. Memory & welcome shard check
	shards, _ := tb.mem.ListUserShards()
	total, anchored, _ := tb.mem.DB().CountFragments()
	if total > 0 {
		checks = append(checks, DoctorCheck{Name: "memory", Status: "green", Detail: fmt.Sprintf("%d fragment(s) across %d shard(s), %d anchored", total, len(shards), anchored)})
	} else {
		checks = append(checks, DoctorCheck{Name: "memory", Status: "amber", Detail: "no user fragments yet"})
		nextSteps = append(nextSteps, "Run `nektar remember -shard notes -text \"...\"`")
	}

	// 2. Wallet, proven fuel stock, unproven depth, pending anchors, and SPV witness check (Finding #42)
	bal, balErr := tb.mem.Chain().Balance(ctx)
	addr, _ := tb.mem.Chain().FundAddress()
	pendingAnchors, _ := tb.mem.DB().CountPendingAnchors()
	unsettledAnchors, _ := tb.mem.DB().CountUnsettledAnchors()
	zcSats, zcCount := tb.mem.Chain().ZeroConfWatch().Exposure()
	if balErr != nil {
		checks = append(checks, DoctorCheck{Name: "wallet", Status: "red", Detail: fmt.Sprintf("wallet balance query failed: %v", balErr)})
	} else if bal.SpendableSats > 0 && bal.SpendableCount >= 2 {
		st := "green"
		detail := fmt.Sprintf("%d sats (%d confirmed / %d unproven) across %d UTXO(s); fuel=%d proven/%d unproven; maxUnprovenDepth=%d (%s)",
			bal.SpendableSats, bal.ConfirmedSats, bal.UnprovenSats, bal.SpendableCount,
			bal.ProvenFuelCount, bal.UnprovenFuelCount, bal.MaxUnprovenDepth, addr)
		if bal.MaxUnprovenDepth >= 15 {
			st = "amber"
			detail += " — unproven chain depth at cap; waiting for block confirmation"
		}
		checks = append(checks, DoctorCheck{Name: "wallet", Status: st, Detail: detail})
	} else if bal.SpendableSats > 0 {
		checks = append(checks, DoctorCheck{Name: "wallet", Status: "amber", Detail: fmt.Sprintf("%d sats (%d confirmed / %d unproven) on %d UTXO(s) — fuel=%d proven/%d unproven", bal.SpendableSats, bal.ConfirmedSats, bal.UnprovenSats, bal.SpendableCount, bal.ProvenFuelCount, bal.UnprovenFuelCount)})
	} else {
		checks = append(checks, DoctorCheck{Name: "wallet", Status: "amber", Detail: fmt.Sprintf("0 sats at %s", addr)})
		nextSteps = append(nextSteps, "Fund wallet via 1-click Testnet Faucet or `nektar serve` dashboard")
	}
	if pendingAnchors > 0 || unsettledAnchors > 0 {
		checks = append(checks, DoctorCheck{
			Name:   "anchors",
			Status: "amber",
			Detail: fmt.Sprintf("%d pending anchor(s), %d broadcast anchor(s) awaiting mined BUMP (settleDepth=%d)", pendingAnchors, unsettledAnchors, tb.mem.Chain().SettleDepth()),
		})
	}

	// 2a. BSV Desktop / BRC-100 custody check (fast <=250ms loopback probe on :3321)
	walletMode := tb.mem.Chain().WalletMode()
	desktopProbe := nwallet.DesktopProbeStatus{WireURL: nwallet.DefaultDesktopWireURL}
	if tb.mem.Chain().Network() != "memory" && !IsContainerEnv() {
		desktopProbe = nwallet.ProbeDesktopWallet(ctx, nwallet.DefaultDesktopWireURL)
	}
	if walletMode == "client" {
		if desktopProbe.Available {
			checks = append(checks, DoctorCheck{
				Name:   "bsv_desktop",
				Status: "green",
				Detail: fmt.Sprintf("direct BSV Desktop BRC-100 custody active on %s (%s) — zero spending keys in Nektar", desktopProbe.WireURL, desktopProbe.Network),
			})
		} else {
			checks = append(checks, DoctorCheck{
				Name:   "bsv_desktop",
				Status: "red",
				Detail: fmt.Sprintf("client custody mode configured but BSV Desktop not reachable on %s", desktopProbe.WireURL),
			})
			nextSteps = append(nextSteps, "Open and unlock BSV Desktop / Metanet Desktop on 127.0.0.1:3321")
		}
	} else if desktopProbe.Available {
		checks = append(checks, DoctorCheck{
			Name:   "bsv_desktop",
			Status: "green",
			Detail: fmt.Sprintf("BSV Desktop detected on %s (%s) — 1-click BRC-29 bridge ready (`nektar wallet fund --from-desktop --sats 25000` | `sweep --to-desktop`, or `--wallet client` for direct custody)", desktopProbe.WireURL, desktopProbe.Network),
		})
		if bal.SpendableSats == 0 {
			nextSteps = append(nextSteps, "Fund from BSV Desktop in 1 click: `nektar wallet fund --from-desktop --sats 25000` (or restart with `nektar up --wallet client`)")
		}
	} else {
		checks = append(checks, DoctorCheck{
			Name:   "bsv_desktop",
			Status: "green",
			Detail: "embedded full BRC-100 wallet active (BSV Desktop not detected on 127.0.0.1:3321)",
		})
	}

	// 2b. Restate durable execution readiness (required for live BSV market trades)
	restateAttached := tb.broker != nil && tb.broker.RestateClient() != nil && tb.isRestateAlive()
	if tb.mem.Chain().Network() == "memory" {
		checks = append(checks, DoctorCheck{Name: "restate", Status: "green", Detail: "memory network — local workflow execution active"})
	} else if restateAttached {
		checks = append(checks, DoctorCheck{Name: "restate", Status: "green", Detail: "Restate durable execution attached (live BSV trades armed)"})
	} else {
		checks = append(checks, DoctorCheck{Name: "restate", Status: "amber", Detail: "Restate durable executor not attached — memory & receipts work locally, live BSV trades require Restate"})
		nextSteps = append(nextSteps, "Run `nektar restate-up` (macOS/Linux/Windows WSL2) then `nektar stop && nektar up` to arm live BSV trading")
	}

	// 3. Operator policy check
	policies, _ := tb.mem.DB().ListOperatorPolicies()
	activePols := 0
	for _, p := range policies {
		if p.Active {
			activePols++
		}
	}
	if tb.mkt != nil && tb.mkt.Policy().TamperedPoliciesCount() > 0 {
		checks = append(checks, DoctorCheck{
			Name:   "policy",
			Status: "red",
			Detail: fmt.Sprintf("%d active operator policy row(s) rejected for hash/signature tamper (fail-closed)", tb.mkt.Policy().TamperedPoliciesCount()),
		})
	} else if activePols > 0 {
		checks = append(checks, DoctorCheck{Name: "policy", Status: "green", Detail: fmt.Sprintf("%d active operator market policy(ies)", activePols)})
	} else {
		checks = append(checks, DoctorCheck{Name: "policy", Status: "amber", Detail: "no market policy approved yet (trades will pause for approval)"})
		nextSteps = append(nextSteps, "Run `nektar policy approve --preset both` to enable autonomous buying & selling")
	}

	// 4. Network hosts & peers check
	var hosts []string
	var peers []discovery.PeerInfo
	if tb.disc != nil {
		hosts = tb.disc.ActiveHosts()
		peers = tb.disc.Peers()
	}
	if len(hosts) > 0 {
		checks = append(checks, DoctorCheck{Name: "network", Status: "green", Detail: fmt.Sprintf("%d active hub host(s), %d verified peer(s)", len(hosts), len(peers))})
	} else {
		checks = append(checks, DoctorCheck{Name: "network", Status: "amber", Detail: "single-player mode (no hub hosts configured)"})
		nextSteps = append(nextSteps, "Start a local Hub (`nektar serve --hub`) or join a Hub in Dashboard -> Network")
	}

	// 5. SSE vs Poll health & host withholding telemetry
	if tb.broker != nil {
		liq := tb.broker.LiquidityMetrics()
		checks = append(checks, DoctorCheck{
			Name:   "market_stream",
			Status: "green",
			Detail: fmt.Sprintf("sseWakes=%v pollWakes=%v bboxWakes=%v", liq["sseWakes"], liq["pollWakes"], liq["bboxWakes"]),
		})
		if mm := tb.broker.MatcherMetrics(); mm.Enabled {
			st := "green"
			ep := ""
			model := ""
			if mm.ClientHealth != nil {
				ep = mm.ClientHealth.Endpoint
				model = mm.ClientHealth.Model
			}
			detail := fmt.Sprintf("provider=openweight endpoint=%s model=%s mode=%s requests=%d rejections=%d escalations=%d fallbacks=%d inputTokens=%d",
				ep, model, mm.DefaultMode, mm.Requests, mm.Rejections, mm.Escalations, mm.Fallbacks, mm.InputTokens)
			if mm.ClientHealth != nil && mm.ClientHealth.CircuitOpen {
				st = "amber"
				detail += " (circuit breaker open)"
			}
			checks = append(checks, DoctorCheck{
				Name:   "jev_matcher",
				Status: st,
				Detail: detail,
			})
		}
	}
	var withholdings []discovery.WithholdingWarning
	if tb.disc != nil {
		withholdings = tb.disc.Withholdings()
		if len(withholdings) > 0 {
			checks = append(checks, DoctorCheck{
				Name:   "host_integrity",
				Status: "amber",
				Detail: fmt.Sprintf("%d host withholding event(s) detected across directory quorum", len(withholdings)),
			})
		}
	}

	// 6. Local shard hash-chain integrity, surviving obligations, and verifiable book reconciliation.
	if reports, iErr := tb.mem.VerifyAllShardsIntegrity(); iErr == nil {
		broken := 0
		var brokenDetail string
		for _, r := range reports {
			if !r.OK {
				broken++
				if brokenDetail == "" {
					brokenDetail = fmt.Sprintf("shard %s broken at seq %d: %s", r.Shard, r.BrokenSeq, r.Reason)
				}
			}
		}
		if broken > 0 {
			checks = append(checks, DoctorCheck{Name: "shard_integrity", Status: "red", Detail: brokenDetail})
		} else {
			checks = append(checks, DoctorCheck{Name: "shard_integrity", Status: "green", Detail: fmt.Sprintf("%d user shard(s) hash-chain verified", len(reports))})
		}
	}
	if openOb, overdueOb, oErr := tb.mem.DB().CountObligations(); oErr == nil {
		st := "green"
		if overdueOb > 0 {
			st = "amber"
		}
		checks = append(checks, DoctorCheck{
			Name:   "obligations",
			Status: st,
			Detail: fmt.Sprintf("open=%d overdue=%d surviving commercial obligations", openOb, overdueOb),
		})
	}
	if bookRep, bErr := market.ReconcileBooks(tb.mem.DB(), tb.mem.Chain().SpendGuard(), time.Now()); bErr == nil {
		st := "green"
		detail := fmt.Sprintf("buyerDeals=%d (%d sats) sellerDeals=%d (%d sats) bilateralReceipts=%d",
			bookRep.SettledBuyerDeals, bookRep.SettledBuyerSats, bookRep.SettledSellerDeals, bookRep.SettledSellerSats, bookRep.BilateralReceipts)
		if !bookRep.OK {
			st = "amber"
			if bookRep.InvalidReceipts > 0 {
				st = "red"
			}
			detail += fmt.Sprintf(" anomalies=%d (%s)", len(bookRep.Anomalies), bookRep.Anomalies[0])
		}
		checks = append(checks, DoctorCheck{Name: "book_reconciliation", Status: st, Detail: detail})
	}
	if wd := tb.currentWatchdog(); wd != nil {
		wst := wd.Status()
		clkStatus := "green"
		if wst.ClockStatus == "warn" || wst.ClockStatus == "degraded-http-date" {
			clkStatus = "amber"
		} else if wst.ClockStatus == "tripped" {
			clkStatus = "red"
			nextSteps = append(nextSteps, "Sync your system clock (NTP/chrony/w32tm) so offset stays well within ±25s of UTC, or run POST /v1/autonomy/reset after fixing time.")
		}
		checks = append(checks, DoctorCheck{
			Name:   "clock_skew_guard",
			Status: clkStatus,
			Detail: fmt.Sprintf("status=%s offsetMs=%d server=%s", wst.ClockStatus, wst.ClockOffsetMs, wst.ClockServer),
		})
		brkStatus := "green"
		brkDetail := fmt.Sprintf("breaker=%s slashesInWindow=%d isolatedPeers=%d", wst.SpendBreakerState, wst.SlashEventsWindow, len(wst.IsolatedPeers))
		if wst.SpendPaused {
			brkStatus = "red"
			brkDetail += " pausedReason=" + wst.PausedReason
			nextSteps = append(nextSteps, "Autonomous spend is paused by Watchdog ("+wst.PausedReason+"). Inspect recent deals/clock and reset via POST /v1/autonomy/reset when resolved.")
		} else if len(wst.IsolatedPeers) > 0 || wst.SlashEventsWindow > 0 {
			brkStatus = "amber"
		}
		checks = append(checks, DoctorCheck{Name: "autonomy_watchdog", Status: brkStatus, Detail: brkDetail})
	}
	if tb.broker != nil && tb.broker.Subscriptions() != nil {
		sst := tb.broker.Subscriptions().Status()
		checks = append(checks, DoctorCheck{
			Name:   "grade_s_subscriptions",
			Status: "green",
			Detail: fmt.Sprintf("running=%v active=%d events=%d slashes=%d", sst.Running, sst.ActiveCount, sst.EventsTotal, sst.SlashesTotal),
		})
	}

	overall := "green"
	for _, c := range checks {
		if c.Status == "red" {
			overall = "red"
			break
		}
		if c.Status == "amber" && overall == "green" {
			overall = "amber"
		}
	}
	return map[string]any{
		"ok":                   overall != "red",
		"verdict":              overall,
		"network":              tb.mem.Chain().Network(),
		"walletMode":           walletMode,
		"desktopWallet":        desktopProbe,
		"restateAttached":      restateAttached,
		"shardNames":           shards,
		"records":              total,
		"anchoredRecords":      anchored,
		"confirmedSats":        bal.ConfirmedSats,
		"unprovenSats":         bal.UnprovenSats,
		"provenFuelCount":      bal.ProvenFuelCount,
		"unprovenFuelCount":    bal.UnprovenFuelCount,
		"maxUnprovenDepth":     bal.MaxUnprovenDepth,
		"pendingAnchors":       pendingAnchors,
		"unsettledAnchors":     unsettledAnchors,
		"zeroConfExposureSats": zcSats,
		"zeroConfCount":        zcCount,
		"settleDepth":          tb.mem.Chain().SettleDepth(),
		"checks":               checks,
		"withholdings":         withholdings,
		"nextSteps":            nextSteps,
		"hints":                nextSteps,
		"truthNote":            reasoning.TruthNote,
	}
}

func (tb *Toolbox) ExecPeers(ctx context.Context, refresh bool) map[string]any {
	var peers []discovery.PeerInfo
	var hosts []string
	if tb.disc != nil {
		hosts = tb.disc.ActiveHosts()
		if refresh {
			peers, _ = tb.disc.DiscoverPeers(ctx, hosts)
		} else {
			peers = tb.disc.Peers()
			if len(peers) == 0 && len(hosts) > 0 {
				peers, _ = tb.disc.DiscoverPeers(ctx, hosts)
			}
		}
	}
	bounties := tb.fetchHubBounties(ctx, hosts)
	return map[string]any{
		"hosts":    hosts,
		"peers":    peers,
		"bounties": bounties,
	}
}

func (tb *Toolbox) fetchHubBounties(ctx context.Context, hosts []string) []hub.Bounty {
	if tb.hubNode != nil {
		if b, err := tb.hubNode.Store().ListBounties(true); err == nil {
			return b
		}
	}
	var out []hub.Bounty
	seen := make(map[string]bool)
	for _, h := range hosts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h, "/")+"/bounties", nil)
		if err != nil {
			continue
		}
		resp, err := tb.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		var parsed struct {
			Bounties []hub.Bounty `json:"bounties"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed)
		resp.Body.Close()
		for _, b := range parsed.Bounties {
			if !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
	}
	return out
}
