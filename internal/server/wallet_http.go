package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
		"network":         ch.Network(),
		"walletMode":      ch.WalletMode(),
		"custody":         custody,
		"identityPub":     ch.IdentityPubHex(),
		"fundAddress":     addr,
		"balanceSats":     bal.SpendableSats,
		"confirmedSats":   bal.ConfirmedSats,
		"unprovenSats":    bal.UnprovenSats,
		"reservedSats":    bal.ReservedSats,
		"totalSats":       bal.TotalSats,
		"utxos":           bal.SpendableCount,
		"fuelCount":       bal.FuelCount,
		"fuelSats":        bal.FuelSats,
		"reserveCount":    bal.ReserveCount,
		"reserveSats":     bal.ReserveSats,
		"defaultCount":    bal.DefaultCount,
		"defaultSats":     bal.DefaultSats,
		"customCount":     bal.CustomCount,
		"maxUnprovenDepth": bal.MaxUnprovenDepth,
		"pendingTxCount":  bal.PendingTxCount,
		"feeRateSatPerKB": ch.FeeRateSatPerKB(r.Context()),
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

func (a *App) handleWalletSync(w http.ResponseWriter, r *http.Request) {
	rep, sErr := a.mem.Chain().SyncFull(r.Context())
	upgraded, _ := a.mem.RefreshAnchorProofs(r.Context())
	bal, _ := a.mem.Chain().Balance(r.Context())
	if sErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":              false,
			"error":           sErr.Error(),
			"report":          rep,
			"balanceSats":     bal.SpendableSats,
			"utxos":           bal.SpendableCount,
			"upgradedAnchors": upgraded,
		})
		return
	}
	a.BroadcastDashboardEvent("wallet_sync")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"added":           rep.AddedOutputs,
		"proved":          rep.ProvedTxs,
		"sentWaiting":     rep.SentWaiting,
		"abortedStale":    rep.AbortedStale,
		"prunedSpent":     rep.PrunedSpent,
		"upgradedAnchors": upgraded,
		"balanceSats":     bal.SpendableSats,
		"confirmedSats":   bal.ConfirmedSats,
		"unprovenSats":    bal.UnprovenSats,
		"utxos":           bal.SpendableCount,
	})
}

func (a *App) handleWalletImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TxID              string `json:"txid,omitempty"`
		BEEFHex           string `json:"beefHex,omitempty"`
		OutputIndex       uint32 `json:"outputIndex,omitempty"`
		DerivationPrefix  string `json:"derivationPrefix,omitempty"`
		DerivationSuffix  string `json:"derivationSuffix,omitempty"`
		SenderIdentityKey string `json:"senderIdentityKey,omitempty"`
		Description       string `json:"description,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.BEEFHex != "" {
		raw, err := hex.DecodeString(strings.TrimSpace(in.BEEFHex))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid beefHex: " + err.Error()})
			return
		}
		// BRC-29 wallet payment internalization if remittance fields are present!
		if in.DerivationPrefix != "" && in.DerivationSuffix != "" && in.SenderIdentityKey != "" {
			desc := in.Description
			if desc == "" {
				desc = "imported BRC-29 payment"
			}
			sats, err := a.mem.Chain().InternalizeBRC29(
				r.Context(), raw, in.OutputIndex,
				in.DerivationPrefix, in.DerivationSuffix, in.SenderIdentityKey,
				desc, []string{"import", "brc29"},
			)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			a.BroadcastDashboardEvent("wallet_import")
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sats": sats, "protocol": "brc29"})
			return
		}
		res, err := a.mem.Chain().ImportBEEF(r.Context(), raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("wallet_import")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "import": res})
		return
	}
	if in.TxID != "" {
		res, err := a.mem.Chain().ImportTxID(r.Context(), in.TxID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("wallet_import")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "import": res})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide beefHex or txid"})
}

func (a *App) handleWalletFundDesktop(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sats    uint64 `json:"sats"`
		WireURL string `json:"wireUrl,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.Sats == 0 {
		in.Sats = 10_000
	}
	res, err := a.mem.Chain().FundFromDesktop(r.Context(), in.WireURL, in.Sats)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("wallet_fund")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleWalletSend(w http.ResponseWriter, r *http.Request) {
	if a.mem.Chain().WalletMode() == "client" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "direct wallet sends are disabled in BSV Desktop client mode — send funds directly from BSV Desktop (Nektar holds zero signing keys)",
		})
		return
	}
	var in struct {
		ToAddress     string `json:"toAddress,omitempty"`
		ToIdentityKey string `json:"toIdentityKey,omitempty"`
		To            string `json:"to,omitempty"` // convenience alias (auto-detects P2PKH address vs 66-hex pubkey)
		Sats          uint64 `json:"sats"`
		Description   string `json:"description,omitempty"`
		Confirm       string `json:"confirm,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.mem.Chain().Mainnet() && in.Confirm != "I_UNDERSTAND" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mainnet send requires confirm=\"I_UNDERSTAND\""})
		return
	}
	toAddr, toID := resolveSendTarget(in.To, in.ToAddress, in.ToIdentityKey)
	res, err := a.mem.Chain().Send(r.Context(), nwallet.SendArgs{
		ToAddress:     toAddr,
		ToIdentityKey: toID,
		Satoshis:      in.Sats,
		Description:   in.Description,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("wallet_send")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleWalletSweep(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ToAddress     string `json:"toAddress,omitempty"`
		ToIdentityKey string `json:"toIdentityKey,omitempty"`
		To            string `json:"to,omitempty"`
		ToDesktop     bool   `json:"toDesktop,omitempty"`
		WireURL       string `json:"wireUrl,omitempty"`
		Confirm       string `json:"confirm,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.mem.Chain().Mainnet() && in.Confirm != "I_UNDERSTAND" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mainnet sweep requires confirm=\"I_UNDERSTAND\""})
		return
	}
	if in.ToDesktop {
		res, err := a.mem.Chain().SweepToDesktop(r.Context(), in.WireURL, true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("wallet_sweep")
		writeJSON(w, http.StatusOK, res)
		return
	}
	toAddr, toID := resolveSendTarget(in.To, in.ToAddress, in.ToIdentityKey)
	res, err := a.mem.Chain().Sweep(r.Context(), nwallet.SweepArgs{
		ToAddress:     toAddr,
		ToIdentityKey: toID,
		Description:   "operator wallet sweep",
		OverrideGuard: true, // operator retrieving own treasury funds
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("wallet_sweep")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleWalletConsolidate(w http.ResponseWriter, r *http.Request) {
	var in nwallet.ConsolidateArgs
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in)
	res, err := a.mem.Chain().Consolidate(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("wallet_consolidate")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleWalletFuel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target int    `json:"target"`
		Sats   uint64 `json:"sats"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in)
	if in.Target <= 0 {
		in.Target = 24
	}
	if in.Sats == 0 {
		in.Sats = 2000
	}
	eng := a.mem.Chain().Engine()
	if eng == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fuel fan-out requires embedded wallet mode"})
		return
	}
	minted, err := eng.FuelKeeper().WarmFuel(r.Context(), in.Target, in.Sats)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	bal, _ := eng.Balance(r.Context())
	actualDenom := eng.FuelDenom()
	if actualDenom == 0 {
		actualDenom = in.Sats
	}
	a.BroadcastDashboardEvent("wallet_fuel")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"minted":    minted,
		"fuelSats":  actualDenom,
		"fuelCount": bal.FuelCount,
		"utxos":     bal.SpendableCount,
		"detail":    fmt.Sprintf("minted %d fuel UTXO(s) of %d sats", minted, actualDenom),
	})
}

func resolveSendTarget(to, toAddress, toIdentityKey string) (addr, idKey string) {
	if toAddress != "" || toIdentityKey != "" {
		return strings.TrimSpace(toAddress), strings.TrimSpace(toIdentityKey)
	}
	t := strings.TrimSpace(to)
	if len(t) == 66 && (strings.HasPrefix(t, "02") || strings.HasPrefix(t, "03")) {
		if _, err := hex.DecodeString(t); err == nil {
			return "", t
		}
	}
	return t, ""
}
