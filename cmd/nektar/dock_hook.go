package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/server"
)

func callLiveTool(info *server.DaemonInfo, tool string, args any) (any, error) {
	b, _ := json.Marshal(args)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(info.URL, "/")+"/v1/tools/"+tool, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if info.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+info.AuthToken)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "doctor", map[string]any{})
		if err == nil {
			return printJSON(map[string]any{"daemon": info, "diagnosis": out})
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
	return printJSON(tb.ExecDoctor(ctx))
}

func cmdPeers(args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "peers", map[string]any{"refresh": true})
		if err == nil {
			return printJSON(out)
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
	return printJSON(tb.ExecPeers(ctx, true))
}

func cmdWant(args []string) error {
	fs := flag.NewFlagSet("want", flag.ExitOnError)
	domains := fs.String("domains", "ops", "Comma-separated market domains")
	question := fs.String("question", "", "Knowledge question to buy")
	maxSats := fs.Uint64("max", 200, "Max satoshis to pay")
	targetShard := fs.String("shard", "", "Target shard to store purchased knowledge")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	if strings.TrimSpace(*question) == "" {
		return errors.New("provide -question \"...\"")
	}
	domList := []string{}
	for _, d := range strings.Split(*domains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domList = append(domList, d)
		}
	}
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "want", map[string]any{
			"domains":     domList,
			"question":    *question,
			"maxSats":     *maxSats,
			"targetShard": *targetShard,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	return errors.New("no live nektar daemon found — start one with `nektar serve` first")
}

func cmdSell(args []string) error {
	fs := flag.NewFlagSet("sell", flag.ExitOnError)
	shard := fs.String("shard", "default", "Local shard to sell")
	domains := fs.String("domains", "ops", "Comma-separated market domains")
	price := fs.Uint64("price", 100, "Price in satoshis")
	desc := fs.String("desc", "", "Short listing description")
	bountyID := fs.String("bounty", "", "Optional Hub bounty ID to sell directly TO the Hub")
	hubURL := fs.String("hub", "", "Optional Hub URL when submitting a bounty")
	seq := fs.Int64("seq", 0, "Optional fragment seq when submitting a Hub bounty")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	domList := []string{}
	for _, d := range strings.Split(*domains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domList = append(domList, d)
		}
	}
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if info, ok := server.DetectLiveDaemon(dir); ok {
		out, err := callLiveTool(info, "sell", map[string]any{
			"shard":       *shard,
			"domains":     domList,
			"priceSats":   *price,
			"description": *desc,
			"bountyId":    *bountyID,
			"hubUrl":      *hubURL,
			"seq":         *seq,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	return errors.New("no live nektar daemon found — start one with `nektar serve` first")
}

func cmdPolicy(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar policy [approve --preset buyer|seller|both | list | revoke <policyId>]")
	}
	sub := args[0]
	rest := args[1:]
	ctx := context.Background()

	switch sub {
	case "approve":
		fs := flag.NewFlagSet("policy approve", flag.ExitOnError)
		preset := fs.String("preset", "both", "Policy preset: buyer | seller | both")
		domains := fs.String("domains", "", "Optional comma-separated allowed domains")
		confirm := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND on mainnet")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)

		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		if rt.mem.Chain().Mainnet() && *confirm != "I_UNDERSTAND" {
			return errors.New("mainnet policy approval requires --confirm I_UNDERSTAND")
		}
		var doms []string
		for _, d := range strings.Split(*domains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				doms = append(doms, d)
			}
		}
		rec, err := market.ApprovePolicyPreset(rt.mem.DB(), rt.mkt.Policy(), *preset, doms, "cli")
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "list":
		fs := flag.NewFlagSet("policy list", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		list, err := rt.mem.DB().ListOperatorPolicies()
		if err != nil {
			return err
		}
		return printJSON(list)

	case "revoke":
		if len(rest) == 0 {
			return errors.New("usage: nektar policy revoke <policyId>")
		}
		policyID := rest[0]
		fs := flag.NewFlagSet("policy revoke", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest[1:])
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		if err := rt.mem.DB().RevokeOperatorPolicy(policyID); err != nil {
			return err
		}
		return printJSON(map[string]any{"revoked": policyID})

	default:
		return fmt.Errorf("unknown policy subcommand %q (use approve, list, or revoke)", sub)
	}
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)
	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	u := "http://127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	if info, ok := server.DetectLiveDaemon(dir); ok {
		u = info.URL
	}
	fmt.Printf("Opening Nektar Dashboard: %s\n", u)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// cmdDock detects installed AI agent clients, merges Nektar MCP config cleanly without overwriting
// existing entries, and runs a live `recall {}` probe to confirm end-to-end readiness.
func cmdDock(args []string) error {
	fs := flag.NewFlagSet("dock", flag.ExitOnError)
	target := fs.String("client", "auto", "Target client: auto|claude|cursor|windsurf|vscode|codex|hermes|proxy")
	writeCfg := fs.Bool("write", false, "Write/merge MCP entry directly into detected client JSON config files")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		*target = strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	}

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	liveURL := "http://127.0.0.1:" + envOr("NEKTAR_HTTP_PORT", DefaultLocalPort)
	var liveInfo *server.DaemonInfo
	if info, ok := server.DetectLiveDaemon(dir); ok {
		liveInfo = info
		liveURL = info.URL
	}
	exePath, err := os.Executable()
	if err != nil {
		exePath = "nektar"
	}

	home, _ := os.UserHomeDir()
	type clientTarget struct {
		Name string
		Path string
	}
	candidates := []clientTarget{
		{Name: "claude", Path: filepath.Join(home, ".claude.json")},
		{Name: "cursor", Path: filepath.Join(home, ".cursor", "mcp.json")},
		{Name: "windsurf", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")},
		{Name: "vscode", Path: filepath.Join(home, ".vscode", "mcp.json")},
	}

	if *target == "proxy" {
		fmt.Printf("Zero-code L0 Memory Proxy:\n  OPENAI_BASE_URL=%s/v1/proxy/openai/v1\n  ANTHROPIC_BASE_URL=%s/v1/proxy/anthropic\n", liveURL, liveURL)
		return nil
	}

	fmt.Printf("Nektar v%s Dock — binary: %s · daemon: %s\n\n", Version, exePath, liveURL)
	merged := 0
	for _, c := range candidates {
		if *target != "auto" && *target != c.Name {
			continue
		}
		_, statErr := os.Stat(filepath.Dir(c.Path))
		detected := statErr == nil
		if *writeCfg && (detected || *target == c.Name) {
			if err := mergeMCPJSON(c.Path, exePath); err != nil {
				fmt.Printf("  [!] %-10s %s (merge failed: %v)\n", c.Name, c.Path, err)
			} else {
				fmt.Printf("  [✓] %-10s wired -> %s\n", c.Name, c.Path)
				merged++
			}
		} else {
			status := "not detected"
			if detected {
				status = "detected (re-run with `nektar dock -write` to wire automatically)"
			}
			fmt.Printf("  [•] %-10s %s — %s\n", c.Name, c.Path, status)
		}
	}

	fmt.Printf(`
Universal MCP stdio snippet (auto-bridges to running daemon — zero lock contention!):
  { "mcpServers": { "nektar": { "command": %q, "args": ["mcp"] } } }

Streamable HTTP MCP / A2A / L0 Proxy / Dashboard:
  MCP:       %s/mcp
  A2A Card:  %s/.well-known/agent.json
  OpenAI L0: OPENAI_BASE_URL=%s/v1/proxy/openai/v1
  Dashboard: %s
`, exePath, liveURL, liveURL, liveURL, liveURL)

	// Run concrete live health probe (`recall {}`)!
	if liveInfo != nil {
		if out, err := callLiveTool(liveInfo, "recall", map[string]any{}); err == nil {
			if m, ok := out.(map[string]any); ok {
				fmt.Printf("\nLive probe: ✓ PASS — daemon %s responded to recall {} (count=%v, shards=%v)\n", liveInfo.URL, m["count"], m["shards"])
			}
		} else {
			fmt.Printf("\nLive probe: ! WARN — daemon reachable but recall failed: %v\n", err)
		}
	} else {
		fmt.Println("\nLive probe: • daemon not running yet — start with `nektar serve` (or `nektar serve --hub`)")
	}
	_ = merged
	return nil
}

func mergeMCPJSON(cfgPath, exePath string) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return err
	}
	root := make(map[string]any)
	if raw, err := os.ReadFile(cfgPath); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("parse existing JSON: %w", err)
		}
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}
	servers["nektar"] = map[string]any{
		"command": exePath,
		"args":    []string{"mcp"},
	}
	root["mcpServers"] = servers
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

var receiptRe = regexp.MustCompile(`nektarr1\.[a-zA-Z0-9_-]{20,}`)

// cmdHook provides native zero-Node.js Claude Code / Codex hooks:
//   - `nektar hook session-start`: prints verified memory summary to stdout
//   - `nektar hook prefetch`: reads user prompt from stdin, recalls matching fragments
//   - `nektar hook verify-receipt`: extracts any nektarr1.* token from stdin/args and verifies it!
func cmdHook(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar hook [session-start | prefetch | verify-receipt]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
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
			return nil // hooks never hard-crash a host session when unconfigured
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
		}
		return nil

	case "verify-receipt":
		stdinBytes, _ := io.ReadAll(io.LimitReader(os.Stdin, 512*1024))
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
		results := make([]any, 0, len(tokens))
		for _, tok := range tokens {
			if live {
				if out, err := callLiveTool(info, "verifyReceipt", map[string]any{"token": tok}); err == nil {
					results = append(results, out)
					continue
				}
			}
			ctx := context.Background()
			if rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort); err == nil {
				v, _ := rt.mem.VerifyReceipt(ctx, tok)
				rt.closeFn()
				results = append(results, v)
			}
		}
		return printJSON(map[string]any{"verified": results})

	default:
		return fmt.Errorf("unknown hook subcommand %q (use session-start, prefetch, or verify-receipt)", sub)
	}
}
