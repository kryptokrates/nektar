package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/market"
)

func (a *App) handleMarketPurchase(w http.ResponseWriter, r *http.Request) {
	if a.broker == nil {
		writeJSON(w, http.StatusServiceUnavailable, market.PurchaseResponse{OK: false, Error: "market broker not attached"})
		return
	}
	var req market.PurchaseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, market.PurchaseResponse{OK: false, Error: err.Error()})
		return
	}
	resp, err := a.broker.HandlePurchase(r.Context(), req)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, chain.ErrTrackerUnavailable) {
			code = http.StatusServiceUnavailable
		} else if errors.Is(err, chain.ErrConflictDetected) {
			code = http.StatusConflict
		}
		writeJSON(w, code, market.PurchaseResponse{OK: false, OfferID: req.OfferID, Error: err.Error()})
		return
	}
	if resp != nil && resp.Status == "awaiting_confirmation" {
		a.BroadcastDashboardEvent("payment_pending")
		writeJSON(w, http.StatusAccepted, resp)
		return
	}
	a.BroadcastDashboardEvent("deal_settled")
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleJoinHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	hubURL, discovered, err := a.disc.JoinHost(r.Context(), in.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.broker != nil {
		_, _ = a.broker.AnnounceSelf(r.Context())
	}
	a.BroadcastDashboardEvent("host_joined")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": hubURL, "discovered": discovered})
}

func (a *App) handleLeaveHost(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("url"))
	if err := a.disc.LeaveHost(u); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("host_left")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": u})
}

func (a *App) handleOnboardFaucet(w http.ResponseWriter, r *http.Request) {
	addr, err := a.mem.Chain().FundAddress()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	txid, sats, err := a.toolbox.requestFaucet(r.Context(), addr, "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("faucet")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "txid": txid, "sats": sats, "address": addr})
}

func (a *App) allowFaucetIP(r *http.Request) bool {
	ip := extractClientIP(r)
	now := time.Now().Unix()
	a.faucetMu.Lock()
	defer a.faucetMu.Unlock()
	if len(a.faucetIPs) > 4096 {
		for k, times := range a.faucetIPs {
			active := false
			for _, ts := range times {
				if now-ts < 3600 {
					active = true
					break
				}
			}
			if !active {
				delete(a.faucetIPs, k)
			}
		}
	}
	prev := a.faucetIPs[ip]
	kept := prev[:0]
	for _, ts := range prev {
		if now-ts < 3600 {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= 5 {
		a.faucetIPs[ip] = kept
		return false
	}
	a.faucetIPs[ip] = append(kept, now)
	return true
}

func (a *App) handlePublicFaucet(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "faucet is only served on Hub nodes"})
		return
	}
	if !a.allowFaucetIP(r) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "faucet rate limit exceeded for this IP — try again later"})
		return
	}
	var in struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	txid, beefHex, sats, err := hub.DripFaucetBEEF(r.Context(), a.mem, in.Address, 2000, 10_000)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "txid": txid, "beefHex": beefHex, "sats": sats})
}

func (a *App) handlePolicyPreset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile     string                   `json:"profile,omitempty"` // easy | standard | secure
		Preset      string                   `json:"preset"`
		Domains     []string                 `json:"domains,omitempty"`
		MaxPerDeal  uint64                   `json:"maxPerDeal,omitempty"`
		MaxPerDay   uint64                   `json:"maxPerDay,omitempty"`
		ZeroConfMax *uint64                  `json:"zeroConfMax,omitempty"`
		Confirm     string                   `json:"confirm,omitempty"`
		AIMatching  *market.AIMatchingPolicy `json:"aiMatching,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.mem.Chain().Mainnet() && in.Confirm != "I_UNDERSTAND" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mainnet policy approval requires confirm=\"I_UNDERSTAND\""})
		return
	}
	tier := strings.TrimSpace(in.Profile)
	if tier == "" {
		tier = "standard"
	}
	prof, ok := market.PolicyProfileDefaults(tier)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown policy profile %q (use easy, standard, or secure)", in.Profile)})
		return
	}
	preset := strings.TrimSpace(in.Preset)
	if preset == "" {
		preset = prof.Preset
	}
	aiPol := in.AIMatching
	if aiPol == nil {
		cp := prof.AIMatching
		aiPol = &cp
	}
	var limits *market.PolicyLimitsOverride
	if in.MaxPerDeal > 0 || in.MaxPerDay > 0 || in.ZeroConfMax != nil || strings.TrimSpace(in.Profile) != "" {
		zc := prof.ZeroConfMax
		if in.ZeroConfMax != nil {
			zc = *in.ZeroConfMax
		}
		deal := in.MaxPerDeal
		if deal == 0 {
			deal = prof.MaxPerDeal
		}
		day := in.MaxPerDay
		if day == 0 {
			day = prof.MaxPerDay
		}
		limits = &market.PolicyLimitsOverride{
			MaxPerDeal:  deal,
			MaxPerDay:   day,
			ZeroConfMax: &zc,
		}
	}
	rec, err := market.ApprovePolicyCustom(a.mem.DB(), a.mkt.Policy(), preset, in.Domains, limits, "dashboard", aiPol)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("policy_approved")
	writeJSON(w, http.StatusOK, rec)
}

func (a *App) handleHubBundle(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "auto-ip"
	}
	net := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("network")))
	if net != "main" && net != "testnet" {
		net = "testnet"
	}
	role := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("role")))
	if role != "spine" {
		role = "leaf"
	}
	feePreset := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("feePreset")))
	if feePreset != "hybrid" && feePreset != "lightbsv_egress" && feePreset != "free" {
		if net == "main" {
			feePreset = "hybrid"
		} else {
			feePreset = "free"
		}
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		domain = "hub.example.com"
	}
	confirmFlag := ""
	confirmEnv := ""
	if net == "main" {
		confirmFlag = " --confirm I_UNDERSTAND"
		confirmEnv = " NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND"
	}
	roleFlags := fmt.Sprintf(" --hub-role %s --fee-preset %s", role, feePreset)
	roleEnv := fmt.Sprintf(" NEKTAR_HUB_ROLE=%s NEKTAR_HUB_FEE_PRESET=%s", role, feePreset)

	cliCmd := fmt.Sprintf("nektar hub up --auto-ip --network %s%s%s", net, confirmFlag, roleFlags)
	nativeCmd := fmt.Sprintf("nektar hub up --native --auto-ip --network %s%s%s", net, confirmFlag, roleFlags)
	vpsCmd := fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_NETWORK=%s%s%s NEKTAR_AUTO_IP=1 bash", net, confirmEnv, roleEnv)
	if mode == "domain" {
		cliCmd = fmt.Sprintf("nektar hub up --domain %s --network %s%s%s", domain, net, confirmFlag, roleFlags)
		nativeCmd = fmt.Sprintf("nektar hub up --native --domain %s --network %s%s%s", domain, net, confirmFlag, roleFlags)
		vpsCmd = fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_NETWORK=%s%s%s NEKTAR_DOMAIN=%s bash", net, confirmEnv, roleEnv, domain)
	} else if mode == "tunnel" {
		cliCmd = fmt.Sprintf("nektar hub up --tunnel --network %s%s%s", net, confirmFlag, roleFlags)
		nativeCmd = fmt.Sprintf("nektar serve --hub --network %s%s%s", net, confirmFlag, roleFlags)
		vpsCmd = fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_NETWORK=%s%s%s NEKTAR_TUNNEL=1 bash", net, confirmEnv, roleEnv)
	}
	localCmd := fmt.Sprintf("nektar serve --hub --network %s%s%s", net, confirmFlag, roleFlags)
	ciConfirm := ""
	if net == "main" {
		ciConfirm = "\n    export NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND"
	}
	ciMode := "export NEKTAR_AUTO_IP=1"
	if mode == "domain" {
		ciMode = "export NEKTAR_DOMAIN=" + domain
	} else if mode == "tunnel" {
		ciMode = "export NEKTAR_TUNNEL=1"
	}
	cloudInit := fmt.Sprintf(`#cloud-config
package_update: true
packages: [curl, ca-certificates, openssl]
runcmd:
  - |
    export NEKTAR_HUB_DIR=/opt/nektar-hub
    export NEKTAR_NETWORK=%s%s
    export NEKTAR_HUB_ROLE=%s
    export NEKTAR_HUB_FEE_PRESET=%s
    %s
    curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | bash
`, net, ciConfirm, role, feePreset, ciMode)
	writeJSON(w, http.StatusOK, map[string]any{
		"cliCommand":         cliCmd,
		"nativeCommand":      nativeCmd,
		"localCommand":       localCmd,
		"vpsOneLiner":        vpsCmd,
		"cloudInitYaml":      cloudInit,
		"role":               role,
		"feePreset":          feePreset,
		"coolifyTemplateUrl": "https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/templates/coolify.compose.yaml",
		"helmCommand":        "helm install nektar-hub deploy/hub/helm --set network=" + net + " --set publicUrl=https://" + domain,
	})
}

func (a *App) handleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Approve bool `json:"approve"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.broker == nil {
		appr, err := a.mem.DB().DecideApproval(id, in.Approve)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("approval_decided")
		writeJSON(w, http.StatusOK, map[string]any{"approval": appr})
		return
	}
	appr, deal, err := a.broker.ApproveAndExecute(r.Context(), id, in.Approve)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("approval_decided")
	writeJSON(w, http.StatusOK, map[string]any{"approval": appr, "deal": deal})
}
