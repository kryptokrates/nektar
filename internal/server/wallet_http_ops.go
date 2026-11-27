package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

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
