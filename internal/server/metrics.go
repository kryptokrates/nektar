package server

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (a *App) registerMetricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/metrics", a.handleMetrics)
	mux.HandleFunc("GET /v1/badge/{token}", a.handleVerifyBadge)
}

func (a *App) recordTTFV() {
	if a.ttfvMs.Load() != 0 {
		return
	}
	ms := time.Since(a.startedAt).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	a.ttfvMs.CompareAndSwap(0, ms)
}

func (a *App) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	writeGauge := func(name, help string, val float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, val)
	}
	writeCounter := func(name, help string, val float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %g\n", name, help, name, name, val)
	}

	writeGauge("nektar_uptime_seconds", "Seconds since nektar server started.", time.Since(a.startedAt).Seconds())
	writeGauge("nektar_ttfv_ms", "Milliseconds from boot to first recall or groundedAnswer (0 if none yet).", float64(a.ttfvMs.Load()))

	var totalFrags, anchoredFrags int
	var receiptCount, dealCount int
	if a.mem != nil && a.mem.DB() != nil {
		totalFrags, anchoredFrags, _ = a.mem.DB().CountFragments()
		if rcps, err := a.mem.DB().ListReceipts(10000); err == nil {
			receiptCount = len(rcps)
		}
		if deals, err := a.mem.DB().ListDeals(10000); err == nil {
			dealCount = len(deals)
		}
	}
	writeGauge("nektar_fragments_total", "Total active memory fragments stored.", float64(totalFrags))
	writeGauge("nektar_anchored_fragments_total", "Total anchored memory fragments.", float64(anchoredFrags))
	writeGauge("nektar_receipts_total", "Total issued nektarr1 receipts.", float64(receiptCount))
	writeCounter("nektar_verify_views_total", "Total receipt verification views.", float64(a.verifyViews.Load()))
	writeGauge("nektar_deals_total", "Total market deals recorded.", float64(dealCount))

	a.evMu.Lock()
	sseClients := len(a.evSubs)
	a.evMu.Unlock()
	writeGauge("nektar_sse_clients", "Connected dashboard SSE clients.", float64(sseClients))

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
		writeGauge("nektar_liquidity_fill_rate", "Fraction of buyer wants that settled.", toF("fillRate"))
		writeGauge("nektar_liquidity_zero_rate", "Fraction of buyer wants receiving zero offers.", toF("zeroRate"))
		writeGauge("nektar_liquidity_hub_supply_share", "Share of settled deals filled by Hub supply.", toF("hubSupplyShare"))
		writeGauge("nektar_liquidity_active_peers", "Verified peers announced on the directory lane.", toF("activePeers"))
	}

	if a.cfg.Hub != nil && a.cfg.Hub.Store() != nil {
		if stats, err := a.cfg.Hub.Store().DomainDensityStats(); err == nil && len(stats) > 0 {
			sort.Slice(stats, func(i, j int) bool { return stats[i].Domain < stats[j].Domain })
			fmt.Fprintln(&b, "# HELP nektar_hub_wants_total Hub wants observed per domain.")
			fmt.Fprintln(&b, "# TYPE nektar_hub_wants_total counter")
			for _, st := range stats {
				fmt.Fprintf(&b, "nektar_hub_wants_total{domain=%q} %d\n", st.Domain, st.Wants)
			}
			fmt.Fprintln(&b, "# HELP nektar_hub_offers_total Hub offers observed per domain.")
			fmt.Fprintln(&b, "# TYPE nektar_hub_offers_total counter")
			for _, st := range stats {
				fmt.Fprintf(&b, "nektar_hub_offers_total{domain=%q} %d\n", st.Domain, st.Offers)
			}
		}
		if bounties, err := a.cfg.Hub.Store().ListBounties(false); err == nil && len(bounties) > 0 {
			byDom := make(map[string]int)
			for _, bt := range bounties {
				byDom[bt.Domain] += bt.Filled
			}
			doms := make([]string, 0, len(byDom))
			for d := range byDom {
				doms = append(doms, d)
			}
			sort.Strings(doms)
			fmt.Fprintln(&b, "# HELP nektar_hub_bounties_filled_total Hub bounties filled per domain.")
			fmt.Fprintln(&b, "# TYPE nektar_hub_bounties_filled_total counter")
			for _, d := range doms {
				fmt.Fprintf(&b, "nektar_hub_bounties_filled_total{domain=%q} %d\n", d, byDom[d])
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func (a *App) handleVerifyBadge(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.PathValue("token"))
	ver, err := a.mem.VerifyReceipt(r.Context(), tok)
	label := "nektar receipt"
	status := "unverified"
	color := "#dc2626"
	if err == nil && ver != nil {
		switch ver.Verdict {
		case "green":
			status = "verified"
			color = "#16a34a"
		case "amber":
			if ver.CitationsSuperseded > 0 {
				status = "superseded"
			} else {
				status = "anchored pending"
			}
			color = "#d97706"
		default:
			status = "unverified"
			color = "#dc2626"
		}
	}
	leftW := 96
	rightW := 10 + len(status)*7
	if rightW < 76 {
		rightW = 76
	}
	totalW := leftW + rightW
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">
<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>
<g clip-path="url(#r)">
  <rect width="%d" height="20" fill="#221e1d"/>
  <rect x="%d" width="%d" height="20" fill="%s"/>
  <rect width="%d" height="20" fill="url(#s)"/>
</g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
  <text x="%d" y="14">%s</text>
  <text x="%d" y="14">%s</text>
</g>
</svg>`,
		totalW, html.EscapeString(label), html.EscapeString(status),
		totalW, leftW, leftW, rightW, color, totalW,
		leftW/2, html.EscapeString(label),
		leftW+rightW/2, html.EscapeString(status),
	)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}
