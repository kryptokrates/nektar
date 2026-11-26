package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/dock"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// DockClientStatus describes one AI client detected (or dockable) on this machine.
type DockClientStatus = dock.ClientStatus

func (a *App) registerDockAndLaunchRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/dock/detect", a.withAuth(http.HandlerFunc(a.handleDockDetect)))
	mux.Handle("POST /v1/dock/apply", a.withOperatorAuth(http.HandlerFunc(a.handleDockApply)))
	mux.Handle("POST /v1/dock/probe", a.withAuth(http.HandlerFunc(a.handleDockProbe)))
	mux.Handle("GET /v1/wallet/qr", a.withAuth(http.HandlerFunc(a.handleWalletQR)))
	mux.Handle("GET /v1/hub/inspect", a.withAuth(http.HandlerFunc(a.handleHubInspect)))
	mux.Handle("GET /v1/hubs/directory", a.withAuth(http.HandlerFunc(a.handleHubsDirectory)))
	mux.Handle("POST /v1/hubs/connect-all", a.withOperatorAuth(http.HandlerFunc(a.handleHubsConnectAll)))
}

func isJSONEntryDocked(cfgPath, rootKey, entryKey string) bool {
	return dock.IsJSONEntryDocked(cfgPath, rootKey, entryKey)
}

func dockMergeJSON(cfgPath, rootKey, entryKey string, val any, uninstall bool) (bool, error) {
	return dock.MergeOrRemoveJSONEntry(cfgPath, rootKey, entryKey, val, false, uninstall)
}

func detectAllDockClients(home string) []DockClientStatus {
	return dock.DetectAll(home)
}

func (a *App) handleDockDetect(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	base := requestBaseURL(r)
	clients := detectAllDockClients(home)
	detectedCount := 0
	dockedCount := 0
	for _, c := range clients {
		if c.Detected {
			detectedCount++
		}
		if c.Docked {
			dockedCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clients":        clients,
		"detectedCount":  detectedCount,
		"dockedCount":    dockedCount,
		"containerMode":  IsContainerEnv(),
		"mcpUrl":         base + "/mcp",
		"openaiProxy":    base + "/v1/proxy/openai/v1",
		"anthropicProxy": base + "/v1/proxy/anthropic",
		"a2aCard":        base + "/.well-known/agent.json",
		"samplePrompt":   "Remember that our production Postgres database runs version 16 on port 5432, then recall it with a verified receipt.",
	})
}

func (a *App) handleDockApply(w http.ResponseWriter, r *http.Request) {
	if IsContainerEnv() {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "containerized daemon cannot edit host IDE configs — run `nektar dock` on your host machine or use the L0 LLM Proxy / MCP URL",
		})
		return
	}
	var in struct {
		Client    string `json:"client"` // "auto" | client id
		Uninstall bool   `json:"uninstall"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	targetID := strings.ToLower(strings.TrimSpace(in.Client))
	if targetID == "" {
		targetID = "auto"
	}
	home, _ := os.UserHomeDir()
	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		exePath = "nektar"
	}
	base := requestBaseURL(r)
	results := dock.Apply(dock.Options{
		Home:      home,
		ExePath:   exePath,
		LiveURL:   base,
		Token:     a.cfg.APIToken,
		Uninstall: in.Uninstall,
	}, []string{targetID})

	var updated []string
	var errs []string
	for _, res := range results {
		if res.Error != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", res.Label, res.Error))
		} else if res.Changed {
			updated = append(updated, res.Label)
		}
	}
	a.BroadcastDashboardEvent("dock_updated")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       len(errs) == 0,
		"updated":  updated,
		"errors":   errs,
		"results":  results,
		"clients":  detectAllDockClients(home),
		"nextStep": "Restart your docked AI app (Claude, Codex, Hermes, OpenClaw, Cursor, VS Code…) to load Nektar, then try the sample prompt.",
	})
}

func (a *App) handleDockProbe(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	ctx := r.Context()
	_, _ = a.mem.EnsureWelcome(ctx)
	shards, items, _ := a.mem.RecallAllShards(ctx, memory.RecallInput{Limit: 8})

	out := map[string]any{
		"ok":           true,
		"shards":       shards,
		"count":        len(items),
		"network":      a.mem.Chain().Network(),
		"truthNote":    reasoning.TruthNote,
		"samplePrompt": "Remember that our production Postgres database runs version 16 on port 5432, then recall it with a verified receipt.",
	}
	// Run a real live groundedAnswer + verifyReceipt check so users immediately experience Magic Moment #1!
	if len(items) > 0 {
		first := items[0]
		snippet := strings.TrimSpace(first.Text)
		if len(snippet) > 90 {
			snippet = snippet[:90]
		}
		if ga, gErr := a.mem.GroundedAnswer(ctx, memory.GroundedAnswerInput{
			Claims: []reasoning.Claim{
				{
					Text:  snippet,
					Cites: []reasoning.FragmentRef{{Shard: first.Shard, Seq: first.Seq}},
				},
			},
		}); gErr == nil && ga != nil && ga.ReceiptToken != "" {
			a.recordTTFV()
			out["receiptToken"] = ga.ReceiptToken
			out["verifyUrl"] = ga.VerifyURL
			if ver, vErr := a.mem.VerifyReceipt(ctx, ga.ReceiptToken); vErr == nil && ver != nil {
				out["verdict"] = ver.Verdict
				out["verified"] = ver.OK
			}
		}
	}
	out["ttfcMs"] = time.Since(t0).Milliseconds()
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleWalletQR(w http.ResponseWriter, r *http.Request) {
	addr, err := a.mem.Chain().FundAddress()
	if err != nil || addr == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "wallet funding address unavailable"})
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	payload := "bitcoin:" + addr
	if mode == "raw" {
		payload = addr
	}
	svg, err := core.RenderQRSVG(payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		writeJSON(w, http.StatusOK, map[string]any{
			"network": a.mem.Chain().Network(),
			"address": addr,
			"uri":     "bitcoin:" + addr,
			"payload": payload,
			"svg":     svg,
		})
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(svg))
}

func (a *App) handleHubInspect(w http.ResponseWriter, r *http.Request) {
	rawURL := strings.TrimRight(strings.TrimSpace(r.URL.Query().Get("url")), "/")
	if rawURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url query parameter is required"})
		return
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}
	baseRoot := strings.TrimSuffix(rawURL, "/hub")
	hubBase := baseRoot + "/hub"
	hc := core.SafeHTTPClientForNetwork(a.mem.Chain().Network(), 8*time.Second)

	res := map[string]any{
		"url":       hubBase,
		"reachable": false,
	}
	// 1. Probe /.well-known/nektar
	if resp, err := hc.Get(baseRoot + "/.well-known/nektar"); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var wk map[string]any
			if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&wk) == nil {
				res["reachable"] = true
				res["network"] = wk["network"]
				res["pub"] = wk["pub"]
				res["publicUrl"] = wk["publicUrl"]
			}
		}
	}
	// 2. Probe /hub/v1/terms (role + fee schedule)
	if resp, err := hc.Get(hubBase + "/v1/terms"); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var terms map[string]any
			if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&terms) == nil {
				res["reachable"] = true
				res["hubRole"] = terms["hubRole"]
				res["feePreset"] = terms["feePreset"]
				res["fees"] = terms["fees"]
				res["egress"] = terms["egress"]
				res["bountyTaper"] = terms["bountyTaper"]
			}
		}
	} else if res["reachable"] == false {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleHubsDirectory(w http.ResponseWriter, r *http.Request) {
	if a.disc == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"network":        a.mem.Chain().Network(),
			"hubs":           []any{},
			"connectedCount": 0,
			"totalCount":     0,
		})
		return
	}
	force := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"
	hubs := a.disc.ProbeHubCatalog(r.Context(), force)
	connectedCount := 0
	for _, h := range hubs {
		if h.Connected {
			connectedCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"network":        a.mem.Chain().Network(),
		"hubs":           hubs,
		"connectedCount": connectedCount,
		"totalCount":     len(hubs),
	})
}

func (a *App) handleHubsConnectAll(w http.ResponseWriter, r *http.Request) {
	if a.disc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "discovery service unavailable"})
		return
	}
	connected, skipped, hubs := a.disc.ConnectAllReachable(r.Context())
	if len(connected) > 0 {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_, _ = a.disc.DiscoverPeers(bgCtx, connected)
			a.BroadcastDashboardEvent("hosts_updated")
		}()
	}
	a.BroadcastDashboardEvent("hosts_updated")
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":      connected,
		"skipped":        skipped,
		"connectedCount": len(connected),
		"hubs":           hubs,
	})
}
