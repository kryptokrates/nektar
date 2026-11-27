package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/wirewallet"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func (a *App) registerWalletRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/wallet", a.withAuth(http.HandlerFunc(a.handleWalletStatus)))
	mux.Handle("GET /v1/wallet/desktop", a.withAuth(http.HandlerFunc(a.handleWalletDesktopProbe)))
	mux.Handle("GET /v1/wallet/baskets", a.withAuth(http.HandlerFunc(a.handleWalletBaskets)))
	mux.Handle("GET /v1/wallet/utxos", a.withAuth(http.HandlerFunc(a.handleWalletUTXOs)))
	mux.Handle("GET /v1/wallet/actions", a.withAuth(http.HandlerFunc(a.handleWalletActions)))
	mux.Handle("POST /v1/wallet/sync", a.withAuth(http.HandlerFunc(a.handleWalletSync)))
	mux.Handle("POST /v1/wallet/import", a.withAuth(http.HandlerFunc(a.handleWalletImport)))
	mux.Handle("POST /v1/wallet/fund-desktop", a.withOperatorAuth(http.HandlerFunc(a.handleWalletFundDesktop)))
	mux.Handle("POST /v1/wallet/send", a.withOperatorAuth(http.HandlerFunc(a.handleWalletSend)))
	mux.Handle("POST /v1/wallet/sweep", a.withOperatorAuth(http.HandlerFunc(a.handleWalletSweep)))
	mux.Handle("POST /v1/wallet/consolidate", a.withOperatorAuth(http.HandlerFunc(a.handleWalletConsolidate)))
	mux.Handle("POST /v1/wallet/fuel", a.withOperatorAuth(http.HandlerFunc(a.handleWalletFuel)))

	// Optional opt-in loopback-only BRC-100 binary wire endpoint (`NEKTAR_EXPOSE_WALLET_WIRE=1`)
	// allowing local BSV tools or another local process to speak BRC-100 wire directly to Nektar's wallet.
	if a.cfg.ExposeWalletWire && a.mem.Chain().WalletInterface() != nil {
		wireHandler := wirewallet.Serve(a.mem.Chain().WalletInterface())
		protectedWire := a.withOperatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isStrictLoopbackWireRequest(r) {
				http.Error(w, "wallet wire is strictly direct loopback-only", http.StatusForbidden)
				return
			}
			if r.Method == http.MethodPost {
				ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
				if ct != "" && !strings.HasPrefix(ct, "application/octet-stream") {
					http.Error(w, "wallet wire requires application/octet-stream", http.StatusUnsupportedMediaType)
					return
				}
			}
			wireHandler.ServeHTTP(w, r)
		}))
		mux.Handle("/v1/wallet/wire/", http.StripPrefix("/v1/wallet/wire", protectedWire))
	}
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isStrictLoopbackWireRequest(r *http.Request) bool {
	if !isLoopbackRequest(r) {
		return false
	}
	// Reject any reverse-proxied or browser cross-origin requests.
	for _, hdr := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "X-Forwarded-Host", "Origin", "Referer", "Sec-Fetch-Site"} {
		if strings.TrimSpace(r.Header.Get(hdr)) != "" {
			return false
		}
	}
	if r.Host != "" && !isLoopbackHost(r.Host) {
		return false
	}
	return true
}

func (a *App) handleWalletStatus(w http.ResponseWriter, r *http.Request) {
	ch := a.mem.Chain()
	addr, _ := ch.FundAddress()
	bal, _ := ch.Balance(r.Context())
	spentToday, maxDay, maxFee, txCount := ch.SpendGuard().Stats()
	custody := "embedded"
	if ch.WalletMode() == "client" {
		custody = "bsv-desktop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"network":          ch.Network(),
		"walletMode":       ch.WalletMode(),
		"custody":          custody,
		"identityPub":      ch.IdentityPubHex(),
		"fundAddress":      addr,
		"balanceSats":      bal.SpendableSats,
		"confirmedSats":    bal.ConfirmedSats,
		"unprovenSats":     bal.UnprovenSats,
		"reservedSats":     bal.ReservedSats,
		"totalSats":        bal.TotalSats,
		"utxos":            bal.SpendableCount,
		"fuelCount":        bal.FuelCount,
		"fuelSats":         bal.FuelSats,
		"reserveCount":     bal.ReserveCount,
		"reserveSats":      bal.ReserveSats,
		"defaultCount":     bal.DefaultCount,
		"defaultSats":      bal.DefaultSats,
		"customCount":      bal.CustomCount,
		"maxUnprovenDepth": bal.MaxUnprovenDepth,
		"pendingTxCount":   bal.PendingTxCount,
		"feeRateSatPerKB":  ch.FeeRateSatPerKB(r.Context()),
		"spendGuard": map[string]any{
			"spentToday": spentToday,
			"maxPerDay":  maxDay,
			"maxFee":     maxFee,
			"txCount":    txCount,
		},
	})
}

func (a *App) handleWalletDesktopProbe(w http.ResponseWriter, r *http.Request) {
	wireURL := strings.TrimSpace(r.URL.Query().Get("wireUrl"))
	if wireURL == "" {
		wireURL = nwallet.DefaultDesktopWireURL
	}
	probe := nwallet.ProbeDesktopWallet(r.Context(), wireURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"walletMode": a.mem.Chain().WalletMode(),
		"network":    a.mem.Chain().Network(),
		"desktop":    probe,
		"patterns": map[string]string{
			"attended":   "nektar up --wallet client   (zero spending keys in Nektar; BSV Desktop holds 100% of custody)",
			"autonomous": "nektar wallet fund --from-desktop --sats 25000  |  nektar wallet sweep --to-desktop",
		},
	})
}

func (a *App) handleWalletUTXOs(w http.ResponseWriter, r *http.Request) {
	basket := strings.TrimSpace(r.URL.Query().Get("basket"))
	if basket == "" {
		basket = "all"
	}
	limit := 100
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if v, err := strconv.Atoi(lStr); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	if eng := a.mem.Chain().Engine(); eng != nil {
		outs, total, err := eng.ListUTXOs(r.Context(), basket, false, limit, 0)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"basket": basket, "total": total, "utxos": outs})
		return
	}
	// Client mode fallback via BRC-100 ListOutputs.
	uLim := uint32(limit)
	baskets := []string{basket}
	if basket == "all" {
		baskets = []string{nwallet.BasketDefault, "nektar funding"}
	}
	var res *sdkwallet.ListOutputsResult
	var err error
	qBasket := baskets[0]
	for _, b := range baskets {
		qBasket = b
		res, err = a.mem.Chain().WalletInterface().ListOutputs(r.Context(), sdkwallet.ListOutputsArgs{
			Basket: b,
			Limit:  &uLim,
		}, nwallet.DefaultDesktopOriginator)
		if err == nil {
			break
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"basket": qBasket, "total": res.TotalOutputs, "utxos": res.Outputs})
}

func (a *App) handleWalletBaskets(w http.ResponseWriter, r *http.Request) {
	ch := a.mem.Chain()
	custody := "embedded"
	if ch.WalletMode() == "client" {
		custody = "bsv-desktop"
	}
	baskets := ch.ListAssetBaskets(r.Context())
	var totalItems int
	var totalLockedSats uint64
	for _, b := range baskets {
		totalItems += b.Count
		totalLockedSats += b.TotalSats
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"walletMode":      ch.WalletMode(),
		"custody":         custody,
		"network":         ch.Network(),
		"totalItems":      totalItems,
		"totalLockedSats": totalLockedSats,
		"baskets":         baskets,
	})
}

func (a *App) handleWalletActions(w http.ResponseWriter, r *http.Request) {
	limit := 25
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if v, err := strconv.Atoi(lStr); err == nil && v > 0 && v <= 500 {
			limit = v
		}
	}
	agentFilter := nwallet.SanitizeAgentID(r.URL.Query().Get("agent"))
	if eng := a.mem.Chain().Engine(); eng != nil {
		txs, total, err := eng.ListRecentActions(r.Context(), limit, 0)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": total, "actions": txs})
		return
	}
	uLim := uint32(limit)
	labels := []string{"nektar"}
	if agentFilter != "" {
		labels = append(labels, "agent:"+agentFilter)
	}
	incLabels := true
	args := sdkwallet.ListActionsArgs{
		Labels:        labels,
		IncludeLabels: &incLabels,
		Limit:         &uLim,
	}
	if len(labels) > 1 {
		args.LabelQueryMode = sdkwallet.QueryModeAll
	}
	res, err := a.mem.Chain().WalletInterface().ListActions(r.Context(), args, nwallet.DefaultDesktopOriginator)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": res.TotalActions, "actions": res.Actions})
}
