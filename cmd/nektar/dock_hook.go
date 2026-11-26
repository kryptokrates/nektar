package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/server"
)

type dockClientResult struct {
	Client  string   `json:"client"`
	Label   string   `json:"label"`
	Level   string   `json:"level"`
	Changed bool     `json:"changed"`
	Files   []string `json:"files,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// cmdDock detects installed AI agent clients, merges non-clobbering L1/L2/L3 Nektar config
// (MCP, hooks, SKILL.md, native Hermes/OpenClaw plugins, or L0 Proxy env), and runs a live `recall {}` probe.
func cmdDock(args []string) error {
	fs := flag.NewFlagSet("dock", flag.ExitOnError)
	target := fs.String("client", "auto", "Target client(s): auto|claude|claude-desktop|cursor|windsurf|vscode|zed|cline|continue|opencode|gemini|goose|codex|hermes|openclaw|proxy")
	dry := fs.Bool("dry", false, "Preview changes without writing files")
	_ = fs.Bool("write", true, "Write changes directly (default true; pass --dry to preview)")
	uninstall := fs.Bool("uninstall", false, "Remove Nektar entries cleanly while leaving sibling configs untouched")
	urlFlag := fs.String("url", "", "Override daemon HTTP base URL")
	tokFlag := fs.String("token", "", "Override bearer token")
	shardFlag := fs.String("shard", "", "Optional default shard hint for L0 proxy")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	var targets []string
	if fs.NArg() > 0 {
		for _, a := range fs.Args() {
			targets = append(targets, strings.ToLower(strings.TrimSpace(a)))
		}
	} else {
		targets = []string{strings.ToLower(strings.TrimSpace(*target))}
	}

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	liveURL := "http://127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	var liveInfo *server.DaemonInfo
	tok := strings.TrimSpace(*tokFlag)
	if info, ok := server.DetectLiveDaemon(dir); ok {
		liveInfo = info
		liveURL = strings.TrimRight(info.URL, "/")
		if tok == "" {
			tok = info.AuthToken
		}
	}
	if strings.TrimSpace(*urlFlag) != "" {
		liveURL = strings.TrimRight(strings.TrimSpace(*urlFlag), "/")
	}
	if tok == "" {
		tok = envOr("NEKTAR_HTTP_TOKEN", os.Getenv("NEKTAR_API_TOKEN"))
	}
	exePath, err := os.Executable()
	if err != nil {
		exePath = "nektar"
	}
	home, _ := os.UserHomeDir()
	mcpURL := liveURL + "/mcp"

	type jsonSpec struct {
		ID      string
		Label   string
		Path    string
		RootKey string // "mcpServers" | "servers" | "context_servers" | "mcp"
		Mode    string // "stdio" | "http" | "opencode" | "gemini"
	}
	jsonSpecs := []jsonSpec{
		{ID: "claude", Label: "Claude Code", Path: filepath.Join(home, ".claude.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "claude-desktop", Label: "Claude Desktop", Path: claudeDesktopPath(home), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "cursor", Label: "Cursor", Path: filepath.Join(home, ".cursor", "mcp.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "windsurf", Label: "Windsurf", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "vscode", Label: "VS Code", Path: vscodeMCPPath(home), RootKey: "servers", Mode: "http"},
		{ID: "zed", Label: "Zed", Path: zedSettingsPath(home), RootKey: "context_servers", Mode: "stdio"},
		{ID: "cline", Label: "Cline", Path: clineMCPPath(home), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "continue", Label: "Continue", Path: filepath.Join(home, ".continue", "mcpServers", "nektar.json"), RootKey: "mcpServers", Mode: "stdio"},
		{ID: "opencode", Label: "OpenCode", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), RootKey: "mcp", Mode: "opencode"},
		{ID: "gemini", Label: "Gemini CLI", Path: filepath.Join(home, ".gemini", "settings.json"), RootKey: "mcpServers", Mode: "gemini"},
	}

	selected := make(map[string]bool)
	explicit := false
	for _, t := range targets {
		if t == "" || t == "auto" {
			continue
		}
		explicit = true
		selected[t] = true
	}

	var results []dockClientResult
	showProxy := selected["proxy"]

	for _, s := range jsonSpecs {
		_, dirErr := os.Stat(filepath.Dir(s.Path))
		detected := dirErr == nil
		if explicit && !selected[s.ID] {
			continue
		}
		if !explicit && !detected {
			continue
		}
		res := dockClientResult{Client: s.ID, Label: s.Label, Level: "L1", Files: []string{s.Path}}
		var entry any
		switch s.Mode {
		case "http":
			e := map[string]any{"type": "http", "url": mcpURL}
			if tok != "" {
				e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
			}
			entry = e
		case "opencode":
			e := map[string]any{"type": "remote", "url": mcpURL, "enabled": true}
			if tok != "" {
				e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
			}
			entry = e
		case "gemini":
			e := map[string]any{"httpUrl": mcpURL}
			if tok != "" {
				e["headers"] = map[string]string{"Authorization": "Bearer " + tok}
			}
			entry = e
		default:
			entry = map[string]any{"command": exePath, "args": []string{"mcp"}}
		}
		changed, mErr := mergeOrRemoveJSONEntry(s.Path, s.RootKey, "nektar", entry, *dry, *uninstall)
		if mErr != nil {
			res.Notes = append(res.Notes, "error: "+mErr.Error())
		} else {
			res.Changed = changed
			res.Notes = append(res.Notes, s.Path)
		}
		if s.ID == "claude" {
			res.Level = "L3"
			hPath := filepath.Join(home, ".claude", "settings.json")
			if hChanged, hErr := wireClaudeHooks(hPath, exePath, *dry, *uninstall); hErr == nil && hChanged {
				res.Changed = true
				res.Files = append(res.Files, hPath)
				res.Notes = append(res.Notes, "hooks -> "+hPath)
			}
			skillDst := filepath.Join(home, ".claude", "skills", "nektar-memory", "SKILL.md")
			if sChanged, sErr := installSkillFile(skillDst, liveURL, *dry, *uninstall); sErr == nil && sChanged {
				res.Changed = true
				res.Files = append(res.Files, skillDst)
				res.Notes = append(res.Notes, "skill -> "+skillDst)
			}
		}
		results = append(results, res)
	}

	// Codex CLI (~/.codex/config.toml)
	codexDir := filepath.Join(home, ".codex")
	if (explicit && selected["codex"]) || (!explicit && dirExists(codexDir)) {
		cfg := filepath.Join(codexDir, "config.toml")
		changed, cErr := wireCodexToml(cfg, exePath, *dry, *uninstall)
		r := dockClientResult{Client: "codex", Label: "Codex CLI", Level: "L2", Changed: changed, Files: []string{cfg}}
		if cErr != nil {
			r.Notes = append(r.Notes, "error: "+cErr.Error())
		} else {
			r.Notes = append(r.Notes, cfg)
		}
		results = append(results, r)
	}

	// Goose (~/.config/goose/config.yaml)
	gooseDir := filepath.Join(home, ".config", "goose")
	if (explicit && selected["goose"]) || (!explicit && dirExists(gooseDir)) {
		cfg := filepath.Join(gooseDir, "config.yaml")
		changed, gErr := wireGooseYaml(cfg, exePath, *dry, *uninstall)
		r := dockClientResult{Client: "goose", Label: "Goose", Level: "L1", Changed: changed, Files: []string{cfg}}
		if gErr != nil {
			r.Notes = append(r.Notes, "error: "+gErr.Error())
		} else {
			r.Notes = append(r.Notes, cfg)
		}
		results = append(results, r)
	}

	// Hermes Agent (~/.hermes) — L3 native MemoryProvider plugin + skill
	hermesDir := filepath.Join(home, ".hermes")
	if (explicit && selected["hermes"]) || (!explicit && dirExists(hermesDir)) {
		r := dockNativeBundle("hermes", "Hermes Agent", filepath.Join(hermesDir, "plugins", "hermes-memory-nektar"), liveURL, *dry, *uninstall)
		results = append(results, r)
	}

	// OpenClaw (~/.openclaw) — L3 native memory slot plugin
	ocDir := filepath.Join(home, ".openclaw")
	if (explicit && selected["openclaw"]) || (!explicit && dirExists(ocDir)) {
		r := dockNativeBundle("openclaw", "OpenClaw", filepath.Join(ocDir, "plugins", "nektar"), liveURL, *dry, *uninstall)
		results = append(results, r)
	}

	if showProxy {
		results = append(results, dockClientResult{
			Client: "proxy",
			Label:  "L0 LLM Memory Proxy",
			Level:  "L0",
			Notes:  []string{liveURL + "/v1/proxy/openai/v1", liveURL + "/v1/proxy/anthropic"},
		})
	}

	var probe map[string]any
	if !*uninstall && liveInfo != nil {
		t0 := time.Now()
		if out, err := callLiveTool(liveInfo, "recall", map[string]any{}); err == nil {
			if m, ok := out.(map[string]any); ok {
				probe = map[string]any{"ok": true, "ttfcMs": time.Since(t0).Milliseconds(), "count": m["count"], "shards": m["shards"]}
			}
		} else {
			probe = map[string]any{"ok": false, "note": err.Error()}
		}
	}

	if *asJSON {
		return printJSON(map[string]any{
			"dry":       *dry,
			"uninstall": *uninstall,
			"url":       liveURL,
			"mcpUrl":    mcpURL,
			"clients":   results,
			"probe":     probe,
		})
	}

	modeTag := ""
	if *dry {
		modeTag = " [dry]"
	}
	if *uninstall {
		modeTag += " [uninstall]"
	}
	fmt.Printf("Nektar v%s dock%s — binary: %s · daemon: %s\n", Version, modeTag, exePath, liveURL)
	fmt.Println("Docking wires your AI apps into this Nektar node so they share one encrypted, verifiable memory & BSV market.")
	fmt.Println()
	tierName := func(lvl string) string {
		switch lvl {
		case "L3":
			return "Native + Hooks"
		case "L2":
			return "MCP + Hooks"
		case "L1":
			return "MCP Tools"
		case "L0":
			return "Zero-Code Proxy"
		default:
			return lvl
		}
	}
	if len(results) == 0 {
		fmt.Printf("  No AI clients auto-detected in %s.\n", home)
		fmt.Println("  Pass an explicit app name: `nektar dock claude cursor claude-desktop vscode codex hermes openclaw proxy`")
	} else {
		for _, r := range results {
			state := "already docked"
			if r.Changed {
				if *dry {
					state = "would update"
				} else if *uninstall {
					state = "removed"
				} else {
					state = "docked ✓"
				}
			}
			fmt.Printf("  [%-14s] %-16s (%s)\n", tierName(r.Level), r.Label, state)
			for _, n := range r.Notes {
				fmt.Printf("                    %s\n", n)
			}
		}
	}

	if showProxy || len(results) == 0 {
		shardHint := *shardFlag
		if shardHint == "" {
			shardHint = "welcome"
		}
		keyHint := "sk-..."
		antHint := "sk-ant-..."
		if tok != "" {
			keyHint = "nkt:" + tok + ":sk-..."
			antHint = "nkt:" + tok + ":sk-ant-..."
		}
		fmt.Printf(`
Zero-Code LLM Memory Proxy (works with ANY OpenAI/Anthropic agent):
  export OPENAI_BASE_URL="%s/v1/proxy/openai/v1"
  export OPENAI_API_KEY="%s"
  export ANTHROPIC_BASE_URL="%s/v1/proxy/anthropic"
  export ANTHROPIC_API_KEY="%s"
  # Optional headers: X-Nektar-Shard: %s | X-Nektar-Capture: 1 | X-Nektar-Recall: 0
`, liveURL, keyHint, liveURL, antHint, shardHint)
	}

	if !*uninstall {
		if probe != nil && probe["ok"] == true {
			fmt.Printf("\nLive probe: ✓ PASS — recall {} answered in %vms (count=%v, shards=%v)\n", probe["ttfcMs"], probe["count"], probe["shards"])
		} else if probe != nil {
			fmt.Printf("\nLive probe: ! WARN — daemon reachable but recall failed: %v\n", probe["note"])
		} else {
			fmt.Println("\nLive probe: • daemon not running yet — start with `nektar serve` (or `nektar serve --network main --confirm I_UNDERSTAND`)")
		}
		if len(results) > 0 && !*dry {
			fmt.Println("\nNext:")
			fmt.Println("  1. Restart your docked AI app (Claude / Cursor / VS Code / Codex…) to pick up the config.")
			fmt.Println(`  2. Try asking it: "Remember that our production API runs on port 8080, then recall it with a verified receipt."`)
			fmt.Println("  3. Revert anytime with: `nektar dock --uninstall`")
		}
	}
	return nil
}

var receiptRe = regexp.MustCompile(`nektarr1\.[a-zA-Z0-9_-]{20,}`)

// cmdHook provides native zero-Node.js Claude Code / Codex hooks:
//   - `nektar hook session-start`: prints verified memory summary to stdout
//   - `nektar hook prefetch`: reads user prompt from stdin, recalls matching fragments
//   - `nektar hook verify-receipt [--codex]`: extracts any nektarr1.* token and blocks once on invalid/red receipts
func cmdHook(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar hook [session-start | prefetch | verify-receipt]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	codexMode := fs.Bool("codex", false, "Codex CLI notify hook formatting")
	verbose := fs.Bool("verbose", false, "Log hook diagnostics to stderr")
	_ = fs.Parse(args[1:])
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}

	switch sub {
	case "session-start":
		if info, ok := server.DetectLiveDaemon(dir); ok {
			if out, err := callLiveTool(info, "recall", map[string]any{"limit": 8}); err == nil {
				b, _ := json.Marshal(out)
				fmt.Printf("[nektar session-start memory] %s\n", string(b))
				return nil
			}
		}
		ctx := context.Background()
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return nil
		}
		defer rt.closeFn()
		_, _ = rt.mem.EnsureWelcome(ctx)
		shards, items, _ := rt.mem.RecallAllShards(ctx, memory.RecallInput{Limit: 8})
		b, _ := json.Marshal(map[string]any{"shards": shards, "items": items})
		fmt.Printf("[nektar session-start memory] %s\n", string(b))
		return nil

	case "prefetch":
		stdinBytes, _ := io.ReadAll(io.LimitReader(os.Stdin, 256*1024))
		var payload struct {
			Prompt string `json:"prompt"`
			Query  string `json:"query"`
		}
		_ = json.Unmarshal(stdinBytes, &payload)
		q := strings.TrimSpace(payload.Prompt)
		if q == "" {
			q = strings.TrimSpace(payload.Query)
		}
		if q == "" {
			q = strings.TrimSpace(string(stdinBytes))
		}
		if info, ok := server.DetectLiveDaemon(dir); ok {
			if out, err := callLiveTool(info, "recall", map[string]any{"query": q, "limit": 6}); err == nil {
				b, _ := json.Marshal(out)
				fmt.Printf("[nektar prefetch] %s\n", string(b))
				return nil
			}
		} else if *verbose {
			fmt.Fprintln(os.Stderr, "[nektar prefetch] no running daemon — start `nektar serve`")
		}
		return nil

	case "verify-receipt":
		stdinBytes, _ := io.ReadAll(io.LimitReader(os.Stdin, 512*1024))
		// Prevent infinite Stop-hook loops when Claude Code retries after a block.
		var hookEnv struct {
			StopHookActive bool `json:"stop_hook_active"`
		}
		if json.Unmarshal(stdinBytes, &hookEnv) == nil && hookEnv.StopHookActive {
			return nil
		}
		text := string(stdinBytes)
		if fs.NArg() > 0 {
			text += " " + strings.Join(fs.Args(), " ")
		}
		rawTokens := receiptRe.FindAllString(text, -1)
		if len(rawTokens) == 0 {
			return nil
		}
		seenTok := make(map[string]bool, len(rawTokens))
		tokens := make([]string, 0, len(rawTokens))
		for _, t := range rawTokens {
			if !seenTok[t] {
				seenTok[t] = true
				tokens = append(tokens, t)
			}
		}
		info, live := server.DetectLiveDaemon(dir)
		var failedReasons []string
		results := make([]any, 0, len(tokens))
		for _, tok := range tokens {
			var res any
			var vErr error
			if live {
				res, vErr = callLiveTool(info, "verifyReceipt", map[string]any{"token": tok})
			} else {
				ctx := context.Background()
				rt, rErr := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
				if rErr != nil {
					vErr = fmt.Errorf("open runtime: %w", rErr)
				} else {
					res, vErr = rt.mem.VerifyReceipt(ctx, tok)
					rt.closeFn()
				}
			}
			if vErr != nil {
				failedReasons = append(failedReasons, fmt.Sprintf("%s: verification error (%v)", tok, vErr))
				continue
			}
			if res == nil {
				failedReasons = append(failedReasons, fmt.Sprintf("%s: empty verification response", tok))
				continue
			}
			results = append(results, res)
			b, mErr := json.Marshal(res)
			if mErr != nil {
				failedReasons = append(failedReasons, fmt.Sprintf("%s: marshal verification response: %v", tok, mErr))
				continue
			}
			var v struct {
				OK      bool   `json:"ok"`
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
			}
			if uErr := json.Unmarshal(b, &v); uErr != nil {
				failedReasons = append(failedReasons, fmt.Sprintf("%s: invalid verification payload (%v)", tok, uErr))
				continue
			}
			if !v.OK || v.Verdict == "red" {
				reason := v.Reason
				if reason == "" {
					reason = "verdict=" + v.Verdict
				}
				failedReasons = append(failedReasons, fmt.Sprintf("%s: %s", tok, reason))
			}
		}
		if len(failedReasons) > 0 {
			if !*codexMode {
				return printJSON(map[string]any{
					"decision": "block",
					"reason":   "Nektar receipt verification failed: " + strings.Join(failedReasons, "; ") + " — re-ground via groundedAnswer or state answer is ungrounded.",
					"verified": results,
				})
			}
			return fmt.Errorf("nektar receipt verification failed: %s", strings.Join(failedReasons, "; "))
		}
		return printJSON(map[string]any{"verified": results})

	default:
		return fmt.Errorf("unknown hook subcommand %q (use session-start, prefetch, or verify-receipt)", sub)
	}
}
