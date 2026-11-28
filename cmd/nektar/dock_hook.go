package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/dock"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/server"
)

// cmdDock detects installed AI agent clients, merges non-clobbering L1/L2/L3 Nektar config
// (MCP, hooks, embedded SKILL.md, embedded Hermes/OpenClaw plugins, or L0 Proxy env), and runs a live receipt probe.
func cmdDock(args []string) error {
	fs := flag.NewFlagSet("dock", flag.ExitOnError)
	target := fs.String("client", "auto", "Target client(s): auto|claude|claude-desktop|antigravity|openai|cursor|windsurf|vscode|zed|cline|continue|opencode|gemini|goose|codex|hermes|openclaw|proxy")
	dry := fs.Bool("dry", false, "Preview changes without writing files")
	uninstall := fs.Bool("uninstall", false, "Remove Nektar entries cleanly while leaving sibling configs untouched")
	urlFlag := fs.String("url", "", "Override daemon HTTP base URL")
	tokFlag := fs.String("token", "", "Override bearer token")
	shardFlag := fs.String("shard", "", "Optional default shard hint for L0 proxy")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	var targets []string
	if fs.NArg() > 0 {
		for _, a := range fs.Args() {
			targets = append(targets, strings.ToLower(strings.TrimSpace(a)))
		}
	} else {
		targets = []string{strings.ToLower(strings.TrimSpace(*target))}
	}

	liveURL := "http://127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	var liveInfo *server.DaemonInfo
	tok := strings.TrimSpace(*tokFlag)
	if info, ok := detectDaemon(*dataDir, *netName); ok {
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
	if err != nil || exePath == "" {
		exePath = "nektar"
	}
	home, _ := os.UserHomeDir()
	mcpURL := liveURL + "/mcp"

	results := dock.Apply(dock.Options{
		Home:      home,
		ExePath:   exePath,
		LiveURL:   liveURL,
		Token:     tok,
		Dry:       *dry,
		Uninstall: *uninstall,
	}, targets)

	showProxy := false
	for _, t := range targets {
		if t == "proxy" || t == "openai" {
			showProxy = true
		}
	}

	var probe map[string]any
	if !*uninstall && liveInfo != nil {
		t0 := time.Now()
		if out, err := callLiveAPI(liveInfo, http.MethodPost, "/v1/dock/probe", map[string]any{}); err == nil {
			if m, ok := out.(map[string]any); ok {
				probe = m
				if _, has := probe["ttfcMs"]; !has {
					probe["ttfcMs"] = time.Since(t0).Milliseconds()
				}
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
	netTag := ""
	if liveInfo != nil && liveInfo.Network != "" {
		netTag = " · net: " + liveInfo.Network
	}
	fmt.Printf("Nektar v%s dock%s — binary: %s · daemon: %s%s\n", Version, modeTag, exePath, liveURL, netTag)
	fmt.Println("Docking wires your AI apps into this Nektar node so they share one encrypted, verifiable memory & BSV market.")
	fmt.Println()
	printDockResults(results, home, *dry, *uninstall)

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
		printDockProbe(probe)
		if len(results) > 0 && !*dry {
			fmt.Println("\nNext:")
			fmt.Println("  1. Restart your docked AI app (Claude / Codex / Hermes / OpenClaw / Cursor / VS Code…) to pick up the config.")
			fmt.Println(`  2. Try asking it: "Remember that our production API runs on port 8080, then recall it with a verified receipt."`)
			fmt.Println("  3. Revert anytime with: `nektar dock --uninstall`")
		}
	}
	return nil
}

func tierDisplayName(lvl string) string {
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

func printDockResults(results []dock.ApplyResult, home string, dry, uninstall bool) {
	if len(results) == 0 {
		fmt.Printf("  No AI clients auto-detected in %s.\n", home)
		fmt.Println("  Pass an explicit app name: `nektar dock claude codex hermes antigravity openai openclaw cursor claude-desktop vscode proxy`")
		return
	}
	for _, r := range results {
		state := "already docked"
		if r.Error != "" {
			state = "error"
		} else if r.Changed {
			if dry {
				state = "would update"
			} else if uninstall {
				state = "removed"
			} else {
				state = "docked ✓"
			}
		}
		fmt.Printf("  [%-14s] %-16s (%s)\n", tierDisplayName(r.Level), r.Label, state)
		for _, n := range r.Notes {
			fmt.Printf("                    %s\n", n)
		}
	}
}

func printDockProbe(probe map[string]any) {
	if probe != nil && probe["ok"] == true {
		fmt.Printf("\nLive probe: ✓ PASS — recalled %v fragment(s) in %vms (shards=%v)\n", probe["count"], probe["ttfcMs"], probe["shards"])
		if tok, _ := probe["receiptToken"].(string); tok != "" {
			verdict, _ := probe["verdict"].(string)
			verifyURL, _ := probe["verifyUrl"].(string)
			fmt.Printf("  receipt : %s (verdict=%s)\n", tok, verdict)
			if verifyURL != "" {
				fmt.Printf("  verify  : %s\n", verifyURL)
			}
		}
	} else if probe != nil {
		fmt.Printf("\nLive probe: ! WARN — daemon reachable but probe failed: %v\n", probe["note"])
	} else {
		fmt.Println("\nLive probe: • daemon not running yet — start with `nektar up` (or `nektar up --network main --confirm I_UNDERSTAND`)")
	}
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

	switch sub {
	case "session-start":
		if info, ok := detectDaemon(*dataDir, ""); ok {
			if out, err := callLiveTool(info, "recall", map[string]any{"limit": 8}); err == nil {
				if txt := formatHookMemoryBullets("nektar session-start memory", out); txt != "" {
					fmt.Print(txt)
				}
				return nil
			}
		}
		dir := strings.TrimSpace(*dataDir)
		if dir == "" {
			dir = defaultDataDir()
		}
		releaseLock, lockErr := acquireInstanceLock(dir, "hook-session-start")
		if lockErr != nil {
			if *verbose {
				fmt.Fprintf(os.Stderr, "[nektar session-start] skipped offline fallback (daemon lock busy): %v\n", lockErr)
			}
			return nil
		}
		defer releaseLock()
		ctx := context.Background()
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			if *verbose {
				fmt.Fprintf(os.Stderr, "[nektar session-start] offline open skipped: %v\n", err)
			}
			return nil
		}
		defer rt.closeFn()
		_, _ = rt.mem.EnsureWelcome(ctx)
		shards, items, _ := rt.mem.RecallAllShards(ctx, memory.RecallInput{Limit: 8})
		if txt := formatHookMemoryBullets("nektar session-start memory", map[string]any{"shards": shards, "items": items}); txt != "" {
			fmt.Print(txt)
		}
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
		if info, ok := detectDaemon(*dataDir, ""); ok {
			if out, err := callLiveTool(info, "recall", map[string]any{"query": q, "limit": 6}); err == nil {
				if txt := formatHookMemoryBullets("nektar prefetch", out); txt != "" {
					fmt.Print(txt)
				}
				return nil
			}
		} else {
			// Guarded offline fallback only when an existing SQLite DB is present and unlocked.
			dir := strings.TrimSpace(*dataDir)
			if dir == "" {
				dir = defaultDataDir()
			}
			if _, sErr := os.Stat(dir + string(os.PathSeparator) + "nektar.sqlite"); sErr == nil {
				if releaseLock, lockErr := acquireInstanceLock(dir, "hook-prefetch"); lockErr == nil {
					ctx := context.Background()
					if rt, rErr := openRuntime(ctx, "", *dataDir, DefaultLocalPort); rErr == nil {
						shards, items, _ := rt.mem.RecallAllShards(ctx, memory.RecallInput{Query: q, Limit: 6})
						rt.closeFn()
						if txt := formatHookMemoryBullets("nektar prefetch", map[string]any{"shards": shards, "items": items}); txt != "" {
							fmt.Print(txt)
						}
					}
					releaseLock()
					return nil
				}
			}
			if *verbose {
				fmt.Fprintln(os.Stderr, "[nektar prefetch] no running daemon — start `nektar up`")
			}
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
		info, live := detectDaemon(*dataDir, "")
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

	case "outcome":
		ofs := flag.NewFlagSet("hook outcome", flag.ExitOnError)
		exitCode := ofs.Int("exit-code", 0, "Task/test exit code (0 = success, non-zero = failure)")
		dealID := ofs.String("deal-id", "", "Optional settled deal ID (defaults to latest settled buyer deal)")
		note := ofs.String("note", "", "Optional short note describing outcome")
		oDataDir := ofs.String("data-dir", *dataDir, "Data directory override")
		_ = ofs.Parse(fs.Args())
		info, live := detectDaemon(*oDataDir, "")
		if !live || info == nil {
			return nil
		}
		out, err := callLiveAPI(info, http.MethodPost, "/v1/reputation/outcome", map[string]any{
			"dealId":   strings.TrimSpace(*dealID),
			"exitCode": *exitCode,
			"note":     strings.TrimSpace(*note),
		})
		if err != nil {
			if *verbose {
				fmt.Fprintf(os.Stderr, "[nektar hook outcome] %v\n", err)
			}
			return nil
		}
		return printJSON(out)

	default:
		return fmt.Errorf("unknown hook subcommand %q (use session-start, prefetch, verify-receipt, or outcome)", sub)
	}
}

// formatHookMemoryBullets renders recalled fragments as compact, model-friendly `[shard:seq]` bullet lines
// (matching Hermes/OpenClaw ambient context framing) instead of dumping raw JSON blobs into prompt context.
func formatHookMemoryBullets(header string, raw any) string {
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	var parsed struct {
		Items []struct {
			Shard string `json:"shard"`
			Seq   int64  `json:"seq"`
			Text  string `json:"text"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &parsed) != nil || len(parsed.Items) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s — verified memory; recorded != true]\n", header)
	for _, it := range parsed.Items {
		txt := strings.Join(strings.Fields(strings.TrimSpace(it.Text)), " ")
		if len(txt) > 220 {
			txt = txt[:220] + "..."
		}
		if txt == "" {
			continue
		}
		fmt.Fprintf(&sb, "- [%s:%d] %s\n", it.Shard, it.Seq, txt)
	}
	return sb.String()
}
