package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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

func callLiveAPI(info *server.DaemonInfo, method, path string, body any) (any, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(info.URL, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok := info.AuthToken
	if tok == "" {
		tok = info.Token
	}
	if opTok := strings.TrimSpace(os.Getenv("NEKTAR_OPERATOR_TOKEN")); opTok != "" {
		tok = opTok
	} else if rawOp, rErr := os.ReadFile(filepath.Join(info.DataDir, "operator-token")); rErr == nil {
		if s := strings.TrimSpace(string(rawOp)); s != "" {
			tok = s
		}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
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

func callLiveTool(info *server.DaemonInfo, tool string, args any) (any, error) {
	return callLiveAPI(info, http.MethodPost, "/v1/tools/"+tool, args)
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
			redacted := *info
			redacted.AuthToken = ""
			return printJSON(map[string]any{"daemon": &redacted, "diagnosis": out})
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
		aiMode := fs.String("ai-mode", "", "Optional Jev AI matching mode: off | advisory | gated | strict")
		aiPrivacy := fs.String("ai-privacy", "", "Optional seller privacy scope: metadata_only | disclosable_preview")
		aiHosts := fs.String("ai-hosts", "", "Optional comma-separated allowed AI egress hostnames")
		aiIntent := fs.String("ai-intent", "", "Optional semantic scope description for Jev policy-fit rubric")
		aiProhibit := fs.String("ai-prohibit", "", "Optional comma-separated prohibited topics for Jev matching")
		aiMinRel := fs.Int64("ai-min-relevance", 0, "Optional minimum relevance floor in basis points (0..10000)")
		aiEscConf := fs.Int64("ai-escalate-confidence", 0, "Optional confidence floor in bps below which deals escalate for approval")
		aiCedarFile := fs.String("ai-cedar-file", "", "Optional path to Cedar post-match market policy file")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)

		var doms []string
		for _, d := range strings.Split(*domains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				doms = append(doms, d)
			}
		}
		var aiPol *market.AIMatchingPolicy
		var hosts, prohib []string
		for _, h := range strings.Split(*aiHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		for _, p := range strings.Split(*aiProhibit, ",") {
			if p = strings.TrimSpace(p); p != "" {
				prohib = append(prohib, p)
			}
		}
		var cedarText string
		if strings.TrimSpace(*aiCedarFile) != "" {
			b, rErr := os.ReadFile(strings.TrimSpace(*aiCedarFile))
			if rErr != nil {
				return fmt.Errorf("read --ai-cedar-file: %w", rErr)
			}
			cedarText = string(b)
		}
		candAI := &market.AIMatchingPolicy{
			Mode:                 strings.TrimSpace(*aiMode),
			AllowedHosts:         hosts,
			SellerPrivacyScope:   strings.TrimSpace(*aiPrivacy),
			MinSellerRelevanceBP: *aiMinRel,
			MinBuyerRelevanceBP:  *aiMinRel,
			EscalateConfidenceBP: *aiEscConf,
			SemanticIntent:       strings.TrimSpace(*aiIntent),
			ProhibitedTopics:     prohib,
			CedarMarketPolicy:    cedarText,
		}
		if !candAI.IsEmpty() {
			if err := candAI.Validate(); err != nil {
				return err
			}
			aiPol = candAI
		}

		dir := *dataDir
		if dir == "" {
			dir = defaultDataDir()
		}
		if info, ok := server.DetectLiveDaemon(dir); ok {
			payload := map[string]any{
				"preset":  *preset,
				"domains": doms,
				"confirm": *confirm,
			}
			if aiPol != nil {
				payload["aiMatching"] = aiPol
			}
			out, err := callLiveAPI(info, http.MethodPost, "/v1/onboard/policy-preset", payload)
			if err != nil {
				return err
			}
			return printJSON(out)
		}

		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		if rt.mem.Chain().Mainnet() && *confirm != "I_UNDERSTAND" {
			return errors.New("mainnet policy approval requires --confirm I_UNDERSTAND")
		}
		rec, err := market.ApprovePolicyPresetWithAI(rt.mem.DB(), rt.mkt.Policy(), *preset, doms, "cli", aiPol)
		if err != nil {
			return err
		}
		return printJSON(rec)

	case "list":
		fs := flag.NewFlagSet("policy list", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		dir := *dataDir
		if dir == "" {
			dir = defaultDataDir()
		}
		if info, ok := server.DetectLiveDaemon(dir); ok {
			if out, err := callLiveAPI(info, http.MethodGet, "/v1/policies", nil); err == nil {
				return printJSON(out)
			}
		}
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
		dir := *dataDir
		if dir == "" {
			dir = defaultDataDir()
		}
		if info, ok := server.DetectLiveDaemon(dir); ok {
			if out, err := callLiveAPI(info, http.MethodDelete, "/v1/policies/"+policyID, nil); err == nil {
				return printJSON(out)
			}
		}
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
		u = strings.TrimRight(info.URL, "/")
		if out, err := callLiveAPI(info, http.MethodPost, "/v1/auth/pair-code", map[string]any{}); err == nil {
			if m, ok := out.(map[string]any); ok {
				if code, _ := m["code"].(string); code != "" {
					u = u + "/pair?code=" + code
				}
			}
		}
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
	fmt.Printf("Nektar v%s dock%s — binary: %s · daemon: %s\n\n", Version, modeTag, exePath, liveURL)
	if len(results) == 0 {
		fmt.Printf("  no AI clients auto-detected in %s — pass explicit names: nektar dock claude cursor vscode hermes openclaw codex proxy\n", home)
	} else {
		for _, r := range results {
			state := "unchanged"
			if r.Changed {
				if *dry {
					state = "would update"
				} else if *uninstall {
					state = "removed"
				} else {
					state = "updated"
				}
			}
			fmt.Printf("  [%s] %-16s (%s)\n", r.Level, r.Label, state)
			for _, n := range r.Notes {
				fmt.Printf("         %s\n", n)
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
L0 Zero-Code LLM Memory Proxy (works with ANY OpenAI/Anthropic agent):
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
			fmt.Println("\nLive probe: • daemon not running yet — start with `nektar serve`")
		}
	}
	return nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func claudeDesktopPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		return filepath.Join(appData, "Claude", "claude_desktop_config.json")
	default:
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}

func vscodeMCPPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
	case "windows":
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		return filepath.Join(appData, "Code", "User", "mcp.json")
	default:
		return filepath.Join(home, ".config", "Code", "User", "mcp.json")
	}
}

func zedSettingsPath(home string) string {
	if runtime.GOOS == "windows" {
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		return filepath.Join(appData, "Zed", "settings.json")
	}
	return filepath.Join(home, ".config", "zed", "settings.json")
}

func clineMCPPath(home string) string {
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	case "windows":
		appData := envOr("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		base = filepath.Join(appData, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	default:
		base = filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings")
	}
	return filepath.Join(base, "cline_mcp_settings.json")
}

func mergeOrRemoveJSONEntry(cfgPath, rootKey, entryKey string, val any, dry, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(cfgPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, fmt.Errorf("parse existing JSON: %w", jErr)
		}
	} else if uninstall {
		return false, nil
	}
	section, _ := root[rootKey].(map[string]any)
	if uninstall {
		if section == nil {
			return false, nil
		}
		if _, ok := section[entryKey]; !ok {
			return false, nil
		}
		delete(section, entryKey)
		root[rootKey] = section
	} else {
		if section == nil {
			section = make(map[string]any)
		}
		section[entryKey] = val
		root[rootKey] = section
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if bytes.Equal(raw, out) {
		return false, nil
	}
	if dry {
		return true, nil
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

func wireClaudeHooks(settingsPath, exePath string, dry, uninstall bool) (bool, error) {
	root := make(map[string]any)
	raw, err := os.ReadFile(settingsPath)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if jErr := json.Unmarshal(raw, &root); jErr != nil {
			return false, jErr
		}
	} else if uninstall {
		return false, nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}
	desired := map[string]string{
		"SessionStart":     fmt.Sprintf("%q hook session-start", exePath),
		"UserPromptSubmit": fmt.Sprintf("%q hook prefetch", exePath),
		"Stop":             fmt.Sprintf("%q hook verify-receipt", exePath),
	}
	for event, cmd := range desired {
		existing, _ := hooks[event].([]any)
		var filtered []any
		for _, item := range existing {
			b, _ := json.Marshal(item)
			if strings.Contains(string(b), "hook session-start") || strings.Contains(string(b), "hook prefetch") || strings.Contains(string(b), "hook verify-receipt") {
				continue
			}
			filtered = append(filtered, item)
		}
		if !uninstall {
			filtered = append(filtered, map[string]any{
				"hooks": []map[string]any{{"type": "command", "command": cmd}},
			})
		}
		if len(filtered) > 0 {
			hooks[event] = filtered
		} else {
			delete(hooks, event)
		}
	}
	root["hooks"] = hooks
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if bytes.Equal(raw, out) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(settingsPath, out, 0o600)
}

func wireCodexToml(cfgPath, exePath string, dry, uninstall bool) (bool, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	const startMark = "# BEGIN NEKTAR MCP"
	const endMark = "# END NEKTAR MCP"
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(startMark) + `.*?` + regexp.QuoteMeta(endMark) + `\n?`)
	cleaned := re.ReplaceAllString(content, "")
	var next string
	if uninstall {
		next = cleaned
	} else {
		block := fmt.Sprintf("%s\n[mcp_servers.nektar]\ncommand = %q\nargs = [\"mcp\"]\n%s\n", startMark, exePath, endMark)
		next = strings.TrimRight(cleaned, "\n") + "\n\n" + block
		if strings.TrimSpace(cleaned) == "" {
			next = block
		}
	}
	if next == content {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(cfgPath, []byte(next), 0o600)
}

func wireGooseYaml(cfgPath, exePath string, dry, uninstall bool) (bool, error) {
	raw, _ := os.ReadFile(cfgPath)
	content := string(raw)
	const startMark = "# BEGIN NEKTAR MCP"
	const endMark = "# END NEKTAR MCP"
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(startMark) + `.*?` + regexp.QuoteMeta(endMark) + `\n?`)
	cleaned := re.ReplaceAllString(content, "")
	var next string
	if uninstall {
		next = cleaned
	} else {
		block := fmt.Sprintf("%s\nextensions:\n  nektar:\n    name: nektar\n    type: stdio\n    cmd: %q\n    args: [\"mcp\"]\n    enabled: true\n%s\n", startMark, exePath, endMark)
		next = strings.TrimRight(cleaned, "\n") + "\n\n" + block
		if strings.TrimSpace(cleaned) == "" {
			next = block
		}
	}
	if next == content {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(cfgPath, []byte(next), 0o600)
}

func locateBundledAsset(rel string) string {
	cands := []string{filepath.Join("/usr/local/share/nektar", rel)}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands, filepath.Join(d, rel), filepath.Join(filepath.Dir(d), rel))
	}
	if cwd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(cwd, rel))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func installSkillFile(dst, liveURL string, dry, uninstall bool) (bool, error) {
	if uninstall {
		if _, err := os.Stat(dst); err != nil {
			return false, nil
		}
		if dry {
			return true, nil
		}
		return true, os.Remove(dst)
	}
	var content []byte
	if src := locateBundledAsset(filepath.Join("skills", "nektar-memory", "SKILL.md")); src != "" {
		content, _ = os.ReadFile(src)
	}
	if len(content) == 0 && liveURL != "" {
		if resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(liveURL + "/v1/adapters/skill.md"); err == nil {
			if resp.StatusCode == http.StatusOK {
				content, _ = io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			}
			resp.Body.Close()
		}
	}
	if len(content) == 0 {
		return false, nil
	}
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, content, 0o644)
}

func dockNativeBundle(name, label, dstDir, liveURL string, dry, uninstall bool) dockClientResult {
	res := dockClientResult{Client: name, Label: label, Level: "L3", Files: []string{dstDir}}
	if uninstall {
		if !dirExists(dstDir) {
			return res
		}
		res.Changed = true
		if !dry {
			_ = os.RemoveAll(dstDir)
		}
		res.Notes = append(res.Notes, "removed "+dstDir)
		return res
	}
	if src := locateBundledAsset(filepath.Join("integrations", name)); src != "" {
		if dry {
			res.Changed = true
			res.Notes = append(res.Notes, "would copy "+src+" -> "+dstDir)
			return res
		}
		if err := copyTreeClean(src, dstDir); err != nil {
			res.Notes = append(res.Notes, "copy error: "+err.Error())
		} else {
			res.Changed = true
			res.Notes = append(res.Notes, dstDir)
		}
		return res
	}
	// Fallback: fetch bundle from live daemon /v1/adapters/bundle/{name}
	if liveURL != "" {
		if resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(liveURL + "/v1/adapters/bundle/" + name); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var bundle struct {
					Files []struct {
						Path    string `json:"path"`
						Content string `json:"content"`
					} `json:"files"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&bundle) == nil && len(bundle.Files) > 0 {
					res.Changed = true
					if dry {
						res.Notes = append(res.Notes, fmt.Sprintf("would fetch %d file(s) -> %s", len(bundle.Files), dstDir))
						return res
					}
					for _, f := range bundle.Files {
						clean := filepath.Clean(filepath.FromSlash(f.Path))
						if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
							continue
						}
						full := filepath.Join(dstDir, clean)
						_ = os.MkdirAll(filepath.Dir(full), 0o750)
						_ = os.WriteFile(full, []byte(f.Content), 0o644)
					}
					res.Notes = append(res.Notes, fmt.Sprintf("installed %d file(s) -> %s", len(bundle.Files), dstDir))
					return res
				}
			}
		}
	}
	res.Notes = append(res.Notes, "start `nektar serve` or install plugin from https://github.com/kryptokrates/nektar/tree/master/integrations/"+name)
	return res
}

func copyTreeClean(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			if base == "__pycache__" || base == "node_modules" || base == ".pytest_cache" || base == ".git" || base == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, ".pyo") {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, p)
		dst := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o644)
	})
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
			if live {
				res, _ = callLiveTool(info, "verifyReceipt", map[string]any{"token": tok})
			} else {
				ctx := context.Background()
				if rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort); err == nil {
					res, _ = rt.mem.VerifyReceipt(ctx, tok)
					rt.closeFn()
				}
			}
			if res != nil {
				results = append(results, res)
				b, _ := json.Marshal(res)
				var v struct {
					OK      bool   `json:"ok"`
					Verdict string `json:"verdict"`
					Reason  string `json:"reason"`
				}
				if json.Unmarshal(b, &v) == nil && (!v.OK || v.Verdict == "red") {
					failedReasons = append(failedReasons, fmt.Sprintf("%s: %s", tok, v.Reason))
				}
			}
		}
		if len(failedReasons) > 0 && !*codexMode {
			return printJSON(map[string]any{
				"decision": "block",
				"reason":   "Nektar receipt verification failed: " + strings.Join(failedReasons, "; ") + " — re-ground via groundedAnswer or state answer is ungrounded.",
				"verified": results,
			})
		}
		return printJSON(map[string]any{"verified": results})

	default:
		return fmt.Errorf("unknown hook subcommand %q (use session-start, prefetch, or verify-receipt)", sub)
	}
}
