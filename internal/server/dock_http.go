package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
)

// DockClientStatus describes one AI client detected (or dockable) on this machine.
type DockClientStatus struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Tier       string `json:"tier"`      // L1 | L2 | L3
	TierLabel  string `json:"tierLabel"` // plain-English description
	Summary    string `json:"summary"`
	ConfigPath string `json:"configPath"`
	Detected   bool   `json:"detected"`
	Docked     bool   `json:"docked"`
	OneClick   bool   `json:"oneClick"`
	Note       string `json:"note,omitempty"`
}

type dockJSONTarget struct {
	ID        string
	Label     string
	Tier      string
	TierLabel string
	Summary   string
	Path      string
	RootKey   string // mcpServers | servers | context_servers | mcp
	Mode      string // stdio | http | opencode | gemini
}

func (a *App) registerDockAndLaunchRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/dock/detect", a.withAuth(http.HandlerFunc(a.handleDockDetect)))
	mux.Handle("POST /v1/dock/apply", a.withOperatorAuth(http.HandlerFunc(a.handleDockApply)))
	mux.Handle("POST /v1/dock/probe", a.withAuth(http.HandlerFunc(a.handleDockProbe)))
	mux.Handle("GET /v1/wallet/qr", a.withAuth(http.HandlerFunc(a.handleWalletQR)))
	mux.Handle("GET /v1/hub/inspect", a.withAuth(http.HandlerFunc(a.handleHubInspect)))
}

func dockJSONTargets(home string) []dockJSONTarget {
	appData := strings.TrimSpace(os.Getenv("APPDATA"))
	if appData == "" {
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	claudeDesktop := filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	vscodeMCP := filepath.Join(home, ".config", "Code", "User", "mcp.json")
	zedSettings := filepath.Join(home, ".config", "zed", "settings.json")
	clineMCP := filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	switch runtime.GOOS {
	case "darwin":
		claudeDesktop = filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
		vscodeMCP = filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
		clineMCP = filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	case "windows":
		claudeDesktop = filepath.Join(appData, "Claude", "claude_desktop_config.json")
		vscodeMCP = filepath.Join(appData, "Code", "User", "mcp.json")
		zedSettings = filepath.Join(appData, "Zed", "settings.json")
		clineMCP = filepath.Join(appData, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	}
	return []dockJSONTarget{
		{ID: "claude", Label: "Claude Code", Tier: "L3", TierLabel: "MCP + Auto-Recall Hooks", Summary: "Adds Nektar MCP tools, session memory prefetch, and receipt verification.", Path: filepath.Join(home, ".claude.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "claude-desktop", Label: "Claude Desktop", Tier: "L1", TierLabel: "MCP Tools", Summary: "Gives Claude Desktop encrypted memory, recall, and knowledge trading.", Path: claudeDesktop, RootKey: "mcpServers", Mode: "stdio"},
		{ID: "cursor", Label: "Cursor", Tier: "L1", TierLabel: "MCP Tools", Summary: "Wires Nektar MCP server into Cursor for persistent verified memory.", Path: filepath.Join(home, ".cursor", "mcp.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "vscode", Label: "VS Code", Tier: "L1", TierLabel: "Streamable HTTP MCP", Summary: "Connects VS Code Copilot/MCP directly to this daemon over HTTP.", Path: vscodeMCP, RootKey: "servers", Mode: "http"},
		{ID: "windsurf", Label: "Windsurf", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP tools to Windsurf.", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "zed", Label: "Zed", Tier: "L1", TierLabel: "MCP Context Server", Summary: "Adds Nektar as a Zed context server.", Path: zedSettings, RootKey: "context_servers", Mode: "stdio"},
		{ID: "cline", Label: "Cline", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP server to Cline.", Path: clineMCP, RootKey: "mcpServers", Mode: "stdio"},
		{ID: "continue", Label: "Continue", Tier: "L1", TierLabel: "MCP Tools", Summary: "Adds Nektar MCP server to Continue.", Path: filepath.Join(home, ".continue", "mcpServers", "nektar.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "opencode", Label: "OpenCode", Tier: "L1", TierLabel: "Remote HTTP MCP", Summary: "Connects OpenCode directly to /mcp.", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), RootKey: "mcp", Mode: "opencode"},
		{ID: "gemini", Label: "Gemini CLI", Tier: "L1", TierLabel: "Streamable HTTP MCP", Summary: "Connects Gemini CLI to /mcp.", Path: filepath.Join(home, ".gemini", "settings.json"), RootKey: "mcpServers", Mode: "gemini"},
	}
}

func isJSONEntryDocked(cfgPath, rootKey, entryKey string) bool {
	raw, err := os.ReadFile(cfgPath)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return false
	}
	sec, _ := root[rootKey].(map[string]any)
	if sec == nil {
		return false
	}
	_, ok := sec[entryKey]
	return ok
}

func dockMergeJSON(cfgPath, rootKey, entryKey string, val any, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(cfgPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, fmt.Errorf("parse %s: %w", filepath.Base(cfgPath), jErr)
		}
	} else if uninstall {
		return false, nil
	}
	sec, _ := root[rootKey].(map[string]any)
	if uninstall {
		if sec == nil {
			return false, nil
		}
		if _, ok := sec[entryKey]; !ok {
			return false, nil
		}
		delete(sec, entryKey)
		root[rootKey] = sec
	} else {
		if sec == nil {
			sec = make(map[string]any)
		}
		sec[entryKey] = val
		root[rootKey] = sec
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if bytes.Equal(raw, out) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return false, err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, cfgPath)
}

func dockEntryPayload(t dockJSONTarget, exePath, mcpURL, tok string) any {
	switch t.Mode {
	case "http":
		e := map[string]any{"type": "http", "url": mcpURL}
		if tok != "" {
			e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
		return e
	case "opencode":
		e := map[string]any{"type": "remote", "url": mcpURL, "enabled": true}
		if tok != "" {
			e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
		return e
	case "gemini":
		e := map[string]any{"httpUrl": mcpURL}
		if tok != "" {
			e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
		}
		return e
	default:
		return map[string]any{"command": exePath, "args": []string{"mcp"}}
	}
}

func detectAllDockClients(home string) []DockClientStatus {
	var out []DockClientStatus
	for _, t := range dockJSONTargets(home) {
		_, dirErr := os.Stat(filepath.Dir(t.Path))
		detected := dirErr == nil
		docked := isJSONEntryDocked(t.Path, t.RootKey, "nektar")
		out = append(out, DockClientStatus{
			ID:         t.ID,
			Label:      t.Label,
			Tier:       t.Tier,
			TierLabel:  t.TierLabel,
			Summary:    t.Summary,
			ConfigPath: t.Path,
			Detected:   detected,
			Docked:     docked,
			OneClick:   true,
		})
	}
	// Detect additional CLI-docked agents (Codex, Hermes, OpenClaw) cleanly.
	codexCfg := filepath.Join(home, ".codex", "config.toml")
	codexRaw, _ := os.ReadFile(codexCfg)
	_, codexDirErr := os.Stat(filepath.Dir(codexCfg))
	out = append(out, DockClientStatus{
		ID:         "codex",
		Label:      "Codex CLI",
		Tier:       "L2",
		TierLabel:  "MCP + Notify Hook",
		Summary:    "Run `nektar dock codex` to add Nektar MCP & receipt check to config.toml.",
		ConfigPath: codexCfg,
		Detected:   codexDirErr == nil,
		Docked:     strings.Contains(string(codexRaw), "[mcp_servers.nektar]"),
		OneClick:   false,
		Note:       "nektar dock codex",
	})

	hermesDir := filepath.Join(home, ".hermes")
	_, hermesErr := os.Stat(hermesDir)
	_, hermesPlugErr := os.Stat(filepath.Join(hermesDir, "plugins", "hermes-memory-nektar"))
	out = append(out, DockClientStatus{
		ID:         "hermes",
		Label:      "Hermes Agent",
		Tier:       "L3",
		TierLabel:  "Native MemoryProvider Slot",
		Summary:    "Run `nektar dock hermes` to install the native Python MemoryProvider plugin.",
		ConfigPath: filepath.Join(hermesDir, "plugins", "hermes-memory-nektar"),
		Detected:   hermesErr == nil,
		Docked:     hermesPlugErr == nil,
		OneClick:   false,
		Note:       "nektar dock hermes",
	})

	ocDir := filepath.Join(home, ".openclaw")
	_, ocErr := os.Stat(ocDir)
	_, ocPlugErr := os.Stat(filepath.Join(ocDir, "plugins", "nektar"))
	out = append(out, DockClientStatus{
		ID:         "openclaw",
		Label:      "OpenClaw",
		Tier:       "L3",
		TierLabel:  "Native Memory Slot",
		Summary:    "Run `nektar dock openclaw` to install the native OpenClaw memory plugin.",
		ConfigPath: filepath.Join(ocDir, "plugins", "nektar"),
		Detected:   ocErr == nil,
		Docked:     ocPlugErr == nil,
		OneClick:   false,
		Note:       "nektar dock openclaw",
	})
	return out
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
		"clients":       clients,
		"detectedCount": detectedCount,
		"dockedCount":   dockedCount,
		"containerMode": IsContainerEnv(),
		"mcpUrl":        base + "/mcp",
		"openaiProxy":   base + "/v1/proxy/openai/v1",
		"anthropicProxy": base + "/v1/proxy/anthropic",
		"a2aCard":       base + "/.well-known/agent.json",
		"samplePrompt":  "Remember that our production Postgres database runs version 16 on port 5432, then recall it with a verified receipt.",
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
	mcpURL := base + "/mcp"
	tok := a.cfg.APIToken

	var updated []string
	var errs []string
	for _, t := range dockJSONTargets(home) {
		if targetID != "auto" && targetID != t.ID {
			continue
		}
		if targetID == "auto" {
			if _, stErr := os.Stat(filepath.Dir(t.Path)); stErr != nil {
				continue
			}
		}
		entry := dockEntryPayload(t, exePath, mcpURL, tok)
		changed, mErr := dockMergeJSON(t.Path, t.RootKey, "nektar", entry, in.Uninstall)
		if mErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.Label, mErr))
		} else if changed {
			updated = append(updated, t.Label)
		}
	}
	a.BroadcastDashboardEvent("dock_updated")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       len(errs) == 0,
		"updated":  updated,
		"errors":   errs,
		"clients":  detectAllDockClients(home),
		"nextStep": "Restart your IDE / AI app to load Nektar MCP tools, then try the sample prompt.",
	})
}

func (a *App) handleDockProbe(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	shards, _ := a.mem.DB().ListShards()
	count := 0
	for _, sh := range shards {
		if frags, err := a.mem.DB().ListFragments(sh); err == nil {
			count += len(frags)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"ttfcMs":       time.Since(t0).Milliseconds(),
		"shards":       shards,
		"count":        count,
		"network":      a.mem.Chain().Network(),
		"samplePrompt": "Remember that our production Postgres database runs version 16 on port 5432, then recall it with a verified receipt.",
	})
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
			}
		}
	} else if res["reachable"] == false {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}
