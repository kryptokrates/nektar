package server

import (
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/kryptokrates/nektar/internal/dock"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// DomainDensityHealth classifies one knowledge domain's liquidity state against cold-start bars.
type DomainDensityHealth struct {
	Domain         string `json:"domain"`
	Status         string `json:"status"` // live | forming | dead
	Wants          int    `json:"wants"`
	Offers         int    `json:"offers"`
	OrganicSellers int    `json:"organicSellers"`
	BountiesFilled int    `json:"bountiesFilled"`
}

// ColdStartBars documents the explicit mechanical thresholds defining a tipped atomic network.
type ColdStartBars struct {
	FillRateMin          float64 `json:"fillRateMin"`
	ZeroRateMax          float64 `json:"zeroRateMax"`
	MedianTimeToOfferMs  int64   `json:"medianTimeToOfferMs"`
	OrganicSellersMin    int     `json:"organicSellersMin"`
	HubSupplyShareMax    float64 `json:"hubSupplyShareMax"`
}

// DefaultColdStartBars returns the written liquidity bars from docs/COLD-START.md.
func DefaultColdStartBars() ColdStartBars {
	return ColdStartBars{
		FillRateMin:         0.80,
		ZeroRateMax:         0.10,
		MedianTimeToOfferMs: 60_000,
		OrganicSellersMin:   3,
		HubSupplyShareMax:   0.20,
	}
}

// BountyTaperPolicy publishes the honest launch-subsidy taper schedule upfront.
func BountyTaperPolicy() map[string]any {
	return map[string]any{
		"phase":              "launch-seeding",
		"initialRewardSats":  100,
		"stepDownAtFills":    25,
		"stepDownRewardSats": 50,
		"maxFillsPerDomain":  50,
		"exitCriterion":      "hubSupplyShare < 20% and organicSellers >= 3 per domain",
		"honestPolicy":       "Launch subsidies are temporary cold-start incentives with a published taper — never quietly degraded.",
	}
}

func (a *App) registerColdStartRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/coldstart/health", a.withAuth(http.HandlerFunc(a.handleColdStartHealth)))
}

func (a *App) handleColdStartHealth(w http.ResponseWriter, _ *http.Request) {
	bars := DefaultColdStartBars()

	var fillRate, zeroRate, hubSupplyShare float64
	var activePeers int
	if a.broker != nil {
		liq := a.broker.LiquidityMetrics()
		toF := func(k string) float64 {
			switch v := liq[k].(type) {
			case float64:
				return v
			case int:
				return float64(v)
			case int64:
				return float64(v)
			default:
				return 0
			}
		}
		fillRate = toF("fillRate")
		zeroRate = toF("zeroRate")
		hubSupplyShare = toF("hubSupplyShare")
		activePeers = int(toF("activePeers"))
	}

	// Build per-domain density table combining Hub stats, bounties, and discovered peers.
	domMap := make(map[string]*DomainDensityHealth)
	ensureDom := func(d string) *DomainDensityHealth {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, "cluster:")))
		if d == "" {
			return nil
		}
		row, ok := domMap[d]
		if !ok {
			row = &DomainDensityHealth{Domain: d, Status: "dead"}
			domMap[d] = row
		}
		return row
	}
	for _, seed := range []string{"ops", "bsv", "agents"} {
		ensureDom(seed)
	}
	if a.cfg.Hub != nil && a.cfg.Hub.Store() != nil {
		if stats, err := a.cfg.Hub.Store().DomainDensityStats(); err == nil {
			for _, st := range stats {
				if row := ensureDom(st.Domain); row != nil {
					row.Wants += int(st.Wants)
					row.Offers += int(st.Offers)
				}
			}
		}
		if bounties, err := a.cfg.Hub.Store().ListBounties(false); err == nil {
			for _, b := range bounties {
				if row := ensureDom(b.Domain); row != nil {
					row.BountiesFilled += b.Filled
				}
			}
		}
	}
	organicPubs := make(map[string]bool)
	if a.disc != nil {
		for _, p := range a.disc.Peers() {
			if !p.IsHub && len(p.SellDomains) > 0 {
				organicPubs[p.Pub] = true
			}
			for _, d := range p.SellDomains {
				if row := ensureDom(d); row != nil && !p.IsHub {
					row.OrganicSellers++
				}
			}
		}
	}

	domains := make([]DomainDensityHealth, 0, len(domMap))
	liveDomains := 0
	formingDomains := 0
	for _, row := range domMap {
		switch {
		case row.OrganicSellers >= bars.OrganicSellersMin && (row.Wants == 0 || float64(row.Offers)/float64(row.Wants) >= bars.FillRateMin):
			row.Status = "live"
			liveDomains++
		case row.OrganicSellers > 0 || row.Offers > 0 || row.BountiesFilled > 0 || row.Wants > 0:
			row.Status = "forming"
			formingDomains++
		default:
			row.Status = "dead"
		}
		domains = append(domains, *row)
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].Domain < domains[j].Domain })

	var dockedCohorts []string
	if !IsContainerEnv() {
		home, _ := os.UserHomeDir()
		for _, c := range dock.DetectAll(home) {
			if c.Docked {
				dockedCohorts = append(dockedCohorts, c.ID)
			}
		}
	}

	receiptCount, _ := a.mem.DB().CountReceipts()
	dealCount, _ := a.mem.DB().CountDeals()
	restateAttached := a.restateClient() != nil || (a.broker != nil && a.broker.RestateClient() != nil)

	overallStatus := "forming"
	if liveDomains > 0 && fillRate >= bars.FillRateMin && zeroRate < bars.ZeroRateMax && hubSupplyShare < bars.HubSupplyShareMax {
		overallStatus = "live"
	} else if receiptCount == 0 && len(dockedCohorts) == 0 && activePeers == 0 {
		overallStatus = "cold"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":          overallStatus,
		"network":         a.mem.Chain().Network(),
		"restateAttached": restateAttached,
		"ttfvMs":          a.ttfvMs.Load(),
		"verifyViews":     a.verifyViews.Load(),
		"receiptCount":    receiptCount,
		"dealCount":       dealCount,
		"dockedCohorts":   dockedCohorts,
		"liquidity": map[string]any{
			"fillRate":       fillRate,
			"zeroRate":       zeroRate,
			"hubSupplyShare": hubSupplyShare,
			"activePeers":    activePeers,
			"organicSellers": len(organicPubs),
			"liveDomains":    liveDomains,
			"formingDomains": formingDomains,
		},
		"bars":        bars,
		"domains":     domains,
		"bountyTaper": BountyTaperPolicy(),
		"truthNote":   reasoning.TruthNote,
	})
}
